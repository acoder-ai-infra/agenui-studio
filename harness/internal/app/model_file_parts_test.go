package app

import (
	"testing"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
)

// model_file_parts_test.go 覆盖 G-C 在 app 层的两处契约：
//   - gatewayMessages 把 file part 序列化为网关 FilePart（data URI）；
//   - Hook 视图往返（native / eino 双路径）不丢内联字节。

// gatewayMessages：非图片文件 part → 网关 file content part。
func TestGatewayMessagesFilePart(t *testing.T) {
	got := gatewayMessages([]agentruntime.ModelCallMessage{{
		Role: "user",
		ContentParts: []agentruntime.ModelContentPart{
			{Type: "text", Text: "看这个文件"},
			{Type: "file", Base64Data: "QUJD", MIMEType: "application/pdf", Name: "report.pdf"},
		},
	}})
	if len(got) != 1 || len(got[0].Parts) != 2 {
		t.Fatalf("expected one message with 2 parts, got %#v", got)
	}
	file := got[0].Parts[1]
	if file.Type != "file" || file.File == nil {
		t.Fatalf("file part must be projected as a gateway file part: %#v", file)
	}
	if file.File.FileData != "data:application/pdf;base64,QUJD" {
		t.Fatalf("file_data = %q", file.File.FileData)
	}
	if file.File.Filename != "report.pdf" {
		t.Fatalf("filename = %q", file.File.Filename)
	}
}

// 缺内容字节的 file part 不下发（避免送出空 file 块）。
func TestGatewayMessagesFilePartWithoutPayloadSkipped(t *testing.T) {
	got := gatewayMessages([]agentruntime.ModelCallMessage{{
		Role:         "user",
		ContentParts: []agentruntime.ModelContentPart{{Type: "file", MIMEType: "application/pdf"}},
	}})
	if len(got) != 1 || len(got[0].Parts) != 0 {
		t.Fatalf("file part without payload must be skipped, got %#v", got[0].Parts)
	}
}

// native 路径 Hook 往返：image/file 的内联字节经 data URI 编码后原样还原。
func TestNativeHookPartRoundTripPreservesInlineBytes(t *testing.T) {
	original := []agentruntime.ModelCallMessage{{
		Role: "user",
		ContentParts: []agentruntime.ModelContentPart{
			{Type: "image_url", Base64Data: "SU1H", MIMEType: "image/png", Name: "frame.png"},
			{Type: "file", Base64Data: "RE9D", MIMEType: "application/pdf", Name: "spec.pdf"},
			{Type: "image_url", URL: "artifact://img_1", MIMEType: "image/jpeg"},
		},
	}}
	view := beforeModelMessagesFromModelCall(original)
	if len(view) != 1 || len(view[0].Parts) != 3 {
		t.Fatalf("hook view shape mismatch: %#v", view)
	}
	// 内联字节在 Hook 视图中以 data URI 呈现（Hook 只有 URL 一个载体）。
	if view[0].Parts[0].URL != "data:image/png;base64,SU1H" {
		t.Fatalf("inline image must surface as data URI, got %q", view[0].Parts[0].URL)
	}
	if view[0].Parts[1].Type != "file" || view[0].Parts[1].URL != "data:application/pdf;base64,RE9D" {
		t.Fatalf("inline file must surface as data URI file part: %#v", view[0].Parts[1])
	}
	// artifact 定位符原样保留，不被误编码。
	if view[0].Parts[2].URL != "artifact://img_1" {
		t.Fatalf("artifact locator must pass through, got %q", view[0].Parts[2].URL)
	}

	back := modelCallMessagesFromBeforeModel(view)
	if len(back) != 1 || len(back[0].ContentParts) != 3 {
		t.Fatalf("round trip shape mismatch: %#v", back)
	}
	for i, want := range original[0].ContentParts {
		got := back[0].ContentParts[i]
		if got.Type != want.Type || got.Base64Data != want.Base64Data || got.MIMEType != want.MIMEType || got.URL != want.URL {
			t.Fatalf("part[%d] round trip lost fidelity: got %#v want %#v", i, got, want)
		}
	}
}

// eino 路径 Hook 往返：file part 经 Hook 视图后仍还原为 eino file part。
func TestEinoHookFilePartRoundTrip(t *testing.T) {
	view := []extension.BeforeModelMessage{{
		Role: "user",
		Parts: []extension.BeforeModelPart{
			{Type: "file", URL: "data:application/pdf;base64,RE9D", MIME: "application/pdf", Filename: "spec.pdf"},
		},
	}}
	messages, err := einoMessagesFromBeforeModel(view)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || len(messages[0].UserInputMultiContent) != 1 {
		t.Fatalf("eino message shape mismatch: %#v", messages)
	}
	part := messages[0].UserInputMultiContent[0]
	if part.File == nil {
		t.Fatalf("file part must be restored as an eino file part: %#v", part)
	}
	if part.File.Base64Data == nil || *part.File.Base64Data != "RE9D" {
		t.Fatalf("file payload lost: %#v", part.File)
	}
	if part.File.MIMEType != "application/pdf" || part.File.Name != "spec.pdf" {
		t.Fatalf("file metadata lost: %#v", part.File)
	}

	// 反向：eino file part → Hook 视图仍是 file 类型且携带 data URI。
	againstView := beforeModelMessagesFromEino(messages)
	if len(againstView) != 1 || len(againstView[0].Parts) != 1 {
		t.Fatalf("reverse projection shape mismatch: %#v", againstView)
	}
	if againstView[0].Parts[0].Type != "file" || againstView[0].Parts[0].URL != "data:application/pdf;base64,RE9D" {
		t.Fatalf("reverse projection lost fidelity: %#v", againstView[0].Parts[0])
	}
}
