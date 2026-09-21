package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
)

var ErrModelInputBudgetExceeded = errors.New("final model input budget exceeded")
var ErrModelInputCompressionInvalid = errors.New("model input compression violated preserve rules")

// ModelInputGovernor is the final, runtime-independent boundary before a
// provider request. Implementations may compact input, but must fail closed
// when the final serialized request still exceeds its input budget.
type ModelInputGovernor interface {
	Govern(ctx context.Context, req ModelInvokeRequest) (ModelInvokeRequest, ModelInputGovernance, error)
}

type ModelInputGovernance struct {
	OriginalTokens    int                       `json:"original_tokens"`
	FinalTokens       int                       `json:"final_tokens"`
	TrimmedMessages   int                       `json:"trimmed_messages,omitempty"`
	AppliedStrategies []string                  `json:"applied_strategies,omitempty"`
	CompactionRecords []ContextCompactionRecord `json:"compaction_records,omitempty"`
}

// ModelInputCompressionStrategy performs one request-local emergency reduction.
// Stateful summarization and durable context replacement belong in Context
// Engine or a runtime-native pre-model middleware. These final strategies are
// ordered and configurable, but must never truncate protocol structures or
// pinned semantics in place.
type ModelInputCompressionStrategy interface {
	Name() string
	Compress(ctx context.Context, req ModelInputCompressionRequest) (ModelInvokeRequest, int, error)
}

type ModelInputCompressionRequest struct {
	Request        ModelInvokeRequest
	Counter        ModelInputTokenCounter
	CurrentTokens  int
	MaxInputTokens int
}

// ModelInputTokenCounter can be replaced by a provider tokenizer at the model
// gateway boundary. EstimateModelInputTokenCounter is a conservative P0
// fallback and is not a substitute for provider-final tokenization.
type ModelInputTokenCounter interface {
	Count(ctx context.Context, messages []ModelCallMessage, tools []ModelToolDefinition) (int, error)
}

type EstimateModelInputTokenCounter struct{}

func (EstimateModelInputTokenCounter) Count(_ context.Context, messages []ModelCallMessage, tools []ModelToolDefinition) (int, error) {
	return estimateModelCallTokens(messages, tools), nil
}

type DefaultModelInputGovernor struct {
	Counter    ModelInputTokenCounter
	Strategies []ModelInputCompressionStrategy
}

func NewDefaultModelInputGovernor(counter ModelInputTokenCounter) *DefaultModelInputGovernor {
	if counter == nil {
		counter = EstimateModelInputTokenCounter{}
	}
	return &DefaultModelInputGovernor{
		Counter:    counter,
		Strategies: []ModelInputCompressionStrategy{CompleteOldTurnCompressionStrategy{}},
	}
}

func (g *DefaultModelInputGovernor) Govern(ctx context.Context, req ModelInvokeRequest) (ModelInvokeRequest, ModelInputGovernance, error) {
	counter := g.Counter
	if counter == nil {
		counter = EstimateModelInputTokenCounter{}
	}
	req = materializeModelInvokeRequest(req)
	messages := req.Messages
	if err := validateModelToolPairing(messages); err != nil {
		return ModelInvokeRequest{}, ModelInputGovernance{}, fmt.Errorf("%w: %v", ErrModelInputCompressionInvalid, err)
	}
	manifest := req.PreserveManifest
	var err error
	if manifest.ManifestHash == "" {
		manifest, err = BuildPreserveManifest(req)
		if err != nil {
			return ModelInvokeRequest{}, ModelInputGovernance{}, err
		}
	}
	if err := ValidatePreserveManifest(manifest, req); err != nil {
		return ModelInvokeRequest{}, ModelInputGovernance{}, err
	}
	req.PreserveManifest = manifest
	tools := req.Tools
	original, err := counter.Count(ctx, messages, tools)
	if err != nil {
		return ModelInvokeRequest{}, ModelInputGovernance{}, fmt.Errorf("count final model input: %w", err)
	}
	result := ModelInputGovernance{OriginalTokens: original, FinalTokens: original}
	limit := req.Package.RuntimeConstraints.TokenBudget.MaxInputTokens
	if limit <= 0 || original <= limit {
		req.Messages = messages
		req.Tools = tools
		req.Package = updateModelInputPackage(req.Package, messages, tools, original, 0)
		return req, result, nil
	}

	req.Messages = messages
	req.Tools = tools
	current := original
	for _, strategy := range g.Strategies {
		if strategy == nil || current <= limit {
			continue
		}
		before := len(req.Messages)
		compressed, tokens, compressErr := strategy.Compress(ctx, ModelInputCompressionRequest{
			Request: req, Counter: counter, CurrentTokens: current, MaxInputTokens: limit,
		})
		if compressErr != nil {
			return ModelInvokeRequest{}, result, fmt.Errorf("compress final model input with %s: %w", strategy.Name(), compressErr)
		}
		compressed.PreserveManifest = manifest
		if validateErr := ValidatePreserveManifest(manifest, compressed); validateErr != nil {
			return ModelInvokeRequest{}, result, fmt.Errorf("%w: strategy=%s: %v", ErrModelInputCompressionInvalid, strategy.Name(), validateErr)
		}
		if tokens >= current && len(compressed.Messages) >= before {
			continue
		}
		result.TrimmedMessages += before - len(compressed.Messages)
		result.AppliedStrategies = append(result.AppliedStrategies, strategy.Name())
		req = compressed
		current = tokens
	}
	result.FinalTokens = current
	if result.TrimmedMessages > 0 {
		policy, _ := NormalizeContextCompactionPolicy(req.Package.RuntimeConstraints.CompactionPolicy)
		record := newContextCompactionRecord(ContextCompactionRecord{
			Phase: ContextCompactionPhaseFinal, PolicyHash: policy.PolicyHash,
			BeforeTokens: original, AfterTokens: current, RemovedItems: result.TrimmedMessages, RemovedMessages: result.TrimmedMessages,
			AppliedStrategies: append([]string(nil), result.AppliedStrategies...),
		})
		result.CompactionRecords = []ContextCompactionRecord{record}
	}
	if current > limit {
		return ModelInvokeRequest{}, result, fmt.Errorf("%w: required=%d limit=%d protected_messages=%d", ErrModelInputBudgetExceeded, current, limit, len(req.Messages))
	}
	req.Package = updateModelInputPackage(req.Package, req.Messages, req.Tools, current, result.TrimmedMessages)
	if len(result.CompactionRecords) > 0 {
		req.Package.Messages.CompactionRecords = append(req.Package.Messages.CompactionRecords, result.CompactionRecords...)
		req.Package.ContextHash = modelContextHash(req.Package)
	}
	req.PreserveManifest = manifest
	return req, result, nil
}

