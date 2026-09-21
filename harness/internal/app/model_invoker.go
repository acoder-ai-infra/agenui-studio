package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// maxInlineImageBytes 限制单张图片解引用后内联为 data URI 的原始字节数，
// 避免超大图片击穿 provider 请求体。
const maxInlineImageBytes = 8 << 20 // 8 MiB

// gatewayModelInvoker is the only Runtime -> Model Gateway translation point.
// Runtime adapters therefore share one governed request and event contract.
type gatewayModelInvoker struct {
	gateway modelgateway.ModelGateway
	// artifacts 解析多模态输入中的 artifact:// 图片引用；为 nil 时遇
	// artifact 图片 fail closed。
	artifacts *artifact.Store
	ids       observability.IDGenerator
}

func newGatewayModelInvoker(gateway modelgateway.ModelGateway, artifacts *artifact.Store) *gatewayModelInvoker {
	return &gatewayModelInvoker{gateway: gateway, artifacts: artifacts, ids: observability.NewULIDGenerator("model")}
}

func (i *gatewayModelInvoker) Invoke(ctx context.Context, req agentruntime.ModelInvokeRequest) (<-chan agentruntime.ModelStreamItem, error) {
	if i == nil || i.gateway == nil {
		return nil, fmt.Errorf("model gateway is not configured")
	}
	messages := req.Messages
	if len(messages) == 0 {
		messages = modelCallMessages(req.Package.Messages.ConversationWindow)
	}
	// 送 provider 前把 artifact:// 引用解析为内联字节（app 层是唯一持有
	// Artifact Store 的解析点）：图片→data URI，文件→内联 base64（G-C）；
	// 解析失败 fail closed。
	messages, err := i.resolveArtifactParts(ctx, req, messages)
	if err != nil {
		return nil, err
	}
	tools, err := json.Marshal(req.Tools)
	if err != nil {
		return nil, err
	}
	modelReq := modelgateway.ModelRequest{
		RequestID: i.ids.NewRequestID(), Trace: req.Trace, AgentID: req.Package.Run.AgentID,
		Messages: gatewayMessages(messages), Streaming: true,
		ContextHash:     req.Package.ContextHash,
		MaxPromptTokens: req.Package.RuntimeConstraints.TokenBudget.MaxInputTokens,
		MaxOutputTokens: req.Package.RuntimeConstraints.TokenBudget.ReservedOutputTokens,
		Required:        modelgateway.CapabilityRequirement{ToolCalling: req.AllowTools && len(req.Tools) > 0},
		Options:         gatewayModelOptions(req.Options),
	}
	if len(req.Tools) > 0 {
		modelReq.ToolsSchema = tools
	}
	if req.Options.Model != nil {
		modelReq.ModelHint = *req.Options.Model
	}
	if len(req.Options.Fallback) > 0 {
		modelReq.ModelFallbackHints = append([]string(nil), req.Options.Fallback...)
	}
	call, err := i.gateway.Chat(ctx, modelReq)
	if err != nil {
		return nil, err
	}
	out := make(chan agentruntime.ModelStreamItem, 64)
	go bridgeModelEvents(ctx, call, out)
	return out, nil
}

func gatewayModelOptions(options agentruntime.ModelCallOptions) modelgateway.ModelOptions {
	result := modelgateway.ModelOptions{
		Stop: append([]string(nil), options.Stop...), ToolChoice: options.ToolChoice,
		ReasoningBudget: options.ReasoningBudget, ReasoningEffort: options.ReasoningEffort,
		// G-A / G-B：结构化输出格式与图片视觉预算透传至网关。
		ResponseFormat: options.ResponseFormat, ImageDetail: options.ImageDetail,
	}
	if options.Temperature != nil {
		value := float64(*options.Temperature)
		result.Temperature = &value
	}
	if options.TopP != nil {
		value := float64(*options.TopP)
		result.TopP = &value
	}
	if options.MaxTokens != nil {
		value := *options.MaxTokens
		result.MaxTokens = &value
	}
	switch options.ReasoningMode {
	case agentruntime.ModelReasoningEnabled:
		result.ReasoningMode = modelgateway.ReasoningEnabled
	case agentruntime.ModelReasoningDisabled:
		result.ReasoningMode = modelgateway.ReasoningDisabled
	case agentruntime.ModelReasoningAuto:
		result.ReasoningMode = modelgateway.ReasoningAuto
	}
	return result
}

