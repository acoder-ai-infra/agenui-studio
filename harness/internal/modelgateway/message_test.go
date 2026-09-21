package modelgateway_test

import (
	"encoding/json"
	"testing"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

// 纯文本消息 -> content 为字符串(OpenAI 兼容)。
func TestChatMessageTextMarshal(t *testing.T) {
	b, err := json.Marshal(mg.TextMessage("user", "你好"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"role":"user","content":"你好"}` {
		t.Fatalf("got %s", b)
	}
}

// 多模态消息 -> content 为数组(text + image_url,qwen-vl 同款形状)。
func TestChatMessageMultimodalMarshal(t *testing.T) {
	m := mg.ChatMessage{Role: "user", Parts: []mg.ContentPart{
		{Type: "text", Text: "这张图是什么?"},
		{Type: "image_url", ImageURL: &mg.ImageURL{URL: "https://x/a.png"}},
	}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	// content 必须是数组
	var probe struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("content 应为数组: %v (%s)", err, b)
	}
	if len(probe.Content) != 2 || probe.Content[0]["type"] != "text" || probe.Content[1]["type"] != "image_url" {
		t.Fatalf("multimodal parts 形状不对: %s", b)
	}
}

// round-trip:字符串与数组都能反序列化回 Content / Parts。
func TestChatMessageRoundTrip(t *testing.T) {
	var text mg.ChatMessage
	if err := json.Unmarshal([]byte(`{"role":"user","content":"hi"}`), &text); err != nil {
		t.Fatal(err)
	}
	if text.Content != "hi" || len(text.Parts) != 0 {
		t.Fatalf("text round-trip: %+v", text)
	}
	var mm mg.ChatMessage
	if err := json.Unmarshal([]byte(`{"role":"user","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"u"}}]}`), &mm); err != nil {
		t.Fatal(err)
	}
	if mm.Content != "" || len(mm.Parts) != 2 || mm.Parts[1].ImageURL == nil || mm.Parts[1].ImageURL.URL != "u" {
		t.Fatalf("multimodal round-trip: %+v", mm)
	}
}

func TestChatMessageToolCallRoundTrip(t *testing.T) {
	original := mg.ChatMessage{Role: "assistant", ReasoningContent: "tool reasoning", ToolCalls: []mg.ChatToolCall{{
		ID: "call_1", Type: "function",
		Function: mg.ChatFunctionCall{Name: "harness.echo", Arguments: `{"text":"proof"}`},
	}}}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded mg.ChatMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ReasoningContent != "tool reasoning" || len(decoded.ToolCalls) != 1 || decoded.ToolCalls[0].ID != "call_1" || decoded.ToolCalls[0].Function.Name != "harness.echo" || decoded.ToolCalls[0].Function.Arguments != `{"text":"proof"}` {
		t.Fatalf("tool call round-trip: raw=%s decoded=%+v", raw, decoded)
	}
}
