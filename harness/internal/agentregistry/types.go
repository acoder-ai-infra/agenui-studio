package agentregistry

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/executionmode"
	"github.com/AGenUI/agenui-studio/harness/internal/gatewaycontract"
	"github.com/AGenUI/agenui-studio/harness/internal/identifiercontract"
	"github.com/AGenUI/agenui-studio/harness/internal/processpresentation"
)

// agent_id 必须满足可执行 Ledger 的 128 字符契约；其余 Registry identity
// 继续与 Registry MySQL DDL 的 255 字符容量一致。
const (
	maxAgentIDRunes          = identifiercontract.MaxAgentIDCharacters
	maxRegistryIdentityRunes = 255
)

var (
	ErrAgentIDRequired       = errors.New("agent_id required")
	ErrAgentTypeRequired     = errors.New("agent_type required")
	ErrAgentVersionRequired  = errors.New("agent version required")
	ErrAgentDisabled         = errors.New("agent disabled")
	ErrAgentNotFound         = errors.New("agent not found")
	ErrDuplicateAgent        = errors.New("duplicate agent")
	ErrPromptRefRequired     = errors.New("prompt_ref required")
	ErrRuntimeRequired       = errors.New("runtime type required")
	ErrGatewayInvalid        = errors.New("agent gateway target config invalid")
	ErrStoreConflict         = errors.New("agent registry store conflict")
	ErrStoreCorrupt          = errors.New("agent registry stored data corrupt")
	ErrConfigDrift           = errors.New("candidate config hash drift")
	ErrConfigSnapshotMissing = errors.New("config snapshot missing")
	ErrAuditWrite            = errors.New("registry audit write failed")
	ErrProductionMemoryStore = errors.New("production agent registry cannot use memory store")
	ErrProductionToolCatalog = errors.New("production agent registry requires tool catalog")
	ErrProductionPromptStore = errors.New("production agent registry requires prompt resolver")
)

type AgentStatus string

const (
	AgentStatusEnabled  AgentStatus = "enabled"
	AgentStatusDisabled AgentStatus = "disabled"

	// 兼容早期管理接口；公共契约统一使用 AgentStatus。
	StatusEnabled  = AgentStatusEnabled
	StatusDisabled = AgentStatusDisabled
)

// ExecutionMode 复用 Binding、Registry 与 Orchestrator 共享的执行范式。
type ExecutionMode = executionmode.Mode

const (
	ExecutionModeDirectAction ExecutionMode = executionmode.DirectAction
	ExecutionModeSingleAgent  ExecutionMode = executionmode.SingleAgent
	ExecutionModeDeepAgent    ExecutionMode = executionmode.DeepAgent
	ExecutionModeWorkflow     ExecutionMode = executionmode.Workflow
	ExecutionModeGraph        ExecutionMode = executionmode.Graph
)

type VersionedRef struct {
	Name    string `json:"name" yaml:"name"`
	Version string `json:"version,omitempty" yaml:"version,omitempty"`
}

type ProtocolPolicy struct {
	Streaming    bool   `json:"streaming" yaml:"streaming"`
	OutputFormat string `json:"output_format,omitempty" yaml:"output_format,omitempty"`
}

type FallbackPolicy struct {
	MaxRetries         int      `json:"max_retries,omitempty" yaml:"max_retries,omitempty"`
	AllowModelFallback bool     `json:"allow_model_fallback,omitempty" yaml:"allow_model_fallback,omitempty"`
	FallbackAgents     []string `json:"fallback_agents,omitempty" yaml:"fallback_agents,omitempty"`
}