func validateCompressedModelInput(original, compressed []ModelCallMessage) error {
	originalSystems := messagesWithRole(original, string(contextpkg.RoleSystem))
	compressedSystems := messagesWithRole(compressed, string(contextpkg.RoleSystem))
	if !containsMessageSubsequence(compressedSystems, originalSystems) {
		return errors.New("required system instruction changed or was removed")
	}
	if latest, ok := latestMessageWithRole(original, string(contextpkg.RoleUser)); ok {
		compressedLatest, found := latestMessageWithRole(compressed, string(contextpkg.RoleUser))
		if !found || !modelCallMessageEqual(latest, compressedLatest) {
			return errors.New("current user input changed or was removed")
		}
	}
	if err := validateModelToolPairing(compressed); err != nil {
		return err
	}
	return nil
}

func messagesWithRole(messages []ModelCallMessage, role string) []ModelCallMessage {
	var result []ModelCallMessage
	for _, message := range messages {
		if message.Role == role {
			result = append(result, message)
		}
	}
	return result
}

func latestMessageWithRole(messages []ModelCallMessage, role string) (ModelCallMessage, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == role {
			return messages[i], true
		}
	}
	return ModelCallMessage{}, false
}

func containsMessageSubsequence(haystack, needle []ModelCallMessage) bool {
	index := 0
	for _, message := range haystack {
		if index < len(needle) && modelCallMessageEqual(message, needle[index]) {
			index++
		}
	}
	return index == len(needle)
}

func modelCallMessageEqual(left, right ModelCallMessage) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func validateModelToolPairing(messages []ModelCallMessage) error {
	calls := make(map[string]int)
	results := make(map[string]int)
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			if call.ToolCallID == "" {
				return errors.New("tool call id is empty")
			}
			calls[call.ToolCallID]++
		}
		if message.Role == string(contextpkg.RoleTool) {
			if message.ToolCallID == "" {
				return errors.New("tool result call id is empty")
			}
			results[message.ToolCallID]++
		}
	}
	for id, count := range calls {
		if count != 1 || results[id] != 1 {
			return fmt.Errorf("tool pair is incomplete: call_id=%s calls=%d results=%d", id, count, results[id])
		}
	}
	for id, count := range results {
		if count != 1 || calls[id] != 1 {
			return fmt.Errorf("tool pair is incomplete: call_id=%s calls=%d results=%d", id, calls[id], count)
		}
	}
	return nil
}

type CompleteOldTurnCompressionStrategy struct{}

func (CompleteOldTurnCompressionStrategy) Name() string { return "complete_old_turns" }

