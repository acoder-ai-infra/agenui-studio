package harness

import "time"

// RunStatus 映射 canonical RunStatus 枚举（详见
// the public protocol contract）。
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

// IsTerminal 判断 s 是否属于 canonical 终态之一。
func (s RunStatus) IsTerminal() bool {
	switch s {
	case RunStatusCompleted, RunStatusFailed, RunStatusCancelled, RunStatusExpired:
		return true
	default:
		return false
	}
}

// RunView 是 Run 持久化状态的只读投影。GetRun 返回它，Execution.Handle().Run()
// 也上报它。
type RunView struct {
	// Identity 是 Run 运行所依赖的冻结身份。
	Identity Identity

	// Status 是当前 RunStatus。
	Status RunStatus

	// ConfigSnapshotRef 指向本 Run 绑定的冻结 Agent 配置快照。调试控制台
	// 与审计侧从中读取。
	ConfigSnapshotRef string
	// ConfigHash 是配置快照的内容 hash。
	ConfigHash string
	// AgentBindingID 是跨多次 resume 尝试保持一致的持久化 BindingID。
	AgentBindingID string

	// PendingControlRequestID 仅在 Status == waiting_control 时置位。
	PendingControlRequestID string
	// CheckpointID 是 Run 挂起时绑定的 checkpoint。
	CheckpointID string

	// 时间戳。
	StartedAt   time.Time
	CompletedAt time.Time
	FailedAt    time.Time

	// ErrorCode / ErrorMessage 在终态错误状态下填充。
	ErrorCode    string
	ErrorMessage string
}

// ResultView 是 Run 抵达终态后浮现的 assistant 最终答案。大答案通过 ContentRef
// （Artifact）承载，而非 Content。
type ResultView struct {
	// RunID / SessionID 标识本结果归属的 Run。
	RunID     string
	SessionID string
	// MessageID 是 assistant 最终答复的持久化 message id。
	MessageID string
	// Content 是最终答案的短内联预览；大答案下为空。
	Content string
	// ContentRef 是大答案对应的 Artifact ref。
	ContentRef string
	// Hash 是内容 hash（ContentRef 存在时与 ArtifactMeta.Hash 一致）。
	Hash string
	// Size 是答案字节数（已知时填写）。
	Size int64
	// CreatedAt 是 final response 事件发出的时刻。
	CreatedAt time.Time
	// Usage 承载本 Run 聚合后的 token / cost / duration 记账信息。
	Usage RunUsage
}

// RunUsage 映射持久化的 Run 级用量记录。字段与 hosted server 的
// /runs/{id}/usage handler 对齐，确保两个入口一致。
type RunUsage struct {
	InputTokens  int64
	OutputTokens int64
	// ReasoningTokens 在 provider 报告时单独计数。
	ReasoningTokens int64
	// TotalTokens 是 provider 报告的总和；对于会报告全部字段的 provider，
	// 等于 InputTokens + OutputTokens + ReasoningTokens。
	TotalTokens int64
	// EstimatedCostUSD 是以美元估算的费用；未配置价格表时为零。
	EstimatedCostUSD float64
	// DurationMS 是 run_started 到 terminal 之间的墙钟耗时（毫秒）。
	DurationMS int64
}

// BuildReport 承载 Build 调用产出的冻结配置与能力。hosted 侧健康检查和
// doctor 的 `check` 子命令都依赖它；SDK 在 Build 之后不会重建它。
type BuildReport struct {
	// SDKVersion 是 harness.Version 常量。
	SDKVersion string
	// GitRevision 是 harness module 的 git revision（在构建时用
	// -ldflags "-X ...GitRevision=<sha>" 注入时填充）。
	GitRevision string
	// ConfigFingerprint 是塑造 kernel 的每一份输入（harness.yaml、组件
	// 配置、已注册扩展 id、资源归属声明）的确定性 hash。
	ConfigFingerprint string
	// SchemaVersions 列出本次构建支持的 canonical schema 版本。
	SchemaVersions []string
	// Environment 映射 internal/kernel.Environment。
	Environment string
	// Runtimes 列出已安装的 runtime 及其能力。
	Runtimes []RuntimeInfo
	// Providers 是已注册的模型 provider 名称扁平列表。
	Providers []string
	// Tenants 映射 tenant_id -> provider 名称。
	Tenants map[string][]string
	// Extensions 声明每一个已注册的扩展及其 order + 策略。
	Extensions []ExtensionInfo
	// Agents 是已注册 Agent 的调试友好列表。
	Agents []AgentInfo
	// Degraded 描述部分可用的子系统（例如在 hosted 部署中 Redis 不可达
	// 时仅剩 broker fan-out）。
	Degraded []string
	// Unsupported 描述调用方要求但当前构建无法提供的能力（例如在没有
	// worker 的情况下要求排程执行）。
	Unsupported []string
	// BuiltAt 是 Build 返回时的墙钟时间。
	BuiltAt time.Time
}

// RuntimeInfo 描述 kernel 下已安装的一个 runtime。
type RuntimeInfo struct {
	Type       string // "eino" | "google_adk" | "native" | "mock"
	Available  bool
	Streaming  bool
	Resume     bool
	Checkpoint bool
	ToolCall   bool
	SubAgent   bool
	Deep       bool
}

// ExtensionInfo 描述一个已注册的扩展实现。
type ExtensionInfo struct {
	ID            string
	Kind          string // harness/extension 中的一个 ExtensionKind 值
	Order         int
	FailurePolicy string // "fail_closed" | "fail_open"
	TimeoutMS     int
	Fingerprint   string
	// HandlerNames 只在 KindToolProvider 条目上填充，浮现 provider 提供的
	// function 工具实现名（rc.6：工具定义与治理属性在 tools.yaml 维护，
	// 代码只交实现），便于 BuildReport 消费方审计实现目录与绑定状态。
	HandlerNames []string
}

// AgentInfo 是一个已注册 Agent 的调试概要。
type AgentInfo struct {
	AgentID      string
	AgentType    string
	AgentVersion string
	Runtime      string
	Mode         string
}

// ReadinessReport 是运行时健康投影，由 Engine.Readiness 与 doctor CLI 返回。
type ReadinessReport struct {
	// Ready 在每一个必需子系统都健康且无关键依赖为 Unsupported 时为 true。
	// Degraded 子系统保持 Ready=true。
	Ready bool
	// Reasons 枚举 Ready 为 false 的原因；Ready 时为空。
	Reasons []string
	// Extensions 映射 BuildReport.Extensions，附带实时 Fingerprint。
	Extensions []ExtensionInfo
	// Runtimes 映射 BuildReport.Runtimes，附带实时 Available 标志。
	Runtimes []RuntimeInfo
	// SchemaVersions 是 BuildReport.SchemaVersions 的拷贝。
	SchemaVersions []string
	// CheckedAt 是 Readiness 计算出的时刻。
	CheckedAt time.Time
}
