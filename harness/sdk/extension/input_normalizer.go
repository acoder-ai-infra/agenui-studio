package extension

import (
	"context"
	"encoding/json"
)

// NormalizedPart 是扩展边界上的 harness.MessagePart 镜像。公共扩展边界
// extension 包搬到 harness/ 之后收敛为共享类型；它的形状与 MessagePart 一致，
// 调用方可以逐字段拷贝，而不必用 JSON round-trip。
type NormalizedPart struct {
	Kind       string
	Order      int
	MIME       string
	Filename   string
	Hash       string
	Visibility string

	Text   string
	JSON   json.RawMessage
	Ref    ArtifactRef
	Inline []byte
}

// NormalizedMessage 是 InputNormalizer 的返回体。Role 永远为 "user"（扩展
// 可以把一个 Turn 拆成有序 Parts，但不允许改变说话人）。
type NormalizedMessage struct {
	Role       string
	Parts      []NormalizedPart
	Visibility string
	Metadata   map[string]string
}

// SourceRef 记录 NormalizedMessage 内容的来源。normalizer 用来合成输出的每
// 一份输入片段都必须在此声明；kernel 会把 source 列表 hash 进冻结的输入快照，
// 供 replay 时验证 normalizer 的确定性。
type SourceRef struct {
	Kind    string // "scoped_data" | "artifact" | "context_fragment"
	Key     string // ScopedData key、artifact ID 或 fragment kind
	Version string // 调用方提供的版本 tag（例如存储行版本）
	Hash    string // 内容 hash（与 source 的 Hash 字段一致）
}

// NormalizeRequest 是 InputNormalizer.Normalize 的入参，承载原始用户消息、
// 冻结的 ScopedData + Artifact 引用，以及此前 ContextContributor 产出的
// context fragment。
type NormalizeRequest struct {
	Ctx Context

	// RawInput 是 SDK 调用方原样撰写的用户 Turn，是 harness.Message 的
	// 直接投影。
	RawInput NormalizedMessage
	// ScopedData 是 Normalize 时点的冻结 scoped-data 视图。
	ScopedData map[string]ScopedDataEntry
	// ArtifactRefs 枚举 RawInput 中每一个 artifact-ref part。
	ArtifactRefs []ArtifactRef
	// ContextContributions 是各 ContextContributor 实现产出的 fragment
	// 列表，已按 Order 与 Priority 排序完毕。
	ContextContributions []ContextFragment
}

// NormalizedInput 是 InputNormalizer.Normalize 的返回值。运行时会把
// ContextFragments 字段接到最终 context 治理器，令 normalizer 已经内联进
// Message 的 fragment 自动从治理预算中扣除。
type NormalizedInput struct {
	// Message 是规范化后的用户 Turn。本层只允许 Kind=text / json /
	// artifact_ref 的 Part。
	Message NormalizedMessage
	// ContextFragments 是 ContribRequest.ContextContributions 中仍需送达
	// 模型的子集（也就是 normalizer 未内联进 Message 的部分）。kernel
	// 强制“不允许内容重复”。
	ContextFragments []ContextFragment
	// SourceRefs 记录 normalizer 消费了哪些 source。确定性 replay 校验依赖
	// 它。
	SourceRefs []SourceRef
}

// InputNormalizer 是强类型的、按 Agent 划分的输入规范化器。每个 (Agent,
// Agent 版本) 最多绑定一个实现。kernel 在每个 Run 内恰好调用一次；多轮模型
// 调用与 Resume 路径复用冻结的 NormalizedInput。
//
// Normalize 返回错误一律 fail closed：抑制“有界输入规范化漂移”本就是本 hook
// 存在的意义，静默 fallback 到原始输入会击穿这一保证
// （详见 the public SDK contract）。
type InputNormalizer interface {
	// ID 返回稳定的 Policy.ID；kernel 会与 Registry 提供的 ID 交叉校验。
	ID() string
	// Normalize 对用户输入做规范化。
	Normalize(ctx context.Context, req NormalizeRequest) (NormalizedInput, error)
}
