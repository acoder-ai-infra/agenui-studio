package extension

import (
	"context"
	"encoding/json"
)

// BeforeModelPart 是逐轮模型输入消息中的单个内容 Part（治理后形状，与
// runtime 的 ModelContentPart 对齐）。
type BeforeModelPart struct {
	// Type 取 text | image_url | file。
	Type string
	Text string
	// URL 承载图片/文件定位符（artifact:// 或 data URI；内联字节在投影
	// 进 Hook 视图时编码为 data URI，保证改写回程不丢内容）。
	URL      string
	MIME     string
	Filename string
}

// BeforeModelToolCall 是助手消息携带的工具调用请求（只读透传，改写它会
// 破坏工具协议配对）。
type BeforeModelToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// BeforeModelMessage 是即将送入模型的一条消息的可改写视图。
type BeforeModelMessage struct {
	Role    string
	Content string
	// Parts 是多模态内容块；非空时 Content 为压扁预览。
	Parts []BeforeModelPart
	// ToolCalls / ToolCallID / ToolName 是工具协议配对事实。Hook 返回的
	// 消息必须原样保留它们（kernel 不校验语义，破坏配对将导致 provider
	// 拒绝请求）。
	ToolCalls  []BeforeModelToolCall
	ToolCallID string
	ToolName   string
}

// ToolDefinition 是 Hook 可见的一款工具的只读描述符（来自本 Run 冻结的
// Agent 配置工具集）。Schema 为能力快照持有的 JSON schema，Hook 不得修改。
type ToolDefinition struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// ModelCallOptions 是逐轮模型调用参数的 provider-neutral 白名单（ADR-007）。
// v1 只开放 ToolChoice；后续按真实消费者增量添加字段（向后兼容）。
type ModelCallOptions struct {
	// ToolChoice 取 "" | "auto" | "required" | "none" | 具体工具名。
	// 指定具体工具名时必须属于本轮可见工具集，否则本轮调用 fail closed。
	ToolChoice string
}

// BeforeModelRequest 是 BeforeModelHook.BeforeModel 的入参。
type BeforeModelRequest struct {
	Ctx Context
	// Round 是本 Run 内的模型调用轮次（从 1 开始，含工具循环轮次）。
	Round int
	// Messages 是本轮即将送入模型的消息序列快照（不含 agent Instruction）。
	Messages []BeforeModelMessage
	// Tools 是本轮候选工具集：链上第一个 Hook 拿到 Agent 配置工具全集，
	// 后续 Hook 拿到前一个 Hook 收窄后的集合（每轮从全集重新开始）。
	Tools []ToolDefinition
	// Options 是本轮调用参数的当前值（链式传递）。
	Options ModelCallOptions
	// ScopedData 是本 Run 冻结的 Run 级 scoped-data 只读视图（调用方
	// StartRequest.ScopedData 与 RunInitializer 产出的合并结果），与
	// InputNormalizer / ContextContributor 收到的冻结视图同源。写入口
	// 唯一属于 RunInitializer；Hook 不得依赖对本 map 的改写产生任何
	// 效果（kernel 每次调用传入深拷贝，改写不会回流也不跨轮传播）。
	ScopedData map[string]ScopedDataEntry
}

// BeforeModelResult 是塑形后的模型请求。空 Messages 视为契约违规
// （fail closed）。Tools 只能从入参集合中继续选择：返回不在入参集合内的
// 工具名会被忽略并留审计痕；返回空集合表示本轮模型不可见任何工具。
type BeforeModelResult struct {
	Messages []BeforeModelMessage
	Tools    []ToolDefinition
	Options  ModelCallOptions
}

// BeforeModelHook 在每次模型调用前对即将送出的模型请求做确定性塑形：
// 改写消息、从 Agent 配置工具集中选择本轮可见工具、调整白名单化的调用
// 参数。与 InputNormalizer（每 Run 一次、作用于用户原始输入）不同，它
// 作用于运行期每一轮模型调用（含多轮工具循环与 Resume 后的轮次）。
//
// 治理边界（ADR-006）：
//
//   - 治理层不变：Agent 配置工具集是本 Run 不可变能力快照，Tool Gateway
//     ACL 按全集执行；Hook 只影响可见层（本轮请求携带哪些工具定义）。
//   - kernel 保证本轮模型发出的 tool call 必须属于本轮可见集（不可见则
//     不可执行）。
//   - 每轮可见集与调用参数的改写以 debug 事件留痕（改写前后指纹）。
//
// 多个 Hook 按 agents.yaml 声明顺序串联（后一个收到前一个的输出）；返回
// 错误一律 fail closed（本轮模型调用失败）。agent 绑定型扩展：注册
// （WithBeforeModelHookProvider / RegisterBeforeModelHooks）只提供实现，
// 是否执行由 agents.yaml `extensions.before_model_hooks` 按 ID 声明，
// 未声明的 agent 不执行且零开销直通；子 agent 使用自己的配置，不继承
// 父 agent 的绑定。
type BeforeModelHook interface {
	// ID 返回稳定的扩展 ID。
	ID() string
	// BeforeModel 对本轮模型请求做塑形。原样返回入参表示不改写。
	BeforeModel(ctx context.Context, req BeforeModelRequest) (BeforeModelResult, error)
}
