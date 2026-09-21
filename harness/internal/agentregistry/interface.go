package agentregistry

import (
	"context"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
)

// AgentRegistry 在 foundation Registry 上补充管理台/评测所需的只读目录能力。
type AgentRegistry interface {
	Registry
	GetAgentDefinition(ctx context.Context, req GetAgentRequest) (*agentruntime.AgentDefinition, error)
	ListCapabilityCards(ctx context.Context, req ListCapabilityCardsRequest) (*ListCapabilityCardsResult, error)
}

type AgentRegistryAdmin interface {
	ValidateAgent(ctx context.Context, manifest AgentConfig) (*ValidationResult, error)
	RegisterAgent(ctx context.Context, manifest AgentConfig) (*RegisterAgentResult, error)
	Bootstrap(ctx context.Context) (*BootstrapResult, error)
	EnableAgent(ctx context.Context, ref AgentRef) error
	DisableAgent(ctx context.Context, ref AgentRef) error
	UpdateGrayPercent(ctx context.Context, ref AgentRef, percent int) error
	RollbackAgent(ctx context.Context, ref AgentRef, targetVersion string) error
	ListAgentVersions(ctx context.Context, agentID string) ([]AgentVersion, error)
	Reload(ctx context.Context) (*ReloadResult, error)
}

type GetAgentRequest struct {
	AgentID         string
	AgentType       string
	Version         string
	TenantID        string
	IncludeDisabled bool
}

type ListCapabilityCardsRequest struct {
	Tenant         string
	Tags           []string
	ExecutionModes []ExecutionMode
	RiskLevel      string
	Channel        string
	Locale         string
	UserBucket     string
}

type ListCapabilityCardsResult struct {
	Cards []CapabilityCard
}

type ValidationResult struct {
	Valid          bool
	Issues         []ValidationIssue
	ResolvedDeps   ResolvedDependencies
	ResolvedPrompt *PromptSnapshot
	RuntimeDryRun  RuntimeBuildResult
	EvalGate       EvalGateResult
}

type ValidationIssue struct {
	Code    ErrorCode
	Message string
	Field   string
}

type RegisterAgentResult struct {
	AgentID        string
	AgentType      string
	Version        string
	ConfigHash     string
	SnapshotRef    string
	CapabilityCard CapabilityCard
}

type ReloadResult struct {
	Loaded     int
	Registered int
	Issues     []ValidationIssue
}

// BootstrapResult 描述共享 Store 的幂等初始化结果；已存在且内容相同的版本不会重复注册。
type BootstrapResult struct {
	Loaded     int
	Registered int
	Unchanged  int
	Issues     []ValidationIssue
}

type DependencyResolver interface {
	Resolve(ctx context.Context, cfg AgentConfig) (ResolvedDependencies, error)
}

type RuntimeValidator interface {
	DryRun(ctx context.Context, cfg AgentConfig) (RuntimeBuildResult, error)
}

type EvalGate interface {
	Evaluate(ctx context.Context, cfg AgentConfig) (EvalGateResult, error)
}

type Loader interface {
	Load(ctx context.Context) ([]AgentConfig, error)
}

type StoreLookup struct {
	AgentID   string
	AgentType string
	Version   string
}

// ConfigSnapshotReader 供恢复或分布式 worker 按不可变引用重建 Runtime 请求。
type ConfigSnapshotReader interface {
	GetConfigSnapshot(ctx context.Context, ref string) (EffectiveConfig, error)
}

// Store 是 Registry 的事实存储端口。Create 接收包含冻结快照的写入聚合；Get/List
// 只返回 Registry 条目，快照统一通过专用读取方法获取。MemoryStore 仅用于本地和测试。
type Store interface {
	Create(ctx context.Context, agent *StoreRecord, audit RegistryEvent) error
	Get(ctx context.Context, lookup StoreLookup) (*StoreRecord, error)
	GetConfigSnapshot(ctx context.Context, ref string) (*ConfigSnapshotRecord, error)
	GetConfigSnapshotByMode(ctx context.Context, ref AgentRef, mode ExecutionMode) (*ConfigSnapshotRecord, error)
	List(ctx context.Context) ([]*StoreRecord, error)
	UpdateStatus(ctx context.Context, ref AgentRef, status AgentStatus, activatedAtMS int64, audit RegistryEvent) error
	UpdateGrayPercent(ctx context.Context, ref AgentRef, percent int, audit RegistryEvent) error
	ListVersions(ctx context.Context, agentID string) ([]AgentVersion, error)
	AppendRegistryEvent(ctx context.Context, event RegistryEvent) error
}

// SnapshotStore 只用于单一配置源的原子全量 Reload。
// 线上共享 SQLStore 不实现该接口，避免一个实例误删其他发布方的历史版本。
type SnapshotStore interface {
	Store
	Replace(ctx context.Context, agents []*StoreRecord) error
}