// AgentConfig 以 foundation 的最小字段为稳定公共契约，其余字段仅扩展注册、能力与发布治理。
type AgentConfig struct {
	AgentID             string                                `json:"agent_id" yaml:"agent_id"`
	AgentType           string                                `json:"agent_type" yaml:"agent_type"`
	Version             string                                `json:"version" yaml:"version"`
	Status              AgentStatus                           `json:"status" yaml:"status"`
	Runtime             agentruntime.RuntimeSpec              `json:"runtime" yaml:"runtime"`
	PromptRef           string                                `json:"prompt_ref" yaml:"prompt_ref"`
	PromptVersion       string                                `json:"prompt_version,omitempty" yaml:"prompt_version,omitempty"`
	Tools               []VersionedRef                        `json:"tools,omitempty" yaml:"tools,omitempty"`
	SubAgents           []string                              `json:"sub_agents,omitempty" yaml:"sub_agents,omitempty"`
	DataPassing         agentruntime.DataPassingPolicy        `json:"data_passing,omitempty" yaml:"data_passing,omitempty"`
	ContextPolicyRef    string                                `json:"context_policy_ref,omitempty" yaml:"context_policy_ref,omitempty"`
	ContextCompaction   agentruntime.ContextCompactionPolicy  `json:"context_compaction,omitempty" yaml:"context_compaction,omitempty"`
	GuardrailPolicyRef  string                                `json:"guardrail_policy_ref,omitempty" yaml:"guardrail_policy_ref,omitempty"`
	ProtocolPolicy      ProtocolPolicy                        `json:"protocol_policy,omitempty" yaml:"protocol_policy,omitempty"`
	FallbackPolicy      FallbackPolicy                        `json:"fallback_policy,omitempty" yaml:"fallback_policy,omitempty"`
	Metadata            map[string]string                     `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	ProcessPresentation processpresentation.StagePresentation `json:"process_presentation,omitempty" yaml:"process_presentation,omitempty"`

	Name          string               `json:"name,omitempty" yaml:"name,omitempty"`
	Description   string               `json:"description,omitempty" yaml:"description,omitempty"`
	Owner         string               `json:"owner,omitempty" yaml:"owner,omitempty"`
	Tags          []string             `json:"tags,omitempty" yaml:"tags,omitempty"`
	Capability    CapabilityConfig     `json:"capability,omitempty" yaml:"capability,omitempty"`
	RuntimePolicy RuntimeConfig        `json:"runtime_policy,omitempty" yaml:"runtime_policy,omitempty"`
	Orchestration OrchestrationConfig  `json:"orchestration,omitempty" yaml:"orchestration,omitempty"`
	Model         ModelConfig          `json:"model,omitempty" yaml:"model,omitempty"`
	ToolPolicy    ToolsConfig          `json:"tool_policy,omitempty" yaml:"tool_policy,omitempty"`
	Skills        SkillsConfig         `json:"skills,omitempty" yaml:"skills,omitempty"`
	Context       ContextConfig        `json:"context,omitempty" yaml:"context,omitempty"`
	Memory        MemoryConfig         `json:"memory,omitempty" yaml:"memory,omitempty"`
	Guardrails    GuardrailsConfig     `json:"guardrails,omitempty" yaml:"guardrails,omitempty"`
	Output        OutputConfig         `json:"output,omitempty" yaml:"output,omitempty"`
	Observability ObservabilityConfig  `json:"observability,omitempty" yaml:"observability,omitempty"`
	Release       ReleaseConfig        `json:"release,omitempty" yaml:"release,omitempty"`
	Gateway       *GatewayTargetConfig `json:"gateway,omitempty" yaml:"gateway,omitempty"`
	// Extensions 声明本 agent 绑定的 agent 级扩展 ID（声明顺序即执行顺序）。
	// 未声明的 kind 一律不执行；子 agent 使用自己的配置，不继承。
	Extensions ExtensionsConfig `json:"extensions,omitempty" yaml:"extensions,omitempty"`
}

type GatewayTargetConfig struct {
	ProviderKind gatewaycontract.SubAgentProviderKind `json:"provider_kind" yaml:"provider_kind"`
	Plugins      []GatewayPluginConfig                `json:"plugins,omitempty" yaml:"plugins,omitempty"`
	Remote       *GatewayRemoteConfig                 `json:"remote,omitempty" yaml:"remote,omitempty"`
}

// GatewayPluginConfig keeps the public Registry config surface aligned with
// the minimal shared Gateway contract without introducing a second DTO.
type GatewayPluginConfig = gatewaycontract.GatewayPluginConfig

type GatewayRemoteConfig struct {
	BaseURL                string   `json:"base_url" yaml:"base_url"`
	CardPath               string   `json:"card_path,omitempty" yaml:"card_path,omitempty"`
	Transport              string   `json:"transport" yaml:"transport"`
	AuthRef                string   `json:"auth_ref,omitempty" yaml:"auth_ref,omitempty"`
	AllowedEndpointOrigins []string `json:"allowed_endpoint_origins,omitempty" yaml:"allowed_endpoint_origins,omitempty"`
	TimeoutMS              int64    `json:"timeout_ms" yaml:"timeout_ms"`
}

type CapabilityConfig struct {
	Intents        []string        `json:"intents,omitempty" yaml:"intents,omitempty"`
	InputSchema    string          `json:"input_schema,omitempty" yaml:"input_schema,omitempty"`
	OutputSchema   string          `json:"output_schema,omitempty" yaml:"output_schema,omitempty"`
	ExecutionModes []ExecutionMode `json:"execution_modes,omitempty" yaml:"execution_modes,omitempty"`
	Protocols      []string        `json:"protocols,omitempty" yaml:"protocols,omitempty"`
}

type RuntimeConfig struct {
	Provider   string `json:"provider,omitempty" yaml:"provider,omitempty"`
	RuntimeRef string `json:"runtime_ref,omitempty" yaml:"runtime_ref,omitempty"`
	TimeoutMS  int64  `json:"timeout_ms,omitempty" yaml:"timeout_ms,omitempty"`
	MaxTurns   int    `json:"max_turns,omitempty" yaml:"max_turns,omitempty"`
	Checkpoint bool   `json:"checkpoint,omitempty" yaml:"checkpoint,omitempty"`
	Stream     bool   `json:"stream,omitempty" yaml:"stream,omitempty"`
}

type OrchestrationConfig struct {
	DefaultMode  ExecutionMode                    `json:"default_mode,omitempty" yaml:"default_mode,omitempty"`
	WorkflowRef  string                           `json:"workflow_ref,omitempty" yaml:"workflow_ref,omitempty"`
	GraphRef     string                           `json:"graph_ref,omitempty" yaml:"graph_ref,omitempty"`
	DeepAgentRef string                           `json:"deep_agent_ref,omitempty" yaml:"deep_agent_ref,omitempty"`
	Workflow     *agentruntime.WorkflowDefinition `json:"workflow,omitempty" yaml:"workflow,omitempty"`
	Graph        *agentruntime.GraphDefinition    `json:"graph,omitempty" yaml:"graph,omitempty"`
	SubAgents    []string                         `json:"sub_agents,omitempty" yaml:"sub_agents,omitempty"`
	A2ATargets   []string                         `json:"a2a_targets,omitempty" yaml:"a2a_targets,omitempty"`
	Fallback     []string                         `json:"fallback,omitempty" yaml:"fallback,omitempty"`
}

type ModelConfig struct {
	RouterPolicy    string                          `json:"router_policy,omitempty" yaml:"router_policy,omitempty"`
	Primary         string                          `json:"primary,omitempty" yaml:"primary,omitempty"`
	Fallback        []string                        `json:"fallback,omitempty" yaml:"fallback,omitempty"`
	Temperature     *float64                        `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	TopP            *float64                        `json:"top_p,omitempty" yaml:"top_p,omitempty"`
	MaxTokens       *int                            `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	Stop            []string                        `json:"stop,omitempty" yaml:"stop,omitempty"`
	ToolChoice      string                          `json:"tool_choice,omitempty" yaml:"tool_choice,omitempty"`
	Retry           int                             `json:"retry,omitempty" yaml:"retry,omitempty"`
	ReasoningMode   agentruntime.ModelReasoningMode `json:"reasoning_mode,omitempty" yaml:"reasoning_mode,omitempty"`
	ReasoningBudget int                             `json:"reasoning_budget_tokens,omitempty" yaml:"reasoning_budget_tokens,omitempty"`
	ReasoningEffort string                          `json:"reasoning_effort,omitempty" yaml:"reasoning_effort,omitempty"`
	// ResponseFormat 声明 provider 侧的约束解码输出格式（G-A）：空=自由
	// 文本；json_object=强制合法 JSON。静态 per-agent 配置，适用于分类/
	// 视觉等机器消费 JSON 的辅助 Agent；json_schema 留待真实消费者。
	ResponseFormat string `json:"response_format,omitempty" yaml:"response_format,omitempty"`
	// ImageDetail 声明本 Agent 请求中全部图片 part 的 provider 视觉编码
	// 预算（G-B）：空=provider 缺省（auto）；low=低分辨率快速便宜；
	// high=高清 tile；auto=显式交给 provider 决定。高频视觉帧识别 Agent
	// 应配 low。
	ImageDetail string `json:"image_detail,omitempty" yaml:"image_detail,omitempty"`
}

type ToolsConfig struct {
	Allowlist  []string `json:"allowlist,omitempty" yaml:"allowlist,omitempty"`
	RiskLevel  string   `json:"risk_level,omitempty" yaml:"risk_level,omitempty"`
	MCPServers []string `json:"mcp_servers,omitempty" yaml:"mcp_servers,omitempty"`
	// HTTPTools references tenant-authored HTTP tool definitions (internal/httptooldef)
	// by bare name. Like MCPServers, these are validated loosely at publish and
	// resolved per-tenant at runtime — they are not global catalog name@version refs.
	HTTPTools         []string `json:"http_tools,omitempty" yaml:"http_tools,omitempty"`
	HITLRequiredTools []string `json:"hitl_required_tools,omitempty" yaml:"hitl_required_tools,omitempty"`
	// Execution 控制一次模型响应含 N 个工具调用时的执行调度（ADR-014）：
	// 空 / "sequential"（默认）=严格按模型返回顺序依次执行；"parallel"=
	// 并发执行同批调用，结果按原始顺序回填。缺省 sequential，避免未显式
	// 配置的 Agent 因 SDK 升级出现新并发副作用。
	Execution string `json:"execution,omitempty" yaml:"execution,omitempty"`
}

// 合法的 tool_policy.execution 取值。
const (
	ToolExecutionSequential = "sequential"
	ToolExecutionParallel   = "parallel"
)

// toolExecutionOrDefault 把未显式配置的 tool_policy.execution 归一为
// sequential（ADR-014 缺省）：未声明的 Agent 不得因 SDK 升级而获得新的
// 并发工具副作用，并行必须由配置显式选入。
func toolExecutionOrDefault(value string) string {
	if value == ToolExecutionParallel {
		return ToolExecutionParallel
	}
	return ToolExecutionSequential
}

type SkillsConfig struct {
	Allowlist []string `json:"allowlist,omitempty" yaml:"allowlist,omitempty"`
	// VersionRange 仅保留旧配置的可诊断性；foundation 只接受精确版本，注册时会拒绝该字段。
	VersionRange string `json:"version_range,omitempty" yaml:"version_range,omitempty"`
	Isolation    string `json:"isolation,omitempty" yaml:"isolation,omitempty"`
}

// ExtensionsConfig 声明 agent 绑定的三类扩展 ID 列表。列表有序：
// transformer 按声明顺序串联，interceptor 先声明者在最外层，validator
// 按声明顺序执行且首次非-Accept 短路。ID 先查 Engine Options 注册
// （WithXxxProvider），未命中再查全集（extension.RegisterXxx），两处都没有
// 时运行期 fail-closed。
type ExtensionsConfig struct {
	BeforeModelHooks     []string `json:"before_model_hooks,omitempty" yaml:"before_model_hooks,omitempty"`
	ToolCallInterceptors []string `json:"tool_call_interceptors,omitempty" yaml:"tool_call_interceptors,omitempty"`
	OutputValidators     []string `json:"output_validators,omitempty" yaml:"output_validators,omitempty"`
}

type ContextConfig struct {
	SystemPrompt  string   `json:"system_prompt,omitempty" yaml:"system_prompt,omitempty"`
	PromptPack    string   `json:"prompt_pack,omitempty" yaml:"prompt_pack,omitempty"`
	RAGNamespaces []string `json:"rag_namespaces,omitempty" yaml:"rag_namespaces,omitempty"`
	Compression   string   `json:"compression,omitempty" yaml:"compression,omitempty"`
	Cache         bool     `json:"cache,omitempty" yaml:"cache,omitempty"`
	// History 控制上下文装配时是否读取并注入会话历史（ADR-019）：
	// 空 / "session"（默认）=从 Session store 装配历史；"none"=跳过历史
	// 装配，本轮模型输入只含当前 Run 材料（历史由宿主经 before_model_hook
	// 自供）。事实落账不受影响（只写不读）。
	History         string `json:"history,omitempty" yaml:"history,omitempty"`
	ArtifactOffload bool   `json:"artifact_offload,omitempty" yaml:"artifact_offload,omitempty"`
}

// 合法的 context.history 取值。
const (
	ContextHistorySession = "session"
	ContextHistoryNone    = "none"
)

type MemoryConfig struct {
	ReadNamespaces  []string `json:"read_namespaces,omitempty" yaml:"read_namespaces,omitempty"`
	WriteNamespaces []string `json:"write_namespaces,omitempty" yaml:"write_namespaces,omitempty"`
	AsyncWrite      bool     `json:"async_write,omitempty" yaml:"async_write,omitempty"`
}

type GuardrailsConfig struct {
	Input    string `json:"input,omitempty" yaml:"input,omitempty"`
	Tool     string `json:"tool,omitempty" yaml:"tool,omitempty"`
	Output   string `json:"output,omitempty" yaml:"output,omitempty"`
	Behavior string `json:"behavior,omitempty" yaml:"behavior,omitempty"`
}

type OutputConfig struct {
	Formats       []string `json:"formats,omitempty" yaml:"formats,omitempty"`
	ArtifactTypes []string `json:"artifact_types,omitempty" yaml:"artifact_types,omitempty"`
}

type ObservabilityConfig struct {
	Trace        bool     `json:"trace,omitempty" yaml:"trace,omitempty"`
	DebugLevel   string   `json:"debug_level,omitempty" yaml:"debug_level,omitempty"`
	SamplingRate float64  `json:"sampling_rate,omitempty" yaml:"sampling_rate,omitempty"`
	Redaction    []string `json:"redaction,omitempty" yaml:"redaction,omitempty"`
}

type ReleaseConfig struct {
	GrayPercent      int    `json:"gray_percent,omitempty" yaml:"gray_percent,omitempty"`
	RollbackVersion  string `json:"rollback_version,omitempty" yaml:"rollback_version,omitempty"`
	EvalGate         string `json:"eval_gate,omitempty" yaml:"eval_gate,omitempty"`
	ApprovalRequired bool   `json:"approval_required,omitempty" yaml:"approval_required,omitempty"`
}

type ResolveRequest struct {
	AgentID       string            `json:"agent_id"`
	Version       string            `json:"version,omitempty"`
	SessionID     string            `json:"session_id,omitempty"`
	RunID         string            `json:"run_id,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	AgentType     string            `json:"agent_type,omitempty"`
	ExecutionMode ExecutionMode     `json:"execution_mode,omitempty"`
	Tenant        string            `json:"tenant,omitempty"`
	Env           string            `json:"env,omitempty"`
	Channel       string            `json:"channel,omitempty"`
	UserBucket    string            `json:"user_bucket,omitempty"`
	RequestID     string            `json:"request_id,omitempty"`
	SelectionHash string            `json:"selection_hash,omitempty"`
	// BindingHash 仅为早期调用方保留；新链路应传 SelectionHash，
	// 避免把 Registry 解析前的选择摘要误解为最终 Binding hash。
	BindingHash         string `json:"binding_hash,omitempty"`
	CandidateConfigHash string `json:"candidate_config_hash,omitempty"`
	IncludeDisabled     bool   `json:"include_disabled,omitempty"`
}

