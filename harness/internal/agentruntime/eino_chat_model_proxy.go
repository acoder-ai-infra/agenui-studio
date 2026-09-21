package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

var (
	ErrEinoChatModelInvokerMissing = errors.New("eino chat model invoker missing")
	ErrEinoChatModelPackageInvalid = errors.New("eino chat model context package invalid")
	ErrEinoChatModelStreamInvalid  = errors.New("eino chat model stream invalid")
	ErrEinoChatModelEventInvalid   = errors.New("eino chat model event invalid")
)

// EinoChatModelProxy implements Eino's immutable tool-calling model contract
// while delegating every model round to the Harness ModelInvoker port.
type EinoChatModelProxy struct {
	invoker ModelInvoker
	pkg     ModelContextPackage
	tools   []ModelToolDefinition
	round   *atomic.Uint64
}

var _ model.ToolCallingChatModel = (*EinoChatModelProxy)(nil)

func NewEinoChatModelProxy(invoker ModelInvoker, pkg ModelContextPackage, governors ...ModelInputGovernor) (*EinoChatModelProxy, error) {
	if invoker == nil {
		return nil, ErrEinoChatModelInvokerMissing
	}
	if pkg.SchemaVersion != ModelContextPackageSchemaVersion || pkg.PackageID == "" || validateModelContextIntegrity(pkg) != nil {
		return nil, ErrEinoChatModelPackageInvalid
	}
	governor := ModelInputGovernor(NewDefaultModelInputGovernor(nil))
	if len(governors) > 0 && governors[0] != nil {
		governor = governors[0]
	}
	return &EinoChatModelProxy{invoker: NewGovernedModelInvoker(invoker, governor), pkg: pkg, round: &atomic.Uint64{}}, nil
}

func (p *EinoChatModelProxy) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	if p == nil || p.invoker == nil {
		return nil, ErrEinoChatModelInvokerMissing
	}
	converted, err := frozenModelToolDefinitions(p.pkg, tools)
	if err != nil {
		return nil, err
	}
	return &EinoChatModelProxy{invoker: p.invoker, pkg: p.pkg, tools: converted, round: p.round}, nil
}

func (p *EinoChatModelProxy) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	stream, err := p.invoke(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	messages := make([]*schema.Message, 0, 4)
	for item := range stream {
		if item.err != nil {
			return nil, item.err
		}
		messages = append(messages, item.message)
	}
	if streamErr := streamError(messages); streamErr != nil {
		return nil, streamErr
	}
	if len(messages) == 0 {
		return nil, ErrEinoChatModelStreamInvalid
	}
	return schema.ConcatMessages(messages)
}

func (p *EinoChatModelProxy) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := p.invoke(streamCtx, input, opts...)
	if err != nil {
		cancel()
		return nil, err
	}
	reader, writer := schema.Pipe[*schema.Message](16)
	go func() {
		defer writer.Close()
		defer cancel()
		for {
			select {
			case <-streamCtx.Done():
				writer.Send(nil, streamCtx.Err())
				return
			case message, ok := <-stream:
				if !ok {
					return
				}
				if message.err != nil {
					writer.Send(nil, message.err)
					return
				}
				if writer.Send(message.message, nil) {
					return
				}
			}
		}
	}()
	return reader, nil
}

type einoModelMessage struct {
	message *schema.Message
	err     error
}

