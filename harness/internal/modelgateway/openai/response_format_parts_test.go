package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

// response_format_parts_test.go 覆盖 G-A/G-B/G-C 的 provider 请求体序列化：
// response_format、图片 part 的 detail、非图片文件 file content part。

// G-A：ResponseFormat 非空时按 OpenAI 兼容形状序列化；空值不下发。
func TestResponseFormatSerializedWhenSet(t *testing.T) {
	body := captureRequestBody(t, mg.ModelOptions{ResponseFormat: "json_object"})
	format, ok := body["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format must be an object, got %v", body["response_format"])
	}
	if format["type"] != "json_object" {
		t.Fatalf("response_format.type = %v, want json_object", format["type"])
	}
}

func TestResponseFormatOmittedWhenEmpty(t *testing.T) {
	body := captureRequestBody(t, mg.ModelOptions{})
	if _, ok := body["response_format"]; ok {
		t.Fatal("empty ResponseFormat must not emit the provider parameter")
	}
}

// captureMultiPartRequestBody 发送一条带多模态 Part 的消息并回收请求体。
func captureMultiPartRequestBody(t *testing.T, options mg.ModelOptions, parts []mg.ContentPart) map[string]any {
	t.Helper()
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	a := New("t", srv.URL, "k")
	stream, err := a.InvokeChat(context.Background(), mg.AdapterRequest{
		Model:    "m",
		Messages: []mg.ChatMessage{{Role: "user", Parts: parts}},
		Options:  options,
	})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	stream.Close()

	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("bad request body %q: %v", gotBody, err)
	}
	return body
}

// firstMessageParts 取请求体首条消息的 content parts。
func firstMessageParts(t *testing.T, body map[string]any) []any {
	t.Helper()
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("messages missing in %v", body)
	}
	message, ok := messages[0].(map[string]any)
	if !ok {
		t.Fatalf("message[0] must be an object, got %v", messages[0])
	}
	parts, ok := message["content"].([]any)
	if !ok {
		t.Fatalf("message content must be parts array, got %v", message["content"])
	}
	return parts
}

// G-B：ImageDetail 非空时写入每个 image_url part 的 detail；空值不写。
func TestImageDetailSerializedWhenSet(t *testing.T) {
	parts := []mg.ContentPart{{Type: "image_url", ImageURL: &mg.ImageURL{URL: "data:image/png;base64,AAA"}}}
	body := captureMultiPartRequestBody(t, mg.ModelOptions{ImageDetail: "low"}, parts)
	got := firstMessageParts(t, body)
	image, ok := got[0].(map[string]any)["image_url"].(map[string]any)
	if !ok {
		t.Fatalf("image_url object missing in %v", got[0])
	}
	if image["detail"] != "low" {
		t.Fatalf("image detail = %v, want low", image["detail"])
	}
	// 源 Part 不得被就地改写（adapter 逐 part 重建 ImageURL）。
	if parts[0].ImageURL.Detail != "" {
		t.Fatalf("adapter must not mutate the caller's part, got %q", parts[0].ImageURL.Detail)
	}
}

func TestImageDetailOmittedWhenEmpty(t *testing.T) {
	parts := []mg.ContentPart{{Type: "image_url", ImageURL: &mg.ImageURL{URL: "data:image/png;base64,AAA"}}}
	body := captureMultiPartRequestBody(t, mg.ModelOptions{}, parts)
	image, ok := firstMessageParts(t, body)[0].(map[string]any)["image_url"].(map[string]any)
	if !ok {
		t.Fatalf("image_url object missing")
	}
	if _, present := image["detail"]; present {
		t.Fatal("empty ImageDetail must not emit the detail field")
	}
}

// G-C：非图片文件序列化为 file content part（file_data 承载 data URI）。
func TestFilePartSerialized(t *testing.T) {
	parts := []mg.ContentPart{{Type: "file", File: &mg.FilePart{
		FileData: "data:application/pdf;base64,QUJD", Filename: "report.pdf",
	}}}
	body := captureMultiPartRequestBody(t, mg.ModelOptions{}, parts)
	got := firstMessageParts(t, body)
	part, ok := got[0].(map[string]any)
	if !ok {
		t.Fatalf("part[0] must be an object, got %v", got[0])
	}
	if part["type"] != "file" {
		t.Fatalf("part type = %v, want file", part["type"])
	}
	file, ok := part["file"].(map[string]any)
	if !ok {
		t.Fatalf("file object missing in %v", part)
	}
	if file["file_data"] != "data:application/pdf;base64,QUJD" {
		t.Fatalf("file_data = %v", file["file_data"])
	}
	if file["filename"] != "report.pdf" {
		t.Fatalf("filename = %v", file["filename"])
	}
}
