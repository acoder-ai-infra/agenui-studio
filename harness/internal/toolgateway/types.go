package toolgateway

import (
	"context"
	"encoding/json"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/processpresentation"
)

type ToolGateway interface {
	Invoke(ctx context.Context, req ToolCallRequest) (*ToolCallResult, error)
}

type StreamingToolGateway interface {
	ToolGateway
	InvokeWithEvents(ctx context.Context, req ToolCallRequest, sink PersistedEventSink) (*ToolCallResult, error)
}

// PersistedEventSink observes the exact canonical event returned by EventStore.
// Consumers must preserve EventID, Sequence, IdempotencyKey and payload refs.
type PersistedEventSink interface {
	Emit(ctx context.Context, event observability.AgentEvent) error
}

const (
	MetadataHarnessSource      = "harness.source"
	MetadataHarnessSourceRef   = "harness.source_ref"
	MetadataHarnessSnapshotRef = "harness.snapshot_ref"
)

type ToolCallRequest struct {
	ToolCallID string `json:"tool_call_id"`
	TraceID    string `json:"trace_id"`
	SpanID     string `json:"span_id,omitempty"`

	TenantID     string `json:"tenant_id"`
	UserID       string `json:"user_id,omitempty"`
	SessionID    string `json:"session_id"`
	RunID        string `json:"run_id"`
	StepID       string `json:"step_id"`
	ParentStepID string `json:"parent_step_id,omitempty"`
	AgentID      string `json:"agent_id"`

	ToolName    string `json:"tool_name"`
	ToolVersion string `json:"tool_version,omitempty"`

	Arguments        json.RawMessage           `json:"arguments,omitempty"`
	ArgumentsRef     string                    `json:"arguments_ref,omitempty"`
	ArgumentsPreview map[string]any            `json:"arguments_preview,omitempty"`
	ProcessStage     processpresentation.Stage `json:"process_stage,omitempty"`

	Caller   ToolCaller        `json:"caller"`
	Policy   ToolCallPolicy    `json:"policy"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Resume   *ToolCallResume   `json:"resume,omitempty"`
}

// ToolCallResume carries runtime-neutral state for continuing a suspended
// ToolCall. Tool Gateway owns the lifecycle; runtime adapters only translate
// between this contract and a runtime's native resume representation.
type ToolCallResume struct {
	WasInterrupted bool            `json:"was_interrupted"`
	IsResumeTarget bool            `json:"is_resume_target"`
	State          json.RawMessage `json:"state,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
}

type ToolCaller struct {
	Type    string `json:"type"`
	AgentID string `json:"agent_id,omitempty"`
	NodeID  string `json:"node_id,omitempty"`
}

type ToolCallPolicy struct {
	RiskLevel       RiskLevel     `json:"risk_level"`
	RequireApproval bool          `json:"require_approval"`
	Timeout         time.Duration `json:"timeout"`
	MaxRetries      int           `json:"max_retries"`
	IdempotencyKey  string        `json:"idempotency_key,omitempty"`
	AllowFallback   bool          `json:"allow_fallback,omitempty"`
	Scopes          []string      `json:"scopes,omitempty"`
}

type RiskLevel string

const (
	RiskLow    RiskLevel = "low"
	RiskMedium RiskLevel = "medium"
	RiskHigh   RiskLevel = "high"
)

type ToolCallStatus string

const (
	ToolCallSucceeded ToolCallStatus = "succeeded"
	ToolCallFailed    ToolCallStatus = "failed"
	ToolCallCancelled ToolCallStatus = "cancelled"
	ToolCallRunning   ToolCallStatus = "running"
	ToolCallSuspended ToolCallStatus = "suspended"
)

type ToolType string

const (
	ToolTypeFunction ToolType = "function"
	ToolTypeHTTP     ToolType = "http"
	ToolTypeMCP      ToolType = "mcp"
)

type ToolDefinition struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Type        ToolType `json:"type"`
	DisplayName string   `json:"display_name,omitempty"`
	Description string   `json:"description,omitempty"`
	Disabled    bool     `json:"disabled,omitempty"`

	InputSchemaRef  string          `json:"input_schema_ref"`
	InputSchema     json.RawMessage `json:"input_schema,omitempty"`
	OutputSchemaRef string          `json:"output_schema_ref,omitempty"`
	OutputSchema    json.RawMessage `json:"output_schema,omitempty"`

	// ConfigSchema, when non-empty, declares this tool as tenant-configurable:
	// it is a JSON Schema describing the per-tenant configuration managed via the
	// tool-config control plane (endpoint/credential-env/host-allowlist/etc.).
	// Tools with no ConfigSchema need no per-tenant configuration.
	ConfigSchema json.RawMessage `json:"config_schema,omitempty"`

	RiskLevel    RiskLevel                     `json:"risk_level"`
	Timeout      time.Duration                 `json:"timeout"`
	Retry        RetryPolicy                   `json:"retry"`
	Permissions  ToolPermissions               `json:"permissions"`
	ResultPolicy ToolOutputPolicy              `json:"result_policy"`
	Visibility   observability.EventVisibility `json:"visibility"`

	Function *FunctionToolSpec `json:"function,omitempty"`
	HTTP     *HTTPToolSpec     `json:"http,omitempty"`
	MCP      *MCPToolSpec      `json:"mcp,omitempty"`

	Metadata map[string]string `json:"metadata,omitempty"`
}

