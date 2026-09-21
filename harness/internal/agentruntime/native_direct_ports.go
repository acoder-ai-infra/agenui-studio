package agentruntime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/processpresentation"
)

// ModelInvoker is the Runtime-side dependency used to consume normalized model streams.
type ModelInvoker interface {
	Invoke(ctx context.Context, req ModelInvokeRequest) (<-chan ModelStreamItem, error)
}

// NativeModelInvokeTransform 在 direct 循环每轮模型调用前改写即将送出的
// ModelInvokeRequest（方案 3.3 native 路径的中立端口，实现由 Composition
// Root 从 BeforeModelHook 扩展投影）。def 携带当前 Run 的 Agent
// 定义，实现据 def.BeforeModelHooks 筛选要执行的扩展（未声明不
// 执行）。返回错误 fail closed。
type NativeModelInvokeTransform func(ctx context.Context, def AgentDefinition, req ModelInvokeRequest) (ModelInvokeRequest, error)

// ModelInvokeRequest is the Runtime projection passed to a model gateway adapter.
type ModelInvokeRequest struct {
	Trace      observability.TraceContext `json:"trace"`
	Package    ModelContextPackage        `json:"package"`
	Round      int                        `json:"round"`
	AllowTools bool                       `json:"allow_tools"`
	Messages   []ModelCallMessage         `json:"messages,omitempty"`
	Tools      []ModelToolDefinition      `json:"tools,omitempty"`
	Options    ModelCallOptions           `json:"options,omitempty"`
	// PreserveManifest is frozen before runtime-native compaction and validated
	// again by the final governor. Model gateway adapters must carry it through
	// provider-final validation and observability.
	PreserveManifest   PreserveManifest        `json:"preserve_manifest"`
	PreModelCompaction ModelPreModelCompaction `json:"pre_model_compaction,omitempty"`
	// ScopedData 是本 Run 冻结的 scoped-data 快照，仅供模型前塑形钩子
	// （BeforeModelHook）只读消费；不参与 provider 请求序列化。
	ScopedData ScopedData `json:"scoped_data,omitempty"`
}

type ModelPreModelCompaction struct {
	Completed         bool                      `json:"completed"`
	PolicyHash        string                    `json:"policy_hash,omitempty"`
	AppliedStrategies []string                  `json:"applied_strategies,omitempty"`
	SummaryRefs       []string                  `json:"summary_refs,omitempty"`
	TrimRecordRef     string                    `json:"trim_record_ref,omitempty"`
	Records           []ContextCompactionRecord `json:"records,omitempty"`
	ObservedTokens    int                       `json:"observed_tokens,omitempty"`
	SoftTriggered     bool                      `json:"soft_triggered,omitempty"`
}

// ModelCallMessage is the runtime-neutral message shape used for an actual
// model round. Package remains the immutable context fact; Messages captures
// runtime-native assistant/tool turns added after that package was built.
type ModelCallMessage struct {
	Role               string             `json:"role"`
	Content            string             `json:"content,omitempty"`
	ContentParts       []ModelContentPart `json:"content_parts,omitempty"`
	Name               string             `json:"name,omitempty"`
	ToolCalls          []ModelToolCall    `json:"tool_calls,omitempty"`
	ToolCallID         string             `json:"tool_call_id,omitempty"`
	ToolName           string             `json:"tool_name,omitempty"`
	ReasoningContent   string             `json:"reasoning_content,omitempty"`
	ReasoningSignature string             `json:"reasoning_signature,omitempty"`
}

// ModelContentPart is the provider-neutral subset of multimodal model input.
// Artifact resolution and URL policy happen before this DTO reaches a provider.
type ModelContentPart struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	URL        string `json:"url,omitempty"`
	Base64Data string `json:"base64_data,omitempty"`
	MIMEType   string `json:"mime_type,omitempty"`
	Name       string `json:"name,omitempty"`
}

// ModelToolDefinition carries the schema presented to the model. Schema is a
// JSON document owned by the capability snapshot and treated as immutable.
type ModelToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
}

// ModelCallOptions contains provider-neutral options accepted by the Harness
// model gateway. Nil pointers distinguish an unset option from a zero value.
type ModelCallOptions struct {
	Model *string `json:"model,omitempty"`
	// Fallback lists the agent-declared fallback model names, in priority order.
	// The gateway resolves each against the configured targets and appends them
	// after the primary so the model call degrades to them on retryable errors.
	Fallback         []string           `json:"fallback,omitempty"`
	Temperature      *float32           `json:"temperature,omitempty"`
	TopP             *float32           `json:"top_p,omitempty"`
	MaxTokens        *int               `json:"max_tokens,omitempty"`
	Stop             []string           `json:"stop,omitempty"`
	ToolChoice       string             `json:"tool_choice,omitempty"`
	AllowedToolNames []string           `json:"allowed_tool_names,omitempty"`
	ReasoningMode    ModelReasoningMode `json:"reasoning_mode,omitempty"`
	ReasoningBudget  int                `json:"reasoning_budget_tokens,omitempty"`
	ReasoningEffort  string             `json:"reasoning_effort,omitempty"`
	// ResponseFormat 是 provider 约束解码输出格式（G-A）："" | json_object。
	ResponseFormat string `json:"response_format,omitempty"`
	// ImageDetail 是本请求全部图片 part 的 provider 视觉编码预算（G-B）：
	// "" | low | high | auto。
	ImageDetail string `json:"image_detail,omitempty"`
}