func (p *EinoChatModelProxy) invoke(ctx context.Context, input []*schema.Message, opts ...model.Option) (<-chan einoModelMessage, error) {
	if p == nil || p.invoker == nil {
		return nil, ErrEinoChatModelInvokerMissing
	}
	emitter, ok := RuntimeEventEmitterFrom(ctx)
	if !ok {
		return nil, ErrRuntimeEventEmitterMissing
	}
	common := model.GetCommonOptions(nil, opts...)
	tools := p.tools
	if common.Tools != nil {
		converted, err := p.WithTools(common.Tools)
		if err != nil {
			return nil, err
		}
		tools = converted.(*EinoChatModelProxy).tools
	}
	round := int(p.round.Add(1))
	messages := toModelCallMessages(input)
	callPackage := deriveModelCallPackage(p.pkg, round, messages, tools)
	req := ModelInvokeRequest{
		Trace:      observabilityTrace(ctx),
		Package:    callPackage,
		Round:      round,
		AllowTools: len(tools) > 0,
		Messages:   messages,
		Tools:      cloneModelToolDefinitions(tools),
		Options:    mergeModelCallOptions(callPackage.RuntimeConstraints.ModelOptions, toModelCallOptions(common)),
	}
	// BeforeModelHook 经 ctx 下发的本轮 ToolChoice 覆盖（eino 路径 ToolChoice
	// 不在 state 中，故走 ctx）。仅非空时覆盖。
	if choice, ok := ModelCallToolChoiceFrom(ctx); ok {
		req.Options.ToolChoice = choice
	}
	if manifest, ok := preserveManifestFromContext(ctx); ok {
		req.PreserveManifest = manifest
		_, policyErr := NormalizeContextCompactionPolicy(callPackage.RuntimeConstraints.CompactionPolicy)
		if policyErr != nil {
			return nil, policyErr
		}
		if compaction, exists := einoPreModelCompactionFromContext(ctx); exists {
			req.PreModelCompaction = compaction
		}
	}
	items, err := p.invoker.Invoke(ctx, req)
	if err != nil {
		return nil, err
	}
	if items == nil {
		return nil, ErrEinoChatModelStreamInvalid
	}
	out := make(chan einoModelMessage, 16)
	go func() {
		defer close(out)
		var pendingToolMessages []*schema.Message
		for {
			select {
			case <-ctx.Done():
				sendEinoModelMessage(ctx, out, einoModelMessage{err: ctx.Err()})
				return
			case item, open := <-items:
				if !open {
					if len(pendingToolMessages) > 0 {
						sendEinoModelMessage(ctx, out, einoModelMessage{err: fmt.Errorf("%w: model stream ended before tool-call completion was committed", ErrEinoChatModelEventInvalid)})
					}
					return
				}
				message := modelStreamItemToEino(item, tools, string(p.pkg.Run.RuntimeMode))
				isToolCall := item.ToolCall != nil
				if isToolCall {
					if message != nil {
						pendingToolMessages = append(pendingToolMessages, message)
					}
				}
				if item.Event.EventType != "" {
					if err := emitEinoModelEvent(ctx, emitter, item.Event); err != nil {
						sendEinoModelMessage(ctx, out, einoModelMessage{err: err})
						return
					}
					if item.Event.Error != nil {
						sendEinoModelMessage(ctx, out, einoModelMessage{err: fmt.Errorf("%w: %s", ErrEinoChatModelEventInvalid, item.Event.Error.Message)})
						return
					}
					if item.Event.EventType == observability.EventModelCallCompleted {
						for _, pending := range pendingToolMessages {
							if !sendEinoModelMessage(ctx, out, einoModelMessage{message: pending}) {
								return
							}
						}
						pendingToolMessages = nil
					}
				}
				if isToolCall {
					// A tool-call message is executable input to Eino. Keep it
					// behind the durable model-completion boundary above.
					continue
				}
				if message != nil && !sendEinoModelMessage(ctx, out, einoModelMessage{message: message}) {
					return
				}
			}
		}
	}()
	return out, nil
}

func emitEinoModelEvent(ctx context.Context, emitter RuntimeEventEmitter, event observability.AgentEvent) error {
	if isBackpressureDroppableModelEvent(event.EventType) {
		if tryEmitter, ok := emitter.(RuntimeEventTryEmitter); ok {
			err := tryEmitter.TryEmit(ctx, event)
			if errors.Is(err, ErrRuntimeEventBridgeFull) {
				return nil
			}
			return err
		}
	}
	return emitter.Emit(ctx, event)
}

func isBackpressureDroppableModelEvent(eventType observability.EventType) bool {
	return eventType == observability.EventModelTokenDelta || eventType == observability.EventModelThoughtDelta
}

func deriveModelCallPackage(base ModelContextPackage, round int, messages []ModelCallMessage, tools []ModelToolDefinition) ModelContextPackage {
	derived := base
	derived.PackageID = base.PackageID + ".round." + strconv.Itoa(round)
	derived.CreatedAt = time.Now()
	derived.Messages.ConversationWindow = modelContextMessagesFromCalls(messages)
	derived.Messages.TokenCount = estimateModelCallTokens(messages, tools)
	derived.Capabilities.ToolDefinitions = cloneModelToolDefinitions(tools)
	derived.ContextHash = modelContextHash(derived)
	return derived
}

func modelContextMessagesFromCalls(messages []ModelCallMessage) []ModelContextMessage {
	out := make([]ModelContextMessage, 0, len(messages))
	for _, message := range messages {
		converted := ModelContextMessage{
			Role:               message.Role,
			Content:            message.Content,
			ContentParts:       append([]ModelContentPart(nil), message.ContentParts...),
			Name:               message.Name,
			ToolCallID:         message.ToolCallID,
			ToolName:           message.ToolName,
			ReasoningContent:   message.ReasoningContent,
			ReasoningSignature: message.ReasoningSignature,
		}
		for _, call := range message.ToolCalls {
			arguments := make(map[string]any)
			if len(call.Arguments) > 0 {
				if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
					arguments = map[string]any{"_raw": string(call.Arguments)}
				}
			}
			converted.ToolCalls = append(converted.ToolCalls, contextpkg.ToolCall{ID: call.ToolCallID, Name: call.Name, Arguments: arguments})
		}
		if message.Role == string(schema.Tool) {
			converted.ToolResult = &contextpkg.ToolResult{CallID: message.ToolCallID, Name: message.ToolName, Content: message.Content}
		}
		out = append(out, converted)
	}
	return out
}