func (CompleteOldTurnCompressionStrategy) Compress(ctx context.Context, req ModelInputCompressionRequest) (ModelInvokeRequest, int, error) {
	prefix, turns := splitModelTurns(req.Request.Messages)
	kept := append([]ModelCallMessage(nil), req.Request.Messages...)
	for index := 0; index < len(turns)-1; {
		if turnContainsRole(turns[index], string(contextpkg.RoleSystem)) {
			index++
			continue
		}
		turns = append(turns[:index], turns[index+1:]...)
		kept = joinModelTurns(prefix, turns)
		tokens, err := req.Counter.Count(ctx, kept, req.Request.Tools)
		if err != nil {
			return ModelInvokeRequest{}, 0, fmt.Errorf("count compacted model input: %w", err)
		}
		if tokens <= req.MaxInputTokens {
			result := req.Request
			result.Messages = kept
			return result, tokens, nil
		}
	}
	finalTokens, err := req.Counter.Count(ctx, kept, req.Request.Tools)
	if err != nil {
		return ModelInvokeRequest{}, 0, fmt.Errorf("count compacted model input: %w", err)
	}
	result := req.Request
	result.Messages = kept
	return result, finalTokens, nil
}

// splitModelTurns keeps only the contiguous system prefix as a prefix and
// groups the remaining sequence by user turns. A later system message stays in
// its original turn and makes that turn non-removable, so governance never
// reorders or silently drops runtime-added instructions.
func splitModelTurns(messages []ModelCallMessage) ([]ModelCallMessage, [][]ModelCallMessage) {
	var prefix []ModelCallMessage
	var turns [][]ModelCallMessage
	for _, message := range messages {
		if len(turns) == 0 && len(prefix) == 0 && message.Role == string(contextpkg.RoleSystem) {
			prefix = append(prefix, message)
			continue
		}
		if len(turns) == 0 && len(prefix) > 0 && message.Role == string(contextpkg.RoleSystem) {
			prefix = append(prefix, message)
			continue
		}
		if message.Role == string(contextpkg.RoleUser) || len(turns) == 0 {
			turns = append(turns, nil)
		}
		turns[len(turns)-1] = append(turns[len(turns)-1], message)
	}
	return prefix, turns
}

func turnContainsRole(messages []ModelCallMessage, role string) bool {
	for _, message := range messages {
		if message.Role == role {
			return true
		}
	}
	return false
}

func joinModelTurns(prefix []ModelCallMessage, turns [][]ModelCallMessage) []ModelCallMessage {
	size := len(prefix)
	for _, turn := range turns {
		size += len(turn)
	}
	out := make([]ModelCallMessage, 0, size)
	out = append(out, prefix...)
	for _, turn := range turns {
		out = append(out, turn...)
	}
	return out
}

func updateModelInputPackage(pkg ModelContextPackage, messages []ModelCallMessage, tools []ModelToolDefinition, tokens, trimmed int) ModelContextPackage {
	pkg.Messages.ConversationWindow = modelContextMessagesFromCalls(messages)
	pkg.Messages.TokenCount = tokens
	pkg.Messages.TrimmedCount += trimmed
	pkg.Capabilities.ToolDefinitions = cloneModelToolDefinitions(tools)
	pkg.ContextHash = modelContextHash(pkg)
	return pkg
}

func modelCallMessagesFromContext(messages []ModelContextMessage) []ModelCallMessage {
	out := make([]ModelCallMessage, 0, len(messages))
	for _, message := range messages {
		converted := ModelCallMessage{
			Role: message.Role, Content: message.Content,
			ContentParts: append([]ModelContentPart(nil), message.ContentParts...),
			Name:         message.Name, ToolCallID: message.ToolCallID,
			ToolName: message.ToolName, ReasoningContent: message.ReasoningContent, ReasoningSignature: message.ReasoningSignature,
		}
		for _, call := range message.ToolCalls {
			arguments, _ := json.Marshal(call.Arguments)
			converted.ToolCalls = append(converted.ToolCalls, ModelToolCall{ToolCallID: call.ID, Name: call.Name, Arguments: arguments})
		}
		if message.ToolResult != nil {
			converted.ToolCallID = message.ToolResult.CallID
			converted.ToolName = message.ToolResult.Name
			if converted.Content == "" {
				converted.Content = message.ToolResult.Content
			}
		}
		out = append(out, converted)
	}
	return out
}

func cloneModelCallMessages(input []ModelCallMessage) []ModelCallMessage {
	out := make([]ModelCallMessage, len(input))
	for i, message := range input {
		out[i] = message
		out[i].ContentParts = append([]ModelContentPart(nil), message.ContentParts...)
		out[i].ToolCalls = append([]ModelToolCall(nil), message.ToolCalls...)
		for j := range out[i].ToolCalls {
			out[i].ToolCalls[j].Arguments = append(json.RawMessage(nil), message.ToolCalls[j].Arguments...)
		}
	}
	return out
}
