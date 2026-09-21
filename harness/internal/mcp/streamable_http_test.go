package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStreamableHTTPClientUsesDeclaredToolsAndCallsRemote(t *testing.T) {
	var sawInitialize, sawInitialized, sawCall bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method=%s, want POST", r.Method)
		}
		var req jsonRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		switch req.Method {
		case "initialize":
			sawInitialize = true
			w.Header().Set("Mcp-Session-Id", "session-test")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`))
		case "notifications/initialized":
			sawInitialized = true
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			sawCall = true
			if got := r.Header.Get("Mcp-Session-Id"); got != "session-test" {
				t.Fatalf("Mcp-Session-Id=%q, want session-test", got)
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"structuredContent":{"ok":true,"source":"knowrag"}}}`))
		default:
			t.Fatalf("unexpected method %q", req.Method)
		}
	}))
	defer server.Close()

	client, err := NewStreamableHTTPClient(server.URL, WithStreamableHTTPDeclaredTools([]Tool{{
		Name:        "hybrid_search_projects",
		Description: "search projects",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}))
	if err != nil {
		t.Fatal(err)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "hybrid_search_projects" {
		t.Fatalf("declared tools not returned: %#v", tools)
	}
	result, err := client.CallTool(context.Background(), "hybrid_search_projects", json.RawMessage(`{"concepts":["成长阵地"]}`), CallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Content) != `{"ok":true,"source":"knowrag"}` {
		t.Fatalf("result content=%s", result.Content)
	}
	if !sawInitialize || !sawInitialized || !sawCall {
		t.Fatalf("initialize=%v initialized=%v call=%v", sawInitialize, sawInitialized, sawCall)
	}
}

func TestStreamableHTTPClientUsesConfiguredProtocolVersion(t *testing.T) {
	var gotProtocol string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		switch req.Method {
		case "initialize":
			params := req.Params.(map[string]any)
			gotProtocol, _ = params["protocolVersion"].(string)
			w.Header().Set("Mcp-Session-Id", "session-test")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{}}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`))
		default:
			t.Fatalf("unexpected method %q", req.Method)
		}
	}))
	defer server.Close()

	client, err := NewStreamableHTTPClient(server.URL, WithStreamableHTTPProtocolVersion("2025-06-18"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotProtocol != "2025-06-18" {
		t.Fatalf("protocolVersion=%q", gotProtocol)
	}
}

func TestStreamableHTTPClientParsesEventStreamJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "session-test")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
			return
		}
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message\n"))
		_, _ = w.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"{\\\"ok\\\":true}\"}]}}\n\n"))
	}))
	defer server.Close()

	client, err := NewStreamableHTTPClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.CallTool(context.Background(), "get_developer_project_graph", json.RawMessage(`{"project_name":"demo"}`), CallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Content) != `{"ok":true}` {
		t.Fatalf("event-stream content=%s", result.Content)
	}
}

func TestStreamableHTTPClientInitializedNotificationHasNoID(t *testing.T) {
	var notification map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]any
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		switch raw["method"] {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "session-test")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
		case "notifications/initialized":
			notification = raw
			if got := r.Header.Get("Mcp-Session-Id"); got != "session-test" {
				t.Fatalf("Mcp-Session-Id=%q, want session-test", got)
			}
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`))
		default:
			t.Fatalf("unexpected method %v", raw["method"])
		}
	}))
	defer server.Close()

	client, err := NewStreamableHTTPClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := notification["id"]; ok {
		t.Fatalf("initialized notification must not contain id: %#v", notification)
	}
}
