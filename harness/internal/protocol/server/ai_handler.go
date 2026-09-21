package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// aiChatRequest 兼容两种输入:简易 {prompt} 或 AI SDK useChat 的 {messages:[...]}。
type aiChatRequest struct {
	SessionID string          `json:"sessionId,omitempty"`
	AgentID   string          `json:"agentId,omitempty"`
	Prompt    string          `json:"prompt,omitempty"`
	Messages  []aiChatMessage `json:"messages,omitempty"`
}

type aiChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
	Parts   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"parts,omitempty"`
}

// handleAIChat 是 Vercel AI SDK v5 端点:POST 一条消息 -> OpenTurn+dispatch ->
// 以 v5 UI message stream 流式返回(供前端 useChat / 调试 demo 消费)。
func (d *Deps) handleAIChat(w http.ResponseWriter, r *http.Request) {
	if d.RunService == nil {
		writeError(w, http.StatusInternalServerError, "RUN_SERVICE_UNAVAILABLE", "run service not configured")
		return
	}
	var body aiChatRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	prompt := body.Prompt
	if prompt == "" {
		prompt = lastUserText(body.Messages)
	}
	// Ownership: a client-supplied sessionId must belong to the caller before we
	// open a turn on it (blocks cross-user injection). Empty → server-minted.
	if !d.authorizeSessionForOpen(w, r, body.SessionID) {
		return
	}
	sid := body.SessionID
	agentID := body.AgentID
	if agentID == "" {
		agentID = d.DefaultAgentID
	}
	if agentID == "" {
		writeError(w, http.StatusInternalServerError, "DEFAULT_AGENT_UNAVAILABLE", "default agent is not configured")
		return
	}

	tc := observability.MustTraceContext(r.Context())
	tc.SessionID = sid
	ctx := observability.WithTraceContext(r.Context(), tc)

	// 入口层预回合扩展管线（与 SDK Start 共享 kernel.TurnPipeline）：未配置
	// 时直通；失败 fail-closed，不得开 Turn。
	prepared, err := d.prepareTurnInput(ctx, &tc, agentID, prompt)
	if err != nil {
		writeTurnPipelineError(w, err)
		return
	}
	prompt = prepared.Preview
	sid = tc.SessionID
	ctx = observability.WithTraceContext(r.Context(), tc)

	res, err := d.RunService.OpenTurn(ctx, storage.OpenTurnRequest{
		SessionID: sid, TenantID: tc.TenantID, UserID: tc.UserID,
		AgentID: agentID, UserContentPreview: prompt,
		IdempotencyKey:   r.Header.Get("Idempotency-Key"),
		ContextFragments: prepared.Fragments,
	})
	if err != nil {
		writeError(w, statusForStorageErr(err), "OPEN_TURN_FAILED", err.Error())
		return
	}
	// 空 sessionId 必须原样交给 OpenTurn。存储层会用稳定的 <new> scope
	// 原子认领 Idempotency-Key，并返回首个请求真正创建的 Session；如果在
	// 协议层提前生成随机 ID，并发重试就会落入不同 scope，产生多个 Turn。
	sid = res.Session.ID
	tc.SessionID = sid
	ctx = observability.WithTraceContext(ctx, tc)
	// Surface the resolved session/run ids so the client can keep a multi-turn
	// conversation (reuse sessionId) and jump straight to this run's telemetry
	// (events/steps) without manual entry. Set before the SSE body is written.
	w.Header().Set("x-harness-session-id", res.Session.ID)
	w.Header().Set("x-harness-run-id", res.Run.RunID)
	// 先订阅实时通道,再 Dispatch:高频逐字增量不落库(层 A),replay 无法兜底,
	// 若先 Dispatch 再订阅,早期 delta 会在订阅建立前发布而永久丢失。
	var sub <-chan observability.AgentEvent
	var cancel func()
	if d.Broker != nil {
		sub, cancel, _ = d.Broker.Subscribe(ctx, res.Run.RunID)
		if cancel != nil {
			defer cancel()
		}
	}

	if err := d.dispatchNewTurn(ctx, res); err != nil {
		writeDispatchError(w, err)
		return
	}

	aw, ok := protocol.NewAISDKWriter(w)
	if !ok {
		writeError(w, http.StatusInternalServerError, "STREAMING_UNSUPPORTED", "response writer is not a flusher")
		return
	}
	d.streamAISDK(ctx, aw, res.Run.RunID, sub)
}

// streamAISDK replays-then-subscribes the run's user-visible events and maps them
// to AI SDK v5 UI message stream parts.
func (d *Deps) streamAISDK(ctx context.Context, aw *protocol.AISDKWriter, runID string, sub <-chan observability.AgentEvent) {
	_ = aw.Part(protocol.AISDKStart(runID))
	_ = aw.Part(protocol.AISDKStartStep())

	debugEnabled := observability.MustTraceContext(ctx).DebugEnabled
	// Query includes debug ONLY so the (debug-visibility) run terminals can be
	// fetched to close the stream; chatRelevant below is the real gate.
	visibleToChat := []observability.EventVisibility{observability.VisibilityUserVisible, observability.VisibilityDebug}
	if debugEnabled {
		visibleToChat = append(visibleToChat, observability.VisibilityInternal, observability.VisibilityRestricted)
	}
	// chatRelevant is what an ordinary chat client may see: user_visible events,
	// plus run terminals (currently emitted at debug visibility, needed to end the
	// stream). Everything else debug/internal (model_*, internal deltas) is dropped
	// here so replay cannot leak it into the chat stream.
	chatRelevant := func(ev observability.AgentEvent) bool {
		if ev.Visibility == observability.VisibilityUserVisible {
			return true
		}
		switch ev.EventType {
		case observability.EventRunFailed, observability.EventRunCancelled, observability.EventRunExpired:
			return true
		}
		if debugEnabled {
			switch ev.Visibility {
			case observability.VisibilityDebug, observability.VisibilityInternal, observability.VisibilityRestricted:
				return true
			}
		}
		return false
	}
	// Dedup by event_id (shared between replay and live): the sequence assigned at
	// persistence is not stamped onto the event pushed to the broker, so a
	// sequence-based dedup would drop every live event.
	seen := map[string]bool{}
	textOpen := false
	const textID = "txt-0"

	// emit maps one event to v5 parts; returns true when the run reached a terminal.
	emit := func(ev observability.AgentEvent) (stop bool) {
		if debugEnabled && ev.Visibility != observability.VisibilityUserVisible {
			_ = aw.Part(protocol.AISDKDataPart("debug-event", aiSDKDebugEvent(ev)))
		}
		switch ev.EventType {
		case observability.EventAgentTextDelta:
			text := eventText(ev)
			if text == "" {
				return false
			}
			if !textOpen {
				_ = aw.Part(protocol.AISDKTextStart(textID))
				textOpen = true
			}
			_ = aw.Part(protocol.AISDKTextDelta(textID, text))
		case observability.EventFinalResponse:
			// Durable fallback for a dropped live delta stream. Do not duplicate text
			// when at least one realtime delta was already delivered.
			if !textOpen {
				text := eventText(ev)
				if text == "" {
					return false
				}
				_ = aw.Part(protocol.AISDKTextStart(textID))
				_ = aw.Part(protocol.AISDKTextDelta(textID, text))
				textOpen = true
			}
		case observability.EventRunFailed:
			if textOpen {
				_ = aw.Part(protocol.AISDKTextEnd(textID))
			}
			_ = aw.Part(protocol.AISDKError("run failed"))
			return true
		case observability.EventRunCompleted, observability.EventRunCancelled, observability.EventRunExpired:
			if textOpen {
				_ = aw.Part(protocol.AISDKTextEnd(textID))
			}
			return true
		case observability.EventToolCallStarted:
			toolCallID, toolName := aiSDKToolIdentity(ev)
			_ = aw.Part(protocol.AISDKToolInputStart(toolCallID, toolName))
			if input, ok := aiSDKToolInput(ev); ok {
				_ = aw.Part(protocol.AISDKToolInputAvailable(toolCallID, toolName, input))
			}
		case observability.EventToolCallProgress:
			_ = aw.Part(protocol.AISDKDataPart("tool-progress", aiSDKEventData(ev)))
		case observability.EventToolArtifactCreated:
			_ = aw.Part(protocol.AISDKDataPart("artifact", aiSDKEventData(ev)))
		case observability.EventToolCallCompleted:
			toolCallID, _ := aiSDKToolIdentity(ev)
			_ = aw.Part(protocol.AISDKToolOutputAvailable(toolCallID, aiSDKToolOutput(ev)))
		case observability.EventToolCallFailed, observability.EventToolCallCancelled:
			_ = aw.Part(protocol.AISDKDataPart("tool-error", aiSDKEventData(ev)))
		case observability.EventControlRequestCreated:
			_ = aw.Part(protocol.AISDKDataPart("control-request", aiSDKEventData(ev)))
		case observability.EventControlResponseReceived:
			_ = aw.Part(protocol.AISDKDataPart("control-response", aiSDKEventData(ev)))
		case observability.EventControlRequestExpired:
			_ = aw.Part(protocol.AISDKDataPart("control-expired", aiSDKEventData(ev)))
		case observability.EventSubAgentStarted, observability.EventSubAgentProgress,
			observability.EventSubAgentCompleted, observability.EventSubAgentFailed:
			_ = aw.Part(protocol.AISDKDataPart("sub-agent", aiSDKEventData(ev)))
		case observability.EventA2ATaskCreated, observability.EventA2ATaskProgress,
			observability.EventA2ATaskCompleted, observability.EventA2ATaskFailed,
			observability.EventA2ATaskCancelled:
			_ = aw.Part(protocol.AISDKDataPart("a2a-task", aiSDKEventData(ev)))
		case observability.EventSkillStarted, observability.EventSkillCompleted, observability.EventSkillFailed:
			_ = aw.Part(protocol.AISDKDataPart("skill", aiSDKEventData(ev)))
		case observability.EventArtifactCreated:
			_ = aw.Part(protocol.AISDKDataPart("artifact", aiSDKEventData(ev)))
		case observability.EventGuardrailTriggered, observability.EventGuardrailBlocked:
			_ = aw.Part(protocol.AISDKDataPart("guardrail", aiSDKEventData(ev)))
		case observability.EventModelFallbackApplied:
			_ = aw.Part(protocol.AISDKDataPart("model-fallback", aiSDKEventData(ev)))
		}
		return false
	}
	finish := func() {
		_ = aw.Part(protocol.AISDKFinishStep())
		_ = aw.Part(protocol.AISDKFinish())
		_ = aw.Done()
	}

	// Durable milestones and realtime deltas form one logical stream. The broker
	// is intentionally best-effort, so periodically reconcile EventStore sequence;
	// a dropped terminal is then recovered and the client cannot wait forever.
	var lastSequence int64
	replay := func() bool {
		for {
			events, err := d.Stores.Events.Query(ctx, storage.EventQuery{
				RunID: runID, AfterSequence: lastSequence, Limit: 500, Visibilities: visibleToChat,
			})
			if err != nil {
				return false
			}
			for _, ev := range events {
				if ev.Sequence > lastSequence {
					lastSequence = ev.Sequence
				}
				if !chatRelevant(ev) || seen[ev.EventID] {
					continue
				}
				seen[ev.EventID] = true
				if emit(ev) {
					return true
				}
			}
			if len(events) < 500 {
				return false
			}
		}
	}
	if replay() {
		finish()
		return
	}
	// Dispatch 失败的落库顺序是先把 Run 收敛为 failed，再追加终态事件。
	// 若事件写入恰好失败，Run 仍是唯一事实来源；重放时据此补一个脱敏终态，
	// 避免客户端永久等待。正常完成仍以 durable run_completed 事件为准。
	if ev, ok := d.redactedTerminalFromRun(ctx, runID); ok {
		_ = emit(ev)
		finish()
		return
	}
	reconcile := time.NewTicker(time.Second)
	defer reconcile.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-reconcile.C:
			if replay() {
				finish()
				return
			}
			if ev, ok := d.redactedTerminalFromRun(ctx, runID); ok {
				_ = emit(ev)
				finish()
				return
			}
		case ev, okCh := <-sub:
			if !okCh {
				sub = nil
				continue
			}
			if seen[ev.EventID] || !chatRelevant(ev) {
				continue
			}
			seen[ev.EventID] = true
			if emit(ev) {
				finish()
				return
			}
		}
	}
}

func aiSDKDebugEvent(event observability.AgentEvent) map[string]any {
	out := map[string]any{
		"event_id":   event.EventID,
		"sequence":   event.Sequence,
		"event_type": string(event.EventType),
		"visibility": string(event.Visibility),
		"trace_id":   event.TraceID,
		"session_id": event.SessionID,
		"run_id":     event.RunID,
		"step_id":    event.StepID,
		"agent_id":   event.AgentID,
		"runtime":    event.Runtime,
		"created_at": event.CreatedAt,
	}
	if len(event.PayloadPreview) > 0 {
		out["payload_preview"] = event.PayloadPreview
	}
	if len(event.Payload) > 0 {
		out["payload"] = event.Payload
	}
	if len(event.Usage) > 0 {
		out["usage"] = event.Usage
	}
	if event.PayloadRef != "" {
		out["payload_ref"] = event.PayloadRef
	}
	if event.DebugRef != "" {
		out["debug_ref"] = event.DebugRef
	}
	if event.Error != nil {
		out["error"] = event.Error
	}
	return out
}

func previewText(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var p map[string]any
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	for _, key := range []string{"text", "preview", "content", "message"} {
		if text, ok := p[key].(string); ok {
			return text
		}
	}
	return ""
}

func eventText(event observability.AgentEvent) string {
	if text := previewText(event.PayloadPreview); text != "" {
		return text
	}
	return previewText(event.Payload)
}

func aiSDKEventPayload(event observability.AgentEvent) map[string]any {
	raw := event.PayloadPreview
	if len(raw) == 0 {
		raw = event.Payload
	}
	if len(raw) == 0 {
		return map[string]any{}
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
		return map[string]any{"raw": string(raw)}
	}
	delete(payload, "resume_token")
	return payload
}

func aiSDKEventData(event observability.AgentEvent) map[string]any {
	data := map[string]any{
		"event_id":   event.EventID,
		"sequence":   event.Sequence,
		"event_type": string(event.EventType),
		"visibility": string(event.Visibility),
		"trace_id":   event.TraceID,
		"session_id": event.SessionID,
		"run_id":     event.RunID,
		"step_id":    event.StepID,
		"agent_id":   event.AgentID,
		"created_at": event.CreatedAt,
		"payload":    aiSDKEventPayload(event),
	}
	if event.ParentRunID != "" {
		data["parent_run_id"] = event.ParentRunID
	}
	if event.PayloadRef != "" {
		data["artifact_ref"] = event.PayloadRef
	}
	if len(event.Usage) > 0 {
		data["usage"] = event.Usage
	}
	if event.Error != nil {
		data["error"] = event.Error
	}
	return data
}

func aiSDKToolIdentity(event observability.AgentEvent) (string, string) {
	payload := aiSDKEventPayload(event)
	toolCallID := stringField(payload, "tool_call_id")
	if toolCallID == "" {
		toolCallID = event.StepID
	}
	if toolCallID == "" {
		toolCallID = event.EventID
	}
	toolName := stringField(payload, "tool_name")
	if toolName == "" {
		toolName = string(event.EventType)
	}
	return toolCallID, toolName
}

func aiSDKToolInput(event observability.AgentEvent) (any, bool) {
	payload := aiSDKEventPayload(event)
	if input, ok := payload["arguments_preview"]; ok && input != nil {
		return input, true
	}
	if input, ok := payload["input"]; ok && input != nil {
		return input, true
	}
	return nil, false
}

func aiSDKToolOutput(event observability.AgentEvent) any {
	payload := aiSDKEventPayload(event)
	if output, ok := payload["result_preview"]; ok && output != nil {
		return output
	}
	if ref := stringField(payload, "result_ref"); ref != "" {
		return map[string]any{"result_ref": ref}
	}
	if event.PayloadRef != "" {
		return map[string]any{"result_ref": event.PayloadRef}
	}
	return aiSDKEventData(event)
}

func stringField(payload map[string]any, key string) string {
	if value, ok := payload[key].(string); ok {
		return value
	}
	return ""
}

func lastUserText(msgs []aiChatMessage) string {
	text := ""
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		if m.Content != "" {
			text = m.Content
			continue
		}
		for _, p := range m.Parts {
			if p.Type == "text" && p.Text != "" {
				text = p.Text
			}
		}
	}
	return text
}
