package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

func TestOpenAIToolsFlattensTopLevelDiscriminatedObjectUnion(t *testing.T) {
	raw := json.RawMessage(`[{"name":"workspace","schema":{"type":"object","x-harness-discriminator":"action","oneOf":[{"type":"object","required":["action","title"],"properties":{"action":{"const":"begin"},"title":{"type":"string"}},"additionalProperties":false},{"type":"object","required":["action","revision"],"properties":{"action":{"const":"commit"},"revision":{"type":"string"}},"additionalProperties":false}]}}]`)

	tools, err := openAITools(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Type != "function" || tools[0].Function.Name != "workspace" {
		t.Fatalf("tools = %#v", tools)
	}
	var parameters map[string]any
	if err := json.Unmarshal(tools[0].Function.Parameters, &parameters); err != nil {
		t.Fatal(err)
	}
	for _, keyword := range []string{"oneOf", "anyOf", "allOf", "enum", "const", "not", "x-harness-discriminator"} {
		if _, exists := parameters[keyword]; exists {
			t.Fatalf("function.parameters retains forbidden root keyword %q: %#v", keyword, parameters)
		}
	}
	if parameters["type"] != "object" {
		t.Fatalf("function.parameters = %#v", parameters)
	}
	required, _ := parameters["required"].([]any)
	if !reflect.DeepEqual(required, []any{"action"}) {
		t.Fatalf("required = %#v, want action", required)
	}
	properties := parameters["properties"].(map[string]any)
	action := properties["action"].(map[string]any)
	if !reflect.DeepEqual(action["enum"], []any{"begin", "commit"}) {
		t.Fatalf("action schema = %#v", action)
	}
	if description, _ := parameters["description"].(string); !strings.Contains(description, "action=commit requires revision") {
		t.Fatalf("description = %q", description)
	}
}

func TestAdapterLearnsRestrictedToolSchemaOnlyAfterProviderRejection(t *testing.T) {
	canonical := json.RawMessage(`[{"name":"workspace","schema":{"type":"object","x-harness-discriminator":"action","oneOf":[{"type":"object","required":["action","slots"],"properties":{"action":{"const":"set_slots"},"slots":{"type":"array"}},"additionalProperties":false},{"type":"object","required":["action","revision"],"properties":{"action":{"const":"commit"},"revision":{"type":"string"}},"additionalProperties":false}]}}]`)
	var received []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		received = append(received, body)
		parameters := requestToolParameters(t, body)
		if len(received) == 1 {
			if _, exists := parameters["oneOf"]; !exists {
				t.Fatalf("first request did not preserve canonical union: %#v", parameters)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Invalid schema for function 'workspace': oneOf is not permitted at the top level","param":"tools[0].function.parameters","code":"invalid_function_parameters"}}`)
			return
		}
		if _, exists := parameters["oneOf"]; exists {
			t.Fatalf("restricted request retained oneOf: %#v", parameters)
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer server.Close()

	adapter := New("test", server.URL, "secret")
	request := mg.AdapterRequest{
		Model: "restricted-model", Streaming: false,
		Messages:    []mg.ChatMessage{{Role: "user", Content: "hello"}},
		ToolsSchema: canonical,
	}
	stream, err := adapter.InvokeChat(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	_ = drain(t, stream)
	_ = stream.Close()
	if len(received) != 2 {
		t.Fatalf("requests = %d, want identity probe plus restricted retry", len(received))
	}

	stream, err = adapter.InvokeChat(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	_ = drain(t, stream)
	_ = stream.Close()
	if len(received) != 3 {
		t.Fatalf("cached restricted profile made %d requests, want 3 total", len(received))
	}
	if _, exists := requestToolParameters(t, received[2])["oneOf"]; exists {
		t.Fatal("cached restricted profile regressed to the canonical union")
	}
}

func TestAdapterDetectsToolSchemaRejectionTunneledAsHTTP200Text(t *testing.T) {
	canonical := json.RawMessage(`[{"name":"workspace","schema":{"type":"object","x-harness-discriminator":"action","oneOf":[{"type":"object","required":["action","slots"],"properties":{"action":{"const":"set_slots"},"slots":{"type":"array"}},"additionalProperties":false},{"type":"object","required":["action","revision"],"properties":{"action":{"const":"commit"},"revision":{"type":"string"}},"additionalProperties":false}]}}]`)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		parameters := requestToolParameters(t, body)
		if requests == 1 {
			if _, exists := parameters["oneOf"]; !exists {
				t.Fatal("identity probe did not preserve oneOf")
			}
			message := `{"error":{"message":"Invalid schema for function 'workspace': oneOf is not permitted at the top level","param":"tools[0].function.parameters","code":"invalid_function_parameters"}}`
			encoded, _ := json.Marshal(message)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":`+string(encoded)+`}}]}`)
			return
		}
		if _, exists := parameters["oneOf"]; exists {
			t.Fatal("restricted retry retained oneOf")
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer server.Close()

	adapter := New("test", server.URL, "secret")
	stream, err := adapter.InvokeChat(context.Background(), mg.AdapterRequest{
		Model: "tunneled-error-model", Streaming: false,
		Messages: []mg.ChatMessage{{Role: "user", Content: "hello"}}, ToolsSchema: canonical,
	})
	if err != nil {
		t.Fatal(err)
	}
	chunks := drain(t, stream)
	_ = stream.Close()
	var text strings.Builder
	for _, chunk := range chunks {
		text.WriteString(chunk.TextDelta)
	}
	if requests != 2 || text.String() != "ok" {
		t.Fatalf("requests=%d text=%q chunks=%#v", requests, text.String(), chunks)
	}
	if adapter.toolSchemaProfile("tunneled-error-model") != toolSchemaProfileRestricted {
		t.Fatal("tunneled schema rejection was not cached as restricted")
	}
}

func requestToolParameters(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", body["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	function, _ := tool["function"].(map[string]any)
	parameters, _ := function["parameters"].(map[string]any)
	if parameters == nil {
		t.Fatalf("parameters = %#v", function["parameters"])
	}
	return parameters
}
