package testkit

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

// MCPCall records one MockMCP.CallTool invocation for post-test assertions.
type MCPCall struct {
	Server    string
	Tool      string
	Arguments json.RawMessage
}

// MCPResponse scripts one CallTool return value.
type MCPResponse struct {
	Result   json.RawMessage
	Content  string
	FailWith error
}

// MockMCP is a lightweight scripted stand-in for a Model Context Protocol
// server. It records every CallTool invocation and pops scripted responses in
// FIFO order.
type MockMCP struct {
	server  string
	mu      sync.Mutex
	scripts map[string][]MCPResponse
	calls   []MCPCall
}

// ErrMCPScriptExhausted is returned when a tool has no scripts left.
var ErrMCPScriptExhausted = errors.New("testkit: mcp script queue exhausted")

// NewMockMCP builds a fresh MCP mock scoped to serverName.
func NewMockMCP(serverName string) *MockMCP {
	return &MockMCP{
		server:  serverName,
		scripts: make(map[string][]MCPResponse),
	}
}

// Enqueue appends more scripted responses for a given tool.
func (m *MockMCP) Enqueue(tool string, responses ...MCPResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scripts[tool] = append(m.scripts[tool], responses...)
}

// CallTool pops the next scripted response for tool and records the call.
func (m *MockMCP) CallTool(ctx context.Context, tool string, arguments json.RawMessage) (MCPResponse, error) {
	select {
	case <-ctx.Done():
		return MCPResponse{}, ctx.Err()
	default:
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, MCPCall{Server: m.server, Tool: tool, Arguments: append(json.RawMessage(nil), arguments...)})
	queue := m.scripts[tool]
	if len(queue) == 0 {
		return MCPResponse{}, ErrMCPScriptExhausted
	}
	resp := queue[0]
	m.scripts[tool] = queue[1:]
	return resp, nil
}

// Calls returns a snapshot of every recorded invocation.
func (m *MockMCP) Calls() []MCPCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MCPCall, len(m.calls))
	copy(out, m.calls)
	return out
}
