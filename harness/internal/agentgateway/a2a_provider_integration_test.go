package agentgateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

func TestA2AProviderInvokesDirectMessageWithRemoteInterfaceTenant(t *testing.T) {
	server, target := newA2ATestServer(t, func(method string, params map[string]any) any {
		if method != "SendMessage" {
			t.Fatalf("method = %q, want SendMessage", method)
		}
		if got := params["tenant"]; got != "remote-tenant" {
			t.Fatalf("tenant = %v, want remote-tenant", got)
		}
		message, ok := params["message"].(map[string]any)
		if !ok {
			t.Fatalf("message params type = %T", params["message"])
		}
		metadata, ok := message["metadata"].(map[string]any)
		if !ok || metadata["harness_invocation_id"] != "inv-1" {
			t.Fatalf("invocation metadata = %#v", message["metadata"])
		}
		return map[string]any{"message": map[string]any{
			"messageId": "message-1", "role": "ROLE_AGENT",
			"parts": []any{map[string]any{"text": "direct result"}},
		}}
	})
	defer server.Close()

	provider, err := NewA2AProvider(A2AProviderConfig{})
	if err != nil {
		t.Fatalf("NewA2AProvider() error = %v", err)
	}
	ctx := a2a.AttachTenant(context.Background(), "platform-tenant-must-not-propagate")
	result, err := provider.Invoke(ctx, target, RemoteInvocationRequest{InvocationID: "inv-1", Description: "book a room"}, nil)
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if result.Content != "direct result" || result.TaskID != "" {
		t.Fatalf("Invoke() result = %#v", result)
	}
}

func TestA2AProviderResultLimitRemainsA2AOwnedAndConfigurable(t *testing.T) {
	provider, err := NewA2AProvider(A2AProviderConfig{})
	if err != nil {
		t.Fatalf("NewA2AProvider: %v", err)
	}
	if provider.maxResultBytes != DefaultA2AMaxResultBytes || DefaultA2AMaxResultBytes != 64*1024 {
		t.Fatalf("default result limit=%d want=%d", provider.maxResultBytes, 64*1024)
	}

	provider, err = NewA2AProvider(A2AProviderConfig{MaxResultBytes: 7})
	if err != nil {
		t.Fatalf("NewA2AProvider custom: %v", err)
	}
	message := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("12345678"))
	if _, err := messageResult(message, provider.maxResultBytes); !errors.Is(err, &GatewayError{Code: CodeRemoteResultTooLarge}) {
		t.Fatalf("messageResult error=%v want %s", err, CodeRemoteResultTooLarge)
	}
}

func TestA2AProviderPollsTaskToTerminalAndEmitsLifecycle(t *testing.T) {
	var getCalls atomic.Int32
	server, target := newA2ATestServer(t, func(method string, params map[string]any) any {
		switch method {
		case "SendMessage":
			return map[string]any{"task": a2aTaskJSON("TASK_STATE_SUBMITTED", false)}
		case "GetTask":
			getCalls.Add(1)
			return a2aTaskJSON("TASK_STATE_COMPLETED", true)
		default:
			t.Fatalf("unexpected method %q", method)
			return nil
		}
	})
	defer server.Close()

	provider, err := NewA2AProvider(A2AProviderConfig{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatalf("NewA2AProvider() error = %v", err)
	}
	updates := &recordingRemoteUpdates{}
	result, err := provider.Invoke(context.Background(), target, RemoteInvocationRequest{InvocationID: "inv-2", Description: "plan"}, updates)
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if getCalls.Load() != 1 || result.Content != "task result" || result.TaskID != "task-1" || result.ContextID != "context-1" {
		t.Fatalf("result = %#v, get calls = %d", result, getCalls.Load())
	}
	want := []RemoteTaskUpdateKind{RemoteTaskCreated, RemoteTaskProgress, RemoteTaskTerminal}
	got := updates.kinds()
	if len(got) != len(want) {
		t.Fatalf("updates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("updates = %v, want %v", got, want)
		}
	}
	if updates.last().Terminal != RemoteTerminalCompleted {
		t.Fatalf("terminal update = %#v", updates.last())
	}
}

func TestA2AProviderCancelsTaskThatRequiresInput(t *testing.T) {
	var cancelCalls atomic.Int32
	server, target := newA2ATestServer(t, func(method string, params map[string]any) any {
		switch method {
		case "SendMessage":
			return map[string]any{"task": a2aTaskJSON("TASK_STATE_INPUT_REQUIRED", false)}
		case "CancelTask":
			cancelCalls.Add(1)
			return a2aTaskJSON("TASK_STATE_CANCELED", false)
		default:
			t.Fatalf("unexpected method %q", method)
			return nil
		}
	})
	defer server.Close()

	provider, err := NewA2AProvider(A2AProviderConfig{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatalf("NewA2AProvider() error = %v", err)
	}
	_, err = provider.Invoke(context.Background(), target, RemoteInvocationRequest{InvocationID: "inv-3", Description: "needs input"}, nil)
	if !errors.Is(err, &GatewayError{Code: CodeRemoteInputRequired}) {
		t.Fatalf("Invoke() error = %v, want %s", err, CodeRemoteInputRequired)
	}
	if cancelCalls.Load() != 1 {
		t.Fatalf("CancelTask calls = %d, want 1", cancelCalls.Load())
	}
}

func newA2ATestServer(t *testing.T, rpc func(method string, params map[string]any) any) (*httptest.Server, RemoteAgent) {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == defaultAgentCardPath {
			_ = json.NewEncoder(w).Encode(a2a.AgentCard{
				Name: "test-agent", Description: "test", Version: "1",
				SupportedInterfaces: []*a2a.AgentInterface{{
					URL: server.URL + "/rpc", ProtocolBinding: a2a.TransportProtocolJSONRPC,
					ProtocolVersion: a2a.Version, Tenant: "remote-tenant",
				}},
				DefaultInputModes: []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"},
			})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/rpc" {
			http.NotFound(w, r)
			return
		}
		var request struct {
			JSONRPC string         `json:"jsonrpc"`
			ID      string         `json:"id"`
			Method  string         `json:"method"`
			Params  map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode JSON-RPC request: %v", err)
		}
		if request.JSONRPC != "2.0" {
			t.Fatalf("jsonrpc = %q", request.JSONRPC)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": request.ID, "result": rpc(request.Method, request.Params),
		})
	}))
	target := RemoteAgent{
		AgentRef: "remote-agent", Protocol: A2AProtocol, BaseURL: server.URL,
		Transport: A2AJSONRPCTransport, Timeout: time.Second,
	}
	return server, target
}

func a2aTaskJSON(state string, withArtifact bool) map[string]any {
	task := map[string]any{
		"id": "task-1", "contextId": "context-1", "status": map[string]any{"state": state},
	}
	if withArtifact {
		task["artifacts"] = []any{map[string]any{
			"artifactId": "artifact-1", "parts": []any{map[string]any{"text": "task result"}},
		}}
	}
	return task
}

type recordingRemoteUpdates struct {
	mu      sync.Mutex
	updates []RemoteTaskUpdate
}

func (r *recordingRemoteUpdates) Emit(_ context.Context, update RemoteTaskUpdate) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates = append(r.updates, update)
	return nil
}

func (r *recordingRemoteUpdates) kinds() []RemoteTaskUpdateKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RemoteTaskUpdateKind, len(r.updates))
	for i := range r.updates {
		out[i] = r.updates[i].Kind
	}
	return out
}

func (r *recordingRemoteUpdates) last() RemoteTaskUpdate {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.updates[len(r.updates)-1]
}
