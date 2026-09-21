package agentruntime

import (
	"encoding/json"
	"time"

	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/processpresentation"
)

type RuntimeType string

const (
	RuntimeTypeAuto      RuntimeType = "auto"
	RuntimeTypeEino      RuntimeType = "eino"
	RuntimeTypeGoogleADK RuntimeType = "google_adk"
	RuntimeTypeNative    RuntimeType = "native"
	RuntimeTypeMock      RuntimeType = "mock"
)

type RuntimeMode string

const (
	RuntimeModeReact       RuntimeMode = "react"
	RuntimeModeDirect      RuntimeMode = "direct"
	RuntimeModeDeepAgent   RuntimeMode = "deep_agent"
	RuntimeModeWorkflow    RuntimeMode = "workflow"
	RuntimeModeGraph       RuntimeMode = "graph"
	RuntimeModePlanExecute RuntimeMode = "plan_execute"
)

type DataPassingMode string

const (
	DataPassingMessages   DataPassingMode = "messages"
	DataPassingState      DataPassingMode = "state"
	DataPassingPlanResult DataPassingMode = "plan_result"
	DataPassingTask       DataPassingMode = "task"
)

type RunStatus string

const (
	RunStatusCreated        RunStatus = "created"
	RunStatusRunning        RunStatus = "running"
	RunStatusWaitingControl RunStatus = "waiting_control"
	RunStatusResuming       RunStatus = "resuming"
	RunStatusCompleted      RunStatus = "completed"
	RunStatusFailed         RunStatus = "failed"
	RunStatusCancelled      RunStatus = "cancelled"
	RunStatusExpired        RunStatus = "expired"
)

type StepStatus string

const (
	StepStatusCreated        StepStatus = "created"
	StepStatusRunning        StepStatus = "running"
	StepStatusWaitingControl StepStatus = "waiting_control"
	StepStatusCompleted      StepStatus = "completed"
	StepStatusFailed         StepStatus = "failed"
	StepStatusCancelled      StepStatus = "cancelled"
	StepStatusSkipped        StepStatus = "skipped"
)

const (
	EventRunStarted              = observability.EventRunStarted
	EventRunCompleted            = observability.EventRunCompleted
	EventRunFailed               = observability.EventRunFailed
	EventRunCancelled            = observability.EventRunCancelled
	EventRunExpired              = observability.EventRunExpired
	EventResumeAccepted          = observability.EventResumeAccepted
	EventResumeFailed            = observability.EventResumeFailed
	EventFinalResponse           = observability.EventFinalResponse
	EventModelContextBuilt       = observability.EventModelContextBuilt
	EventModelContextBuildFailed = observability.EventModelContextBuildFailed
	EventRuntimeStepStarted      = observability.EventRuntimeStepStarted
	EventRuntimeStepCompleted    = observability.EventRuntimeStepCompleted
	EventRuntimeStepFailed       = observability.EventRuntimeStepFailed
	EventRuntimeStepCancelled    = observability.EventRuntimeStepCancelled
	EventAgentStarted            = observability.EventAgentStarted
	EventAgentTextDelta          = observability.EventAgentTextDelta
	EventFallback                = observability.EventFallbackApplied
)

type StepKind string

const (
	StepKindModelContext         StepKind = "model_context"
	StepKindRuntimeAdapter       StepKind = "runtime_adapter"
	StepKindRuntimeInternalAgent StepKind = "runtime_internal_agent"
	StepKindModelCall            StepKind = "model_call"
	StepKindToolCall             StepKind = "tool_call"
	StepKindSubAgent             StepKind = "sub_agent"
	StepKindWorkflowNode         StepKind = "workflow_node"
	StepKindControlRequest       StepKind = "control_request"
	StepKindProcessorHook        StepKind = "processor_hook"
)

