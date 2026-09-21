package extension

import (
	"context"
	"encoding/json"
)

// ArtifactRef 与 harness.ArtifactRef 保持一致，但不引入包循环。扩展实现只读
// 接收它、不得改写字段。若 extension 包未来移动到公共根包，
// 收敛为包级 alias。
type ArtifactRef struct {
	ID        string
	TenantID  string
	SessionID string
	RunID     string
	MIME      string
	Filename  string
	Hash      string
	Size      int64
}

// ScopedDataEntry 是 ScopedData 中的一项，字段名与 harness.ScopedDataItem 对齐；
// kernel 在管道边界上做两者互拷贝。
type ScopedDataEntry struct {
	Source     string
	Visibility string
	Value      json.RawMessage
	Ref        string
	Hash       string
}

// RunInitRequest 是 RunInitializer.Initialize 的入参，只承载 initializer 合法
// 需要的信息：身份、当前输入的预览 / ref，以及此前已收集到的 base scoped data。
type RunInitRequest struct {
	Ctx Context
	// InputPreview 是本轮用户输入内容的短安全预览（不得携带敏感 PII；扩展
	// 不得在此处对完整内容做处理）。
	InputPreview string
	// InputArtifactRefs 枚举本轮用户输入中每一个 artifact-ref part。
	InputArtifactRefs []ArtifactRef
	// SeedScopedData 是先前的 initializer 或调用方已经产出的 scoped data。
	SeedScopedData map[string]ScopedDataEntry
}

// RunInitOutput 是 RunInitializer.Initialize 的返回值。kernel 会把返回的
// ScopedData 合并进 Run 的冻结 ScopedData 快照，并把附加的 ref 记入 Run 的
// Artifact ledger。
type RunInitOutput struct {
	// ScopedData 与用户输入 ScopedData 使用相同 key 空间的追加项。调用方
	// 不得使用与 kernel 保留名（Binding/RunID/安全指令等）冲突的 key，
	// kernel 会拒绝这类冲突。
	ScopedData map[string]ScopedDataEntry
	// ArtifactRefs 是 initializer 额外产出的 artifact（例如已上传的预处理
	// 图片）。它们必须已经存在于 Artifact Store；initializer 不得在此处
	// 上传内联字节。
	ArtifactRefs []ArtifactRef
}

// RunInitializer 在 IdentityResolver 之后、AgentBinding 之前，为本轮注入
// workshop 状态。它是设计 §7.1 中唯一被允许在 Turn 中间追加 artifact /
// scoped data 的扩展类型。
type RunInitializer interface {
	Initialize(ctx context.Context, req RunInitRequest) (RunInitOutput, error)
}