type EffectiveConfig struct {
	Definition        agentruntime.AgentDefinition `json:"definition"`
	ConfigSnapshotRef string                       `json:"config_snapshot_ref"`
	ConfigHash        string                       `json:"config_hash"`
	PromptHash        string                       `json:"prompt_hash,omitempty"`
	SchemaHash        string                       `json:"schema_hash,omitempty"`
	PolicyHash        string                       `json:"policy_hash,omitempty"`
	ResolvedDeps      ResolvedDependencies         `json:"resolved_deps,omitempty"`
	EffectiveAt       time.Time                    `json:"effective_at,omitempty"`
	SourceVersionIDs  []string                     `json:"source_version_ids,omitempty"`
	Gateway           *EffectiveGatewayTarget      `json:"gateway,omitempty"`
}

// EffectiveGatewayTarget is the frozen execution-time Gateway target config.
type EffectiveGatewayTarget struct {
	ProviderKind gatewaycontract.SubAgentProviderKind `json:"provider_kind"`
	Plugins      []GatewayPluginConfig                `json:"plugins,omitempty"`
	Remote       *GatewayRemoteConfig                 `json:"remote,omitempty"`
}

type CapabilityCard struct {
	AgentID        string          `json:"agent_id"`
	Version        string          `json:"version"`
	AgentType      string          `json:"agent_type,omitempty"`
	Status         AgentStatus     `json:"status"`
	ConfigHash     string          `json:"config_hash"`
	CardHash       string          `json:"card_hash,omitempty"`
	OutputFormat   string          `json:"output_format,omitempty"`
	Owner          string          `json:"owner,omitempty"`
	Description    string          `json:"description,omitempty"`
	Intents        []string        `json:"intents,omitempty"`
	ExecutionModes []ExecutionMode `json:"execution_modes,omitempty"`
	InputSchema    string          `json:"input_schema,omitempty"`
	OutputSchema   string          `json:"output_schema,omitempty"`
	RiskLevel      string          `json:"risk_level,omitempty"`
	Protocols      []string        `json:"protocols,omitempty"`
	SLA            SLA             `json:"sla,omitempty"`
	ScoreHints     ScoreHints      `json:"score_hints,omitempty"`
	Tags           []string        `json:"tags,omitempty"`
}