func estimateModelCallTokens(messages []ModelCallMessage, tools []ModelToolDefinition) int {
	counter := contextpkg.EstimateCounter{}
	total := 0
	for _, message := range messages {
		data, _ := json.Marshal(message)
		total += counter.Count(string(data)) + 4
	}
	for _, tool := range tools {
		data, _ := json.Marshal(tool)
		total += counter.Count(string(data)) + 8
	}
	return total
}

func modelStreamItemToEino(item ModelStreamItem, tools []ModelToolDefinition, runtimeMode string) *schema.Message {
	if item.TextDelta != "" || item.ReasoningDelta != "" || item.ReasoningSignature != "" {
		message := &schema.Message{Role: schema.Assistant, Content: item.TextDelta, ReasoningContent: item.ReasoningDelta}
		if item.ReasoningDelta != "" || item.ReasoningSignature != "" {
			message.AssistantGenMultiContent = []schema.MessageOutputPart{{
				Type:      schema.ChatMessagePartTypeReasoning,
				Reasoning: &schema.MessageOutputReasoning{Text: item.ReasoningDelta, Signature: item.ReasoningSignature},
			}}
		}
		return message
	}
	if item.ToolCall != nil {
		if !modelToolCallAuthorized(tools, *item.ToolCall) {
			return &schema.Message{Role: schema.Assistant, Content: unauthorizedModelToolCallMessage(item.ToolCall.Name, tools, runtimeMode)}
		}
		return &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			ID: item.ToolCall.ToolCallID, Type: "function",
			Function: schema.FunctionCall{Name: item.ToolCall.Name, Arguments: string(item.ToolCall.Arguments)},
		}}}
	}
	return nil
}

func modelToolCallAuthorized(tools []ModelToolDefinition, call ModelToolCall) bool {
	name, _ := splitVersionedRef(call.Name)
	for _, tool := range tools {
		if tool.Name != name && tool.Name != call.Name {
			continue
		}
		return true
	}
	return false
}

func unauthorizedModelToolCallMessage(name string, tools []ModelToolDefinition, runtimeMode string) string {
	available := make([]string, 0, len(tools))
	for _, tool := range tools {
		if tool.Name != "" {
			available = append(available, tool.Name)
		}
	}
	if len(available) == 0 {
		return fmt.Sprintf("The requested tool %q is not installed or authorized for this run. I cannot call it. Continue with the available information, or ask the user to install/authorize the required capability.", name)
	}
	return fmt.Sprintf("The requested tool %q is not installed or authorized for this run. Available runtime tools for %s are: %s. Continue with the available information, or ask the user to install/authorize the required capability.", name, runtimeMode, strings.Join(available, ", "))
}

func toModelCallMessages(input []*schema.Message) []ModelCallMessage {
	out := make([]ModelCallMessage, 0, len(input))
	for _, message := range input {
		if message == nil {
			continue
		}
		calls := make([]ModelToolCall, 0, len(message.ToolCalls))
		for _, call := range message.ToolCalls {
			calls = append(calls, ModelToolCall{ToolCallID: call.ID, Name: call.Function.Name, Arguments: json.RawMessage(call.Function.Arguments)})
		}
		out = append(out, ModelCallMessage{Role: string(message.Role), Content: message.Content, ContentParts: toModelContentParts(message.UserInputMultiContent), Name: message.Name, ToolCalls: calls, ToolCallID: message.ToolCallID, ToolName: message.ToolName, ReasoningContent: message.ReasoningContent, ReasoningSignature: einoReasoningSignature(message)})
	}
	return out
}

func toModelContentParts(input []schema.MessageInputPart) []ModelContentPart {
	out := make([]ModelContentPart, 0, len(input))
	for _, part := range input {
		converted := ModelContentPart{Type: string(part.Type), Text: part.Text}
		switch {
		case part.Image != nil:
			converted.URL, converted.Base64Data, converted.MIMEType = modelPartCommon(part.Image.MessagePartCommon)
		case part.Audio != nil:
			converted.URL, converted.Base64Data, converted.MIMEType = modelPartCommon(part.Audio.MessagePartCommon)
		case part.Video != nil:
			converted.URL, converted.Base64Data, converted.MIMEType = modelPartCommon(part.Video.MessagePartCommon)
		case part.File != nil:
			// eino 的 file part 类型串为 "file_url"，归一为 runtime canonical
			// 的 "file"（与 gateway ContentPart 对齐，G-C）。
			converted.Type = "file"
			converted.URL, converted.Base64Data, converted.MIMEType = modelPartCommon(part.File.MessagePartCommon)
			converted.Name = part.File.Name
		}
		out = append(out, converted)
	}
	return out
}

