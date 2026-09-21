package testkit

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

// ToolInvocation records one call to MockTool.Invoke. Tests inspect the slice
// returned by MockTool.Invocations() to assert the model produced the
// expected arguments in the expected order.
type ToolInvocation struct {
	Name      string
	Version   string
	Arguments json.RawMessage
}

// ToolScript scripts one tool response. Repeat by enqueuing the same script.
type ToolScript struct {
	// Output is the small inline result JSON. Empty when ArtifactRef is set.
	Output json.RawMessage
	// ArtifactRef is optional; used when the tool result is too large to inline.
	ArtifactRef string
	// FailWith aborts Invoke with a classified failure. Retryable adjusts how
	// the retry policy interprets the error.
	FailWith  error
	Retryable bool
}

// MockTool is a scripted business function tool suitable for testkit fixtures.
// Its Invoke method pops the next script from the queue; missing scripts
// return ErrToolScriptExhausted so tests fail loudly.
type MockTool struct {
	name    string
	version string
	mu      sync.Mutex
	scripts []ToolScript
	calls   []ToolInvocation
}

// ErrToolScriptExhausted is returned when Invoke runs out of scripts.
var ErrToolScriptExhausted = errors.New("testkit: tool script queue exhausted")

// NewMockTool constructs a tool bound to name+version, preloaded with scripts.
func NewMockTool(name, version string, scripts ...ToolScript) *MockTool {
	return &MockTool{
		name:    name,
		version: version,
		scripts: append([]ToolScript(nil), scripts...),
	}
}

// Name / Version echo the constructor arguments.
func (t *MockTool) Name() string    { return t.name }
func (t *MockTool) Version() string { return t.version }

// Enqueue appends more scripts to the queue.
func (t *MockTool) Enqueue(scripts ...ToolScript) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.scripts = append(t.scripts, scripts...)
}

// Invoke pops the next script and records the invocation. It respects ctx
// cancellation.
func (t *MockTool) Invoke(ctx context.Context, arguments json.RawMessage) (ToolScript, error) {
	select {
	case <-ctx.Done():
		return ToolScript{}, ctx.Err()
	default:
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, ToolInvocation{
		Name:      t.name,
		Version:   t.version,
		Arguments: append(json.RawMessage(nil), arguments...),
	})
	if len(t.scripts) == 0 {
		return ToolScript{}, ErrToolScriptExhausted
	}
	script := t.scripts[0]
	t.scripts = t.scripts[1:]
	return script, nil
}

// Invocations returns a snapshot of every recorded call.
func (t *MockTool) Invocations() []ToolInvocation {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]ToolInvocation, len(t.calls))
	copy(out, t.calls)
	return out
}