type SLA struct {
	P95FirstEventMS int64 `json:"p95_first_event_ms,omitempty"`
	P95TotalMS      int64 `json:"p95_total_ms,omitempty"`
}

type ScoreHints struct {
	HealthScore       float64 `json:"health_score,omitempty"`
	HistoricalSuccess float64 `json:"historical_success,omitempty"`
	CostLatencyScore  float64 `json:"cost_latency_score,omitempty"`
}

type Registry interface {
	ResolveEffectiveConfig(ctx context.Context, req ResolveRequest) (EffectiveConfig, error)
	GetCapabilityCard(ctx context.Context, agentID, version string) (CapabilityCard, error)
}

// PreparedAgent is the immutable runtime projection produced by the Registry's
// release gates. It is persisted with a tenant configuration version so a new
// Run can resolve it without recompiling mutable catalog state.
type PreparedAgent struct {
	Card             CapabilityCard         `json:"card"`
	Effective        EffectiveConfig        `json:"effective"`
	ConfigSnapshots  []ConfigSnapshotRecord `json:"config_snapshots"`
	SubAgentVersions map[string]string      `json:"sub_agent_versions,omitempty"`
}

type ResolvedDependencies struct {
	Models                 []string `json:"models,omitempty"`
	Tools                  []string `json:"tools,omitempty"`
	AuthoringMaxToolRisk   string   `json:"authoring_max_tool_risk,omitempty"`
	AuthoringHighRiskTools []string `json:"authoring_high_risk_tools,omitempty"`
	MCPServers             []string `json:"mcp_servers,omitempty"`
	HTTPTools              []string `json:"http_tools,omitempty"`
	Prompts                []string `json:"prompts,omitempty"`
	Schemas                []string `json:"schemas,omitempty"`
	Skills                 []string `json:"skills,omitempty"`
	Guardrails             []string `json:"guardrails,omitempty"`
	Agents                 []string `json:"agents,omitempty"`
	Protocols              []string `json:"protocols,omitempty"`
}