func modelPartCommon(common schema.MessagePartCommon) (url, base64Data, mimeType string) {
	if common.URL != nil {
		url = *common.URL
	}
	if common.Base64Data != nil {
		base64Data = *common.Base64Data
	}
	return url, base64Data, common.MIMEType
}

func toModelCallOptions(options *model.Options) ModelCallOptions {
	if options == nil {
		return ModelCallOptions{}
	}
	result := ModelCallOptions{Model: cloneStringPointer(options.Model), Temperature: cloneFloat32Pointer(options.Temperature), TopP: cloneFloat32Pointer(options.TopP), MaxTokens: cloneIntPointer(options.MaxTokens), Stop: append([]string(nil), options.Stop...), AllowedToolNames: append([]string(nil), options.AllowedToolNames...)}
	if options.ToolChoice != nil {
		result.ToolChoice = canonicalToolChoice(*options.ToolChoice)
	}
	return result
}

func canonicalToolChoice(choice schema.ToolChoice) string {
	switch choice {
	case schema.ToolChoiceAllowed:
		return "auto"
	case schema.ToolChoiceForbidden:
		return "none"
	case schema.ToolChoiceForced:
		return "required"
	default:
		return string(choice)
	}
}

func mergeModelCallOptions(base, override ModelCallOptions) ModelCallOptions {
	result := cloneModelCallOptions(base)
	if override.Model != nil {
		result.Model = cloneStringPointer(override.Model)
	}
	if override.Temperature != nil {
		result.Temperature = cloneFloat32Pointer(override.Temperature)
	}
	if override.TopP != nil {
		result.TopP = cloneFloat32Pointer(override.TopP)
	}
	if override.MaxTokens != nil {
		result.MaxTokens = cloneIntPointer(override.MaxTokens)
	}
	if len(override.Fallback) > 0 {
		result.Fallback = append([]string(nil), override.Fallback...)
	}
	if len(override.Stop) > 0 {
		result.Stop = append([]string(nil), override.Stop...)
	}
	if override.ToolChoice != "" {
		result.ToolChoice = override.ToolChoice
	}
	if len(override.AllowedToolNames) > 0 {
		result.AllowedToolNames = append([]string(nil), override.AllowedToolNames...)
	}
	if override.ReasoningMode != "" {
		result.ReasoningMode = override.ReasoningMode
	}
	if override.ReasoningBudget > 0 {
		result.ReasoningBudget = override.ReasoningBudget
	}
	if override.ReasoningEffort != "" {
		result.ReasoningEffort = override.ReasoningEffort
	}
	return result
}

func einoReasoningSignature(message *schema.Message) string {
	for i := len(message.AssistantGenMultiContent) - 1; i >= 0; i-- {
		part := message.AssistantGenMultiContent[i]
		if part.Type == schema.ChatMessagePartTypeReasoning && part.Reasoning != nil && part.Reasoning.Signature != "" {
			return part.Reasoning.Signature
		}
	}
	return ""
}

func cloneModelCallOptions(input ModelCallOptions) ModelCallOptions {
	input.Model = cloneStringPointer(input.Model)
	input.Fallback = append([]string(nil), input.Fallback...)
	input.Temperature = cloneFloat32Pointer(input.Temperature)
	input.TopP = cloneFloat32Pointer(input.TopP)
	input.MaxTokens = cloneIntPointer(input.MaxTokens)
	input.Stop = append([]string(nil), input.Stop...)
	input.AllowedToolNames = append([]string(nil), input.AllowedToolNames...)
	return input
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneFloat32Pointer(value *float32) *float32 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneModelToolDefinitions(input []ModelToolDefinition) []ModelToolDefinition {
	out := make([]ModelToolDefinition, len(input))
	for i, tool := range input {
		out[i] = tool
		out[i].Schema = append(json.RawMessage(nil), tool.Schema...)
	}
	return out
}

func observabilityTrace(ctx context.Context) observability.TraceContext {
	return observability.MustTraceContext(ctx)
}

func sendEinoModelMessage(ctx context.Context, out chan<- einoModelMessage, message einoModelMessage) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- message:
		return true
	}
}

func streamError(messages []*schema.Message) error {
	for _, message := range messages {
		if message == nil {
			return ErrEinoChatModelStreamInvalid
		}
	}
	return nil
}
