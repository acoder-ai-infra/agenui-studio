package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

// TestInvokeChatNonStreaming 验证 Streaming=false 时:请求以 stream:false 发出、不带
// SSE Accept 头,响应为单条 chat.completion JSON,适配器把 message.content 归一化为一个
// ChunkToken,usage 归一化为 ChunkUsage。
func TestInvokeChatNonStreaming(t *testing.T) {
	var gotAccept, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":3,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":1},`+
			`"completion_tokens_details":{"reasoning_tokens":4}}}`)
	}))
	defer srv.Close()

	a := New("t", srv.URL, "k")
	stream, err := a.InvokeChat(context.Background(), mg.AdapterRequest{
		Model:     "m",
		Messages:  []mg.ChatMessage{{Role: "user", Content: "hey"}},
		Streaming: false,
	})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	defer stream.Close()

	if strings.Contains(gotAccept, "text/event-stream") {
		t.Fatalf("non-streaming request must not ask for SSE, Accept=%q", gotAccept)
	}
	var reqBody map[string]any
	if err := json.Unmarshal([]byte(gotBody), &reqBody); err != nil {
		t.Fatalf("bad request body %q: %v", gotBody, err)
	}
	if reqBody["stream"] != false {
		t.Fatalf("expected stream:false in body, got %v", reqBody["stream"])
	}

	chunks := drain(t, stream)
	var text string
	var gotUsage bool
	for _, c := range chunks {
		switch c.Kind {
		case mg.ChunkToken:
			text += c.TextDelta
		case mg.ChunkUsage:
			gotUsage = c.Usage != nil && c.Usage.PromptTokens == 3 && c.Usage.CompletionTokens == 2 &&
				c.Usage.CacheReadTokens == 1 && c.Usage.ReasoningTokens == 4
		}
	}
	if text != "hi there" {
		t.Fatalf("expected full content, got %q", text)
	}
	if !gotUsage {
		t.Fatalf("expected usage chunk mapped identically to streaming path, chunks=%+v", chunks)
	}
}

func TestInvokeChatDoesNotSendAnthropicPromptCacheAffinityHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Session-Id")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	stream, err := New("openai", srv.URL, "key").InvokeChat(context.Background(), mg.AdapterRequest{
		Model: "qwen", PromptCacheAffinityKey: "pc1_must-not-leak",
		Messages: []mg.ChatMessage{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_ = drain(t, stream)
	if got != "" {
		t.Fatalf("OpenAI-compatible provider received Anthropic-only X-Session-Id: %q", got)
	}
}

func TestInvokeChatTranslatesCanonicalToolsToOpenAIWireFormat(t *testing.T) {
	var requestBody struct {
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	adapter := New("test", srv.URL, "key")
	stream, err := adapter.InvokeChat(context.Background(), mg.AdapterRequest{
		Model: "model", Streaming: false,
		ToolsSchema: json.RawMessage(`[{"name":"harness.echo","description":"echo text","schema":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}}]`),
	})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	defer stream.Close()
	_ = drain(t, stream)

	if len(requestBody.Tools) != 1 || requestBody.Tools[0].Type != "function" || requestBody.Tools[0].Function.Name != "harness.echo" || requestBody.Tools[0].Function.Description != "echo text" {
		t.Fatalf("canonical tool was not translated: %#v", requestBody.Tools)
	}
	var parameters map[string]any
	if err := json.Unmarshal(requestBody.Tools[0].Function.Parameters, &parameters); err != nil || parameters["type"] != "object" {
		t.Fatalf("tool parameters were not preserved: %s err=%v", requestBody.Tools[0].Function.Parameters, err)
	}
}

func TestInvokeChatSerializesToolLoopMessagesWithProviderContentShape(t *testing.T) {
	var requestBody struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	adapter := New("test", srv.URL, "key")
	stream, err := adapter.InvokeChat(context.Background(), mg.AdapterRequest{
		Model: "model", Streaming: false,
		Messages: []mg.ChatMessage{
			{Role: "system", Content: "rules"},
			{Role: "assistant", ToolCalls: []mg.ChatToolCall{{
				ID: "call_1", Type: "function",
				Function: mg.ChatFunctionCall{Name: "git_clone", Arguments: `{"repo_url":"https://example.test/repo.git"}`},
			}}},
			{Role: "tool", ToolCallID: "call_1", Content: `{"code":"TOOL_CONTROL_REQUIRED"}`},
		},
	})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	defer stream.Close()
	_ = drain(t, stream)

	if len(requestBody.Messages) != 3 {
		t.Fatalf("messages missing: %#v", requestBody.Messages)
	}
	if got := strings.TrimSpace(string(requestBody.Messages[0].Content)); got == "" || got[0] != '"' {
		t.Fatalf("system content must be a string, got %s", got)
	}
	if got := strings.TrimSpace(string(requestBody.Messages[1].Content)); got != "" {
		t.Fatalf("assistant tool_call content should be omitted, got %s", got)
	}
	if got := strings.TrimSpace(string(requestBody.Messages[2].Content)); got == "" || got[0] != '"' {
		t.Fatalf("tool content must be a string, got %s", got)
	}
}

func TestInvokeChatRejectsInvalidCanonicalToolBeforeProviderCall(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()

	adapter := New("test", srv.URL, "key")
	_, err := adapter.InvokeChat(context.Background(), mg.AdapterRequest{
		Model: "model", ToolsSchema: json.RawMessage(`[{"name":"missing-schema"}]`),
	})
	if err == nil || called {
		t.Fatalf("invalid canonical tool must fail before provider call: err=%v called=%v", err, called)
	}
}

func TestInvokeChatSerializesProviderNeutralModelOptions(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()
	temperature, topP, maxTokens := 0.0, 0.8, 128
	adapter := New("test", srv.URL, "key")
	stream, err := adapter.InvokeChat(context.Background(), mg.AdapterRequest{
		Model: "qwen-plus", Options: mg.ModelOptions{
			Temperature: &temperature, TopP: &topP, MaxTokens: &maxTokens,
			Stop: []string{"END"}, ToolChoice: "auto",
			ReasoningMode: mg.ReasoningEnabled, ReasoningBudget: 256, ReasoningEffort: "low",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_ = drain(t, stream)
	if body["temperature"] != 0.0 || body["top_p"] != 0.8 || body["max_tokens"] != float64(128) || body["tool_choice"] != "auto" || body["enable_thinking"] != true || body["thinking_budget"] != float64(256) || body["reasoning_effort"] != "low" {
		t.Fatalf("model options changed at provider boundary: %#v", body)
	}
	stop, ok := body["stop"].([]any)
	if !ok || len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("stop option changed: %#v", body["stop"])
	}
}

func TestInvokeChatMapsDisabledReasoningToQwenEnableThinkingFalse(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	stream, err := New("test", srv.URL, "key").InvokeChat(
		context.Background(),
		mg.AdapterRequest{
			Model: "qwen3.8-max",
			Options: mg.ModelOptions{
				ReasoningMode: mg.ReasoningDisabled,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_ = drain(t, stream)

	enabled, exists := body["enable_thinking"]
	if !exists || enabled != false {
		t.Fatalf("enable_thinking = %#v, exists=%v, want explicit false; body=%#v", enabled, exists, body)
	}
	if _, exists := body["thinking_budget"]; exists {
		t.Fatalf("disabled reasoning unexpectedly sent thinking_budget: %#v", body)
	}
}

// TestInvokeChatNonStreamingNoUsage 验证 usage 缺省时只产出 token,不产出 usage chunk。
func TestInvokeChatNonStreamingNoUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":"solo"}}]}`)
	}))
	defer srv.Close()

	a := New("t", srv.URL, "k")
	stream, err := a.InvokeChat(context.Background(), mg.AdapterRequest{Model: "m", Streaming: false})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	defer stream.Close()

	chunks := drain(t, stream)
	if len(chunks) != 1 || chunks[0].Kind != mg.ChunkToken || chunks[0].TextDelta != "solo" {
		t.Fatalf("expected single token chunk, got %+v", chunks)
	}
}

