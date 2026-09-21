package agentruntime

import (
	"testing"

	"github.com/cloudwego/eino/schema"
	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
)

// 验证 Ledger Parts → ModelContentPart 投影：text/json 透传为文本，
// image_ref 投影为 image_url（artifact:// 归一），非图片附件降级占位文本。
func TestModelPartsFromContextProjection(t *testing.T) {
	parts := modelPartsFromContext([]contextpkg.ContentPart{
		{Kind: "text", Text: "描述这张图"},
		{Kind: "image_ref", MIME: "image/png", Filename: "a.png", ArtifactRef: "img_123"},
		{Kind: "image_ref", MIME: "image/jpeg", ArtifactRef: "artifact://t/img2"},
		{Kind: "json", JSON: []byte(`{"k":1}`)},
		{Kind: "file_ref", ArtifactRef: "artifact://t/doc1"},
	})
	if len(parts) != 5 {
		t.Fatalf("parts = %#v", parts)
	}
	if parts[0].Type != "text" || parts[0].Text != "描述这张图" {
		t.Fatalf("text part: %+v", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].URL != "artifact://img_123" || parts[1].MIMEType != "image/png" || parts[1].Name != "a.png" {
		t.Fatalf("bare image ref must be normalized: %+v", parts[1])
	}
	if parts[2].URL != "artifact://t/img2" {
		t.Fatalf("scheme-qualified ref must pass through: %+v", parts[2])
	}
	if parts[3].Type != "text" || parts[3].Text != `{"k":1}` {
		t.Fatalf("json part: %+v", parts[3])
	}
	if parts[4].Type != "file" || parts[4].URL != "artifact://t/doc1" {
		// G-C：file_ref 直达 file part（保留 artifact 定位符），不再降级文本。
		t.Fatalf("file ref must become a file part: %+v", parts[4])
	}
}

// 验证 einoMessages 对多 Part 用户输入填充 UserInputMultiContent，
// 纯文本消息路径零变化。
func TestEinoMessagesMultiPartProjection(t *testing.T) {
	pkg := ModelContextPackage{Messages: ModelContextMessages{ConversationWindow: []ModelContextMessage{
		{ID: "m1", Role: string(schema.User), Content: "看图", ContentParts: []ModelContentPart{
			{Type: "text", Text: "看图"},
			{Type: "image_url", URL: "artifact://t/img1", MIMEType: "image/png"},
		}},
		{ID: "m2", Role: string(schema.Assistant), Content: "好的"},
	}}}
	messages := einoMessages(pkg)
	if len(messages) != 2 {
		t.Fatalf("messages = %#v", messages)
	}
	multi := messages[0].UserInputMultiContent
	if messages[0].Content != "" || len(multi) != 2 {
		t.Fatalf("multi-part user message must move content into UserInputMultiContent: %#v", messages[0])
	}
	if multi[0].Type != schema.ChatMessagePartTypeText || multi[0].Text != "看图" {
		t.Fatalf("text input part: %+v", multi[0])
	}
	if multi[1].Type != schema.ChatMessagePartTypeImageURL || multi[1].Image == nil || multi[1].Image.URL == nil || *multi[1].Image.URL != "artifact://t/img1" || multi[1].Image.MIMEType != "image/png" {
		t.Fatalf("image input part: %+v", multi[1])
	}
	if messages[1].Content != "好的" || len(messages[1].UserInputMultiContent) != 0 {
		t.Fatalf("plain message must stay unchanged: %#v", messages[1])
	}
}

// 验证 mergeContextMessages 本地追加路径与 sameInputFact 均覆盖 Parts 事实。
func TestMergeContextMessagesCarriesParts(t *testing.T) {
	input := Message{ID: "input_1", Role: string(contextpkg.RoleUser), Content: "看图", Parts: []contextpkg.ContentPart{
		{Kind: "text", Text: "看图"},
		{Kind: "image_ref", MIME: "image/png", ArtifactRef: "artifact://t/img1"},
	}}
	merged, err := mergeContextMessages(ContextSnapshot{}, "s1", []Message{input}, false, false)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(merged) != 1 || len(merged[0].Parts) != 2 || merged[0].Parts[1].ArtifactRef != "artifact://t/img1" {
		t.Fatalf("parts must survive merge: %#v", merged[0])
	}
	if !sameInputFact(merged[0], input) {
		t.Fatal("identical parts must be the same input fact")
	}
	changed := input
	changed.Parts = []contextpkg.ContentPart{{Kind: "text", Text: "看图"}}
	if sameInputFact(merged[0], changed) {
		t.Fatal("parts drift must be detected as a different fact")
	}
}
