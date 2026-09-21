package runtimeadapter

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

func TestModelToolSnapshotProviderResolvesTenantScopedExactDefinitions(t *testing.T) {
	registry := newRecordingToolRegistry(toolDefinition("weather", "v1", `{"type":"object"}`))
	provider := ModelToolSnapshotProvider{Registry: registry}
	run := toolSnapshotRunRequest()

	resolved, err := provider.ResolveSnapshot(context.Background(), run, []string{"weather@v1"})
	if err != nil {
		t.Fatalf("resolve snapshot: %v", err)
	}
	if resolved.SnapshotID == "" || resolved.CapabilityHash == "" || resolved.PolicyHash == "" {
		t.Fatalf("snapshot identity missing: %#v", resolved)
	}
	if len(resolved.Definitions) != 1 || resolved.Definitions[0].Name != "weather" || string(resolved.Definitions[0].Schema) != `{"type":"object"}` {
		t.Fatalf("unexpected definitions: %#v", resolved.Definitions)
	}
	request := registry.snapshotRequest()
	if request.TenantID != run.TenantID || request.AgentID != run.Definition.AgentID || len(request.ToolRefs) != 1 || request.ToolRefs[0].Version != "v1" {
		t.Fatalf("principal or exact ref lost: %#v", request)
	}
	governed, err := provider.ResolveGovernedToolDefinitions(context.Background(), run, []string{"weather@v1"})
	if err != nil {
		t.Fatalf("resolve governed snapshot: %v", err)
	}
	if governed.Snapshot.SnapshotID == "" || governed.Snapshot.CapabilityHash == "" || governed.Snapshot.PolicyHash == "" ||
		len(governed.Snapshot.ToolRefs) != 1 || governed.Snapshot.ToolRefs[0] != "weather@v1" {
		t.Fatalf("runtime snapshot identity missing: %#v", governed.Snapshot)
	}

	resolved.Definitions[0].Schema[0] = '['
	again, err := provider.ResolveToolDefinitions(context.Background(), run, []string{"weather@v1"})
	if err != nil {
		t.Fatalf("resolve definitions again: %v", err)
	}
	if string(again[0].Schema) != `{"type":"object"}` {
		t.Fatalf("caller mutated provider output: %s", again[0].Schema)
	}
}

func TestModelToolSnapshotProviderRejectsUnversionedReferenceBeforeRegistry(t *testing.T) {
	registry := newRecordingToolRegistry(toolDefinition("weather", "v1", `{"type":"object"}`))
	provider := ModelToolSnapshotProvider{Registry: registry}

	_, err := provider.ResolveToolDefinitions(context.Background(), toolSnapshotRunRequest(), []string{"weather"})
	if !errors.Is(err, ErrToolReferenceInvalid) {
		t.Fatalf("unversioned error = %v", err)
	}
	if registry.snapshotCalls() != 0 {
		t.Fatalf("registry called for invalid authoring ref: %d", registry.snapshotCalls())
	}
}

func TestModelToolSnapshotProviderRejectsWhitespaceInsideReferenceBeforeRegistry(t *testing.T) {
	registry := newRecordingToolRegistry(toolDefinition("weather", "v1", `{"type":"object"}`))
	provider := ModelToolSnapshotProvider{Registry: registry}

	_, err := provider.ResolveToolDefinitions(context.Background(), toolSnapshotRunRequest(), []string{"weather @v1"})
	if !errors.Is(err, ErrToolReferenceInvalid) {
		t.Fatalf("whitespace error = %v", err)
	}
	if registry.snapshotCalls() != 0 {
		t.Fatalf("registry called for invalid authoring ref: %d", registry.snapshotCalls())
	}
}

func TestModelToolSnapshotProviderRejectsEmptyTenantBeforeRegistry(t *testing.T) {
	registry := newRecordingToolRegistry(toolDefinition("weather", "v1", `{"type":"object"}`))
	provider := ModelToolSnapshotProvider{Registry: registry}
	run := toolSnapshotRunRequest()
	run.TenantID = ""

	_, err := provider.ResolveToolDefinitions(context.Background(), run, []string{"weather@v1"})
	if !errors.Is(err, ErrToolSnapshotInvalid) {
		t.Fatalf("empty tenant error = %v", err)
	}
	if registry.snapshotCalls() != 0 {
		t.Fatalf("registry called without tenant: %d", registry.snapshotCalls())
	}
}

func TestModelToolSnapshotProviderResolvesSchemaArtifactThroughPort(t *testing.T) {
	definition := toolDefinition("poi", "v2", "")
	definition.InputSchemaRef = "artifact://schemas/poi-v2"
	registry := newRecordingToolRegistry(definition)
	called := false
	provider := ModelToolSnapshotProvider{
		Registry: registry,
		Schemas: InputSchemaResolverFunc(func(_ context.Context, def toolgateway.ToolDefinition) (json.RawMessage, error) {
			called = true
			if def.InputSchemaRef != definition.InputSchemaRef {
				t.Fatalf("schema ref = %q", def.InputSchemaRef)
			}
			return json.RawMessage(`{"type":"object","required":["q"]}`), nil
		}),
	}

	definitions, err := provider.ResolveToolDefinitions(context.Background(), toolSnapshotRunRequest(), []string{"poi@v2"})
	if err != nil {
		t.Fatalf("resolve schema artifact: %v", err)
	}
	if !called || len(definitions) != 1 || !json.Valid(definitions[0].Schema) {
		t.Fatalf("schema resolver was not used: %#v", definitions)
	}
}

