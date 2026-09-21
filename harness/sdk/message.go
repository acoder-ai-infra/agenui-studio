package harness

import (
	"encoding/json"
)

// MaxInlineBinaryBytes 是单个 PartKindInlineBinary payload 的字节上限（ADR-013）。
// 视频帧等小二进制内联直达模型请求（不写 Artifact Store）；超限的 payload
// 必须先上传为 Artifact 再以 ref 传入，否则 Start 请求校验 fail closed。
const MaxInlineBinaryBytes = 5 << 20 // 5 MiB

// Visibility 是 canonical Visibility 枚举（详见 harness-canonical-contract.md
// §5）在 SDK 侧的镜像，决定一个 Part 或 Event 是否可以出现在 Harness 进程
// 外部。
type Visibility string

const (
	// VisibilityUserVisible 表示可以呈现给最终用户；也是 Protocol Projector
	// 唯一可以推给公开通道的 visibility。
	VisibilityUserVisible Visibility = "user_visible"
	// VisibilityDebug 供调试控制台和支持工具使用，永远不会进入公开通道。
	VisibilityDebug Visibility = "debug"
	// VisibilityInternal 是系统控制细节（如 planner 步骤、binding hash）。
	// runtime 可以读，但 SDK 不会向外浮现。
	VisibilityInternal Visibility = "internal"
	// VisibilityRestricted 是受显式策略约束的内容（PII、凭据、受限知识）。
	// 访问需要策略驱动的 ACL 授权。
	VisibilityRestricted Visibility = "restricted"
)

// PartKind enumerates the supported MessagePart payload shapes. Callers must
// preserve part order and use a distinct artifact reference per attachment.
type PartKind string

const (
	// PartKindText 是一段 UTF-8 纯文本。Text 字段承载值，其他字段为零值。
	PartKindText PartKind = "text"
	// PartKindJSON 是结构化数据。JSON 字段以 json.RawMessage 承载值；SDK
	// 在这一层不做 schema 校验。
	PartKindJSON PartKind = "json"
	// PartKindImageRef 指向已作为 Artifact 上传的图片。必须填 Ref；不允许
	// 使用 Inline。
	PartKindImageRef PartKind = "image_ref"
	// PartKindFileRef 指向已作为 Artifact 上传的非图片文件（文档、数据集）。
	PartKindFileRef PartKind = "file_ref"
	// PartKindArtifactRef 是任意已上传、已经过 ACL 校验的 Artifact 的通用
	// 引用。类型已知时优先使用 PartKindImageRef / FileRef，方便下游选择正确
	// 的渲染器。
	PartKindArtifactRef PartKind = "artifact_ref"
	// PartKindInlineBinary 是内联的小二进制 blob。仅允许在文档规定的本地
	// 大小上限之内使用；超限 payload 必须先上传到 Artifact Store 后以
	// PartKindArtifactRef 传递。
	PartKindInlineBinary PartKind = "inline_binary"
)

// ArtifactRef 指向一个已经上传到 Harness Artifact Store 的 Artifact。kernel
// 会先按 tenant / session ACL、MIME 与完整性 hash 校验后才接受该引用。
type ArtifactRef struct {
	// ID 是上传路径返回的 Artifact 稳定标识。
	ID string
	// TenantID / SessionID / RunID 是 Artifact 创建时的 ACL 作用域。为空时
	// 回退到当前 Identity。
	TenantID  string
	SessionID string
	RunID     string
	// MIME 是登记时记录的 content type；供 projector 选择渲染器。
	MIME string
	// Filename 是原始文件名（当 Artifact 由文件上传路径创建时）；由程序
	// 写入的 Artifact 为空。
	Filename string
	// Hash 是 Artifact 内容 hash（算法与编码记录在 ArtifactMeta 中）。调用
	// 方不得篡改。
	Hash string
	// Size 是 Artifact 字节数（引用时已知时填写）。
	Size int64
}

// MessagePart 是一条 Message ordered Parts 列表中的一项。它是 SDK 承载
// 多模态输入的载体：多段文本、多张图片、JSON payload 与 Artifact 引用
// 共享顺序 + MIME + hash + visibility。
//
// Contract:
//
//   - Parts 必须视为有序；kernel 在装配 runtime 请求时会保留顺序。
//   - 每个 Part 只有一个 payload 字段生效：由 Kind 决定；填多个会返回
//     请求错误。
//   - 大 inline payload（不加限制的 Text、超过本地上限的 InlineBinary）必须
//     先上传到 Artifact Store，再以 ref 形式传入。kernel 会拒绝超限内联
//     payload。
type MessagePart struct {
	// Kind 决定哪一个 payload 字段是权威。
	Kind PartKind
	// Order 是调用方指定的零起点位置。若所有 Part 的 Order 都为零，kernel
	// 会以 slice 索引推断顺序；只要有任一 Part 设了非零 Order，全部 Part
	// 就必须各自设定唯一的 Order。
	Order int
	// MIME 是 payload 的媒体类型（image/png、application/json、text/plain、
	// application/pdf 等）。PartKindText 时可选。
	MIME string
	// Filename 供渲染器标注 payload，可选。
	Filename string
	// Hash 是 payload 内容 hash，带算法前缀（例如 "sha256:<hex>"）。文本
	// 可选；JSON / 内联二进制推荐提供；artifact ref 必须提供（与
	// ArtifactRef.Hash 一致）。
	Hash string
	// Visibility 用来覆盖 Message 级 visibility；为空则继承 Message。
	// restricted Part 不得泄露到 user_visible frame。
	Visibility Visibility

	// Text 在 Kind == PartKindText 时承载 UTF-8 片段。
	Text string
	// JSON 在 Kind == PartKindJSON 时承载结构化数据。
	JSON json.RawMessage
	// Ref 在 Kind 为 PartKindImageRef / PartKindFileRef / PartKindArtifactRef
	// 时承载一个 Artifact 引用。
	Ref ArtifactRef
	// Inline 在 Kind == PartKindInlineBinary 时承载小二进制 payload，
	// 受 kernel 配置的最大内联尺寸约束。
	Inline []byte
}

// Message 是一次 Turn 中由单一 Role 撰写的 ordered content。
//
// The kernel preserves part order, records an InputManifest, and rejects an
// unsupported shape before opening the turn.
type Message struct {
	// Role 在 Start 时必须是 RoleUser（见 Role 文档）。通过 RunView 浮现
	// 的历史 Message 可以是任意角色。
	Role Role
	// Parts 是 ordered payload 列表。空 Parts 是请求错误。
	Parts []MessagePart
	// Visibility 是那些没有单独设 visibility 的 Part 的默认 visibility。
	// 为空时默认 VisibilityUserVisible。
	Visibility Visibility
	// Metadata 是一小份字符串标签映射，会随 trace 一并浮现并交给 observer；
	// 不会作为 event payload 持久化。
	Metadata map[string]string
}

// TextMessage 是最常用 Start 路径的便捷构造器：单 Part 用户文本消息。kernel
// 会自动推断 Kind、Order 和 Visibility（默认 user_visible）。
func TextMessage(content string) Message {
	return Message{
		Role: RoleUser,
		Parts: []MessagePart{{
			Kind: PartKindText,
			Text: content,
		}},
	}
}
