package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

func TestInvokeChatMapsCanonicalRequestAndNonStreamResponse(t *testing.T) {
	var gotPath string
	var gotHeaders http.Header
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHeaders = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet",
			"content":[
				{"type":"thinking","thinking":"inspect","signature":"sig-response"},
				{"type":"text","text":"done"},
				{"type":"tool_use","id":"tool_2","name":"harness.echo","input":{"text":"done"}}
			],
			"usage":{"input_tokens":11,"output_tokens":7,"cache_creation_input_tokens":3,"cache_read_input_tokens":5}
		}`)
	}))
	defer server.Close()

	maxTokens := 4096
	temperature := 0.2
	adapter := New("claude", server.URL, "secret")
	stream, err := adapter.InvokeChat(context.Background(), mg.AdapterRequest{
		Model: "claude-sonnet", Streaming: false,
		Messages: []mg.ChatMessage{
			{Role: "system", Content: "system rules"},
			{Role: "user", Content: "hello"},
			{Role: "assistant", ReasoningContent: "inspect", ReasoningSignature: "sig-request", ToolCalls: []mg.ChatToolCall{{
				ID: "tool_1", Type: "function", Function: mg.ChatFunctionCall{Name: "harness.echo", Arguments: `{"text":"hello"}`},
			}}},
			{Role: "tool", ToolCallID: "tool_1", Content: "hello"},
		},
		ToolsSchema: json.RawMessage(`[{"name":"harness.echo","description":"echo text","schema":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}}]`),
		Options: mg.ModelOptions{
			Temperature: &temperature, MaxTokens: &maxTokens, Stop: []string{"END"}, ToolChoice: "auto",
			ReasoningMode: mg.ReasoningEnabled, ReasoningBudget: 2048,
		},
	})
	if err != nil {
		t.Fatalf("InvokeChat: %v", err)
	}
	defer stream.Close()

	if gotPath != "/v1/messages" {
		t.Fatalf("path = %q, want /v1/messages", gotPath)
	}
	if gotHeaders.Get("x-api-key") != "secret" || gotHeaders.Get("anthropic-version") == "" {
		t.Fatalf("anthropic headers missing: %#v", gotHeaders)
	}
	if gotHeaders.Get("accept") != "application/json" {
		t.Fatalf("accept = %q", gotHeaders.Get("accept"))
	}
	assertMappedRequest(t, gotBody)

	chunks := drain(t, stream)
	assertResponseChunks(t, chunks)
}

func TestConvertToolsFlattensTopLevelObjectOneOf(t *testing.T) {
	raw := json.RawMessage(`[{"name":"workspace","schema":{"type":"object","x-harness-discriminator":"action","oneOf":[{"type":"object","required":["action","title"],"properties":{"action":{"const":"begin"},"title":{"type":"string"}},"additionalProperties":false},{"type":"object","required":["action","revision"],"properties":{"action":{"const":"commit"},"revision":{"type":"string"}},"additionalProperties":false}]}}]`)

	tools, err := convertTools(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}
	var schema map[string]any
	if err := json.Unmarshal(tools[0].InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["type"] != "object" || schema["oneOf"] != nil || schema["anyOf"] != nil || schema["allOf"] != nil {
		t.Fatalf("top-level schema was not flattened: %#v", schema)
	}
	required, _ := schema["required"].([]any)
	if len(required) != 1 || required[0] != "action" {
		t.Fatalf("required = %#v, want shared action", required)
	}
	properties := schema["properties"].(map[string]any)
	action := properties["action"].(map[string]any)
	if !reflect.DeepEqual(action["enum"], []any{"begin", "commit"}) {
		t.Fatalf("action schema = %#v", action)
	}
	description, _ := schema["description"].(string)
	if !strings.Contains(description, "action=begin requires title") || !strings.Contains(description, "action=commit requires revision") {
		t.Fatalf("operation requirements missing from description: %q", description)
	}
	title := properties["title"].(map[string]any)
	if !strings.Contains(title["description"].(string), "Required when action is one of: begin") {
		t.Fatalf("title action guidance = %#v", title)
	}
}

func TestInvokeChatSendsPromptCacheAffinityOnlyWhenExplicitlyEnabled(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		enabled bool
		key     string
		want    string
	}{
		{name: "disabled even when key is present", baseURL: "http://localhost/open_api/anthropic", key: "pc1_must-not-leak"},
		{name: "controlled endpoint enabled with key", baseURL: "http://localhost/open_api/anthropic", enabled: true, key: "pc1_test-affinity", want: "pc1_test-affinity"},
		{name: "controlled endpoint enabled without key", baseURL: "http://localhost/open_api/anthropic", enabled: true},
		{name: "public endpoint mislabeled enabled", baseURL: "https://api.anthropic.com", enabled: true, key: "pc1_must-not-leak"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got string
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				got = r.Header.Get("X-Session-Id")
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"content":[{"type":"text","text":"ok"}]}`)),
					Request:    r,
				}, nil
			})}

			var options []Option
			if test.enabled {
				options = append(options, WithPromptCacheSessionAffinity())
			}
			adapter := New("claude", test.baseURL, "secret", options...)
			adapter.client = client
			stream, err := adapter.InvokeChat(context.Background(), mg.AdapterRequest{
				Model: "claude-sonnet", PromptCacheAffinityKey: test.key,
				Messages: []mg.ChatMessage{{Role: "user", Content: "hello"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			_ = drain(t, stream)
			if got != test.want {
				t.Fatalf("X-Session-Id = %q, want %q", got, test.want)
			}
		})
	}
}

