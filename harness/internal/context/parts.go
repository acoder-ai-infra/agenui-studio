package context

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PartsEnvelopeSchemaVersion 标识多 Part 消息正文的 canonical envelope 版本。
// 含非纯文本 Part 的用户消息以该 envelope JSON 作为消息正文持久化（零 DDL），
// 读取侧据此还原结构化 Parts；纯文本消息正文保持裸文本不变。
const PartsEnvelopeSchemaVersion = "harness.message_parts.v1"

// partsEnvelopePrefix 是 envelope 判定前缀。EncodePartsEnvelope 保证
// schema_version 是第一个字段，判定无需完整解析。
const partsEnvelopePrefix = `{"schema_version":"` + PartsEnvelopeSchemaVersion + `"`

// partsEnvelope 是多 Part 消息正文的持久化形状。
type partsEnvelope struct {
	SchemaVersion string        `json:"schema_version"`
	Preview       string        `json:"preview,omitempty"`
	Parts         []ContentPart `json:"parts"`
}

// EncodePartsEnvelope 把结构化 Parts 与压扁预览编码为 envelope JSON。
// parts 为空时返回 preview 本身（纯文本路径零变化）。
func EncodePartsEnvelope(parts []ContentPart, preview string) string {
	if len(parts) == 0 {
		return preview
	}
	data, err := json.Marshal(partsEnvelope{
		SchemaVersion: PartsEnvelopeSchemaVersion,
		Preview:       preview,
		Parts:         parts,
	})
	if err != nil {
		return preview
	}
	return string(data)
}

// IsPartsEnvelope 报告一段消息正文是否是多 Part envelope。
func IsPartsEnvelope(content string) bool {
	return strings.HasPrefix(content, partsEnvelopePrefix)
}

// DecodePartsEnvelope 尝试把消息正文解码为 (parts, preview)。非 envelope
// 正文按纯文本返回（parts 为 nil、preview 即原文），decoded 为 false。
// envelope 前缀命中但解析失败视为纯文本处理，避免历史脏数据 fail closed。
func DecodePartsEnvelope(content string) (parts []ContentPart, preview string, decoded bool) {
	if !IsPartsEnvelope(content) {
		return nil, content, false
	}
	var envelope partsEnvelope
	if err := json.Unmarshal([]byte(content), &envelope); err != nil || envelope.SchemaVersion != PartsEnvelopeSchemaVersion || len(envelope.Parts) == 0 {
		return nil, content, false
	}
	return envelope.Parts, envelope.Preview, true
}

// FlattenParts 把 Parts 压扁为人类可读预览（会话标题、事件 text 字段共用），
// 与 harness 入口的 renderPreview 语义一致。
func FlattenParts(parts []ContentPart) string {
	var b strings.Builder
	for _, part := range parts {
		segment := ""
		switch part.Kind {
		case "text":
			segment = part.Text
		case "json":
			segment = string(part.JSON)
		case "image_ref", "file_ref", "artifact_ref":
			segment = "[artifact:" + part.ArtifactRef + "]"
		case "inline_binary":
			segment = fmt.Sprintf("[inline:%d bytes]", len(part.Inline))
		}
		if segment == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(segment)
	}
	return b.String()
}