func bridgeModelEvents(ctx context.Context, call *modelgateway.ModelCall, out chan<- agentruntime.ModelStreamItem) {
	defer close(out)
	var completed *observability.AgentEvent
	// 按 tool-call index 聚合流式 delta：一次模型响应可含 0/1/N 个工具调用，
	// 每个调用的 id/name/arguments 可能分多个 chunk 到达（A4）。
	deltaByIndex := make(map[int]*toolCallDeltaAgg)
	var deltaOrder []int
	for event := range call.Events {
		item := agentruntime.ModelStreamItem{Event: event}
		switch event.EventType {
		case observability.EventModelTokenDelta:
			item.TextDelta = eventText(event.Payload)
		case observability.EventModelThoughtDelta:
			var payload modelgateway.ModelThoughtDeltaPayload
			if json.Unmarshal(event.Payload, &payload) == nil {
				item.ReasoningDelta = payload.Text
			}
		case observability.EventModelToolCallDelta:
			var payload modelgateway.ModelToolCallDeltaPayload
			if json.Unmarshal(event.Payload, &payload) == nil {
				agg, ok := deltaByIndex[payload.Index]
				if !ok {
					agg = &toolCallDeltaAgg{}
					deltaByIndex[payload.Index] = agg
					deltaOrder = append(deltaOrder, payload.Index)
				}
				if payload.ToolCallID != "" {
					agg.id = payload.ToolCallID
				}
				if payload.Name != "" {
					agg.name = payload.Name
				}
				agg.args.WriteString(payload.ArgumentsDelta)
			}
			continue
		case observability.EventModelCallCompleted:
			copy := event
			completed = &copy
			continue
		}
		if !sendModelItem(ctx, out, item) {
			return
		}
	}
	response, awaitErr := call.Await()
	if awaitErr != nil {
		sendModelItem(ctx, out, agentruntime.ModelStreamItem{Event: observability.AgentEvent{
			EventType: observability.EventModelCallFailed, Visibility: observability.VisibilityDebug,
			Error: &observability.EventError{Code: "MODEL_GATEWAY_AWAIT_FAILED", Type: observability.EventErrorUpstream, Message: "model gateway stream failed", Retryable: true},
		}})
		return
	}
	// 归一化出本轮全部工具调用：response.ToolCalls 为权威事实；字段缺失时
	// 用同 index 的流式 delta 聚合回填。response.ToolCalls 为空但有 delta
	// 聚合时（部分 provider 只走流式），按 index 顺序回退。
	calls := normalizeResponseToolCalls(response, deltaByIndex, deltaOrder)
	if len(calls) > 0 && response != nil && response.ReasoningSignature != "" {
		if !sendModelItem(ctx, out, agentruntime.ModelStreamItem{ReasoningSignature: response.ReasoningSignature}) {
			return
		}
	}
	// 每个工具调用发一条 ModelStreamItem（保序）：模型响应/Runtime/存储对
	// 同一批调用有一致语义（A4）。
	for i := range calls {
		call := calls[i]
		payload := modelToolCallEventPayload(call)
		call = normalizeModelToolCallArguments(call)
		if !sendModelItem(ctx, out, agentruntime.ModelStreamItem{Event: observability.AgentEvent{
			EventType: observability.EventModelToolCallDelta, Visibility: observability.VisibilityDebug,
			Payload: payload,
		}, ToolCall: &call}) {
			return
		}
	}
	if completed != nil {
		sendModelItem(ctx, out, agentruntime.ModelStreamItem{Event: *completed})
	}
}