func TestPromptCacheAffinityRequestDoesNotFollowRedirect(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{
			StatusCode: http.StatusTemporaryRedirect,
			Header:     http.Header{"Location": []string{"https://api.anthropic.com/v1/messages"}},
			Body:       io.NopCloser(strings.NewReader("redirect")),
			Request:    request,
		}, nil
	})}
	adapter := New("claude", "http://localhost/open_api/anthropic", "secret", WithPromptCacheSessionAffinity())
	adapter.client = client
	if _, err := adapter.InvokeChat(context.Background(), mg.AdapterRequest{
		Model: "claude-sonnet", PromptCacheAffinityKey: "pc1_secret-routing-key",
		Messages: []mg.ChatMessage{{Role: "user", Content: "hello"}},
	}); err == nil {
		t.Fatal("redirect response must fail instead of being followed")
	}
	if requests != 1 {
		t.Fatalf("affinity request followed redirect: requests=%d", requests)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestInvokeChatMapsAdaptiveThinkingEffortForOpus(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}]}`)
	}))
	defer server.Close()

	temperature := 0.3
	adapter := New("claude", server.URL+"/v1", "secret")
	stream, err := adapter.InvokeChat(context.Background(), mg.AdapterRequest{
		Model: "claude-opus-4-7", Streaming: false,
		Messages: []mg.ChatMessage{{Role: "user", Content: "hello"}},
		Options:  mg.ModelOptions{Temperature: &temperature, ReasoningMode: mg.ReasoningEnabled, ReasoningEffort: "low"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_ = drain(t, stream)

	thinking, _ := body["thinking"].(map[string]any)
	output, _ := body["output_config"].(map[string]any)
	if thinking["type"] != "adaptive" || output["effort"] != "low" {
		t.Fatalf("adaptive thinking options = thinking:%#v output:%#v", thinking, output)
	}
	if _, exists := body["temperature"]; exists {
		t.Fatalf("opus adaptive thinking must omit temperature: %#v", body)
	}
}

func TestInvokeChatMapsDisabledReasoningToAnthropicThinkingDisabled(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}]}`)
	}))
	defer server.Close()

	stream, err := New("qwen", server.URL, "secret").InvokeChat(context.Background(), mg.AdapterRequest{
		Model:    "qwen3.8-max",
		Messages: []mg.ChatMessage{{Role: "user", Content: "hello"}},
		Options: mg.ModelOptions{
			ReasoningMode:   mg.ReasoningDisabled,
			ReasoningBudget: 2048,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_ = drain(t, stream)

	thinking, ok := body["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Fatalf("thinking = %#v, want explicit disabled; body=%#v", body["thinking"], body)
	}
	if _, exists := thinking["budget_tokens"]; exists {
		t.Fatalf("disabled reasoning unexpectedly sent budget_tokens: %#v", thinking)
	}
	if _, exists := body["output_config"]; exists {
		t.Fatalf("disabled reasoning unexpectedly sent output_config: %#v", body)
	}
}

func TestStreamNormalizesAnthropicEvents(t *testing.T) {
	body := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_creation_input_tokens":2,"cache_read_input_tokens":4}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-stream"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tool_1","name":"harness.echo","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"text\":"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"hello\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"done"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":6}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	chunks := drain(t, newStream(io.NopCloser(strings.NewReader(body))))
	var thought, signature, text, args string
	var toolID, toolName string
	var usage *mg.ModelUsage
	var done bool
	for _, chunk := range chunks {
		switch chunk.Kind {
		case mg.ChunkThought:
			thought += chunk.ThoughtDelta
			if chunk.ReasoningSignature != "" {
				signature = chunk.ReasoningSignature
			}
		case mg.ChunkToolCall:
			if chunk.ToolCallDelta.ToolCallID != "" {
				toolID = chunk.ToolCallDelta.ToolCallID
			}
			if chunk.ToolCallDelta.Name != "" {
				toolName = chunk.ToolCallDelta.Name
			}
			args += chunk.ToolCallDelta.ArgumentsDelta
		case mg.ChunkToken:
			text += chunk.TextDelta
		case mg.ChunkUsage:
			usage = chunk.Usage
		case mg.ChunkDone:
			done = true
		}
	}
	if thought != "plan" || signature != "sig-stream" || text != "done" || toolID != "tool_1" || toolName != "harness.echo" || args != `{"text":"hello"}` {
		t.Fatalf("normalized stream mismatch: thought=%q signature=%q text=%q tool=%q/%q args=%q", thought, signature, text, toolID, toolName, args)
	}
	if usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 6 || usage.CacheReadTokens != 4 || usage.CacheWriteTokens != 2 || !done {
		t.Fatalf("usage=%#v done=%v", usage, done)
	}
}

func TestStreamPreservesEmptyToolInputFromBlockStart(t *testing.T) {
	body := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tool_empty","name":"list_developer_operators","input":{}}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	chunks := drain(t, newStream(io.NopCloser(strings.NewReader(body))))
	var toolID, toolName, arguments string
	for _, chunk := range chunks {
		if chunk.Kind != mg.ChunkToolCall || chunk.ToolCallDelta == nil {
			continue
		}
		if chunk.ToolCallDelta.ToolCallID != "" {
			toolID = chunk.ToolCallDelta.ToolCallID
		}
		if chunk.ToolCallDelta.Name != "" {
			toolName = chunk.ToolCallDelta.Name
		}
		arguments += chunk.ToolCallDelta.ArgumentsDelta
	}
	if toolID != "tool_empty" || toolName != "list_developer_operators" || arguments != `{}` {
		t.Fatalf("empty tool input was lost: id=%q name=%q arguments=%q chunks=%#v", toolID, toolName, arguments, chunks)
	}
}

func TestToolSchemaCompilerPreservesOptionalEmptyObjectInput(t *testing.T) {
	compiled, err := NewToolSchemaCompiler().Compile(json.RawMessage(`{
		"type":"object",
		"properties":{"page_no":{"type":"integer"},"page_size":{"type":"integer"}},
		"additionalProperties":false
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(compiled, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["type"] != "object" || schema["required"] != nil || schema["additionalProperties"] != false {
		t.Fatalf("optional object schema changed during Anthropic compilation: %s", compiled)
	}
}

func TestInvokeChatClassifiesHTTPAndStreamErrors(t *testing.T) {
	t.Run("http rate limit", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
		}))
		defer server.Close()
		_, err := New("claude", server.URL, "secret").InvokeChat(context.Background(), mg.AdapterRequest{
			Model: "m", Messages: []mg.ChatMessage{{Role: "user", Content: "hello"}},
		})
		var adapterErr *mg.AdapterError
		if !errors.As(err, &adapterErr) || adapterErr.Class != mg.ErrorRateLimited || !adapterErr.Retryable || adapterErr.HTTPStatus != http.StatusTooManyRequests {
			t.Fatalf("error = %#v", err)
		}
	})

	t.Run("stream overload", func(t *testing.T) {
		body := "data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n"
		chunks := drain(t, newStream(io.NopCloser(strings.NewReader(body))))
		if len(chunks) != 1 || chunks[0].Kind != mg.ChunkError || chunks[0].Err == nil || chunks[0].Err.Class != mg.ErrorProvider5xx || !chunks[0].Err.Retryable {
			t.Fatalf("chunks = %#v", chunks)
		}
	})
}

func TestInvokeChatFailsClosedBeforeProviderForInvalidInput(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()
	adapter := New("claude", server.URL, "secret")

	tests := []mg.AdapterRequest{
		{Model: "m", ToolsSchema: json.RawMessage(`[{"name":"missing-schema"}]`)},
		{Model: "m", Messages: []mg.ChatMessage{{Role: "tool", Content: "result"}}},
		{Model: "m", Messages: []mg.ChatMessage{{Role: "user", Parts: []mg.ContentPart{{Type: "input_audio", InputAudio: &mg.InputAudio{Data: "AA==", Format: "wav"}}}}}},
	}
	for _, request := range tests {
		if _, err := adapter.InvokeChat(context.Background(), request); err == nil {
			t.Fatalf("request should fail closed: %#v", request)
		}
	}
	if called {
		t.Fatal("provider was called for invalid input")
	}
}

func TestStreamHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := newStream(io.NopCloser(strings.NewReader(""))).Next(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Next error = %v", err)
	}
}

func TestInvokeChatClassifiesCanceledRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New("claude", server.URL, "secret").InvokeChat(ctx, mg.AdapterRequest{
		Model: "m", Messages: []mg.ChatMessage{{Role: "user", Content: "hello"}},
	})
	var adapterErr *mg.AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Class != mg.ErrorStreamInterrupted {
		t.Fatalf("canceled request error = %#v", err)
	}
}

func TestStreamFailsClosedOnMalformedOrTruncatedSSE(t *testing.T) {
	t.Run("malformed event", func(t *testing.T) {
		stream := newStream(io.NopCloser(strings.NewReader("data: {bad-json}\n\n")))
		_, err := stream.Next(context.Background())
		var adapterErr *mg.AdapterError
		if !errors.As(err, &adapterErr) || adapterErr.Class != mg.ErrorStreamInterrupted || !adapterErr.Retryable {
			t.Fatalf("malformed stream error = %#v", err)
		}
	})

	t.Run("missing message stop", func(t *testing.T) {
		stream := newStream(io.NopCloser(strings.NewReader("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n")))
		chunk, err := stream.Next(context.Background())
		if err != nil || chunk.Kind != mg.ChunkToken || chunk.TextDelta != "partial" {
			t.Fatalf("first chunk = %#v err=%v", chunk, err)
		}
		_, err = stream.Next(context.Background())
		var adapterErr *mg.AdapterError
		if !errors.As(err, &adapterErr) || adapterErr.Class != mg.ErrorStreamInterrupted || !adapterErr.Retryable {
			t.Fatalf("truncated stream error = %#v", err)
		}
	})
}

func assertMappedRequest(t *testing.T, body map[string]any) {
	t.Helper()
	if body["model"] != "claude-sonnet" || body["stream"] != false || body["max_tokens"] != float64(4096) || body["temperature"] != float64(1) {
		t.Fatalf("base options mismatch: %#v", body)
	}
	thinking, _ := body["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(2048) {
		t.Fatalf("thinking = %#v", thinking)
	}
	system, _ := body["system"].([]any)
	if len(system) != 1 || system[0].(map[string]any)["text"] != "system rules" {
		t.Fatalf("system = %#v", system)
	}
	messages, _ := body["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %#v", messages)
	}
	assistant := messages[1].(map[string]any)["content"].([]any)
	if assistant[0].(map[string]any)["type"] != "thinking" || assistant[0].(map[string]any)["signature"] != "sig-request" || assistant[1].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("assistant blocks = %#v", assistant)
	}
	toolResult := messages[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if messages[2].(map[string]any)["role"] != "user" || toolResult["type"] != "tool_result" || toolResult["tool_use_id"] != "tool_1" {
		t.Fatalf("tool result = %#v", messages[2])
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "harness.echo" || tools[0].(map[string]any)["input_schema"].(map[string]any)["type"] != "object" {
		t.Fatalf("tools = %#v", tools)
	}
}

func assertResponseChunks(t *testing.T, chunks []mg.NormalizedChunk) {
	t.Helper()
	var text, thought, signature string
	var tool *mg.ToolCallDelta
	var usage *mg.ModelUsage
	for _, chunk := range chunks {
		switch chunk.Kind {
		case mg.ChunkToken:
			text += chunk.TextDelta
		case mg.ChunkThought:
			thought += chunk.ThoughtDelta
			if chunk.ReasoningSignature != "" {
				signature = chunk.ReasoningSignature
			}
		case mg.ChunkToolCall:
			tool = chunk.ToolCallDelta
		case mg.ChunkUsage:
			usage = chunk.Usage
		}
	}
	if text != "done" || thought != "inspect" || signature != "sig-response" || tool == nil || tool.ToolCallID != "tool_2" || tool.Name != "harness.echo" || tool.ArgumentsDelta != `{"text":"done"}` {
		t.Fatalf("response chunks = %#v", chunks)
	}
	if usage == nil || usage.PromptTokens != 11 || usage.CompletionTokens != 7 || usage.CacheWriteTokens != 3 || usage.CacheReadTokens != 5 || usage.Source != mg.UsageSourceGateway {
		t.Fatalf("usage = %#v", usage)
	}
}

func drain(t *testing.T, stream mg.AdapterStream) []mg.NormalizedChunk {
	t.Helper()
	var chunks []mg.NormalizedChunk
	for {
		chunk, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return chunks
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		chunks = append(chunks, chunk)
	}
}
