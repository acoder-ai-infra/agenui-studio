package toolgateway

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/objectstore"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestResolveToolInputAndSchemaRejectOversizedArtifacts(t *testing.T) {
	req := baseToolRequest(nil)
	req.ArgumentsRef = "artifact://oversized"
	argumentsStore := oversizedArtifactStore{object: &artifact.ArtifactObject{
		Meta:    artifact.ArtifactMeta{TenantID: req.TenantID, SessionID: req.SessionID, RunID: req.RunID},
		Content: io.NopCloser(io.LimitReader(zeroReader{}, int64(maxJSONValueBytes)+1)),
	}}
	if _, err := resolveToolInput(context.Background(), argumentsStore, req); !IsErrorType(err, ErrorTypeArtifactError) {
		t.Fatalf("oversized arguments error = %v", err)
	}

	schemaStore := oversizedArtifactStore{object: &artifact.ArtifactObject{
		Meta:    artifact.ArtifactMeta{TenantID: req.TenantID, ArtifactType: artifact.ArtifactTypeSchema},
		Content: io.NopCloser(io.LimitReader(zeroReader{}, int64(maxJSONSchemaBytes)+1)),
	}}
	if _, err := resolveToolSchema(context.Background(), schemaStore, req, nil, "artifact://schema"); !IsErrorType(err, ErrorTypeArtifactError) {
		t.Fatalf("oversized schema error = %v", err)
	}
}

type oversizedArtifactStore struct {
	artifact.ArtifactStore
	object *artifact.ArtifactObject
}

func (s oversizedArtifactStore) Get(context.Context, string, artifact.GetOptions) (*artifact.ArtifactObject, error) {
	return s.object, nil
}

type zeroReader struct{}

func (zeroReader) Read(buffer []byte) (int, error) {
	for index := range buffer {
		buffer[index] = 'x'
	}
	return len(buffer), nil
}

func TestGatewayLoadsArgumentsRef(t *testing.T) {
	store, ctx := newScopedArtifactStore(t)
	meta := putScopedArtifact(t, store, "tenant-a", "sess-1", "run-1", artifact.ArtifactTypeDebugPayload, `{"query":"hotel"}`)
	var received json.RawMessage
	gateway, _ := newArtifactGateway(store, defaultSearchDefinition(), func(_ context.Context, call FunctionCall) (*FunctionResult, error) {
		received = append(json.RawMessage(nil), call.Arguments...)
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})
	req := baseToolRequest(nil)
	req.ArgumentsRef = meta.ArtifactRef

	result, err := gateway.Invoke(ctx, req)
	if err != nil || result.Status != ToolCallSucceeded || string(received) != `{"query":"hotel"}` {
		t.Fatalf("result=%#v err=%v received=%s", result, err, received)
	}
	if result.Usage.InputBytes != int64(len(received)) {
		t.Fatalf("input bytes = %d, want %d", result.Usage.InputBytes, len(received))
	}
}