// TestInvokeChatStreamingStillSSE 验证 Streaming=true 仍走 SSE 解析路径,请求带 SSE
// Accept 头且 stream:true。
func TestInvokeChatStreamingStillSSE(t *testing.T) {
	var gotAccept, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"hi"}}]}`,
			`data: {"choices":[{"delta":{"content":" there"}}]}`,
			`data: [DONE]`,
			``,
		}, "\n"))
	}))
	defer srv.Close()

	a := New("t", srv.URL, "k")
	stream, err := a.InvokeChat(context.Background(), mg.AdapterRequest{Model: "m", Streaming: true})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	defer stream.Close()

	if !strings.Contains(gotAccept, "text/event-stream") {
		t.Fatalf("streaming request must ask for SSE, Accept=%q", gotAccept)
	}
	var reqBody map[string]any
	if err := json.Unmarshal([]byte(gotBody), &reqBody); err != nil {
		t.Fatalf("bad request body %q: %v", gotBody, err)
	}
	if reqBody["stream"] != true {
		t.Fatalf("expected stream:true in body, got %v", reqBody["stream"])
	}
	streamOptions, ok := reqBody["stream_options"].(map[string]any)
	if !ok || streamOptions["include_usage"] != true {
		t.Fatalf("streaming request must ask for provider usage, got %v", reqBody["stream_options"])
	}

	chunks := drain(t, stream)
	var text string
	var gotDone bool
	for _, c := range chunks {
		switch c.Kind {
		case mg.ChunkToken:
			text += c.TextDelta
		case mg.ChunkDone:
			gotDone = true
		}
	}
	if text != "hi there" || !gotDone {
		t.Fatalf("streaming SSE parse broken: text=%q done=%v", text, gotDone)
	}
}