type AgentManifest = AgentConfig
type AgentCapabilityCard = CapabilityCard
type ResolveEffectiveConfigRequest = ResolveRequest

type AgentRef struct {
	AgentID string
	Version string
}

type AgentVersion struct {
	AgentID      string
	AgentType    string
	Version      string
	Status       AgentStatus
	ConfigHash   string
	RegisteredAt time.Time
	GrayPercent  int
	Revision     int64
}

type RuntimeBuildResult struct {
	Passed bool   `json:"passed"`
	Mock   bool   `json:"mock,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type EvalGateResult struct {
	Passed bool   `json:"passed"`
	Mock   bool   `json:"mock,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type InMemoryRegistry struct{ service *Service }

func ValidateAgentConfig(cfg AgentConfig) error {
	cfg = normalizeConfig(cfg)
	if cfg.AgentID == "" {
		return ErrAgentIDRequired
	}
	if cfg.AgentType == "" {
		return ErrAgentTypeRequired
	}
	if cfg.Version == "" {
		return ErrAgentVersionRequired
	}
	for _, identity := range []struct {
		field string
		value string
		max   int
	}{
		{field: "agent_id", value: cfg.AgentID, max: maxAgentIDRunes},
		{field: "agent_type", value: cfg.AgentType, max: maxRegistryIdentityRunes},
		{field: "version", value: cfg.Version, max: maxRegistryIdentityRunes},
	} {
		if !utf8.ValidString(identity.value) {
			return newError(CodeInvalidConfig, identity.field, identity.field+" must be valid UTF-8")
		}
		if utf8.RuneCountInString(identity.value) > identity.max {
			return newError(CodeInvalidConfig, identity.field, identity.field+" exceeds "+strconv.Itoa(identity.max)+" characters")
		}
		if strings.IndexFunc(identity.value, unicode.IsControl) >= 0 {
			return newError(CodeInvalidConfig, identity.field, identity.field+" must not contain control characters")
		}
	}
	if cfg.Runtime.Type == "" {
		return ErrRuntimeRequired
	}
	if cfg.PromptRef == "" {
		return ErrPromptRefRequired
	}
	if !cfg.Model.ReasoningMode.Valid() {
		return newError(CodeInvalidConfig, "model.reasoning_mode", "must be auto, enabled or disabled")
	}
	if cfg.Model.ReasoningBudget < 0 || (cfg.Model.ReasoningMode == agentruntime.ModelReasoningDisabled && cfg.Model.ReasoningBudget > 0) {
		return newError(CodeInvalidConfig, "model.reasoning_budget_tokens", "must be non-negative and cannot be set when reasoning is disabled")
	}
	if !validReasoningEffort(cfg.Model.ReasoningEffort) || (cfg.Model.ReasoningEffort != "" && cfg.Model.ReasoningMode != agentruntime.ModelReasoningEnabled) {
		return newError(CodeInvalidConfig, "model.reasoning_effort", "must be low, medium, high, xhigh or max and requires reasoning enabled")
	}
	if cfg.Model.Temperature != nil && *cfg.Model.Temperature < 0 {
		return newError(CodeInvalidConfig, "model.temperature", "must be non-negative")
	}
	if cfg.Model.TopP != nil && (*cfg.Model.TopP < 0 || *cfg.Model.TopP > 1) {
		return newError(CodeInvalidConfig, "model.top_p", "must be between 0 and 1")
	}
	if cfg.Model.MaxTokens != nil && *cfg.Model.MaxTokens <= 0 {
		return newError(CodeInvalidConfig, "model.max_tokens", "must be positive when configured")
	}
	switch cfg.Model.ToolChoice {
	case "", "auto", "none", "required":
	default:
		return newError(CodeInvalidConfig, "model.tool_choice", "must be auto, none or required")
	}
	// G-A：response_format 白名单取值，非法值在发布期 fail-closed；
	// json_schema 留待真实消费者，未开放前同样拒绝。
	switch cfg.Model.ResponseFormat {
	case "", "json_object":
	default:
		return newError(CodeInvalidConfig, "model.response_format", "must be empty or json_object")
	}
	// G-B：image_detail 白名单取值，控制本 Agent 请求内全部图片 part 的
	// provider 视觉编码预算。
	switch cfg.Model.ImageDetail {
	case "", "low", "high", "auto":
	default:
		return newError(CodeInvalidConfig, "model.image_detail", "must be empty, low, high or auto")
	}
	for _, stop := range cfg.Model.Stop {
		if strings.TrimSpace(stop) == "" {
			return newError(CodeInvalidConfig, "model.stop", "must not contain empty values")
		}
	}
	if cfg.Context.Compression != "" {
		return newError(CodeInvalidConfig, "context.compression", "legacy context compression alias is unsupported; use context_compaction")
	}
	if _, err := agentruntime.NormalizeContextCompactionPolicy(cfg.ContextCompaction); err != nil {
		return err
	}
	if err := validateGatewayTargetConfig(cfg.Gateway); err != nil {
		return err
	}
	if err := validateExecutionConfig(cfg); err != nil {
		return err
	}
	if err := validateExtensionsConfig(cfg); err != nil {
		return err
	}
	if err := validateContextConfig(cfg); err != nil {
		return err
	}
	if err := validateFoundationCapabilityRefs(cfg); err != nil {
		return err
	}
	for _, key := range []string{"policy", "deep_agent_instruction"} {
		if _, exists := cfg.Metadata[key]; exists {
			return newError(
				CodePolicyViolation,
				"metadata."+key,
				"system instructions must come from PromptStore or a governed capability snapshot",
			)
		}
	}
	return nil
}

func validateGatewayTargetConfig(cfg *GatewayTargetConfig) error {
	if cfg == nil {
		return nil
	}
	if !cfg.ProviderKind.IsGatewayRouted() {
		return newError(CodeInvalidConfig, "gateway", "provider_kind is required")
	}
	seenPlugins := make(map[string]struct{}, len(cfg.Plugins))
	for index, plugin := range cfg.Plugins {
		field := "gateway.plugins[" + strconv.Itoa(index) + "]"
		if strings.TrimSpace(plugin.PluginID) == "" {
			return newError(CodeInvalidConfig, field+".plugin_id", "must not be empty")
		}
		if _, exists := seenPlugins[plugin.PluginID]; exists {
			return newError(CodeInvalidConfig, field+".plugin_id", "duplicate plugin id")
		}
		seenPlugins[plugin.PluginID] = struct{}{}
		if _, err := gatewaycontract.CanonicalizePluginConfig(plugin.Config); err != nil {
			return newError(CodeInvalidConfig, field+".config", "must be a JSON object")
		}
	}
	if cfg.ProviderKind == gatewaycontract.SubAgentProviderLocalAgent {
		if cfg.Remote != nil {
			return newError(CodeInvalidConfig, "gateway.remote", "local_agent cannot declare a remote binding")
		}
		return nil
	}
	if cfg.Remote == nil || cfg.Remote.Transport != "JSONRPC" || cfg.Remote.TimeoutMS <= 0 {
		return newError(CodeInvalidConfig, "gateway.remote", "remote_a2a requires JSONRPC binding and a positive timeout")
	}
	base, err := url.Parse(cfg.Remote.BaseURL)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return newError(CodeInvalidConfig, "gateway.remote.base_url", "valid http(s) base_url required")
	}
	return nil
}

