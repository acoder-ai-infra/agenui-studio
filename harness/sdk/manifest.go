package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// InputManifest 是一份冻结后、有序的输入清单，列出 SDK 调用方在 Start
// 时提供的每一份 payload。它会盖印到 Run 的存储行上，让审计、重放、
// 扩展协议看到与 kernel 消耗时一致的输入形状。
//
// manifest 有意采用“内容 hash 优先”：kernel 不会在 manifest 中持久化
// 大内内联 body，而是存 hash + ref。
type InputManifest struct {
	// Role 是消息角色（Start 时恒为 RoleUser）。
	Role Role
	// Parts 是与 Message.Parts 对齐的有序清单项列表。
	Parts []InputManifestPart
	// Visibility 是消息级的有效 visibility。
	Visibility Visibility
	// Hash 是 manifest 的内容 hash（sha256 hex，带算法前缀），作为
	// 确定性重放的锚点。
	Hash string
}

// InputManifestPart 是 InputManifest 中的一项。每项都必须提供 Hash；
// kernel 拒绝接受 hash 为空的 manifest。
type InputManifestPart struct {
	Order      int
	Kind       PartKind
	MIME       string
	Filename   string
	Hash       string
	Visibility Visibility
	// Ref 仅在 artifact-ref 类型的 Part 上填充。
	Ref ArtifactRef
	// PreviewLen 记录文本/JSON 预览的字节长度，供审计使用但不内嵌 body。
	PreviewLen int
}

// BuildInputManifest 检查调用方提供的 Message，产出本轮的冻结 InputManifest。
// 调用方不应直接构造 manifest；SDK 内部适配层与测试使用它计算稳定 hash 并
// 将形状与 Run 行一同持久化。
func BuildInputManifest(msg Message) InputManifest {
	manifest := InputManifest{
		Role:       msg.Role,
		Visibility: msg.Visibility,
	}
	if manifest.Role == "" {
		manifest.Role = RoleUser
	}
	if manifest.Visibility == "" {
		manifest.Visibility = VisibilityUserVisible
	}
	partOrders := make(map[int]int, len(msg.Parts))
	for i, part := range msg.Parts {
		order := part.Order
		if order == 0 {
			order = i
		}
		partOrders[order] = i
	}
	manifest.Parts = make([]InputManifestPart, 0, len(msg.Parts))
	for _, part := range msg.Parts {
		manifest.Parts = append(manifest.Parts, InputManifestPart{
			Order:      part.Order,
			Kind:       part.Kind,
			MIME:       part.MIME,
			Filename:   part.Filename,
			Hash:       ensurePartHash(part),
			Visibility: partVisibility(part.Visibility, manifest.Visibility),
			Ref:        part.Ref,
			PreviewLen: partPreviewLen(part),
		})
	}
	manifest.Hash = manifestHash(manifest)
	return manifest
}

func ensurePartHash(p MessagePart) string {
	if p.Hash != "" {
		return p.Hash
	}
	h := sha256.New()
	h.Write([]byte(p.Kind))
	h.Write([]byte("\x00"))
	switch p.Kind {
	case PartKindText:
		h.Write([]byte(p.Text))
	case PartKindJSON:
		h.Write(p.JSON)
	case PartKindImageRef, PartKindFileRef, PartKindArtifactRef:
		h.Write([]byte(p.Ref.ID))
		h.Write([]byte("\x00"))
		h.Write([]byte(p.Ref.Hash))
	case PartKindInlineBinary:
		h.Write(p.Inline)
	}
	sum := h.Sum(nil)
	return "sha256:" + hex.EncodeToString(sum[:16])
}

func partVisibility(part, message Visibility) Visibility {
	if part != "" {
		return part
	}
	return message
}

func partPreviewLen(p MessagePart) int {
	switch p.Kind {
	case PartKindText:
		return len(p.Text)
	case PartKindJSON:
		return len(p.JSON)
	case PartKindInlineBinary:
		return len(p.Inline)
	}
	return 0
}

// manifestHash 为 manifest 产出 canonical hash。当 Parts 完全一致（维持顺序）
// 时在多次 Run 中都能得到确定性结果。
func manifestHash(m InputManifest) string {
	h := sha256.New()
	h.Write([]byte(m.Role))
	h.Write([]byte("\x00"))
	h.Write([]byte(m.Visibility))
	h.Write([]byte("\x00"))
	for _, part := range m.Parts {
		h.Write([]byte(fmt.Sprintf("%d", part.Order)))
		h.Write([]byte("\x00"))
		h.Write([]byte(part.Kind))
		h.Write([]byte("\x00"))
		h.Write([]byte(part.Hash))
		h.Write([]byte("\x00"))
		h.Write([]byte(part.MIME))
		h.Write([]byte("\x00"))
		h.Write([]byte(part.Visibility))
		h.Write([]byte("\x00"))
	}
	sum := h.Sum(nil)
	return "sha256:" + hex.EncodeToString(sum[:16])
}

// EncodeManifest 返回 manifest 的稳定 JSON 编码，适合与 Run 行一同持久化。
// 它故意保持最小信息，以便审计日志在不接触业务 payload 的情况下完成
// round-trip。
func EncodeManifest(m InputManifest) ([]byte, error) {
	if len(m.Parts) == 0 {
		return nil, fmt.Errorf("%w: empty manifest", ErrInvalidRequest)
	}
	// InputManifest 字段均为导出的，json.Marshal 安全可用。
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// stripSpace 供多 Part 预览渲染器判空使用。
func stripSpace(s string) string {
	return strings.TrimSpace(s)
}