func TestModelToolSnapshotProviderFailsClosedForUnavailableSchema(t *testing.T) {
	definition := toolDefinition("poi", "v2", "")
	definition.InputSchemaRef = "artifact://schemas/poi-v2"
	provider := ModelToolSnapshotProvider{Registry: newRecordingToolRegistry(definition)}

	_, err := provider.ResolveToolDefinitions(context.Background(), toolSnapshotRunRequest(), []string{"poi@v2"})
	if !errors.Is(err, ErrToolInputSchemaUnavailable) {
		t.Fatalf("missing schema resolver error = %v", err)
	}
}

func TestModelToolSnapshotProviderClassifiesRegistryAvailability(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		retryable bool
	}{
		{
			name:      "retryable backend",
			err:       toolgateway.NewToolError(toolgateway.ErrorTypeUpstreamError, "registry unavailable", true, errors.New("dial timeout")),
			retryable: true,
		},
		{
			name: "permanent permission",
			err:  toolgateway.NewToolError(toolgateway.ErrorTypePermissionDenied, "agent denied", false, nil),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := ModelToolSnapshotProvider{Registry: failingToolRegistry{err: tt.err}}
			_, err := provider.ResolveSnapshot(context.Background(), toolSnapshotRunRequest(), []string{"weather@v1"})
			if got := errors.Is(err, agentruntime.ErrProductionCapabilityUnavailable); got != tt.retryable {
				t.Fatalf("capability-unavailable marker=%v, want=%v, err=%v", got, tt.retryable, err)
			}
		})
	}
}

func TestModelToolSnapshotProviderRejectsSnapshotDrift(t *testing.T) {
	registry := newRecordingToolRegistry(toolDefinition("weather", "v1", `{"type":"object"}`))
	registry.mutateSnapshot = func(snapshot *toolgateway.ToolSnapshot) {
		snapshot.ToolRefs[0].Version = "v2"
	}
	provider := ModelToolSnapshotProvider{Registry: registry}

	_, err := provider.ResolveToolDefinitions(context.Background(), toolSnapshotRunRequest(), []string{"weather@v1"})
	if !errors.Is(err, ErrToolSnapshotInvalid) {
		t.Fatalf("snapshot drift error = %v", err)
	}
}

func TestModelToolSnapshotProviderRejectsSnapshotChangingDuringAssembly(t *testing.T) {
	registry := newRecordingToolRegistry(toolDefinition("weather", "v1", `{"type":"object"}`))
	var calls int
	registry.mutateSnapshot = func(snapshot *toolgateway.ToolSnapshot) {
		calls++
		if calls == 2 {
			snapshot.PolicyHash = "sha256:changed"
		}
	}
	provider := ModelToolSnapshotProvider{Registry: registry}

	_, err := provider.ResolveToolDefinitions(context.Background(), toolSnapshotRunRequest(), []string{"weather@v1"})
	if !errors.Is(err, ErrToolSnapshotInvalid) {
		t.Fatalf("concurrent snapshot drift error = %v", err)
	}
}

func toolSnapshotRunRequest() agentruntime.RunRequest {
	return agentruntime.RunRequest{
		RunID: "run_1", SessionID: "session_1", TenantID: "tenant_1", UserID: "user_1",
		Definition: agentruntime.AgentDefinition{AgentID: "agent_1", Version: "v1"},
		Trace:      observability.TraceContext{TraceID: "trace_1", TenantID: "tenant_1", UserID: "user_1"},
	}
}

func toolDefinition(name, version, schema string) toolgateway.ToolDefinition {
	return toolgateway.ToolDefinition{
		Name: name, Version: version, Type: toolgateway.ToolTypeFunction,
		Description: "test tool", InputSchema: json.RawMessage(schema),
		RiskLevel: toolgateway.RiskLow, Timeout: time.Second,
		Permissions: toolgateway.ToolPermissions{AllowedAgents: []string{"agent_1"}},
		Visibility:  observability.VisibilityUserVisible,
		Function:    &toolgateway.FunctionToolSpec{HandlerName: name},
	}
}

type recordingToolRegistry struct {
	delegate       toolgateway.ToolRegistry
	mu             sync.Mutex
	requests       []toolgateway.ResolveToolSnapshotRequest
	mutateSnapshot func(*toolgateway.ToolSnapshot)
}

type failingToolRegistry struct {
	err error
}

func (r failingToolRegistry) Get(context.Context, string, string) (*toolgateway.ToolDefinition, error) {
	return nil, r.err
}

func (r failingToolRegistry) ResolveSnapshot(context.Context, toolgateway.ResolveToolSnapshotRequest) (*toolgateway.ToolSnapshot, error) {
	return nil, r.err
}

func newRecordingToolRegistry(definitions ...toolgateway.ToolDefinition) *recordingToolRegistry {
	return &recordingToolRegistry{delegate: toolgateway.NewStaticRegistry(definitions)}
}

func (r *recordingToolRegistry) Get(ctx context.Context, name, version string) (*toolgateway.ToolDefinition, error) {
	return r.delegate.Get(ctx, name, version)
}

func (r *recordingToolRegistry) ResolveSnapshot(ctx context.Context, req toolgateway.ResolveToolSnapshotRequest) (*toolgateway.ToolSnapshot, error) {
	r.mu.Lock()
	r.requests = append(r.requests, req)
	r.mu.Unlock()
	snapshot, err := r.delegate.ResolveSnapshot(ctx, req)
	if err == nil && r.mutateSnapshot != nil {
		r.mutateSnapshot(snapshot)
	}
	return snapshot, err
}

func (r *recordingToolRegistry) snapshotRequest() toolgateway.ResolveToolSnapshotRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests[len(r.requests)-1]
}

func (r *recordingToolRegistry) snapshotCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}
