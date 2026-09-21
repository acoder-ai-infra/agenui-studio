package harness

import (
	"context"
	"encoding/json"
	"time"
)

// EventType 映射 canonical Event Type Registry（详见
// the public protocol contract）。SDK 以 typed string 暴露它，方便
// 调用方在不 import 内部包的情况下 switch。
type EventType string

// 下列常量是 SDK 向调用方公开的 canonical 事件类型。它们是 kernel 产生的确切
// 字符串值；canonical registry 新增的事件类型必须也只能在此处添加，之后外部
// 消费方才可以依赖它们。
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

// allEventTypes 是本包对外暴露的全部 canonical 事件类型清单。它与
// internal/observability 的 Event Type Registry 由 event_parity_test.go
// 自动门禁保持一致：在上面新增常量时必须同步本列表，否则门禁失败。
func allEventTypes() []EventType {
	return []EventType{
		EventSessionCreated, EventUserMessageReceived,
		EventRunCreated, EventRunStarted, EventRunCompleted,
		EventRunFailed, EventRunCancelled, EventRunExpired,
		EventResumeAccepted, EventResumeFailed,
		EventAgentBinding, EventAgentBindingFailed,
		EventContextBuildStarted, EventContextSnapshotCreated, EventContextDeltaCreated,
		EventModelContextBuilt, EventModelContextBuildFailed, EventContextBuildFailed,
		EventRuntimeStepStarted, EventRuntimeStepCompleted,
		EventRuntimeStepFailed, EventRuntimeStepCancelled,
		EventAgentStarted, EventAgentTextDelta, EventReasoningSummary,
		EventAgentCompleted, EventAgentFailed,
		EventModelCallStarted, EventModelTokenDelta, EventModelThoughtDelta,
		EventModelToolCallDelta, EventModelUsageDelta, EventModelCallCompleted,
		EventModelCallFailed, EventModelFallbackApplied,
		EventToolCallStarted, EventToolCallProgress, EventToolCallCompleted,
		EventToolCallFailed, EventToolCallCancelled, EventToolArtifactCreated,
		EventSubAgentStarted, EventSubAgentProgress, EventSubAgentCompleted, EventSubAgentFailed,
		EventA2ATaskCreated, EventA2ATaskProgress, EventA2ATaskCompleted,
		EventA2ATaskFailed, EventA2ATaskCancelled,
		EventWorkflowStepStarted, EventWorkflowStepCompleted, EventWorkflowStepFailed,
		EventSkillStarted, EventSkillCompleted, EventSkillFailed,
		EventControlRequestCreated, EventControlResponseReceived, EventControlRequestExpired,
		EventGuardrailTriggered, EventGuardrailBlocked,
		EventCheckpointCreated, EventArtifactCreated, EventFallbackApplied,
		EventFeedbackReceived, EventFinalResponse,
	}
}

// EventErrorType 映射 canonical EventError.Type 枚举。
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

// EventError 是 canonical 事件错误记录在 SDK 侧的视图。
type EventError struct {
	Code      string         `json:"code"`
	Type      EventErrorType `json:"type"`
	Message   string         `json:"message,omitempty"`
	Retryable bool           `json:"retryable,omitempty"`
}

// Event 是 canonical AgentEvent（harness.agent_event.v1）的 SDK 侧只读投影。
// 字段映射 the public protocol contract；测试之外调用方不得手工
// 构造 Event —— kernel 是唯一生产者。
//
// 大 payload 通过 PayloadRef（artifact 引用）而非 PayloadPreview 承载，用来
// 约束内存。restricted / debug visibility 事件也会送达 SDK 调用方，便于宿主
// 构建审计投影，但 Protocol Projector 在把事件转成 user_visible frame 时
// 必须遵守 Visibility。
type Event struct {
	// EventID 是全局唯一事件 id（推荐 ULID）。
	EventID string `json:"event_id"`
	// SchemaVersion 对本 SDK kernel 产出的事件恒为 "harness.agent_event.v1"。
	// 消费方应拒绝未知版本。
	SchemaVersion string `json:"schema_version"`
	// Sequence 在同一 RunID 内单调递增。调用方在断线重连时以它作为
	// SubscribeRequest.AfterSequence 的游标。
	Sequence int64 `json:"sequence"`
	// IdempotencyKey 标识重复写入；无调用方 key 的事件为空。
	IdempotencyKey string `json:"idempotency_key,omitempty"`

	TraceID      string `json:"trace_id"`
	SpanID       string `json:"span_id,omitempty"`
	ParentSpanID string `json:"parent_span_id,omitempty"`

	SessionID   string `json:"session_id,omitempty"`
	RunID       string `json:"run_id,omitempty"`
	ParentRunID string `json:"parent_run_id,omitempty"`
	StepID      string `json:"step_id,omitempty"`
	AgentID     string `json:"agent_id,omitempty"`
	AgentType   string `json:"agent_type,omitempty"`
	Runtime     string `json:"runtime,omitempty"`

	EventType  EventType  `json:"event_type"`
	Visibility Visibility `json:"visibility"`

	// PayloadPreview 是一段已消毒的 JSON 预览，面向终端用户可见。大 payload
	// 请放在 PayloadRef 中。
	PayloadPreview json.RawMessage `json:"payload_preview,omitempty"`
	// PayloadRef 指向承载完整 payload 的 Artifact；SDK 不会把 artifact 字节
	// 内嵌到事件中。
	PayloadRef string `json:"payload_ref,omitempty"`
	// Usage 是一小份 JSON blob，承载 token / cost / duration 归属信息；
	// 只在事件类型自带用量（如 model_call_completed、tool_call_completed）
	// 时填写。
	Usage json.RawMessage `json:"usage,omitempty"`
	// DebugRef 指向一个 debug visibility 的 artifact；仅授权的调试控制台
	// 可访问。
	DebugRef string `json:"debug_ref,omitempty"`
	// Error 只在 *_failed 事件上填充。
	Error *EventError `json:"error,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// Cursor is the opaque anchor used to resume an Event subscription after a
// disconnect. It records the last sequence durably processed by the client.
type Cursor struct {
	// AfterSequence 是客户端已持久处理的最后一个 Sequence。零值表示
	//“从本 Run 最早可用事件开始”。
	AfterSequence int64 `json:"after_sequence,omitempty"`
	// RunID 可选但推荐；kernel 用它校验 cursor 属于当前订阅的 Run。
	RunID string `json:"run_id,omitempty"`
}

// EventStream 是 SDK 侧的订阅句柄。它由 Start / Resume（内嵌在
// Execution.Events）和 Subscribe 返回。stream 会按 Sequence 顺序推送本 Run
// 的每一条 AgentEvent，直至 Run 产出终态事件或调用方 Close。
//
// 契约：
//   - Next 会阻塞直到下一条事件抵达、Run 结束、或 ctx 被取消 / stream 被
//     Close。当 Run 已经产出终态事件时返回 io.EOF（见 errors.go）。
//   - Close stream 会解除 broker 订阅但不会取消 Run；取消 Run 请使用
//     Engine.Cancel。
//   - Cursor 返回最后观察到的 Sequence，用于断线切换。
type EventStream interface {
	// Next 返回下一条事件或错误。当 stream 已耗尽（进入终态或已 Close）时
	// 返回 io.EOF。
	Next(ctx context.Context) (Event, error)
	// Close 释放底层订阅。可多次调用。
	Close() error
	// Cursor 返回最后观察到的 Sequence 与 RunID。
	Cursor() Cursor
}
