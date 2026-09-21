package storage

import (
	"encoding/json"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// ControlRequestSchemaVersion is the canonical schema version for ControlRequest
// records (canonical contract §3).
const ControlRequestSchemaVersion = "harness.control_request.v1"

// Session is the long-lived conversation container. It is distinct from
// context.Session (the runtime working context) and from the app-layer session
// service; this is the durable ledger record.
type Session struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	// UserID 非空时最多 64 个 UTF-8 字符；所有 Store 必须在写入前校验。
	UserID    string            `json:"user_id"`
	Channel   string            `json:"channel,omitempty"` // app|web|miniapp|desktop|vehicle
	AgentID   string            `json:"agent_id,omitempty"`
	Title     string            `json:"title,omitempty"`
	Status    string            `json:"status"` // active|archived|deleted
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	Version   int64             `json:"version"` // optimistic lock (CAS)
	Metadata  map[string]string `json:"metadata,omitempty"`
}

const (
	SessionStatusActive   = "active"
	SessionStatusArchived = "archived"
	SessionStatusDeleted  = "deleted"
)

// Turn is one user input and its associated run(s).
type Turn struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Seq       int       `json:"seq"`
	CreatedAt time.Time `json:"created_at"`
}

// Message is a user-facing conversation message. Large bodies live in artifact
// and are referenced via ContentRef.
type Message struct {
	ID             string                        `json:"id"`
	SessionID      string                        `json:"session_id"`
	TurnID         string                        `json:"turn_id,omitempty"`
	RunID          string                        `json:"run_id,omitempty"`
	TenantID       string                        `json:"tenant_id"`
	Role           string                        `json:"role"` // user|assistant|system
	Visibility     observability.EventVisibility `json:"visibility"`
	ContentRef     string                        `json:"content_ref,omitempty"`
	ContentPreview string                        `json:"content_preview,omitempty"`
	CreatedAt      time.Time                     `json:"created_at"`
}

// ModelUsageRecord is the queryable billing/accounting fact for one settled
// downstream model attempt, including failed and fallback attempts. Events keep
// lifecycle status; this record is the normalized tenant/session/run ledger.
type ModelUsageRecord struct {
	ID                    string    `json:"id"`
	RequestID             string    `json:"request_id,omitempty"`
	TraceID               string    `json:"trace_id,omitempty"`
	TenantID              string    `json:"tenant_id"`
	SessionID             string    `json:"session_id,omitempty"`
	RunID                 string    `json:"run_id,omitempty"`
	AgentID               string    `json:"agent_id,omitempty"`
	Provider              string    `json:"provider,omitempty"`
	Model                 string    `json:"model,omitempty"`
	Attempt               int       `json:"attempt,omitempty"`
	FallbackApplied       bool      `json:"fallback_applied,omitempty"`
	PromptTokens          int       `json:"prompt_tokens"`
	CompletionTokens      int       `json:"completion_tokens"`
	ReasoningTokens       int       `json:"reasoning_tokens,omitempty"`
	CacheReadTokens       int       `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens      int       `json:"cache_write_tokens,omitempty"`
	UsageSource           string    `json:"usage_source,omitempty"`
	Currency              string    `json:"currency,omitempty"`
	EstimatedCost         float64   `json:"estimated_cost,omitempty"`
	TotalLatencyMS        int64     `json:"total_latency_ms"`
	FirstTokenObserved    bool      `json:"first_token_observed"`
	FirstTokenMS          int64     `json:"first_token_ms"`
	GenerationDurationMS  int64     `json:"generation_duration_ms"`
	OutputTokensPerSecond float64   `json:"output_tokens_per_second"`
	CacheHit              bool      `json:"cache_hit,omitempty"`
	OutputRef             string    `json:"output_ref,omitempty"`
	CreatedAt             time.Time `json:"created_at"`
}

// ModelUsageQuery filters usage records for tenant accounting views.
type ModelUsageQuery struct {
	TenantID  string
	SessionID string
	RunID     string
	AgentID   string
	Provider  string
	Model     string
	From      time.Time
	To        time.Time
	Limit     int
}

type ModelUsageSummary struct {
	TenantID         string  `json:"tenant_id,omitempty"`
	Records          int     `json:"records"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	ReasoningTokens  int     `json:"reasoning_tokens,omitempty"`
	CacheReadTokens  int     `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int     `json:"cache_write_tokens,omitempty"`
	EstimatedCost    float64 `json:"estimated_cost,omitempty"`
	Currency         string  `json:"currency,omitempty"`
}

// Run is one Agent execution instance. Status is owned by this package.
type Run struct {
	RunID              string          `json:"run_id"`
	SessionID          string          `json:"session_id"`
	TurnID             string          `json:"turn_id,omitempty"`
	ParentRunID        string          `json:"parent_run_id,omitempty"`
	TenantID           string          `json:"tenant_id"`
	AgentID            string          `json:"agent_id,omitempty"`
	Runtime            string          `json:"runtime,omitempty"`
	RuntimeBinding     json.RawMessage `json:"runtime_binding,omitempty"`
	Status             RunStatus       `json:"status"`
	TraceID            string          `json:"trace_id,omitempty"`
	ConfigSnapshotRef  string          `json:"config_snapshot_ref,omitempty"`
	ContextSnapshotRef string          `json:"context_snapshot_ref,omitempty"`
	AgentBindingID     string          `json:"agent_binding_id,omitempty"`
	ResumeAttemptID    string          `json:"resume_attempt_id,omitempty"`
	StartedAt          time.Time       `json:"started_at,omitempty"`
	EndedAt            time.Time       `json:"ended_at,omitempty"`
	ErrorCode          string          `json:"error_code,omitempty"`
	ErrorMessage       string          `json:"error_message,omitempty"`
	Version            int64           `json:"version"` // optimistic lock (CAS)
}

// RunMutation carries fields applied atomically during a status transition.
type RunMutation struct {
	EndedAt      time.Time
	ErrorCode    string
	ErrorMessage string
}

// ResumeClaimCommand carries only the hashed resume credential. AttemptID is
// the explicit owner used to fence concurrent resume requests.
type ResumeClaimCommand struct {
	RunID            string
	SessionID        string
	CheckpointID     string
	ControlRequestID string
	ResumeTokenHash  string
	AttemptID        string
	Event            observability.AgentEvent
}

// ResumeWaitCommand atomically binds a persisted checkpoint to a pending
// control request, moves the Run to waiting_control, and appends its event.
type ResumeWaitCommand struct {
	Control *ControlRequest
	Event   observability.AgentEvent
}

type ResumeFailureCommand struct {
	RunID        string
	AttemptID    string
	Event        observability.AgentEvent
	ErrorCode    string
	ErrorMessage string
	Retryable    bool
}

type ResumeActivationCommand struct {
	RunID     string
	AttemptID string
}

// Step is a run-internal step record. Status is stored as a canonical string;
// the StepStatus enum and its transition rules are owned by agentruntime (D1-A).
type Step struct {
	StepID       string    `json:"step_id"`
	RunID        string    `json:"run_id"`
	ParentStepID string    `json:"parent_step_id,omitempty"`
	StepType     string    `json:"step_type"` // model|tool|subagent|workflow_node|control_request
	Name         string    `json:"name,omitempty"`
	Status       string    `json:"status"`
	StartedAt    time.Time `json:"started_at"`
	EndedAt      time.Time `json:"ended_at,omitempty"`
}

// CheckpointMeta holds resume metadata; the checkpoint content lives in artifact
// and is referenced via StateRef.
type CheckpointMeta struct {
	CheckpointID  string    `json:"checkpoint_id"`
	RunID         string    `json:"run_id"`
	TenantID      string    `json:"tenant_id"`
	Runtime       string    `json:"runtime,omitempty"`
	Type          string    `json:"type"` // run|agent|workflow_node|tool
	StateRef      string    `json:"state_ref"`
	EventSequence int64     `json:"event_sequence"`
	CreatedReason string    `json:"created_reason,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at,omitempty"`
}

// ControlRequest is the persisted state of an AskUser/HITL/permission/elicitation
// interaction. The status is a canonical string; the ControlRequestStatus enum
// and its transitions are owned by internal/control (D2).
type ControlRequest struct {
	RequestID       string    `json:"request_id"`
	RunID           string    `json:"run_id"`
	TenantID        string    `json:"tenant_id"`
	CheckpointID    string    `json:"checkpoint_id,omitempty"`
	Type            string    `json:"type"` // ask_user|permission_request|hitl_review|mcp_elicitation
	Status          string    `json:"status"`
	ResumeTokenHash string    `json:"resume_token_hash,omitempty"`
	ToolUseID       string    `json:"tool_use_id,omitempty"`
	PromptPreview   string    `json:"prompt_preview,omitempty"`
	ResponseRef     string    `json:"response_ref,omitempty"`
	SchemaVersion   string    `json:"schema_version"`
	CreatedAt       time.Time `json:"created_at"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
	Version         int64     `json:"version"` // optimistic lock (CAS)
}

