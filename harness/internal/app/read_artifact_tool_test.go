package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	metamem "github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	objmem "github.com/AGenUI/agenui-studio/harness/internal/artifact/objectstore"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway/runtimeadapter"
)

func TestReadArtifactToolReadsPagedUTF8AcrossRunsInSameSession(t *testing.T) {
	store := artifact.NewStore(artifact.StoreConfig{ObjectStore: objmem.NewMemory(), MetadataStore: metamem.NewMemory()})
	ref := putReadableArtifact(t, store, "tenant-1", "user-1", "sess-1", "run-1", artifact.ArtifactTypeFile, "text/plain", "你好-world")
	handler := newReadArtifactTool(store)
	trace := observability.TraceContext{TenantID: "tenant-1", UserID: "user-1", SessionID: "sess-1", RunID: "run-2", AgentID: "demo"}

	first := callReadArtifact(t, handler, trace, map[string]any{"artifact_ref": ref, "offset_bytes": 0, "limit_bytes": 6})
	if first.Content != "你好" || first.NextOffsetBytes != 6 || first.EOF {
		t.Fatalf("first page = %#v", first)
	}
	second := callReadArtifact(t, handler, trace, map[string]any{"artifact_ref": ref, "offset_bytes": first.NextOffsetBytes, "limit_bytes": 16})
	if second.Content != "-world" || !second.EOF {
		t.Fatalf("second page = %#v", second)
	}
}

func TestReadArtifactToolFailsClosedForScopeTypeAndBounds(t *testing.T) {
	store := artifact.NewStore(artifact.StoreConfig{ObjectStore: objmem.NewMemory(), MetadataStore: metamem.NewMemory(), MaxObjectBytes: 2 << 20})
	readable := putReadableArtifact(t, store, "tenant-1", "user-1", "sess-1", "run-1", artifact.ArtifactTypeFile, "text/plain", "你好")
	checkpoint := putReadableArtifact(t, store, "tenant-1", "user-1", "sess-1", "run-1", artifact.ArtifactTypeCheckpointState, "application/json", `{}`)
	oversized := putReadableArtifact(t, store, "tenant-1", "user-1", "sess-1", "run-1", artifact.ArtifactTypeFile, "text/plain", strings.Repeat("x", maxModelReadableArtifactBytes+1))
	handler := newReadArtifactTool(store)

	tests := []struct {
		name  string
		trace observability.TraceContext
		args  map[string]any
	}{
		{name: "cross user", trace: observability.TraceContext{TenantID: "tenant-1", UserID: "user-2", SessionID: "sess-1", RunID: "run-2"}, args: map[string]any{"artifact_ref": readable}},
		{name: "cross session", trace: observability.TraceContext{TenantID: "tenant-1", UserID: "user-1", SessionID: "sess-2", RunID: "run-2"}, args: map[string]any{"artifact_ref": readable}},
		{name: "checkpoint type", trace: observability.TraceContext{TenantID: "tenant-1", UserID: "user-1", SessionID: "sess-1", RunID: "run-2"}, args: map[string]any{"artifact_ref": checkpoint}},
		{name: "oversized object", trace: observability.TraceContext{TenantID: "tenant-1", UserID: "user-1", SessionID: "sess-1", RunID: "run-2"}, args: map[string]any{"artifact_ref": oversized}},
		{name: "split rune offset", trace: observability.TraceContext{TenantID: "tenant-1", UserID: "user-1", SessionID: "sess-1", RunID: "run-2"}, args: map[string]any{"artifact_ref": readable, "offset_bytes": 1}},
		{name: "page cannot fit rune", trace: observability.TraceContext{TenantID: "tenant-1", UserID: "user-1", SessionID: "sess-1", RunID: "run-2"}, args: map[string]any{"artifact_ref": readable, "limit_bytes": 1}},
		{name: "excessive page", trace: observability.TraceContext{TenantID: "tenant-1", UserID: "user-1", SessionID: "sess-1", RunID: "run-2"}, args: map[string]any{"artifact_ref": readable, "limit_bytes": maxReadArtifactPageBytes + 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args, _ := json.Marshal(tc.args)
			if _, err := handler(context.Background(), toolgateway.FunctionCall{Arguments: args, Trace: tc.trace}); err == nil {
				t.Fatal("read_artifact unexpectedly allowed request")
			}
		})
	}
}

