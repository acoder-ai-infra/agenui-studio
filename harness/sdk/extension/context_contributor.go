package extension

import "context"

// FragmentSource 标记一个 context fragment 的来源，方便下游治理在不检查
// 内容的情况下推理来源可信度。
type FragmentSource string

const (
	FragmentSourceBusiness   FragmentSource = "business"
	FragmentSourceKnowledge  FragmentSource = "knowledge"
	FragmentSourceTemplate   FragmentSource = "template"
	FragmentSourceHistory    FragmentSource = "history"
	FragmentSourceRule       FragmentSource = "rule"
	FragmentSourceExperiment FragmentSource = "experiment"
)

// ContextFragment 是一位 ContextContributor 贡献的一份业务上下文片段。多个
// fragment 会按 Priority 从高到低排序，最终 context 治理器在超预算时按此顺序
// 截断。
type ContextFragment struct {
	// Kind 是 BuildReport 与审计用的短标签（"api_schema"、"design_token"、
	// "customer_profile" 等）。
	Kind string
	// Source 标记来源（business / knowledge / template / history / rule / ...）。
	Source FragmentSource
	// Priority 是非负整数；较高的 fragment 会在截断时先保留。
	Priority int
	// TokenBudget 是调用方对该 fragment 期望占用 token 数的提示。零值表示
	//“由内容计算”。
	TokenBudget int
	// Text 是正文。大 body 应先上传为 Artifact 再以 Ref 传递；kernel 会
	// 对每个 fragment 强制大小上限。
	Text string
	// Ref 指向承载完整正文的 Artifact。
	Ref ArtifactRef
	// Hash 是用于稳定 snapshot ID 的内容 hash。Text 场景下可选，Ref 场景
	// 下必填。
	Hash string
	// Visibility 覆盖 Run 级 visibility；为空则继承。
	Visibility string
}

// ContribRequest 是 ContextContributor.Contribute 的入参。
type ContribRequest struct {
	Ctx Context
	// ScopedData 是本贡献阶段的冻结 scoped-data 视图。Contributor 只读；
	// 不得修改它（不可变拷贝）。
	ScopedData map[string]ScopedDataEntry
	// Budget 是最终 context 治理器给贡献阶段的 token 预算总额。Contributor
	// 应把它视为软上限并尊重它。
	Budget int
}

// ContextContributor 在 AgentBinding 与 CapabilitySnapshot 冻结之后、
// InputNormalizer 之前产出业务 context fragment。可以注册多个 contributor；
// kernel 按 Order 顺序执行，把它们的输出拼接后交给 context 治理器做去重和
// 最终预算控制（详见 the public SDK contract）。
type ContextContributor interface {
	Contribute(ctx context.Context, req ContribRequest) ([]ContextFragment, error)
}