// normalizeModelToolCallArguments repairs only the transport envelope: every
// model-originated tool call handed to Eino has one valid JSON object. The Tool
// Gateway remains authoritative for schema and business validation. This keeps
// the assistant tool call and its eventual tool result as a complete pair.
func normalizeModelToolCallArguments(call agentruntime.ModelToolCall) agentruntime.ModelToolCall {
	if len(call.Arguments) == 0 || !json.Valid(call.Arguments) {
		call.Arguments = json.RawMessage(`{}`)
	}
	return call
}

func modelToolCallEventPayload(call agentruntime.ModelToolCall) json.RawMessage {
	if len(call.Arguments) > 0 && json.Valid(call.Arguments) {
		return agentruntime.JSONPayload(call)
	}
	digest := sha256.Sum256(call.Arguments)
	reason := "arguments are empty"
	if len(strings.TrimSpace(string(call.Arguments))) > 0 {
		var value any
		if err := json.Unmarshal(call.Arguments, &value); err != nil {
			reason = err.Error()
		} else {
			reason = "arguments are not a valid JSON value"
		}
	}
	return agentruntime.JSONPayload(map[string]any{
		"tool_call_id":      call.ToolCallID,
		"name":              call.Name,
		"version":           call.Version,
		"arguments_invalid": true,
		"argument_bytes":    len(call.Arguments),
		"arguments_hash":    fmt.Sprintf("sha256:%x", digest),
		"validation_error":  reason,
		"repair_applied":    true,
		"normalized_to":     "{}",
	})
}

// toolCallDeltaAgg 聚合单个 tool-call index 的流式 delta 分片。
type toolCallDeltaAgg struct {
	id, name string
	args     strings.Builder
}

// normalizeResponseToolCalls 把网关响应中的 N 个工具调用归一为 runtime 视图，
// 缺失字段用同 index 的流式 delta 聚合回填；保序（response 顺序优先，纯
// delta 回退时按 index 升序）。
func normalizeResponseToolCalls(response *modelgateway.ModelResponse, deltaByIndex map[int]*toolCallDeltaAgg, deltaOrder []int) []agentruntime.ModelToolCall {
	if response != nil && len(response.ToolCalls) > 0 {
		out := make([]agentruntime.ModelToolCall, 0, len(response.ToolCalls))
		for i, tool := range response.ToolCalls {
			call := agentruntime.ModelToolCall{ToolCallID: tool.ToolCallID, Name: tool.Name, Arguments: json.RawMessage(tool.Arguments)}
			// 字段缺失时用同 index 的 delta 聚合回填（provider 未在最终响应
			// 复述完整字段的兜底）。
			if agg, ok := deltaByIndex[i]; ok {
				if call.ToolCallID == "" {
					call.ToolCallID = agg.id
				}
				if call.Name == "" {
					call.Name = agg.name
				}
				if len(call.Arguments) == 0 {
					call.Arguments = json.RawMessage(agg.args.String())
				}
			}
			out = append(out, call)
		}
		return out
	}
	// response.ToolCalls 为空但有流式 delta：按 index 升序回退（部分 provider
	// 仅走流式，不在最终响应重复工具调用）。
	if len(deltaByIndex) == 0 {
		return nil
	}
	indexes := append([]int(nil), deltaOrder...)
	sort.Ints(indexes)
	out := make([]agentruntime.ModelToolCall, 0, len(indexes))
	for _, idx := range indexes {
		agg := deltaByIndex[idx]
		if agg == nil || agg.name == "" {
			continue
		}
		out = append(out, agentruntime.ModelToolCall{
			ToolCallID: agg.id, Name: agg.name, Arguments: json.RawMessage(agg.args.String()),
		})
	}
	return out
}