// AppendResult is returned by EventStore.Append. Idempotent is true when the
// append matched an already-persisted event (duplicate event_id/idempotency_key).
type AppendResult struct {
	Event      observability.AgentEvent
	Idempotent bool
}

type OpenTurnCommand struct {
	Session          Session
	Run              Run
	Message          Message
	Events           []observability.AgentEvent
	IdempotencyKey   string
	IdempotencyScope string
	RequestHash      string
}

type OpenTurnCommit struct {
	Session    *Session
	Run        *Run
	Message    *Message
	Events     []observability.AgentEvent
	Idempotent bool
}

// --- query / pagination types -------------------------------------------------

// SessionListQuery is a keyset (cursor) query over sessions, ordered by
// updated_at desc, session_id desc (session-run-storage-design.md §12.1).
type SessionListQuery struct {
	UserID          string
	AgentID         string
	BeforeUpdatedAt time.Time
	BeforeSessionID string
	Limit           int
	IncludeArchived bool
}

type SessionPage struct {
	Items               []*Session
	NextBeforeUpdatedAt time.Time
	NextBeforeSessionID string
	HasMore             bool
}

// MessageListQuery pages messages either backward (BeforeMessageID) or forward
// (AfterMessageID), ordered by created_at asc, message_id asc.
type MessageListQuery struct {
	SessionID       string
	BeforeMessageID string
	AfterMessageID  string
	Limit           int
	// Visibilities filters the returned messages; empty means user_visible only.
	Visibilities []observability.EventVisibility
}

type MessagePage struct {
	Items               []*Message
	NextBeforeMessageID string
	NextAfterMessageID  string
	HasMore             bool
}

// EventQuery is an after_sequence replay query over a run's events.
type EventQuery struct {
	RunID string
	// AfterSequence is exclusive: results start at AfterSequence+1.
	AfterSequence int64
	Limit         int
	// Visibilities filters results; empty means all visibilities (admin view).
	Visibilities []observability.EventVisibility
	// EventTypes optionally restricts to specific event types.
	EventTypes []observability.EventType
}

// IdemKey is a namespaced idempotency key (e.g. resume tokens, control responses).
type IdemKey struct {
	TenantID  string
	Namespace string // e.g. "resume_token", "control_response"
	Key       string
	TTL       time.Duration
}

const (
	IdemNamespaceResumeToken     = "resume_token"
	IdemNamespaceControlResponse = "control_response"
)