func (cfg AgentConfig) ToAgentDefinition() agentruntime.AgentDefinition {
	cfg = normalizeConfig(cfg)
	compaction, _ := agentruntime.NormalizeContextCompactionPolicy(cfg.ContextCompaction)
	mode := executionMode(cfg)
	toolRefs := make([]string, 0, len(cfg.Tools))
	for _, tool := range cfg.Tools {
		ref := tool.Name
		if tool.Version != "" {
			ref += "@" + tool.Version
		}
		toolRefs = append(toolRefs, ref)
	}
	metadata := cloneStringMap(cfg.Metadata)
	setMetadata(metadata, "guardrail_policy_ref", cfg.GuardrailPolicyRef)
	setMetadata(metadata, "context_policy_ref", cfg.ContextPolicyRef)
	setMetadata(metadata, "output_format", cfg.ProtocolPolicy.OutputFormat)
	setMetadata(metadata, "prompt_version", cfg.PromptVersion)
	setMetadata(metadata, "runtime_ref", cfg.RuntimePolicy.RuntimeRef)
	setMetadata(metadata, "description", cfg.Description)
	setMetadata(metadata, "max_iterations", positiveInt(cfg.RuntimePolicy.MaxTurns))
	setMetadata(metadata, "tool_risk_level", cfg.ToolPolicy.RiskLevel)
	setMetadataList(metadata, "skill_refs", cfg.Skills.Allowlist)
	setMetadataList(metadata, "mcp_servers", cfg.ToolPolicy.MCPServers)
	setMetadataList(metadata, "http_tools", cfg.ToolPolicy.HTTPTools)
	setMetadataList(metadata, "hitl_required_tools", cfg.ToolPolicy.HITLRequiredTools)
	setMetadata(metadata, "workflow_ref", cfg.Orchestration.WorkflowRef)
	setMetadata(metadata, "graph_ref", cfg.Orchestration.GraphRef)
	setMetadata(metadata, "deep_agent_ref", cfg.Orchestration.DeepAgentRef)
	// 自由 metadata 不能成为 system 指令；生产指令只来自受治理的 Prompt/能力快照。
	delete(metadata, "policy")
	delete(metadata, "deep_agent_instruction")
	definition := agentruntime.AgentDefinition{
		AgentID:              cfg.AgentID,
		AgentType:            cfg.AgentType,
		Version:              cfg.Version,
		Runtime:              cfg.Runtime,
		ModelOptions:         modelCallOptions(cfg.Model),
		ContextCompaction:    compaction,
		ContextHistory:       cfg.Context.History,
		ToolExecution:        toolExecutionOrDefault(cfg.ToolPolicy.Execution),
		PromptRef:            cfg.PromptRef,
		ToolRefs:             toolRefs,
		SubAgentRefs:         append([]string(nil), cfg.SubAgents...),
		DataPassing:          cloneDataPassingPolicy(cfg.DataPassing),
		RequiredCapabilities: requiredCapabilities(cfg, mode),
		Timeout:              time.Duration(cfg.RuntimePolicy.TimeoutMS) * time.Millisecond,
		Metadata:             metadata,
		ProcessPresentation:  cfg.ProcessPresentation,
		// agent 级扩展绑定透传为一等字段（声明顺序即执行顺序，不排序）。
		BeforeModelHooks:     append([]string(nil), cfg.Extensions.BeforeModelHooks...),
		ToolCallInterceptors: append([]string(nil), cfg.Extensions.ToolCallInterceptors...),
		OutputValidators:     append([]string(nil), cfg.Extensions.OutputValidators...),
	}
	if mode == executionmode.Workflow {
		definition.Workflow = cloneWorkflowDefinition(cfg.Orchestration.Workflow)
	}
	if mode == executionmode.Graph {
		definition.Graph = cloneGraphDefinition(cfg.Orchestration.Graph)
	}
	return definition
}