type ToolPermissions struct {
	AllowedAgents  []string `json:"allowed_agents,omitempty"`
	RequiredScopes []string `json:"required_scopes,omitempty"`
	TenantScope    string   `json:"tenant_scope,omitempty"`
}

type RetryPolicy struct {
	MaxAttempts int  `json:"max_attempts"`
	Idempotent  bool `json:"idempotent"`
}

type ToolOutputPolicy struct {
	MaxInlineBytes         int  `json:"max_inline_bytes"`
	MaxModelContextBytes   int  `json:"max_model_context_bytes"`
	MaxSSEPreviewBytes     int  `json:"max_sse_preview_bytes"`
	ArtifactThresholdBytes int  `json:"artifact_threshold_bytes"`
	RedactSensitiveFields  bool `json:"redact_sensitive_fields"`
	RequireOutputSchema    bool `json:"require_output_schema"`
	SummarizeWhenTruncated bool `json:"summarize_when_truncated"`
}

func DefaultToolOutputPolicy() ToolOutputPolicy {
	return ToolOutputPolicy{
		MaxInlineBytes:         4096,
		MaxModelContextBytes:   12000,
		MaxSSEPreviewBytes:     1024,
		ArtifactThresholdBytes: 4096,
		RedactSensitiveFields:  true,
		RequireOutputSchema:    true,
		SummarizeWhenTruncated: true,
	}
}

type FunctionToolSpec struct {
	HandlerName string `json:"handler_name"`
}

type HTTPToolSpec struct {
	Method       string            `json:"method"`
	URL          string            `json:"url"`
	Headers      map[string]string `json:"headers,omitempty"`
	Timeout      time.Duration     `json:"timeout"`
	ResponseMode string            `json:"response_mode"`
	Write        bool              `json:"write,omitempty"`
}

type MCPToolSpec struct {
	ServerID    string `json:"server_id"`
	SnapshotID  string `json:"snapshot_id"`
	MCPToolName string `json:"mcp_tool_name"`
}

type ToolCallResult struct {
	ToolCallID         string                     `json:"tool_call_id"`
	ToolName           string                     `json:"tool_name"`
	ToolVersion        string                     `json:"tool_version,omitempty"`
	Status             ToolCallStatus             `json:"status"`
	ResultPreview      json.RawMessage            `json:"result_preview,omitempty"`
	ResultRef          string                     `json:"result_ref,omitempty"`
	ModelContextResult json.RawMessage            `json:"model_context_result,omitempty"`
	DebugRef           string                     `json:"debug_ref,omitempty"`
	Failure            *ToolFailure               `json:"failure,omitempty"`
	Usage              ToolUsage                  `json:"usage"`
	Events             []observability.AgentEvent `json:"events,omitempty"`
}

type ToolUsage struct {
	Duration     time.Duration `json:"duration"`
	InputBytes   int64         `json:"input_bytes"`
	OutputBytes  int64         `json:"output_bytes"`
	RetryCount   int           `json:"retry_count"`
	ArtifactRefs []string      `json:"artifact_refs,omitempty"`
}

type ToolFailure struct {
	ToolCallID       string                   `json:"tool_call_id"`
	ToolName         string                   `json:"tool_name"`
	ErrorType        string                   `json:"error_type"`
	Retryable        bool                     `json:"retryable"`
	UserVisible      bool                     `json:"user_visible"`
	Executed         bool                     `json:"executed"`
	PartialResultRef string                   `json:"partial_result_ref,omitempty"`
	SafeUserMessage  string                   `json:"safe_user_message,omitempty"`
	ModelGuidance    ToolFailureModelGuidance `json:"model_guidance"`
}

type ToolFailureModelGuidance struct {
	Instruction        string   `json:"instruction"`
	AllowedNextActions []string `json:"allowed_next_actions,omitempty"`
	ForbiddenClaims    []string `json:"forbidden_claims,omitempty"`
	RetryBudget        int      `json:"retry_budget,omitempty"`
}