func TestReadArtifactToolRunsThroughGatewayLifecycle(t *testing.T) {
	store := artifact.NewStore(artifact.StoreConfig{ObjectStore: objmem.NewMemory(), MetadataStore: metamem.NewMemory()})
	ref := putReadableArtifact(t, store, "tenant-1", "user-1", "sess-1", "run-1", artifact.ArtifactTypeFile, "text/plain", "gateway body")
	definition := toolgateway.ToolDefinition{
		Name: "harness.read_artifact", Version: "1.0.0", Type: toolgateway.ToolTypeFunction,
		InputSchema: json.RawMessage(`{"type":"object","required":["artifact_ref"],"properties":{"artifact_ref":{"type":"string"}},"additionalProperties":false}`),
		RiskLevel:   toolgateway.RiskLow, Timeout: time.Second, Visibility: observability.VisibilityInternal,
		Permissions: toolgateway.ToolPermissions{AllowedAgents: []string{"demo"}, TenantScope: "tenant"},
		Function:    &toolgateway.FunctionToolSpec{HandlerName: "harness.read_artifact"},
	}
	registry := toolgateway.NewStaticRegistry([]toolgateway.ToolDefinition{definition})
	events := toolgateway.NewMemoryEventStore()
	gateway := toolgateway.NewGateway(toolgateway.GatewayConfig{
		Registry: registry, EventStore: events, ArtifactStore: store,
		Executors: []toolgateway.ToolExecutor{toolgateway.NewFunctionExecutor(map[string]toolgateway.FunctionTool{
			"harness.read_artifact": newReadArtifactTool(store),
		})},
	})
	adapter, err := runtimeadapter.New(gateway, runtimeadapter.InvocationResolverFunc(func(context.Context, agentruntime.ToolInvocationRequest) (runtimeadapter.ResolvedInvocation, error) {
		return runtimeadapter.ResolvedInvocation{
			ToolName: definition.Name, ToolVersion: definition.Version, StepID: "step-1", ParentStepID: "parent-1",
			Policy: toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskLow, Timeout: time.Second},
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	trace := observability.TraceContext{TraceID: "trace-1", TenantID: "tenant-1", UserID: "user-1", SessionID: "sess-1", RunID: "run-2", AgentID: "demo"}
	ctx := observability.WithTraceContext(context.Background(), trace)
	args, _ := json.Marshal(map[string]any{"artifact_ref": ref})
	sink := &readArtifactEventSink{}
	result, err := adapter.Invoke(ctx, agentruntime.ToolInvocationRequest{
		Trace: trace, SessionID: trace.SessionID, RunID: trace.RunID, AgentID: trace.AgentID,
		ToolCallID: "call-1", ToolName: definition.Name, ToolVersion: definition.Version,
		Source: agentruntime.ToolSourceRegistry, SourceRef: definition.Name + "@" + definition.Version, Arguments: args,
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || result.Content == "" {
		t.Fatalf("gateway result = %#v", result)
	}
	persisted := events.Events()
	if len(persisted) != 2 || persisted[0].EventType != observability.EventToolCallStarted || persisted[1].EventType != observability.EventToolCallCompleted {
		t.Fatalf("gateway lifecycle = %#v", persisted)
	}
	if len(sink.events) != len(persisted) {
		t.Fatalf("streamed events=%d persisted=%d", len(sink.events), len(persisted))
	}
}

type readArtifactEventSink struct{ events []observability.AgentEvent }

func (s *readArtifactEventSink) Emit(_ context.Context, event observability.AgentEvent) error {
	s.events = append(s.events, event)
	return nil
}

func TestBuildConfiguredToolsRequiresAndInstallsReadArtifactHandler(t *testing.T) {
	catalog := toolCatalogConfig{Enabled: []string{"harness.read_artifact@1.0.0"}, Definitions: []configuredTool{{
		Name: "harness.read_artifact", Version: "1.0.0", Type: "function", Handler: "harness.read_artifact",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
	}}}
	if _, _, err := buildConfiguredTools(catalog, nil, nil); err == nil {
		t.Fatal("read_artifact installed without Artifact Store")
	}
	store := artifact.NewStore(artifact.StoreConfig{ObjectStore: objmem.NewMemory(), MetadataStore: metamem.NewMemory()})
	definitions, handlers, err := buildConfiguredTools(catalog, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 1 || handlers["harness.read_artifact"] == nil {
		t.Fatalf("definitions=%#v handlers=%#v", definitions, handlers)
	}
}

type readArtifactResult struct {
	ArtifactRef     string `json:"artifact_ref"`
	Content         string `json:"content"`
	OffsetBytes     int    `json:"offset_bytes"`
	NextOffsetBytes int    `json:"next_offset_bytes"`
	EOF             bool   `json:"eof"`
}

func callReadArtifact(t *testing.T, handler toolgateway.FunctionTool, trace observability.TraceContext, values map[string]any) readArtifactResult {
	t.Helper()
	args, _ := json.Marshal(values)
	result, err := handler(context.Background(), toolgateway.FunctionCall{Arguments: args, Trace: trace})
	if err != nil {
		t.Fatal(err)
	}
	var decoded readArtifactResult
	if err := json.Unmarshal(result.Data, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func putReadableArtifact(t *testing.T, store *artifact.Store, tenantID, userID, sessionID, runID string, artifactType artifact.ArtifactType, mimeType, body string) string {
	t.Helper()
	ctx := artifact.ContextWithActor(context.Background(), artifact.Actor{
		Role: artifact.ActorRuntime, TenantID: tenantID, UserID: userID, SessionID: sessionID, RunID: runID,
	})
	meta, err := store.Put(ctx, artifact.PutArtifactRequest{
		TenantID: tenantID, UserID: userID, SessionID: sessionID, RunID: runID,
		OwnerModule: artifact.OwnerModuleRuntime, OwnerID: runID, ArtifactType: artifactType,
		MimeType: mimeType, Name: "artifact.txt", Visibility: artifact.VisibilityInternal,
		RetentionPolicy: artifact.RetentionSessionTTL, Content: strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	return meta.ArtifactRef
}