type ModelReasoningMode string

const (
	ModelReasoningAuto     ModelReasoningMode = "auto"
	ModelReasoningEnabled  ModelReasoningMode = "enabled"
	ModelReasoningDisabled ModelReasoningMode = "disabled"
)

func (m ModelReasoningMode) Valid() bool {
	return m == "" || m == ModelReasoningAuto || m == ModelReasoningEnabled || m == ModelReasoningDisabled
}

// ModelStreamItem couples a canonical model event with fields needed by the direct loop.
type ModelStreamItem struct {
	Event              observability.AgentEvent `json:"event"`
	TextDelta          string                   `json:"text_delta,omitempty"`
	ReasoningDelta     string                   `json:"reasoning_delta,omitempty"`
	ReasoningSignature string                   `json:"reasoning_signature,omitempty"`
	ToolCall           *ModelToolCall           `json:"tool_call,omitempty"`
}

// ModelToolCall is a complete tool request normalized by the model gateway adapter.
type ModelToolCall struct {
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
	Version    string          `json:"version,omitempty"`
	Arguments  json.RawMessage `json:"arguments"`
}

// ToolInvoker is the Runtime-side dependency used to execute tools through Tool Gateway.
type ToolInvoker interface {
	Invoke(ctx context.Context, req ToolInvocationRequest, events ToolEventSink) (ToolInvocationResult, error)
}

type ToolInvocationSource string

const (
	ToolSourceRegistry ToolInvocationSource = "tool_registry"
	ToolSourceMCP      ToolInvocationSource = "mcp"
	ToolSourceSkill    ToolInvocationSource = "skill"
	ToolSourceHTTPTool ToolInvocationSource = "http_tool"
)

// ToolEventSink receives canonical tool events while a tool invocation is running.
type ToolEventSink interface {
	Emit(ctx context.Context, event observability.AgentEvent) error
}

// ToolInvocationRequest is the Runtime projection consumed by a Tool Gateway adapter.
type ToolInvocationRequest struct {
	Trace        observability.TraceContext `json:"trace"`
	SessionID    string                     `json:"session_id"`
	RunID        string                     `json:"run_id"`
	AgentID      string                     `json:"agent_id"`
	ToolCallID   string                     `json:"tool_call_id"`
	ToolName     string                     `json:"tool_name"`
	ToolVersion  string                     `json:"tool_version,omitempty"`
	Source       ToolInvocationSource       `json:"source,omitempty"`
	SourceRef    string                     `json:"source_ref,omitempty"`
	SnapshotRef  string                     `json:"snapshot_ref,omitempty"`
	ProcessStage processpresentation.Stage  `json:"process_stage,omitempty"`
	Arguments    json.RawMessage            `json:"arguments"`
	Resume       *ToolInvocationResume      `json:"resume,omitempty"`
}

// ToolInvocationResume is runtime-neutral resume state passed back to Tool
// Gateway after a tool-originated ControlRequest has been answered.
type ToolInvocationResume struct {
	WasInterrupted bool            `json:"was_interrupted"`
	IsResumeTarget bool            `json:"is_resume_target"`
	State          json.RawMessage `json:"state,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
}

// ToolInvocationInterruptedError asks the active runtime to persist native
// tool state and surface a canonical ControlRequest. Runtime adapters translate
// it to their native interrupt primitive; Tool Gateway never imports a runtime.
type ToolInvocationInterruptedError struct {
	Info  any
	State json.RawMessage
	Cause error
}

func (e *ToolInvocationInterruptedError) Error() string {
	return "tool invocation interrupted for control request"
}

func (e *ToolInvocationInterruptedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func IsToolInvocationInterrupted(err error) bool {
	var interrupted *ToolInvocationInterruptedError
	return errors.As(err, &interrupted)
}

// ToolInvocationResult carries the protected result returned by Tool Gateway.
type ToolInvocationResult struct {
	Content    string `json:"content,omitempty"`
	ContentRef string `json:"content_ref,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
}

// ModelContextRebuilder rebuilds a package after a tool result becomes a context fact.
type ModelContextRebuilder interface {
	Rebuild(ctx context.Context, req ModelContextRebuildRequest) (ModelContextPackage, error)
}

// ModelContextRebuildRequest contains the facts required for a post-tool context rebuild.
type ModelContextRebuildRequest struct {
	Run            RunRequest           `json:"run"`
	InitialPackage ModelContextPackage  `json:"initial_package"`
	ToolCall       ModelToolCall        `json:"tool_call"`
	Result         ToolInvocationResult `json:"result"`
}
