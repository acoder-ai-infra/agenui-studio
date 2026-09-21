package extension

import (
	"context"
	"encoding/json"
)

// OutputAction 告诉 kernel 如何处置当前输出，是 OutputValidator 返回值的
// 分支判别符。
type OutputAction string

const (
	// OutputAccept 保留当前输出不变。
	OutputAccept OutputAction = "accept"
	// OutputRetry 请求 kernel 触发一次有界重试（结果里的 RetryBudget 决定
	// 上限）。重试会回到模型调用；不会重跑 IdentityResolver / RunInitializer。
	OutputRetry OutputAction = "retry"
	// OutputFail 强制以 classifier 已归类的错误结束为 run_failed。
	OutputFail OutputAction = "fail"
)

// OutputValidateRequest 是 OutputValidator.Validate 的入参。
type OutputValidateRequest struct {
	Ctx Context
	// Text 是模型给出的文本答复（若只有工具调用则可为空）。
	Text string
	// StructuredJSON 是调用方要求 JSON 模式时模型返回的结构化输出；
	// 模型返回自由文本时为空。
	StructuredJSON json.RawMessage
	// ToolCalls 枚举模型本轮产出的 tool_call 消息。
	ToolCalls []OutputToolCall
	// Attempt 是重试环记账用的当前尝试序号。
	Attempt int
}

// OutputToolCall 是模型产出的 tool call 在扩展层的镜像。
type OutputToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// OutputValidateResult 是 OutputValidator.Validate 的返回值。
type OutputValidateResult struct {
	Action OutputAction
	// RetryBudget 是 Action == OutputRetry 时允许的最大额外尝试次数。
	// 零值表示“不重试”。
	RetryBudget int
	// Reason 是 Action == OutputFail 时写入 Run 错误记录的短分类字符串，
	// 永远不含原始 payload / PII。
	Reason string
	// RepairFeedback 是 Action == OutputRetry 时交给下一次模型尝试的、
	// 已脱敏且有边界的修复说明。Harness 原样传递该说明，但不解释其业务
	// 语义；空值时 Harness 使用通用的契约修复提示。
	RepairFeedback string
	// PatchedText / PatchedJSON 允许 validator 在不发起额外模型调用的
	// 情况下做小规模结构规范化修复。Action != accept 时被忽略。
	PatchedText string
	PatchedJSON json.RawMessage
}

// OutputValidator 检查模型的结构化输出并请求重试或失败。它无法改写 canonical
// event，只能通过 OutputAction 控制重试环。安全关键的 validator 必须使用
// FailClosed 策略（设计 §7.1）。
//
// 注册（WithOutputValidatorProvider / RegisterOutputValidators）只提供实现；
// 是否执行由 agents.yaml `extensions.output_validators` 按 ID 绑定，未声明的
// agent 不执行。多个 validator 按声明顺序执行，首次非-Accept 结果短路。
type OutputValidator interface {
	// ID 返回稳定的扩展 ID（agents.yaml 绑定引用的键）。
	ID() string
	Validate(ctx context.Context, req OutputValidateRequest) (OutputValidateResult, error)
}