func sendModelItem(ctx context.Context, out chan<- agentruntime.ModelStreamItem, item agentruntime.ModelStreamItem) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- item:
		return true
	}
}

func modelCallMessages(messages []agentruntime.ModelContextMessage) []agentruntime.ModelCallMessage {
	out := make([]agentruntime.ModelCallMessage, 0, len(messages))
	for _, message := range messages {
		content := message.Content
		if content == "" && message.ToolResult != nil {
			content = message.ToolResult.Content
		}
		out = append(out, agentruntime.ModelCallMessage{Role: message.Role, Content: content, ContentParts: message.ContentParts, Name: message.Name, ToolCallID: message.ToolCallID, ToolName: message.ToolName, ReasoningContent: message.ReasoningContent, ReasoningSignature: message.ReasoningSignature})
	}
	return out
}

// resolveArtifactParts 把消息内 image_url / file Part 的 artifact:// 引用
// 解引用为内联字节（图片→data URI，文件→Base64Data，G-C）。走
// PurposeModelContext 权限（同会话内容可读）；超限、缺 MIME 或读取
// 失败 fail closed，不允许把无法解析的引用静默送给 provider。
func (i *gatewayModelInvoker) resolveArtifactParts(ctx context.Context, req agentruntime.ModelInvokeRequest, messages []agentruntime.ModelCallMessage) ([]agentruntime.ModelCallMessage, error) {
	resolved := false
	for mi := range messages {
		for pi := range messages[mi].ContentParts {
			part := messages[mi].ContentParts[pi]
			if (part.Type != "image_url" && part.Type != "file") || !strings.HasPrefix(part.URL, "artifact://") {
				continue
			}
			if i.artifacts == nil {
				return nil, fmt.Errorf("resolve %s part: artifact store unavailable", part.Type)
			}
			if !resolved {
				// 浅拷贝消息与 Parts，避免就地修改调用方持有的切片。
				messages = append([]agentruntime.ModelCallMessage(nil), messages...)
				resolved = true
			}
			parts := append([]agentruntime.ModelContentPart(nil), messages[mi].ContentParts...)
			switch part.Type {
			case "image_url":
				dataURI, err := i.inlineArtifactImage(ctx, req, part)
				if err != nil {
					return nil, err
				}
				parts[pi].URL = dataURI
			case "file":
				base64Data, mime, err := i.inlineArtifactFile(ctx, req, part)
				if err != nil {
					return nil, err
				}
				parts[pi].URL = ""
				parts[pi].Base64Data = base64Data
				parts[pi].MIMEType = mime
			}
			messages[mi].ContentParts = parts
		}
	}
	return messages, nil
}