func TestGatewayRejectsArgumentsAndArgumentsRef(t *testing.T) {
	store, ctx := newScopedArtifactStore(t)
	called := false
	gateway, eventStore := newArtifactGateway(store, defaultSearchDefinition(), func(context.Context, FunctionCall) (*FunctionResult, error) {
		called = true
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.ArgumentsRef = "artifact://must-not-be-read"

	result, err := gateway.Invoke(ctx, req)
	if err != nil || called {
		t.Fatalf("result=%#v err=%v called=%v", result, err, called)
	}
	assertFailedResult(t, result, ErrorTypeSchemaValidationFailed, false)
	assertOnlyFailedEvent(t, eventStore, ErrorTypeSchemaValidationFailed)
}

func TestGatewayRejectsCrossScopeArgumentsRef(t *testing.T) {
	cases := []struct {
		name      string
		tenantID  string
		sessionID string
		runID     string
	}{
		{name: "cross tenant", tenantID: "tenant-b", sessionID: "sess-1", runID: "run-1"},
		{name: "cross session", tenantID: "tenant-a", sessionID: "sess-2", runID: "run-1"},
		{name: "cross run", tenantID: "tenant-a", sessionID: "sess-1", runID: "run-2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, ctx := newScopedArtifactStore(t)
			meta := putScopedArtifact(t, store, tc.tenantID, tc.sessionID, tc.runID, artifact.ArtifactTypeDebugPayload, `{"query":"hotel"}`)
			called := false
			gateway, eventStore := newArtifactGateway(store, defaultSearchDefinition(), func(context.Context, FunctionCall) (*FunctionResult, error) {
				called = true
				return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
			})
			req := baseToolRequest(nil)
			req.ArgumentsRef = meta.ArtifactRef

			result, err := gateway.Invoke(ctx, req)
			if err != nil || called {
				t.Fatalf("result=%#v err=%v called=%v", result, err, called)
			}
			assertFailedResult(t, result, ErrorTypeArtifactError, false)
			assertOnlyFailedEvent(t, eventStore, ErrorTypeArtifactError)
		})
	}
}

func TestGatewayLoadsInputAndOutputSchemaRefs(t *testing.T) {
	store, ctx := newScopedArtifactStore(t)
	inputSchema := putScopedArtifact(
		t, store, "tenant-a", "sess-1", "run-1", artifact.ArtifactTypeSchema,
		`{"type":"object","required":["query"],"properties":{"query":{"type":"string"}}}`,
	)
	outputSchema := putScopedArtifact(
		t, store, "tenant-a", "sess-1", "run-1", artifact.ArtifactTypeSchema,
		`{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"}}}`,
	)
	def := defaultSearchDefinition()
	def.InputSchema = nil
	def.InputSchemaRef = inputSchema.ArtifactRef
	def.OutputSchemaRef = outputSchema.ArtifactRef

	t.Run("input schema rejects before execution", func(t *testing.T) {
		called := false
		gateway, eventStore := newArtifactGateway(store, def, func(context.Context, FunctionCall) (*FunctionResult, error) {
			called = true
			return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
		})

		result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"limit":3}`)))
		if err != nil || called {
			t.Fatalf("result=%#v err=%v called=%v", result, err, called)
		}
		assertFailedResult(t, result, ErrorTypeSchemaValidationFailed, false)
		assertOnlyFailedEvent(t, eventStore, ErrorTypeSchemaValidationFailed)
	})

	t.Run("output schema rejects after execution", func(t *testing.T) {
		called := false
		gateway, eventStore := newArtifactGateway(store, def, func(context.Context, FunctionCall) (*FunctionResult, error) {
			called = true
			return &FunctionResult{Data: json.RawMessage(`{"ok":"yes"}`)}, nil
		})

		result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
		if err != nil || !called {
			t.Fatalf("result=%#v err=%v called=%v", result, err, called)
		}
		assertFailedResult(t, result, ErrorTypeSchemaValidationFailed, true)
		if got := eventTypes(eventStore.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolCallFailed {
			t.Fatalf("expected started then failed, got %#v", got)
		}
	})
}

func TestGatewayRejectsNonSchemaArtifactAsSchema(t *testing.T) {
	store, ctx := newScopedArtifactStore(t)
	nonSchema := putScopedArtifact(
		t, store, "tenant-a", "sess-1", "run-1", artifact.ArtifactTypeDebugPayload,
		`{"type":"object","required":["query"]}`,
	)
	def := defaultSearchDefinition()
	def.InputSchema = nil
	def.InputSchemaRef = nonSchema.ArtifactRef
	called := false
	gateway, eventStore := newArtifactGateway(store, def, func(context.Context, FunctionCall) (*FunctionResult, error) {
		called = true
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil || called {
		t.Fatalf("result=%#v err=%v called=%v", result, err, called)
	}
	assertFailedResult(t, result, ErrorTypeArtifactError, false)
	assertOnlyFailedEvent(t, eventStore, ErrorTypeArtifactError)
}

func TestGatewayDoesNotAcceptSchemaRefsFromRequestData(t *testing.T) {
	store, ctx := newScopedArtifactStore(t)
	strictSchema := putScopedArtifact(
		t, store, "tenant-a", "sess-1", "run-1", artifact.ArtifactTypeSchema,
		`{"type":"object","required":["forbidden"]}`,
	)
	def := defaultSearchDefinition()
	def.InputSchema = json.RawMessage(`{"type":"object"}`)
	called := 0
	gateway, _ := newArtifactGateway(store, def, func(context.Context, FunctionCall) (*FunctionResult, error) {
		called++
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})

	requests := []ToolCallRequest{
		baseToolRequest(json.RawMessage(`{"input_schema_ref":"` + strictSchema.ArtifactRef + `"}`)),
		baseToolRequest(json.RawMessage(`{"query":"hotel"}`)),
	}
	requests[0].ToolCallID = "tc-arguments-schema"
	requests[1].ToolCallID = "tc-metadata-schema"
	requests[1].Metadata = map[string]string{"input_schema_ref": strictSchema.ArtifactRef}
	for _, req := range requests {
		result, err := gateway.Invoke(ctx, req)
		if err != nil || result.Status != ToolCallSucceeded {
			t.Fatalf("request=%#v result=%#v err=%v", req, result, err)
		}
	}
	if called != len(requests) {
		t.Fatalf("executor calls = %d, want %d", called, len(requests))
	}
}

func newScopedArtifactStore(t *testing.T) (artifact.ArtifactStore, context.Context) {
	t.Helper()
	store := artifact.NewStore(artifact.StoreConfig{
		ObjectStore:   objectstore.NewMemory(),
		MetadataStore: metastore.NewMemory(),
	})
	ctx := artifact.ContextWithActor(testTraceContext(), artifact.Actor{
		TenantID:  "tenant-a",
		UserID:    "runtime",
		SessionID: "sess-1",
		RunID:     "run-1",
		AgentID:   "agent-a",
		Role:      artifact.ActorRuntime,
	})
	return store, ctx
}

func putScopedArtifact(
	t *testing.T,
	store artifact.ArtifactStore,
	tenantID string,
	sessionID string,
	runID string,
	artifactType artifact.ArtifactType,
	content string,
) *artifact.ArtifactMeta {
	t.Helper()
	ctx := artifact.ContextWithActor(context.Background(), artifact.Actor{
		TenantID: tenantID, SessionID: sessionID, RunID: runID, Role: artifact.ActorRuntime,
	})
	meta, err := store.Put(ctx, artifact.PutArtifactRequest{
		TenantID: tenantID, SessionID: sessionID, RunID: runID, StepID: "step-source",
		OwnerModule: artifact.OwnerModuleToolGateway, OwnerID: "tc-source",
		ArtifactType: artifactType, MimeType: "application/json",
		Visibility: artifact.VisibilityDebug, Content: strings.NewReader(content),
		RetentionPolicy: artifact.RetentionDebugShortTTL, CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	return meta
}

func newArtifactGateway(
	store artifact.ArtifactStore,
	def ToolDefinition,
	handler FunctionTool,
) (*DefaultGateway, *recordingEventStore) {
	eventStore := &recordingEventStore{}
	return NewGateway(GatewayConfig{
		Registry:        NewStaticRegistry([]ToolDefinition{def}),
		SchemaValidator: BasicSchemaValidator{},
		EventStore:      eventStore,
		ArtifactStore:   store,
		Executors:       []ToolExecutor{NewFunctionExecutor(map[string]FunctionTool{"search": handler})},
		IDGenerator:     fixedIDGenerator{},
		Clock:           fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	}), eventStore
}