type StepStart struct {
	StepID       string            `json:"step_id,omitempty"`
	Kind         StepKind          `json:"kind"`
	Name         string            `json:"name,omitempty"`
	ParentStepID string            `json:"parent_step_id,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

type AgentDefinition struct {
	AgentID           string                  `json:"agent_id"`
	AgentType         string                  `json:"agent_type"`
	Version           string                  `json:"version"`
	Runtime           RuntimeSpec             `json:"runtime"`
	Workflow          *WorkflowDefinition     `json:"workflow,omitempty"`
	Graph             *GraphDefinition        `json:"graph,omitempty"`
	DataPassing       DataPassingPolicy       `json:"data_passing,omitempty"`
	ModelOptions      ModelCallOptions        `json:"model_options,omitempty"`
	ContextCompaction ContextCompactionPolicy `json:"context_compaction,omitempty"`
	// ContextHistory 控制上下文装配是否读取会话历史（ADR-019）：
	// 空 / "session"=装配历史；"none"=跳过历史装配（本轮只含当前 Run 材料）。
	ContextHistory string `json:"context_history,omitempty"`
	// ToolExecution 控制一次模型响应含 N 个工具调用时的执行调度（ADR-014）：
	// 空 / "sequential"=按序执行；"parallel"=并发执行、结果按原序回填。
	ToolExecution        string                                `json:"tool_execution,omitempty"`
	PromptRef            string                                `json:"prompt_ref,omitempty"`
	ToolRefs             []string                              `json:"tool_refs,omitempty"`
	SubAgentRefs         []string                              `json:"sub_agent_refs,omitempty"`
	RequiredCapabilities []string                              `json:"required_capabilities,omitempty"`
	Timeout              time.Duration                         `json:"timeout,omitempty"`
	Metadata             map[string]string                     `json:"metadata,omitempty"`
	ProcessPresentation  processpresentation.StagePresentation `json:"process_presentation,omitempty"`
	// 下列三个字段是 agent 级扩展绑定（agents.yaml `extensions` 编译产物）：
	// 列表内为扩展 ID，声明顺序即执行顺序；未声明（空列表）时对应 kind
	// 的扩展不执行；子 agent 使用自己的绑定，不继承父 agent。
	BeforeModelHooks     []string `json:"before_model_hooks,omitempty"`
	ToolCallInterceptors []string `json:"tool_call_interceptors,omitempty"`
	OutputValidators     []string `json:"output_validators,omitempty"`
}

type RuntimeSpec struct {
	Type       RuntimeType   `json:"type"`
	Mode       RuntimeMode   `json:"mode,omitempty"`
	Preferred  RuntimeType   `json:"preferred,omitempty"`
	Candidates []RuntimeType `json:"candidates,omitempty"`
}

type DataPassingPolicy struct {
	Mode           DataPassingMode `json:"mode,omitempty"`
	StateSchemaRef string          `json:"state_schema_ref,omitempty"`
	ArtifactKeys   []string        `json:"artifact_keys,omitempty"`
	ScopedDataKeys []string        `json:"scoped_data_keys,omitempty"`
}

type WorkflowDefinition struct {
	WorkflowID string         `json:"workflow_id,omitempty"`
	EntryNode  string         `json:"entry_node"`
	Nodes      []WorkflowNode `json:"nodes"`
	Edges      []WorkflowEdge `json:"edges,omitempty"`
}

type WorkflowNode struct {
	NodeID     string            `json:"node_id"`
	NodeType   string            `json:"node_type"`
	AgentID    string            `json:"agent_id,omitempty"`
	ToolRef    string            `json:"tool_ref,omitempty"`
	InputKeys  []string          `json:"input_keys,omitempty"`
	OutputKeys []string          `json:"output_keys,omitempty"`
	Config     map[string]string `json:"config,omitempty"`
}

type WorkflowEdge struct {
	FromNodeID string `json:"from_node_id"`
	ToNodeID   string `json:"to_node_id"`
	Condition  string `json:"condition,omitempty"`
}

type GraphDefinition struct {
	GraphID        string         `json:"graph_id,omitempty"`
	EntryNode      string         `json:"entry_node"`
	Nodes          []WorkflowNode `json:"nodes"`
	Edges          []WorkflowEdge `json:"edges,omitempty"`
	StateSchemaRef string         `json:"state_schema_ref,omitempty"`
}

type RuntimeCapabilities struct {
	Streaming        bool `json:"streaming"`
	Resume           bool `json:"resume"`
	Checkpoint       bool `json:"checkpoint"`
	ControlRequest   bool `json:"control_request"`
	ToolCall         bool `json:"tool_call"`
	ParallelToolCall bool `json:"parallel_tool_call"`
	SubAgent         bool `json:"sub_agent"`
	Workflow         bool `json:"workflow"`
	DeepAgent        bool `json:"deep_agent"`
	Artifact         bool `json:"artifact"`
	Memory           bool `json:"memory"`
	Cancellation     bool `json:"cancellation"`
	A2A              bool `json:"a2a"`
	MCP              bool `json:"mcp"`
	MaxInputTokens   int  `json:"max_input_tokens,omitempty"`
	MaxOutputTokens  int  `json:"max_output_tokens,omitempty"`
}

type RuntimeHealth struct {
	Available bool      `json:"available"`
	Reason    string    `json:"reason,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

type AgentHandle struct {
	Definition AgentDefinition `json:"definition"`
	Runtime    RuntimeType     `json:"runtime"`
	Binding    RuntimeBinding  `json:"runtime_binding"`
	BuiltAt    time.Time       `json:"built_at"`
}

type Message struct {
	ID             string `json:"id,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	Role           string `json:"role"`
	Content        string `json:"content"`
	// Parts 是多 Part 输入的结构化事实（与 contextpkg.Message.Parts 同形）。
	// 纯文本输入为空；非空时 Content 保存压扁预览。
	Parts []contextpkg.ContentPart `json:"parts,omitempty"`
}

type ScopedData struct {
	Run    map[string]ScopedDataItem            `json:"run,omitempty"`
	Agents map[string]map[string]ScopedDataItem `json:"agents,omitempty"`
}

type ScopedDataItem struct {
	Source     string          `json:"source"`
	Visibility string          `json:"visibility,omitempty"`
	Value      json.RawMessage `json:"value,omitempty"`
	Ref        string          `json:"ref,omitempty"`
	Hash       string          `json:"hash,omitempty"`
}

// AgentDefinition.ToolExecution 的合法取值（与 agentregistry 同名常量对齐；
// runtime 不反向依赖 registry 包，故各自声明）。
const (
	ToolExecutionSequential = "sequential"
	ToolExecutionParallel   = "parallel"
)

type RunRequest struct {
	SessionID          string                     `json:"session_id"`
	RunID              string                     `json:"run_id"`
	ParentRunID        string                     `json:"parent_run_id,omitempty"`
	Definition         AgentDefinition            `json:"definition"`
	Input              []Message                  `json:"input,omitempty"`
	ContextSnapshotRef string                     `json:"context_snapshot_ref,omitempty"`
	ConfigSnapshotRef  string                     `json:"config_snapshot_ref,omitempty"`
	ConfigHash         string                     `json:"config_hash,omitempty"`
	AgentBindingID     string                     `json:"agent_binding_id,omitempty"`
	ScopedData         ScopedData                 `json:"scoped_data,omitempty"`
	UserID             string                     `json:"user_id,omitempty"`
	TenantID           string                     `json:"tenant_id,omitempty"`
	Trace              observability.TraceContext `json:"trace"`
	Metadata           map[string]string          `json:"metadata,omitempty"`
	// ExternalFragments 是入口层 ContextContributor 扩展产出、经 dispatcher
	// 透传的业务上下文片段；ModelContext 装配时与 snapshot/capability
	// fragment 合并后参与预算治理。
	ExternalFragments []contextpkg.ContextFragment `json:"external_fragments,omitempty"`
	// ResultVisibility controls the durable visibility of this Run's final
	// message, final-response event, terminal event, and optional artifact.
	// It is set by trusted Harness composition code, never by model output.
	ResultVisibility observability.EventVisibility `json:"result_visibility,omitempty"`
}

type ResumeRequest struct {
	SessionID          string                        `json:"session_id"`
	RunID              string                        `json:"run_id"`
	ParentRunID        string                        `json:"parent_run_id,omitempty"`
	Definition         AgentDefinition               `json:"definition"`
	ContextSnapshotRef string                        `json:"context_snapshot_ref,omitempty"`
	ConfigSnapshotRef  string                        `json:"config_snapshot_ref,omitempty"`
	ConfigHash         string                        `json:"config_hash,omitempty"`
	AgentBindingID     string                        `json:"agent_binding_id,omitempty"`
	TenantID           string                        `json:"tenant_id,omitempty"`
	UserID             string                        `json:"user_id,omitempty"`
	CheckpointID       string                        `json:"checkpoint_id,omitempty"`
	ControlRequestID   string                        `json:"control_request_id,omitempty"`
	ResumeToken        string                        `json:"resume_token,omitempty"`
	ControlPayload     json.RawMessage               `json:"control_payload,omitempty"`
	ScopedData         ScopedData                    `json:"scoped_data,omitempty"`
	Trace              observability.TraceContext    `json:"trace"`
	ResultVisibility   observability.EventVisibility `json:"result_visibility,omitempty"`
}

type WaitingControlRequest struct {
	TenantID         string                   `json:"tenant_id,omitempty"`
	SessionID        string                   `json:"session_id"`
	RunID            string                   `json:"run_id"`
	CheckpointID     string                   `json:"checkpoint_id"`
	ControlRequestID string                   `json:"control_request_id"`
	ResumeToken      string                   `json:"resume_token"`
	Type             string                   `json:"type,omitempty"`
	PromptPreview    string                   `json:"prompt_preview,omitempty"`
	Event            observability.AgentEvent `json:"-"`
}

type CancelRequest struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
	Reason    string `json:"reason,omitempty"`
}

type FallbackDecision struct {
	RunID       string      `json:"run_id"`
	FromRuntime RuntimeType `json:"from_runtime"`
	ToRuntime   RuntimeType `json:"to_runtime"`
	Reason      string      `json:"reason"`
	CreatedAt   time.Time   `json:"created_at"`
}

type EventAppendResult struct {
	Sequence int64 `json:"sequence"`
}

type RunSnapshot struct {
	RunID                   string         `json:"run_id"`
	SessionID               string         `json:"session_id"`
	AgentID                 string         `json:"agent_id"`
	Status                  RunStatus      `json:"status"`
	RuntimeBinding          RuntimeBinding `json:"runtime_binding"`
	CheckpointID            string         `json:"checkpoint_id,omitempty"`
	PendingControlRequestID string         `json:"pending_control_request_id,omitempty"`
	// ResumeAttemptID 是当前恢复请求的 owner/fencing token。异常卡住时通过
	// 日志告警、外部一次性任务或人工修复，不在 Runtime 内维护续租协程。
	ResumeAttemptID string    `json:"resume_attempt_id,omitempty"`
	StartedAt       time.Time `json:"started_at,omitempty"`
	CompletedAt     time.Time `json:"completed_at,omitempty"`
	FailedAt        time.Time `json:"failed_at,omitempty"`
	ErrorCode       string    `json:"error_code,omitempty"`
	ErrorMessage    string    `json:"error_message,omitempty"`
}

type StepSnapshot struct {
	StepID       string            `json:"step_id"`
	RunID        string            `json:"run_id"`
	Kind         StepKind          `json:"kind"`
	Name         string            `json:"name,omitempty"`
	ParentStepID string            `json:"parent_step_id,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	Status       StepStatus        `json:"status"`
	StartedAt    time.Time         `json:"started_at"`
	EndedAt      time.Time         `json:"ended_at,omitempty"`
}
