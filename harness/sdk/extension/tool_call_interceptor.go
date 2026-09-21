package extension

import (
	"context"
	"encoding/json"
)

// ToolCallInfo 是被拦截的一次工具调用的元信息与入参。
type ToolCallInfo struct {
	Ctx Context
	// Name 是工具公开名（含 task 子 Agent 调用）。
	Name string
	// CallID 是模型侧工具调用 ID。
	CallID string
	// Arguments 是模型给出的 JSON 入参（已过 Tool Gateway schema 校验）。
	Arguments json.RawMessage
}

// ToolCallOutcome 是返回给模型的工具结果。
type ToolCallOutcome struct {
	// Result 是模型可见的结果文本/JSON。
	Result string
}

// ToolCallNext 触发真实工具执行（经 Tool Gateway 治理链）。拦截器可传入
// 改写后的 arguments；Tool Gateway 落账的入参即为改写后的真实执行事实。
type ToolCallNext func(ctx context.Context, arguments json.RawMessage) (ToolCallOutcome, error)

// ToolCallInterceptor 环绕一次工具调用：可在执行前改写入参、在执行后改写
// 返回给模型的结果、并执行同步侧写（写宿主自有存储）。不调用 next 即短路
// （结果视为拦截器合成）。
//
// 治理红线：Tool Gateway 的审批 / 幂等 / 事件链完全不变——tool_call_completed
// 落账的是真实执行结果；拦截器改写的只是模型可见结果，改写发生时 kernel 以
// debug 事件留痕（原始/改写后指纹）。拦截器返回错误一律 fail closed。
//
// 本拦截器覆盖 DeepAgent 的平台工具、MCP 工具与 task 子 Agent 调用（仅
// eino 路径有挂载点）。agent 绑定型扩展：注册（WithToolCallInterceptorProvider /
// RegisterToolCallInterceptors）只提供实现，是否执行由 agents.yaml
// `extensions.tool_call_interceptors` 按 ID 声明，未声明的 agent 不执行；
// 子 agent 使用自己的配置，不继承。多个拦截器按声明顺序嵌套，先声明者
// 在最外层。
type ToolCallInterceptor interface {
	// ID 返回稳定的扩展 ID。
	ID() string
	// Intercept 环绕执行一次工具调用。
	Intercept(ctx context.Context, call ToolCallInfo, next ToolCallNext) (ToolCallOutcome, error)
}
