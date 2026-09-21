package observability

import (
	"context"
	"encoding/json"
	"sort"
	"time"
)

type AgentEvent struct {
	EventID        string `json:"event_id"`
	SchemaVersion  string `json:"schema_version"`
	Sequence       int64  `json:"sequence"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	TraceID        string `json:"trace_id"`
	SpanID         string `json:"span_id,omitempty"`
	ParentSpanID   string `json:"parent_span_id,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	RunID          string `json:"run_id,omitempty"`
	// ParentRunID attributes a child (sub-agent) run's event to its parent run
	// when fanned into the parent run-tree stream. It is empty for a run's own
	// events; it is stamped only on the run-tree fan-in path (never persisted),
	// so the client can nest sub-agent events under their parent.
	ParentRunID    string          `json:"parent_run_id,omitempty"`
	StepID         string          `json:"step_id,omitempty"`
	AgentID        string          `json:"agent_id,omitempty"`
	AgentType      string          `json:"agent_type,omitempty"`
	Runtime        string          `json:"runtime,omitempty"`
	EventType      EventType       `json:"event_type"`
	Visibility     EventVisibility `json:"visibility"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	PayloadPreview json.RawMessage `json:"payload_preview,omitempty"`
	PayloadRef     string          `json:"payload_ref,omitempty"`
	Usage          json.RawMessage `json:"usage,omitempty"`
	DebugRef       string          `json:"debug_ref,omitempty"`
	Error          *EventError     `json:"error,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

const AgentEventSchemaVersion = "harness.agent_event.v1"

type EventType string

const (
	EventSessionCreated          EventType = "session_created"
	EventUserMessageReceived     EventType = "user_message_received"
	EventRunCreated              EventType = "run_created"
	EventRunStarted              EventType = "run_started"
	EventRunCompleted            EventType = "run_completed"
	EventRunFailed               EventType = "run_failed"
	EventRunCancelled            EventType = "run_cancelled"
	EventRunExpired              EventType = "run_expired"
	EventResumeAccepted          EventType = "resume_accepted"
	EventResumeFailed            EventType = "resume_failed"
	EventAgentBinding            EventType = "agent_binding"
	EventAgentBindingFailed      EventType = "agent_binding_failed"
	EventContextBuildStarted     EventType = "context_build_started"
	EventContextSnapshotCreated  EventType = "context_snapshot_created"
	EventContextDeltaCreated     EventType = "context_delta_created"
	EventModelContextBuilt       EventType = "model_context_built"
	EventModelContextBuildFailed EventType = "model_context_build_failed"
	EventContextBuildFailed      EventType = "context_build_failed"
	EventRuntimeStepStarted      EventType = "runtime_step_started"
	EventRuntimeStepCompleted    EventType = "runtime_step_completed"
	EventRuntimeStepFailed       EventType = "runtime_step_failed"
	EventRuntimeStepCancelled    EventType = "runtime_step_cancelled"
	EventAgentStarted            EventType = "agent_started"
	EventAgentTextDelta          EventType = "agent_text_delta"
	EventReasoningSummary        EventType = "reasoning_summary"
	EventAgentCompleted          EventType = "agent_completed"
	EventAgentFailed             EventType = "agent_failed"
	EventModelCallStarted        EventType = "model_call_started"
	EventModelTokenDelta         EventType = "model_token_delta"
	EventModelThoughtDelta       EventType = "model_thought_delta"
	EventModelToolCallDelta      EventType = "model_tool_call_delta"
	EventModelUsageDelta         EventType = "model_usage_delta"
	EventModelCallCompleted      EventType = "model_call_completed"
	EventModelCallFailed         EventType = "model_call_failed"
	EventModelFallbackApplied    EventType = "model_fallback_applied"
	EventToolCallStarted         EventType = "tool_call_started"
	EventToolCallProgress        EventType = "tool_call_progress"
	EventToolCallCompleted       EventType = "tool_call_completed"
	EventToolCallFailed          EventType = "tool_call_failed"
	EventToolCallCancelled       EventType = "tool_call_cancelled"
	EventToolArtifactCreated     EventType = "tool_artifact_created"
	EventSubAgentStarted         EventType = "sub_agent_started"
	EventSubAgentProgress        EventType = "sub_agent_progress"
	EventSubAgentCompleted       EventType = "sub_agent_completed"
	EventSubAgentFailed          EventType = "sub_agent_failed"
	EventA2ATaskCreated          EventType = "a2a_task_created"
	EventA2ATaskProgress         EventType = "a2a_task_progress"
	EventA2ATaskCompleted        EventType = "a2a_task_completed"
	EventA2ATaskFailed           EventType = "a2a_task_failed"
	EventA2ATaskCancelled        EventType = "a2a_task_cancelled"
	EventWorkflowStepStarted     EventType = "workflow_step_started"
	EventWorkflowStepCompleted   EventType = "workflow_step_completed"
	EventWorkflowStepFailed      EventType = "workflow_step_failed"
	EventSkillStarted            EventType = "skill_started"
	EventSkillCompleted          EventType = "skill_completed"
	EventSkillFailed             EventType = "skill_failed"
	EventControlRequestCreated   EventType = "control_request_created"
	EventControlResponseReceived EventType = "control_response_received"
	EventControlRequestExpired   EventType = "control_request_expired"
	EventGuardrailTriggered      EventType = "guardrail_triggered"
	EventGuardrailBlocked        EventType = "guardrail_blocked"
	EventCheckpointCreated       EventType = "checkpoint_created"
	EventArtifactCreated         EventType = "artifact_created"
	EventFallbackApplied         EventType = "fallback_applied"
	EventFeedbackReceived        EventType = "feedback_received"
	EventFinalResponse           EventType = "final_response"
)

// registeredEventTypes is the canonical Event Type Registry (contract §6). Only
// these types may be persisted to the Event Store; unregistered types must be
// normalized to a canonical type or rejected (conformance E-002).
var registeredEventTypes = map[EventType]bool{
	EventSessionCreated: true, EventUserMessageReceived: true,
	EventRunCreated: true, EventRunStarted: true, EventRunCompleted: true,
	EventRunFailed: true, EventRunCancelled: true, EventRunExpired: true,
	EventResumeAccepted: true, EventResumeFailed: true,
	EventAgentBinding: true, EventAgentBindingFailed: true,
	EventContextBuildStarted: true, EventContextSnapshotCreated: true, EventContextDeltaCreated: true,
	EventContextBuildFailed: true, EventModelContextBuilt: true, EventModelContextBuildFailed: true,
	EventRuntimeStepStarted: true, EventRuntimeStepCompleted: true,
	EventRuntimeStepFailed: true, EventRuntimeStepCancelled: true,
	EventAgentStarted: true, EventAgentTextDelta: true, EventReasoningSummary: true,
	EventAgentCompleted: true, EventAgentFailed: true,
	EventModelCallStarted: true, EventModelTokenDelta: true, EventModelThoughtDelta: true,
	EventModelToolCallDelta: true, EventModelUsageDelta: true, EventModelCallCompleted: true,
	EventModelCallFailed: true, EventModelFallbackApplied: true,
	EventToolCallStarted: true, EventToolCallProgress: true, EventToolCallCompleted: true,
	EventToolCallFailed: true, EventToolCallCancelled: true, EventToolArtifactCreated: true,
	EventSubAgentStarted: true, EventSubAgentProgress: true, EventSubAgentCompleted: true,
	EventSubAgentFailed: true,
	EventA2ATaskCreated: true, EventA2ATaskProgress: true, EventA2ATaskCompleted: true,
	EventA2ATaskFailed: true, EventA2ATaskCancelled: true,
	EventWorkflowStepStarted: true, EventWorkflowStepCompleted: true, EventWorkflowStepFailed: true,
	EventSkillStarted: true, EventSkillCompleted: true, EventSkillFailed: true,
	EventControlRequestCreated: true, EventControlResponseReceived: true, EventControlRequestExpired: true,
	EventGuardrailTriggered: true, EventGuardrailBlocked: true,
	EventCheckpointCreated: true, EventArtifactCreated: true, EventFallbackApplied: true,
	EventFeedbackReceived: true, EventFinalResponse: true,
}

// IsRegisteredEventType reports whether t is a canonical, persistable event type.
func IsRegisteredEventType(t EventType) bool {
	return registeredEventTypes[t]
}

// AllEventTypes 返回 canonical Event Type Registry 的全部事件类型（按字符串
// 升序）。它是 SDK 侧事件枚举一致性门禁（harness/event_parity_test.go）的
// 事实源：新增事件类型必须先登记到 registeredEventTypes，门禁会自动要求
// 公共 SDK 同步暴露对应常量。
func AllEventTypes() []EventType {
	out := make([]EventType, 0, len(registeredEventTypes))
	for t := range registeredEventTypes {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// AllEventErrorTypes 返回 canonical EventError.Type 枚举的全部值，供 SDK 侧
// parity 门禁比对；新增错误类型常量时必须同步本列表。
func AllEventErrorTypes() []EventErrorType {
	return []EventErrorType{
		EventErrorTimeout, EventErrorCancelled, EventErrorPermissionDenied,
		EventErrorGuardrailBlocked, EventErrorSchemaValidation, EventErrorUpstream,
		EventErrorRateLimited, EventErrorResourceExhausted, EventErrorInternal,
	}
}

// AllEventVisibilities 返回 canonical Visibility 枚举的全部值，供 SDK 侧
// parity 门禁比对；新增 visibility 常量时必须同步本列表。
func AllEventVisibilities() []EventVisibility {
	return []EventVisibility{
		VisibilityUserVisible, VisibilityDebug, VisibilityInternal, VisibilityRestricted,
	}
}

// terminalRunEventTypes 是 Run 状态机的终态事件集合（canonical contract 状态机
// 定义）：观察到其中任意一条即表示本 Run 不会再产生新事件。
var terminalRunEventTypes = map[EventType]bool{
	EventRunCompleted: true,
	EventRunFailed:    true,
	EventRunCancelled: true,
	EventRunExpired:   true,
}

// IsTerminalRunEventType 报告 t 是否为 Run 的终态事件类型。SDK 与协议层判断
// “流是否结束”时必须复用本函数，不得自行维护终态集合副本。
func IsTerminalRunEventType(t EventType) bool {
	return terminalRunEventTypes[t]
}

// ephemeralDeltaEventTypes 是高频"逐字实时增量"事件:它们是打字机动画的载体,不是
// 事实账本。其完整内容可从 model_call_completed / final_response 重建,因此走实时
// 通道(Broker/SSE + HotBuffer 短 TTL 兜底),不进 EventStore,以避免 token 级逐条
// 落库压垮存储(model-gateway-landing-design §9.1/§9.2 实时/事实通道分离)。
//
// 注意:model_thought_delta / model_tool_call_delta / model_usage_delta 是事实
// (思维链、工具参数、用量),别处无法重建,故不在此列,仍照常落库。
var ephemeralDeltaEventTypes = map[EventType]bool{
	EventModelTokenDelta: true,
	EventAgentTextDelta:  true,
}

// IsEphemeralDelta reports whether t is a high-frequency realtime delta that
// should be streamed live (and buffered short-TTL) but NOT persisted to the
// EventStore. Reconnect anchors must never point at an ephemeral delta.
func IsEphemeralDelta(t EventType) bool {
	return ephemeralDeltaEventTypes[t]
}

type EventVisibility string

const (
	VisibilityUserVisible EventVisibility = "user_visible"
	VisibilityDebug       EventVisibility = "debug"
	VisibilityInternal    EventVisibility = "internal"
	VisibilityRestricted  EventVisibility = "restricted"
)

type EventError struct {
	Code      string         `json:"code"`
	Type      EventErrorType `json:"type"`
	Message   string         `json:"message,omitempty"`
	Retryable bool           `json:"retryable,omitempty"`
}

type EventErrorType string

const (
	EventErrorTimeout           EventErrorType = "timeout"
	EventErrorCancelled         EventErrorType = "cancelled"
	EventErrorPermissionDenied  EventErrorType = "permission_denied"
	EventErrorGuardrailBlocked  EventErrorType = "guardrail_blocked"
	EventErrorSchemaValidation  EventErrorType = "schema_validation_failed"
	EventErrorUpstream          EventErrorType = "upstream_error"
	EventErrorRateLimited       EventErrorType = "rate_limited"
	EventErrorResourceExhausted EventErrorType = "resource_exhausted"
	EventErrorInternal          EventErrorType = "internal_error"
)

type EventEmitter interface {
	Emit(ctx context.Context, event AgentEvent) error
}

type LoggingEventEmitter struct {
	Logger StructuredLogger
	IDs    IDGenerator
}

func (e LoggingEventEmitter) Emit(ctx context.Context, event AgentEvent) error {
	logger := e.Logger
	if logger == nil {
		logger = NoopLogger{}
	}
	ids := e.IDs
	if ids == nil {
		ids = NewULIDGenerator("")
	}
	tc := MustTraceContext(ctx)
	if event.EventID == "" {
		event.EventID = ids.NewEventID()
	}
	if event.SchemaVersion == "" {
		event.SchemaVersion = AgentEventSchemaVersion
	}
	if event.TraceID == "" {
		event.TraceID = tc.TraceID
	}
	if event.SpanID == "" {
		event.SpanID = tc.SpanID
	}
	if event.SessionID == "" {
		event.SessionID = tc.SessionID
	}
	if event.RunID == "" {
		event.RunID = tc.RunID
	}
	if event.AgentID == "" {
		event.AgentID = tc.AgentID
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	logger.Info(ctx, "agent event emitted",
		String("event_id", event.EventID),
		String("event_type", string(event.EventType)),
		String("visibility", string(event.Visibility)),
		String("payload_ref", event.PayloadRef),
	)
	return nil
}
