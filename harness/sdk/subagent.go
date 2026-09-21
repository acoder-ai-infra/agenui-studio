package harness

// SubAgentResultType 是 SubAgentInvocationResult 的分支判别符。canonical
// 契约（the public SDK contract）里只有两种分支：正常完成，或
// 上交给父 Run 的 ask-user proposal。
type SubAgentResultType string

const (
	// SubAgentResultCompleted 是正常终态分支：子 Run 完成本次尝试并返回
	// Content 或 ContentRef（也可能带分类失败的 IsError）。下游流程在父
	// Run 继续。
	SubAgentResultCompleted SubAgentResultType = "completed"

	// SubAgentResultAskUser 表示子叶 Run 发现自己需要用户答复才能继续。
	// 子 Run 不会创建 ControlRequest 或 checkpoint；它把一个
	// InteractionProposal 向上交给父 Run —— Control 归父 Run 所有。
	SubAgentResultAskUser SubAgentResultType = "ask_user"
)

// SubAgentInvocationResult 是子 Agent 调用的扩展返回值。它只在 runtime
// 内部使用；SDK 调用方仅在父 Run 创建 canonical ControlRequest 后，通过父 Run
// 的 control_request_created 事件间接看到其中的 InteractionProposal。
//
// 这个类型放在公开包里，是因为业务扩展（例如 Runtime 拦截器）可能需要在
// 组合自定义 sub-agent shim 时对分支做穷举判断。它不携带任何 resume 秘密。
type SubAgentInvocationResult struct {
	// ResultType 是分支判别符；下游必须做穷举 switch。
	ResultType SubAgentResultType `json:"result_type"`
	// InteractionProposal 仅在 ResultType == SubAgentResultAskUser 时置位。
	InteractionProposal *InteractionProposal `json:"interaction_proposal,omitempty"`

	// ChildRunID 是产出本结果的子 Run 尝试 id。
	ChildRunID string `json:"child_run_id"`
	// Content 是子 Run 答复的短内联预览（用于 completed 分支）；使用
	// ContentRef 时为空。
	Content string `json:"content,omitempty"`
	// ContentRef 指向承载 completed 分支完整答复的 Artifact；Content 内联
	// 时为空。
	ContentRef string `json:"content_ref,omitempty"`
	// IsError 表示子 Run 以分类失败结束。AskUser 结果必须 IsError=false。
	IsError bool `json:"is_error,omitempty"`
}

// InteractionProposal 是子叶 Run 通过 Runtime Adapter 上交给父 Run 的
// “请求用户输入”。它复用 canonical AskUser schema（question、options、input
// 约束），但显式不携带任何 control 状态：没有 control_request_id、没有
// checkpoint_id、没有 resume_token、没有 status。父 Run 的 ControlRequest
// Service 会把 proposal 转成 canonical harness.control_request.v1 记录。
type InteractionProposal struct {
	// Prompt 是要展示给人类的可读问题。
	Prompt string `json:"prompt"`
	// Kind 对交互分类，方便 protocol projector 选择 UI
	//（如 "ask_user"、"confirm"、"permission"、"elicit"）。
	Kind string `json:"kind"`
	// Options 是可选的有限选项集（多选题场景）；自由输入时为空。
	Options []InteractionOption `json:"options,omitempty"`
	// Input 对自由输入答复做约束（例如 JSON schema 片段、文本最大长度）。
	// kernel 视其为不透明；渲染器负责解释。
	Input InteractionInputConstraint `json:"input,omitempty"`
	// TaskID 是 proposal 归属的父 Run 任务 id。由 runtime adapter 填写，
	// 而非子叶 Run 直接填写。
	TaskID string `json:"task_id,omitempty"`
	// AttemptID 是当前尝试 id；父 Run 在使用用户答复 resume 时会创建一个
	// 新的 AttemptID。
	AttemptID string `json:"attempt_id,omitempty"`
}

// InteractionOption 是 InteractionProposal.Options 列表中的一个选项。
type InteractionOption struct {
	Value       string            `json:"value"`
	Label       string            `json:"label,omitempty"`
	Description string            `json:"description,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// InteractionInputConstraint 约束自由输入答复。字段均可选，由客户端渲染器
// 解释。
type InteractionInputConstraint struct {
	// SchemaRef 指向一个 JSON Schema artifact（当答复必须是 JSON 时）。
	SchemaRef string `json:"schema_ref,omitempty"`
	// MaxLength 限制自由文本答复长度；零值表示不限。
	MaxLength int `json:"max_length,omitempty"`
	// AllowMultiple 在 Options 存在时允许多选。
	AllowMultiple bool `json:"allow_multiple,omitempty"`
	// Required 标记答复必须提供（不允许空答复）。
	Required bool `json:"required,omitempty"`
}
