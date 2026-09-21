package extension

// Kind 标识一个扩展实现参与固定扩展管道的哪一阶段。kernel 会严格按下面的
// 顺序执行扩展。大多数扩展是
// Engine 级语义：同一 kind 可注册多个实现并按注册顺序执行，不与 agent_id
// 绑定；例外是 before_model_hook / tool_call_interceptor /
// output_validator 三类 agent 绑定型扩展：注册只提供实现，是否执行由
// agents.yaml `extensions` 按 ID 声明，未声明的 agent 不执行。
type Kind string

const (
	// KindIdentityResolver 把业务身份映射为 Harness 身份
	//（tenant/user/session）。它在入口 guardrail 之前执行。
	KindIdentityResolver Kind = "identity_resolver"

	// KindRunInitializer 在身份解析之后、AgentBinding 之前，为本轮
	// 注入 ScopedData 与 Artifact 引用。
	KindRunInitializer Kind = "run_initializer"

	// KindContextContributor 在 AgentBinding / CapabilitySnapshot 之后、
	// InputNormalizer 之前，产出业务 context fragment。
	KindContextContributor Kind = "context_contributor"

	// KindInputNormalizer 确定性地把用户输入规范化为目标 Agent 可消费的
	// 形状；可注册多个，按注册顺序链式执行（Engine 级，不与 agent_id 绑定）。
	KindInputNormalizer Kind = "input_normalizer"

	// KindToolProvider 注册业务函数工具的实现提供者（定义与治理属性在
	// tools.yaml 维护），所有调用统一经过 Tool Gateway。
	KindToolProvider Kind = "tool_provider"

	// KindOutputValidator 检查模型的结构化输出，可以请求有限次重试；
	// 但不允许改写 canonical event。agent 绑定型：由 agents.yaml
	// `extensions.output_validators` 按 ID 声明执行。
	KindOutputValidator Kind = "output_validator"

	// KindProtocolProjector 把 canonical AgentEvent 投影为零到多条业务
	// 协议帧。只读；不会伪造 canonical event。
	KindProtocolProjector Kind = "protocol_projector"

	// KindEventObserver 是审计 / 指标等旁路 observer。它不能阻塞主管道，
	// 也不能修改事实链。
	KindEventObserver Kind = "event_observer"

	// KindBeforeModelHook 在每次模型调用前塑形即将送出的模型请求：改写
	// 消息序列、从 Agent 配置工具集中选择本轮可见工具、调整白名单化的
	// ModelCallOptions（逐轮生效，含工具循环轮次）。agent 绑定型：由
	// agents.yaml `extensions.before_model_hooks` 按 ID 声明执行。
	KindBeforeModelHook Kind = "before_model_hook"

	// KindToolCallInterceptor 环绕一次工具调用：可改写入参与模型可见
	// 结果；Tool Gateway 落账事实不变。agent 绑定型：由 agents.yaml
	// `extensions.tool_call_interceptors` 按 ID 声明执行（仅 eino 路径）。
	KindToolCallInterceptor Kind = "tool_call_interceptor"
)

// Phase 是每个 Kind 在管道里的精确序号，用于计算 Fingerprint 与
// BuildReport 排序。
type Phase int

const (
	PhaseIdentity Phase = iota + 1
	PhaseRunInit
	PhaseContextContribute
	PhaseInputNormalize
	PhaseToolProvide
	PhaseOutputValidate
	PhaseProtocolProject
	PhaseEventObserve
	PhaseBeforeModel
	PhaseToolCallIntercept
)

// PhaseOf 返回 Kind 对应的管道阶段。
func PhaseOf(k Kind) Phase {
	switch k {
	case KindIdentityResolver:
		return PhaseIdentity
	case KindRunInitializer:
		return PhaseRunInit
	case KindContextContributor:
		return PhaseContextContribute
	case KindInputNormalizer:
		return PhaseInputNormalize
	case KindToolProvider:
		return PhaseToolProvide
	case KindOutputValidator:
		return PhaseOutputValidate
	case KindProtocolProjector:
		return PhaseProtocolProject
	case KindEventObserver:
		return PhaseEventObserve
	case KindBeforeModelHook:
		return PhaseBeforeModel
	case KindToolCallInterceptor:
		return PhaseToolCallIntercept
	default:
		return 0
	}
}
