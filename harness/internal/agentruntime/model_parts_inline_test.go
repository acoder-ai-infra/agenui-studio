package agentruntime

import (
	"strings"
	"testing"

	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
)

// model_parts_inline_test.go 覆盖 A3 + G-C：inline_binary / file_ref 多 Part 从
// 上下文装配到模型请求的贯通（图片内联直达 data URI；非图片文件直达
// file part，不再降级为占位文本）。

func TestModelPartsFromContextInlineImage(t *testing.T) {
	parts := []contextpkg.ContentPart{
		{Kind: "text", Text: "看这张图"},
		{Kind: "inline_binary", MIME: "image/png", Filename: "frame.png", Inline: []byte("PNGDATA")},
	}
	got := modelPartsFromContext(parts)
	if len(got) != 2 {
		t.Fatalf("expected 2 model parts, got %d", len(got))
	}
	if got[0].Type != "text" || got[0].Text != "看这张图" {
		t.Fatalf("text part = %#v", got[0])
	}
	img := got[1]
	if img.Type != "image_url" || img.MIMEType != "image/png" || img.URL != "" {
		t.Fatalf("inline image must become image_url with empty URL: %#v", img)
	}
	// PNGDATA 的 base64。
	if img.Base64Data != "UE5HREFUQQ==" {
		t.Fatalf("inline image must carry base64 payload, got %q", img.Base64Data)
	}
}

// G-C：非图片内联文件直达 file part（携带 base64 字节），不再降级为
// 占位文本——否则用户发文件提问时模型读不到内容。
func TestModelPartsFromContextInlineNonImageBecomesFilePart(t *testing.T) {
	parts := []contextpkg.ContentPart{
		{Kind: "inline_binary", MIME: "application/pdf", Filename: "report.pdf", Inline: []byte("rawbytes")},
	}
	got := modelPartsFromContext(parts)
	if len(got) != 1 {
		t.Fatalf("expected 1 model part, got %d", len(got))
	}
	file := got[0]
	if file.Type != "file" || file.MIMEType != "application/pdf" || file.Name != "report.pdf" {
		t.Fatalf("non-image inline must become a file part: %#v", file)
	}
	// rawbytes 的 base64。
	if file.Base64Data != "cmF3Ynl0ZXM=" {
		t.Fatalf("file part must carry base64 payload, got %q", file.Base64Data)
	}
}

// 空内联字节仍降级为占位文本（无内容可送）。
func TestModelPartsFromContextEmptyInlineDowngrades(t *testing.T) {
	parts := []contextpkg.ContentPart{
		{Kind: "inline_binary", MIME: "application/octet-stream"},
	}
	got := modelPartsFromContext(parts)
	if len(got) != 1 || got[0].Type != "text" || !strings.Contains(got[0].Text, "inline:") {
		t.Fatalf("empty inline must downgrade to placeholder text: %#v", got)
	}
}

// G-C：file_ref 投影为 file part 并保留 artifact:// 定位符（由 app 层
// 在送 provider 前解析为内联字节）；通用 artifact_ref 仍为占位文本。
func TestModelPartsFromContextFileRefBecomesFilePart(t *testing.T) {
	got := modelPartsFromContext([]contextpkg.ContentPart{
		{Kind: "file_ref", ArtifactRef: "art_doc", MIME: "application/pdf", Filename: "spec.pdf"},
		{Kind: "artifact_ref", ArtifactRef: "art_other"},
	})
	if len(got) != 2 {
		t.Fatalf("expected 2 model parts, got %d", len(got))
	}
	if got[0].Type != "file" || !strings.HasPrefix(got[0].URL, "artifact://") || got[0].Name != "spec.pdf" {
		t.Fatalf("file_ref must become a file part with artifact locator: %#v", got[0])
	}
	if got[1].Type != "text" || !strings.Contains(got[1].Text, "artifact:") {
		t.Fatalf("generic artifact_ref must stay placeholder text: %#v", got[1])
	}
}