func modelCallOptions(cfg ModelConfig) agentruntime.ModelCallOptions {
	options := agentruntime.ModelCallOptions{
		Fallback: append([]string(nil), cfg.Fallback...),
		Stop:     append([]string(nil), cfg.Stop...), ToolChoice: cfg.ToolChoice,
		ReasoningMode: cfg.ReasoningMode, ReasoningBudget: cfg.ReasoningBudget, ReasoningEffort: cfg.ReasoningEffort,
		// G-A / G-B：结构化输出格式与图片视觉预算随 Agent 配置冻结透传。
		ResponseFormat: cfg.ResponseFormat, ImageDetail: cfg.ImageDetail,
	}
	// Carry the agent's declared model so the gateway routes to (and fail-closes
	// on) exactly that model, instead of silently using the tenant default.
	if cfg.Primary != "" {
		primary := cfg.Primary
		options.Model = &primary
	}
	if cfg.Temperature != nil {
		value := float32(*cfg.Temperature)
		options.Temperature = &value
	}
	if cfg.TopP != nil {
		value := float32(*cfg.TopP)
		options.TopP = &value
	}
	if cfg.MaxTokens != nil {
		value := *cfg.MaxTokens
		options.MaxTokens = &value
	}
	return options
}

func validReasoningEffort(value string) bool {
	switch value {
	case "", "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}

func cloneDataPassingPolicy(policy agentruntime.DataPassingPolicy) agentruntime.DataPassingPolicy {
	policy.ArtifactKeys = append([]string(nil), policy.ArtifactKeys...)
	policy.ScopedDataKeys = append([]string(nil), policy.ScopedDataKeys...)
	return policy
}

func cloneStringMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values)+8)
	for key, value := range values {
		out[key] = value
	}
	return out
}

func setMetadata(values map[string]string, key, value string) {
	if value != "" {
		values[key] = value
	}
}

func setMetadataList(values map[string]string, key string, items []string) {
	if len(items) == 0 {
		return
	}
	// []string 不包含 json.Marshal 的失败类型；统一编码为 Runtime 的 canonical 数组契约。
	payload, _ := json.Marshal(items)
	values[key] = string(payload)
}

func positiveInt(value int) string {
	if value <= 0 {
		return ""
	}
	return strconv.Itoa(value)
}
