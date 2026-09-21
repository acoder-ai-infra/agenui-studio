package extension

import (
	"encoding/json"
	"time"
)

// InvocationKind 标识一次扩展调用是发生在根 Turn 还是子 Agent 调用上。有些
// 扩展类型（例如 InputNormalizer）在两种场景下行为不同。
type InvocationKind string

const (
	InvocationRoot     InvocationKind = "root"
	InvocationSubAgent InvocationKind = "sub_agent"
)

// Context 是每个扩展在每次调用时都会收到的只读上下文。它承载冻结的身份、
// 快照引用、trace 钩子，以及本次调用 ID 对应的冻结配置视图。扩展不得依赖
// 进程全局状态来推导这些值。
//
// 组成：
//   - Identity：由 Run 持久化行 + IdentityResolver 输出填充。
//   - Snapshots：不透明的 ref；kernel 负责解引用，扩展返回类型化贡献。
//   - Config：本扩展 ID 对应的、由 YAML 派生的冻结 JSON 配置视图。
type Context struct {
	// InvocationKind 说明调用方是根 Turn 还是子 Agent。
	InvocationKind InvocationKind

	// Identity 是冻结后的 tenant / user / session / run / agent 作用域。
	TenantID  string
	UserID    string
	SessionID string
	RunID     string
	// ParentRunID is set for a child Agent run. RootRunID identifies the
	// top-level Run for both root and child executions.
	ParentRunID    string
	RootRunID      string
	StepID         string
	AgentID        string
	AgentVersion   string
	AgentBindingID string

	// 快照引用；对 kernel 之外的调用方均视为不透明。
	ConfigSnapshotRef      string
	CapabilitySnapshotRefs []string
	ContextSnapshotRef     string
	InputMessageRef        string

	// Trace 与可观测性。
	TraceID string
	SpanID  string
	// Deadline 是 kernel 对本次调用强制的绝对截止时间（受 Policy.Timeout
	// 与外层 Run deadline 共同约束）。
	Deadline time.Time
	// Attempt 是重试步的当前尝试序号（从 1 起）。
	Attempt int

	// Environment 映射 internal/kernel.Environment；便于按 local / production
	// 分层的扩展做差异化处理。
	Environment string
	// SDKVersion 映射 harness.Version。
	SDKVersion string
	// SchemaVersions 列出本 Run 绑定的 canonical schema。
	SchemaVersions []string

	// Config 是本扩展 ID 对应的冻结 YAML 配置。每次调用都会做深拷贝，
	// 令实现无法修改共享 registry 状态。
	Config json.RawMessage
}

// DecodeConfig 把 ctx.Config 反序列化到 dst，方便扩展不必各自重复实现错误
// 包装。
func (c Context) DecodeConfig(dst any) error {
	if len(c.Config) == 0 || dst == nil {
		return nil
	}
	return json.Unmarshal(c.Config, dst)
}
