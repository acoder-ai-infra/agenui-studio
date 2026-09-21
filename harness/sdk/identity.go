package harness

// Identity 承载所有 SDK 调用运行在其中的冻结身份（tenant/user/session/run
// 作用域）。信任边界和配额记账都基于这些字段，因此调用方必须借助受信的
// IdentityResolver（见 harness/extension）从业务身份映射到 Harness 身份，
// 而不是在测试之外手工填 Identity。
//
// 在 DTO 层所有字段都是可选的；具体必需字段由每个操作单独强制。例如
// Start 需要 TenantID + SessionID（SessionID 可为空以由 SDK 生成），Resume
// 则额外需要 RunID。
type Identity struct {
	// TenantID 是存储、配额、Tool ACL 的冻结租户分区，最多 64 个 UTF-8 字符。
	TenantID string
	// UserID 标识租户内的活动主体。DTO 层可选，非空时最多 64 个 UTF-8
	// 字符；超长身份会在持久化前以 ErrInvalidRequest 拒绝，绝不截断。
	UserID string
	// SessionID 是持久化的会话/轮次 id。Start 时为空表示“开一个新会话”；
	// 后续同一会话的每次调用必须复用 RunView.SessionID 返回的值，非空时最多
	// 64 个 UTF-8 字符。
	SessionID string
	// RunID 标识单次 Agent Run。Start 时可为空让 kernel 生成；Resume /
	// Cancel / Subscribe 必须显式提供。
	RunID string
	// AgentID 选择注册中的哪个 Agent 配置来运行。为空则回退到 kernel
	// 的 DefaultAgentID（harness.runtime.default_agent_id），非空时最多 128
	// 个 UTF-8 字符。
	AgentID string
	// AgentVersion 在 registry 中存在多个 Agent 版本时钉住某一个。为空
	// 表示“解析最新可用版本”。
	AgentVersion string
}

// Role 标记一条 Message 的发送方角色。它是 canonical 的角色词汇；kernel
// 负责把它映射到目标 runtime 的角色模型。
type Role string

const (
	// RoleUser 表示最终用户撰写、或宿主代表用户撰写的内容。它会经过
	// Ingress Guardrail，并在最终输入治理阶段由 PreserveManifest 保留。
	RoleUser Role = "user"
	// RoleAssistant 表示 Agent 撰写的内容。SDK 不会在 Start 时合成
	// assistant 消息；RoleAssistant 只会在通过 RunView / ResultView
	// 浮现的历史消息中出现。
	RoleAssistant Role = "assistant"
	// RoleSystem 表示系统指令。SDK 不接受 Start 时传入 RoleSystem：
	// 系统指令归属 Agent Registry 所有，会在上下文装配阶段注入。
	RoleSystem Role = "system"
	// RoleTool 表示回传给模型的工具调用结果。它由 Tool Gateway 内部
	// 生成；宿主不会在 Start 时写入 RoleTool。
	RoleTool Role = "tool"
)