func (i *gatewayModelInvoker) inlineArtifactImage(ctx context.Context, req agentruntime.ModelInvokeRequest, part agentruntime.ModelContentPart) (string, error) {
	trace := req.Trace
	actorCtx := artifact.ContextWithActor(ctx, artifact.Actor{
		Role: artifact.ActorContextEngine, TenantID: trace.TenantID, UserID: trace.UserID,
		SessionID: trace.SessionID, RunID: trace.RunID,
	})
	object, err := i.artifacts.Get(actorCtx, part.URL, artifact.GetOptions{Purpose: artifact.PurposeModelContext})
	if err != nil {
		return "", fmt.Errorf("resolve image part %s: %w", part.URL, err)
	}
	defer object.Content.Close()
	data, err := io.ReadAll(io.LimitReader(object.Content, maxInlineImageBytes+1))
	if err != nil {
		return "", fmt.Errorf("read image part %s: %w", part.URL, err)
	}
	if len(data) == 0 || len(data) > maxInlineImageBytes {
		return "", fmt.Errorf("image part %s is empty or exceeds %d bytes", part.URL, maxInlineImageBytes)
	}
	mime := part.MIMEType
	if mime == "" {
		mime = object.Meta.MimeType
	}
	if !strings.HasPrefix(mime, "image/") {
		return "", fmt.Errorf("image part %s has non-image MIME %q", part.URL, mime)
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// inlineArtifactFile 把 file Part 的 artifact:// 引用读取为内联 base64
// （G-C）：返回编码后的字节与生效 MIME，上限与图片一致（大文件应经
// 专门的文件理解服务，不走内联直传）。
func (i *gatewayModelInvoker) inlineArtifactFile(ctx context.Context, req agentruntime.ModelInvokeRequest, part agentruntime.ModelContentPart) (string, string, error) {
	trace := req.Trace
	actorCtx := artifact.ContextWithActor(ctx, artifact.Actor{
		Role: artifact.ActorContextEngine, TenantID: trace.TenantID, UserID: trace.UserID,
		SessionID: trace.SessionID, RunID: trace.RunID,
	})
	object, err := i.artifacts.Get(actorCtx, part.URL, artifact.GetOptions{Purpose: artifact.PurposeModelContext})
	if err != nil {
		return "", "", fmt.Errorf("resolve file part %s: %w", part.URL, err)
	}
	defer object.Content.Close()
	data, err := io.ReadAll(io.LimitReader(object.Content, maxInlineImageBytes+1))
	if err != nil {
		return "", "", fmt.Errorf("read file part %s: %w", part.URL, err)
	}
	if len(data) == 0 || len(data) > maxInlineImageBytes {
		return "", "", fmt.Errorf("file part %s is empty or exceeds %d bytes", part.URL, maxInlineImageBytes)
	}
	mime := part.MIMEType
	if mime == "" {
		mime = object.Meta.MimeType
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	return base64.StdEncoding.EncodeToString(data), mime, nil
}

func gatewayMessages(messages []agentruntime.ModelCallMessage) []modelgateway.ChatMessage {
	out := make([]modelgateway.ChatMessage, 0, len(messages))
	for _, message := range messages {
		converted := modelgateway.ChatMessage{Role: message.Role, Content: message.Content, Name: message.Name, ToolCallID: message.ToolCallID, ReasoningContent: message.ReasoningContent, ReasoningSignature: message.ReasoningSignature}
		for _, call := range message.ToolCalls {
			converted.ToolCalls = append(converted.ToolCalls, modelgateway.ChatToolCall{
				ID: call.ToolCallID, Type: "function",
				Function: modelgateway.ChatFunctionCall{Name: call.Name, Arguments: string(call.Arguments)},
			})
		}
		for _, part := range message.ContentParts {
			switch part.Type {
			case "text":
				converted.Parts = append(converted.Parts, modelgateway.ContentPart{Type: "text", Text: part.Text})
			case "image_url":
				// URL 优先（含 artifact:// 解析后的 data URI）；否则由内联
				// Base64Data + MIME 组装 data URI（inline_binary 帧，ADR-013）。
				url := part.URL
				if url == "" && part.Base64Data != "" {
					mime := part.MIMEType
					if mime == "" {
						mime = "image/png"
					}
					url = "data:" + mime + ";base64," + part.Base64Data
				}
				converted.Parts = append(converted.Parts, modelgateway.ContentPart{Type: "image_url", ImageURL: &modelgateway.ImageURL{URL: url}})
			case "file":
				// 非图片文件直传（G-C）：内联 Base64Data + MIME 组装 data URI
				// 进 file content part，供支持文件理解的模型原生读取。
				// artifact:// 引用已在 resolveArtifactParts 解析为 Base64Data。
				if part.Base64Data == "" {
					continue
				}
				mime := part.MIMEType
				if mime == "" {
					mime = "application/octet-stream"
				}
				converted.Parts = append(converted.Parts, modelgateway.ContentPart{Type: "file", File: &modelgateway.FilePart{
					FileData: "data:" + mime + ";base64," + part.Base64Data,
					Filename: part.Name,
				}})
			}
		}
		out = append(out, converted)
	}
	return out
}
