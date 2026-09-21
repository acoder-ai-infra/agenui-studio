package context

import (
	"encoding/json"
	"testing"
)

// envelope 编解码往返 + 纯文本零变化。
func TestPartsEnvelopeRoundTrip(t *testing.T) {
	parts := []ContentPart{
		{Kind: "text", Text: "你好"},
		{Kind: "image_ref", MIME: "image/png", Filename: "a.png", ArtifactRef: "artifact://t/img1", Hash: "sha256:abc"},
		{Kind: "json", JSON: json.RawMessage(`{"k":1}`)},
	}
	encoded := EncodePartsEnvelope(parts, "你好\n[artifact:artifact://t/img1]")
	if !IsPartsEnvelope(encoded) {
		t.Fatalf("encoded content must be detectable: %s", encoded)
	}
	decoded, preview, ok := DecodePartsEnvelope(encoded)
	if !ok || preview != "你好\n[artifact:artifact://t/img1]" || len(decoded) != 3 {
		t.Fatalf("decode = %v %q %v", decoded, preview, ok)
	}
	if decoded[1].ArtifactRef != "artifact://t/img1" || decoded[1].MIME != "image/png" {
		t.Fatalf("image part lost fields: %+v", decoded[1])
	}
	// 纯文本正文原样透传，不误判。
	if parts, preview, ok := DecodePartsEnvelope("plain text"); ok || parts != nil || preview != "plain text" {
		t.Fatal("plain text must pass through unchanged")
	}
	// 空 parts 编码等于 preview 本身（裸文本路径零变化）。
	if got := EncodePartsEnvelope(nil, "raw"); got != "raw" {
		t.Fatalf("empty parts must return preview, got %q", got)
	}
}

// 消息序列化形状回归：无 Parts 的消息 JSON 不出现 parts 字段（存量零迁移），
// 有 Parts 的消息纳入序列化（快照 ContentHash 因此覆盖 Parts 事实）。
func TestMessagePartsSerializationShape(t *testing.T) {
	plain, _ := json.Marshal(Message{ID: "m1", Role: RoleUser, Content: "hi"})
	if string(plain) != "" && jsonHasKey(plain, "parts") {
		t.Fatalf("plain message must not serialize parts: %s", plain)
	}
	withParts, _ := json.Marshal(Message{ID: "m2", Role: RoleUser, Content: "hi", Parts: []ContentPart{{Kind: "text", Text: "hi"}}})
	if !jsonHasKey(withParts, "parts") {
		t.Fatalf("parts must be serialized when present: %s", withParts)
	}
	left := hashSnapshotFacts([]Message{{ID: "m", Role: RoleUser, Content: "c"}}, nil, nil)
	right := hashSnapshotFacts([]Message{{ID: "m", Role: RoleUser, Content: "c", Parts: []ContentPart{{Kind: "text", Text: "c"}}}}, nil, nil)
	if left == right {
		t.Fatal("snapshot content hash must cover parts facts")
	}
}

func jsonHasKey(data []byte, key string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}
