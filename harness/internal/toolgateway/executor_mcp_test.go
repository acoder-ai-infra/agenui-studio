package toolgateway

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestMCPExecutorPropagatesCapabilityContext(t *testing.T) {
	service := &recordingMCPService{
		result: &MCPToolResult{
			Data:         json.RawMessage(`{"ok":true}`),
			MimeType:     "application/json",
			ResourceRefs: []string{"artifact://tenant-a/resource-1"},
			Debug:        json.RawMessage(`{"request_id":"debug-1"}`),
		},
	}
	executor := NewMCPExecutor(service)
	wantTrace := observability.TraceContext{
		TraceID:   "trace-1",
		SpanID:    "span-mcp-parent",
		TenantID:  "tenant-a",
		UserID:    "user-a",
		SessionID: "sess-1",
		RunID:     "run-1",
		AgentID:   "agent-a",
	}
	ctx := observability.WithTraceContext(context.Background(), wantTrace)
	arguments := json.RawMessage(`{"query":"hotel"}`)

	raw, err := executor.Execute(ctx, &ToolDefinition{
		Name:    "mcp_search",
		Version: "v1",
		Type:    ToolTypeMCP,
		MCP: &MCPToolSpec{
			ServerID:    "server-a",
			SnapshotID:  "snapshot-a",
			MCPToolName: "search",
		},
		Timeout: 2 * time.Second,
	}, ToolCallRequest{
		ToolCallID: "tc-mcp-1",
		ToolName:   "mcp_search",
		TenantID:   "tenant-a",
		UserID:     "user-a",
		SessionID:  "sess-1",
		RunID:      "run-1",
		StepID:     "step-1",
		AgentID:    "agent-a",
		Arguments:  arguments,
		Policy:     ToolCallPolicy{Timeout: 750 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("execute mcp tool: %v", err)
	}
	arguments[0] = '['
	if string(raw.Data) != `{"ok":true}` {
		t.Fatalf("raw data = %s", raw.Data)
	}
	if len(raw.ArtifactRefs) != 1 || raw.ArtifactRefs[0] != "artifact://tenant-a/resource-1" || string(raw.Debug) != `{"request_id":"debug-1"}` {
		t.Fatalf("raw MCP result mapping=%#v", raw)
	}
	if service.resolveCalls != 0 || service.callCalls != 1 {
		t.Fatalf("resolve_calls=%d call_calls=%d, want 0/1", service.resolveCalls, service.callCalls)
	}
	call := service.call
	if call.TenantID != "tenant-a" || call.UserID != "user-a" || call.SessionID != "sess-1" ||
		call.RunID != "run-1" || call.StepID != "step-1" || call.AgentID != "agent-a" || call.ToolCallID != "tc-mcp-1" {
		t.Fatalf("request identity not propagated: %#v", call)
	}
	if call.ServerID != "server-a" || call.SnapshotID != "snapshot-a" || call.ToolName != "search" || call.Timeout != 750*time.Millisecond {
		t.Fatalf("unexpected mcp target/timeout: %#v", call)
	}
	if string(call.Arguments) != `{"query":"hotel"}` {
		t.Fatalf("arguments were not defensively propagated: %s", call.Arguments)
	}
	if call.Trace.TraceID != wantTrace.TraceID || call.Trace.SpanID != wantTrace.SpanID || call.Trace.TenantID != wantTrace.TenantID ||
		call.Trace.UserID != wantTrace.UserID || call.Trace.SessionID != wantTrace.SessionID || call.Trace.RunID != wantTrace.RunID || call.Trace.AgentID != wantTrace.AgentID {
		t.Fatalf("trace context not propagated: %#v", call.Trace)
	}
}

func TestMCPExecutorMapsGenericErrorsToUpstreamError(t *testing.T) {
	executor := NewMCPExecutor(&recordingMCPService{err: errors.New("mcp unavailable")})
	_, err := executor.Execute(context.Background(), &ToolDefinition{
		Name: "mcp_search",
		Type: ToolTypeMCP,
		MCP:  &MCPToolSpec{ServerID: "server-a", SnapshotID: "snapshot-a", MCPToolName: "search"},
	}, ToolCallRequest{ToolCallID: "tc-mcp-1", Arguments: json.RawMessage(`{"query":"hotel"}`)})
	if !IsErrorType(err, ErrorTypeUpstreamError) {
		t.Fatalf("expected upstream_error, got %v", err)
	}
}

type recordingMCPService struct {
	call         MCPToolCallRequest
	result       *MCPToolResult
	err          error
	resolveCalls int
	callCalls    int
}

func (s *recordingMCPService) ResolveSnapshot(context.Context, ResolveMCPSnapshotRequest) (*MCPCapabilitySnapshot, error) {
	s.resolveCalls++
	return nil, nil
}

func (s *recordingMCPService) CallTool(_ context.Context, req MCPToolCallRequest) (*MCPToolResult, error) {
	s.callCalls++
	s.call = req
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}
