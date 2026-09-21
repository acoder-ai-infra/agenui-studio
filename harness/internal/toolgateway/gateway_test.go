package toolgateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestGatewayInvokesFunctionToolAndEmitsEvents(t *testing.T) {
	eventStore := &recordingEventStore{}
	registry := NewStaticRegistry([]ToolDefinition{{
		Name:        "search_kb",
		Version:     "v1",
		Type:        ToolTypeFunction,
		DisplayName: "Knowledge Search",
		InputSchema: json.RawMessage(`{"type":"object","required":["query"],"properties":{"query":{"type":"string"}}}`),
		RiskLevel:   RiskLow,
		Timeout:     time.Second,
		Visibility:  observability.VisibilityUserVisible,
		Function:    &FunctionToolSpec{HandlerName: "search"},
		Permissions: ToolPermissions{AllowedAgents: []string{"agent-a"}},
	}})
	executor := NewFunctionExecutor(map[string]FunctionTool{
		"search": func(ctx context.Context, call FunctionCall) (*FunctionResult, error) {
			if call.ToolCallID != "tc-1" {
				t.Fatalf("tool call id propagated to function = %q", call.ToolCallID)
			}
			if string(call.Arguments) != `{"query":"hotel"}` {
				t.Fatalf("arguments propagated to function = %s", call.Arguments)
			}
			return &FunctionResult{
				Data:     json.RawMessage(`{"count":1,"items":["doc-a"]}`),
				MimeType: "application/json",
			}, nil
		},
	})
	gateway := NewGateway(GatewayConfig{
		Registry:        registry,
		SchemaValidator: BasicSchemaValidator{},
		EventStore:      eventStore,
		Executors:       []ToolExecutor{executor},
		IDGenerator:     fixedIDGenerator{},
		Clock:           fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{
		TraceID:   "trace-1",
		SpanID:    "span-parent",
		TenantID:  "tenant-a",
		UserID:    "user-a",
		SessionID: "sess-1",
		RunID:     "run-1",
		AgentID:   "agent-a",
	})

	result, err := gateway.Invoke(ctx, ToolCallRequest{
		ToolCallID:  "tc-1",
		TenantID:    "tenant-a",
		UserID:      "user-a",
		SessionID:   "sess-1",
		RunID:       "run-1",
		StepID:      "step-1",
		AgentID:     "agent-a",
		ToolName:    "search_kb",
		ToolVersion: "v1",
		Arguments:   json.RawMessage(`{"query":"hotel"}`),
		Caller:      ToolCaller{Type: "runtime", AgentID: "agent-a"},
		Policy:      ToolCallPolicy{RiskLevel: RiskLow},
	})
	if err != nil {
		t.Fatalf("invoke function tool: %v", err)
	}
	if result.Status != ToolCallSucceeded {
		t.Fatalf("status = %s", result.Status)
	}
	if string(result.ResultPreview) != `{"count":1,"items":["doc-a"]}` {
		t.Fatalf("result preview = %s", result.ResultPreview)
	}
	if string(result.ModelContextResult) != `{"count":1,"items":["doc-a"]}` {
		t.Fatalf("model context result = %s", result.ModelContextResult)
	}
	if len(result.Events) != 2 {
		t.Fatalf("expected started/completed events in result, got %d", len(result.Events))
	}
	events := eventStore.events
	if len(events) != 2 {
		t.Fatalf("expected two persisted events, got %d", len(events))
	}
	if events[0].EventType != observability.EventToolCallStarted || events[1].EventType != observability.EventToolCallCompleted {
		t.Fatalf("unexpected event order: %#v", []observability.EventType{events[0].EventType, events[1].EventType})
	}
	if events[0].SchemaVersion != observability.AgentEventSchemaVersion || events[1].SchemaVersion != observability.AgentEventSchemaVersion {
		t.Fatalf("event store should persist canonical agent events: %#v", []string{events[0].SchemaVersion, events[1].SchemaVersion})
	}
	if events[0].IdempotencyKey != "run-1:tc-1:started" || events[1].IdempotencyKey != "run-1:tc-1:completed" {
		t.Fatalf("unexpected lifecycle idempotency keys: %#v", []string{events[0].IdempotencyKey, events[1].IdempotencyKey})
	}
	if events[0].Sequence >= events[1].Sequence {
		t.Fatalf("terminal event sequence should be after started: %d >= %d", events[0].Sequence, events[1].Sequence)
	}
	if events[0].TraceID != "trace-1" || events[0].RunID != "run-1" || events[0].StepID != "step-1" || events[0].AgentID != "agent-a" {
		t.Fatalf("event context not populated: %#v", events[0])
	}
	var startedPayload map[string]any
	if err := json.Unmarshal(events[0].PayloadPreview, &startedPayload); err != nil {
		t.Fatalf("decode started payload preview: %v", err)
	}
	if startedPayload["tool_call_id"] != "tc-1" || startedPayload["tool_name"] != "search_kb" || startedPayload["tool_type"] != "function" {
		t.Fatalf("unexpected started payload: %#v", startedPayload)
	}
}

func TestGatewayFunctionToolEmitsCanonicalProcessEvents(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	source := putToolEventArtifact(t, artifactStore, ctx, artifact.PutArtifactRequest{
		TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-1", RunID: "run-1", StepID: "source-step",
		OwnerModule: artifact.OwnerModuleContextEngine, OwnerID: "source-owner", ArtifactType: artifact.ArtifactTypeFile,
		MimeType: "text/plain", Visibility: artifact.VisibilityInternal, Content: strings.NewReader("authoritative artifact"), RetentionPolicy: artifact.RetentionRunTTL,
	})
	eventStore := &recordingEventStore{}
	sink := NewDefaultToolEventSink(ToolEventSinkConfig{
		EventStore: eventStore, ArtifactStore: artifactStore,
		IDGenerator: fixedIDGenerator{}, Clock: fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})
	def := defaultSearchDefinition()
	handlerCalled := false
	gateway := NewGateway(GatewayConfig{
		Registry: NewStaticRegistry([]ToolDefinition{def}), SchemaValidator: BasicSchemaValidator{},
		EventStore: eventStore, ArtifactStore: artifactStore, ToolEventSink: sink,
		Executors: []ToolExecutor{NewFunctionExecutor(map[string]FunctionTool{
			"search": func(_ context.Context, call FunctionCall) (*FunctionResult, error) {
				handlerCalled = true
				if call.ToolContext == nil {
					t.Fatalf("gateway function handler received nil ToolContext")
				}
				percent := 25
				if err := call.ToolContext.EmitProgress(context.Background(), ToolProgressEvent{Stage: "search", Message: "working", Percent: &percent}); err != nil {
					t.Fatalf("emit progress: %v", err)
				}
				if err := call.ToolContext.EmitWarning(context.Background(), ToolWarningEvent{Code: "DEGRADED", Message: "using cache", Retryable: true}); err != nil {
					t.Fatalf("emit warning: %v", err)
				}
				if err := call.ToolContext.EmitDebug(context.Background(), ToolDebugEvent{Message: "wire trace", Data: json.RawMessage(`{"password":"debug-secret"}`)}); err != nil {
					t.Fatalf("emit debug: %v", err)
				}
				if err := call.ToolContext.EmitArtifact(context.Background(), ToolArtifactEvent{
					ArtifactRef: source.ArtifactRef, MimeType: "forged/type", SizeBytes: 9999, Hash: "forged",
				}); err != nil {
					t.Fatalf("emit artifact: %v", err)
				}
				return &FunctionResult{Data: json.RawMessage(`{"ok":true}`), MimeType: "application/json"}, nil
			},
		})},
		IDGenerator: fixedIDGenerator{}, Clock: fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
		Logger: observability.NoopLogger{},
	})

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("invoke process-emitting function: %v", err)
	}
	if !handlerCalled || result.Status != ToolCallSucceeded {
		t.Fatalf("handler_called=%t result=%#v", handlerCalled, result)
	}
	wantTypes := []observability.EventType{
		observability.EventToolCallStarted,
		observability.EventToolCallProgress,
		observability.EventToolCallProgress,
		observability.EventToolArtifactCreated,
		observability.EventToolCallCompleted,
	}
	assertGatewayEventSequence(t, eventStore.events, wantTypes)
	assertGatewayResultEventsMatchStore(t, result, eventStore.events)
	warning := decodeEventPayload(t, eventStore.events[2])
	if warning["kind"] != "warning" || warning["code"] != "DEGRADED" {
		t.Fatalf("warning payload=%#v", warning)
	}
	artifactPayload := decodeEventPayload(t, eventStore.events[3])
	assertAuthoritativeArtifactPayload(t, artifactPayload, source)
	if eventStore.events[3].Visibility != observability.VisibilityInternal {
		t.Fatalf("artifact visibility=%q", eventStore.events[3].Visibility)
	}
	debugArtifacts := listToolCallArtifacts(t, ctx, artifactStore)
	if len(debugArtifacts) != 1 || debugArtifacts[0].ArtifactType != artifact.ArtifactTypeDebugPayload || debugArtifacts[0].Visibility != artifact.VisibilityDebug || debugArtifacts[0].RetentionPolicy != artifact.RetentionDebugShortTTL {
		t.Fatalf("debug artifacts=%#v", debugArtifacts)
	}
	debugContent := readDebugArtifact(t, ctx, artifactStore, debugArtifacts[0].ArtifactRef)
	if strings.Contains(string(debugContent), "debug-secret") || !strings.Contains(string(debugContent), "[REDACTED]") {
		t.Fatalf("unsafe debug artifact: %s", debugContent)
	}
}

func TestGatewayDoesNotCreateToolContextBeforeStartedPersists(t *testing.T) {
	appendErr := errors.New("started append unavailable")
	eventStore := &recordingEventStore{err: appendErr}
	sink := &recordingToolContextSink{}
	def := defaultSearchDefinition()
	handlerCalls := 0
	gateway := NewGateway(GatewayConfig{
		Registry: NewStaticRegistry([]ToolDefinition{def}), SchemaValidator: BasicSchemaValidator{},
		EventStore: eventStore, ToolEventSink: sink,
		Executors: []ToolExecutor{NewFunctionExecutor(map[string]FunctionTool{
			"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
				handlerCalls++
				return &FunctionResult{}, nil
			},
		})},
		IDGenerator: fixedIDGenerator{}, Clock: fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})

	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if result != nil || !errors.Is(err, appendErr) {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if handlerCalls != 0 || len(sink.Events()) != 0 {
		t.Fatalf("handler_calls=%d process_events=%#v", handlerCalls, sink.Events())
	}
}

func TestGatewayKeepsP0LifecycleWhenProgressSinkUnavailable(t *testing.T) {
	for _, tt := range []struct {
		name string
		sink ToolEventSink
	}{
		{name: "missing"},
		{name: "failing", sink: failingToolEventSink{err: errors.New("sink unavailable")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			eventStore := &recordingEventStore{}
			logger := &recordingToolContextLogger{}
			def := defaultSearchDefinition()
			handlerCalls := 0
			gateway := NewGateway(GatewayConfig{
				Registry: NewStaticRegistry([]ToolDefinition{def}), SchemaValidator: BasicSchemaValidator{},
				EventStore: eventStore, ToolEventSink: tt.sink, Logger: logger,
				Executors: []ToolExecutor{NewFunctionExecutor(map[string]FunctionTool{
					"search": func(_ context.Context, call FunctionCall) (*FunctionResult, error) {
						handlerCalls++
						if call.ToolContext == nil {
							t.Fatalf("handler received nil ToolContext")
						}
						if err := call.ToolContext.EmitProgress(context.Background(), ToolProgressEvent{Message: "working"}); err != nil {
							t.Fatalf("progress degradation returned error: %v", err)
						}
						if err := call.ToolContext.EmitWarning(context.Background(), ToolWarningEvent{Code: "WARN", Message: "warning"}); err != nil {
							t.Fatalf("warning degradation returned error: %v", err)
						}
						return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
					},
				})},
				IDGenerator: fixedIDGenerator{}, Clock: fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
			})

			result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
			if err != nil || result == nil || result.Status != ToolCallSucceeded || handlerCalls != 1 {
				t.Fatalf("result=%#v err=%v handler_calls=%d", result, err, handlerCalls)
			}
			wantTypes := []observability.EventType{observability.EventToolCallStarted, observability.EventToolCallCompleted}
			assertGatewayEventSequence(t, eventStore.events, wantTypes)
			assertGatewayResultEventsMatchStore(t, result, eventStore.events)
			degradationLogs := 0
			for _, entry := range logger.Entries() {
				if entry.message == "tool event sink degraded" {
					degradationLogs++
				}
			}
			if degradationLogs != 2 {
				t.Fatalf("degradation log count=%d entries=%#v", degradationLogs, logger.Entries())
			}
		})
	}
}

func TestGatewayFailedFunctionToolIncludesCanonicalProcessEvents(t *testing.T) {
	eventStore := &recordingEventStore{}
	sink := NewDefaultToolEventSink(ToolEventSinkConfig{EventStore: eventStore, IDGenerator: fixedIDGenerator{}})
	def := defaultSearchDefinition()
	gateway := NewGateway(GatewayConfig{
		Registry: NewStaticRegistry([]ToolDefinition{def}), SchemaValidator: BasicSchemaValidator{},
		EventStore: eventStore, ToolEventSink: sink,
		Executors: []ToolExecutor{NewFunctionExecutor(map[string]FunctionTool{
			"search": func(_ context.Context, call FunctionCall) (*FunctionResult, error) {
				if err := call.ToolContext.EmitProgress(context.Background(), ToolProgressEvent{Message: "working"}); err != nil {
					t.Fatalf("emit progress: %v", err)
				}
				if err := call.ToolContext.EmitWarning(context.Background(), ToolWarningEvent{Code: "DEGRADED", Message: "warning"}); err != nil {
					t.Fatalf("emit warning: %v", err)
				}
				return nil, errors.New("function failed")
			},
		})},
		IDGenerator: fixedIDGenerator{}, Clock: fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})

	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil || result == nil || result.Status != ToolCallFailed || result.Failure == nil || !result.Failure.Executed {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	wantTypes := []observability.EventType{
		observability.EventToolCallStarted,
		observability.EventToolCallProgress,
		observability.EventToolCallProgress,
		observability.EventToolCallFailed,
	}
	assertGatewayEventSequence(t, eventStore.events, wantTypes)
	assertGatewayResultEventsMatchStore(t, result, eventStore.events)
	if result.Failure.ErrorType != string(ErrorTypeInternal) {
		t.Fatalf("untyped local function error classified as %q", result.Failure.ErrorType)
	}
}

func TestGatewayPreservesBusinessValidationFailure(t *testing.T) {
	eventStore := &recordingEventStore{}
	gateway := newTestGateway(eventStore, map[string]FunctionTool{
		"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
			return nil, NewToolError(ErrorTypeInvalidArgument, "target does not exist", false, nil)
		},
	}, defaultSearchDefinition())

	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"title"}`)))
	if err != nil {
		t.Fatal(err)
	}
	assertFailedResult(t, result, ErrorTypeInvalidArgument, true)
	assertGatewayEventSequence(t, eventStore.events, []observability.EventType{
		observability.EventToolCallStarted, observability.EventToolCallFailed,
	})
	if eventStore.events[1].Error == nil || eventStore.events[1].Error.Code != "TOOL_INVALID_ARGUMENT" ||
		eventStore.events[1].Error.Type != observability.EventErrorSchemaValidation {
		t.Fatalf("failed event error=%#v", eventStore.events[1].Error)
	}
}

func TestGatewayIdempotencyReplayRetainsCanonicalProcessEvents(t *testing.T) {
	eventStore := &recordingEventStore{}
	sink := NewDefaultToolEventSink(ToolEventSinkConfig{EventStore: eventStore, IDGenerator: fixedIDGenerator{}})
	def := defaultSearchDefinition()
	handlerCalls := 0
	gateway := NewGateway(GatewayConfig{
		Registry: NewStaticRegistry([]ToolDefinition{def}), SchemaValidator: BasicSchemaValidator{},
		EventStore: eventStore, ToolEventSink: sink, Idempotency: NewMemoryIdempotencyStore(),
		Executors: []ToolExecutor{NewFunctionExecutor(map[string]FunctionTool{
			"search": func(_ context.Context, call FunctionCall) (*FunctionResult, error) {
				handlerCalls++
				if err := call.ToolContext.EmitProgress(context.Background(), ToolProgressEvent{Message: "working"}); err != nil {
					t.Fatalf("emit progress: %v", err)
				}
				return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
			},
		})},
		IDGenerator: fixedIDGenerator{}, Clock: fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.IdempotencyKey = "process-replay"

	first, err := gateway.Invoke(testTraceContext(), req)
	if err != nil || first == nil || first.Status != ToolCallSucceeded {
		t.Fatalf("first result=%#v err=%v", first, err)
	}
	second, err := gateway.Invoke(testTraceContext(), req)
	if err != nil || second == nil || second.Status != ToolCallSucceeded {
		t.Fatalf("replay result=%#v err=%v", second, err)
	}
	if handlerCalls != 1 || len(eventStore.events) != 3 {
		t.Fatalf("handler_calls=%d persisted=%#v", handlerCalls, eventTypes(eventStore.events))
	}
	wantTypes := []observability.EventType{observability.EventToolCallStarted, observability.EventToolCallProgress, observability.EventToolCallCompleted}
	assertGatewayEventSequence(t, eventStore.events, wantTypes)
	assertGatewayResultEventsMatchStore(t, first, eventStore.events)
	assertGatewayResultEventsMatchStore(t, second, eventStore.events)
	first.Events[1].PayloadPreview[0] = '['
	if string(second.Events[1].PayloadPreview) == string(first.Events[1].PayloadPreview) {
		t.Fatalf("idempotency replay events are not copy-safe")
	}
}

func TestGatewayPostStartFailuresRetainCanonicalProcessEvents(t *testing.T) {
	for _, tt := range []struct {
		name      string
		configure func(*ToolDefinition, *contextEmittingTestExecutor)
	}{
		{name: "output schema", configure: func(def *ToolDefinition, executor *contextEmittingTestExecutor) {
			def.OutputSchema = json.RawMessage(`{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"}}}`)
			executor.raw = &ToolRawResult{Data: json.RawMessage(`{"ok":"invalid"}`), MimeType: "application/json"}
		}},
		{name: "normalization", configure: func(def *ToolDefinition, executor *contextEmittingTestExecutor) {
			def.ResultPolicy = ToolOutputPolicy{ArtifactThresholdBytes: 1, MaxInlineBytes: 4096, MaxModelContextBytes: 128, MaxSSEPreviewBytes: 128}
			executor.raw = &ToolRawResult{Data: json.RawMessage(`{"message":"requires artifact"}`), MimeType: "application/json"}
		}},
		{name: "partial", configure: func(_ *ToolDefinition, executor *contextEmittingTestExecutor) {
			executor.raw = &ToolRawResult{Data: json.RawMessage(`{"partial":true}`), MimeType: "application/json", Partial: true}
			executor.err = errors.New("partial upstream failure")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			eventStore := &recordingEventStore{}
			sink := NewDefaultToolEventSink(ToolEventSinkConfig{EventStore: eventStore, IDGenerator: fixedIDGenerator{}})
			def := defaultSearchDefinition()
			executor := &contextEmittingTestExecutor{}
			tt.configure(&def, executor)
			gateway := NewGateway(GatewayConfig{
				Registry: NewStaticRegistry([]ToolDefinition{def}), SchemaValidator: BasicSchemaValidator{},
				EventStore: eventStore, ToolEventSink: sink, Executors: []ToolExecutor{executor},
				IDGenerator: fixedIDGenerator{}, Clock: fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
			})

			result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
			if err != nil || result == nil || result.Status != ToolCallFailed {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			wantTypes := []observability.EventType{observability.EventToolCallStarted, observability.EventToolCallProgress, observability.EventToolCallFailed}
			assertGatewayEventSequence(t, eventStore.events, wantTypes)
			assertGatewayResultEventsMatchStore(t, result, eventStore.events)
			if executor.contextCalls != 1 || executor.legacyCalls != 0 {
				t.Fatalf("context_calls=%d legacy_calls=%d", executor.contextCalls, executor.legacyCalls)
			}
		})
	}
}

func TestGatewayPropagatesResolvedSingleToolVersion(t *testing.T) {
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	gateway := newTestGateway(eventStore, map[string]FunctionTool{
		"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
			return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
		},
	}, def)
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.ToolVersion = ""

	result, err := gateway.Invoke(testTraceContext(), req)
	if err != nil {
		t.Fatalf("invoke empty version: %v", err)
	}
	if result.ToolVersion != def.Version {
		t.Fatalf("result version = %q, want %q", result.ToolVersion, def.Version)
	}
	for _, event := range result.Events {
		if !strings.Contains(string(event.PayloadPreview), `"tool_version":"`+def.Version+`"`) {
			t.Fatalf("event did not carry resolved version: %s", event.PayloadPreview)
		}
	}
}

func TestGatewayRegistersMultipleFunctionAndHTTPTools(t *testing.T) {
	eventStore := &recordingEventStore{}
	httpHits := map[string]string{}
	client := newHTTPExecutorClient(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		httpHits[r.URL.Path] = string(body)
		switch r.URL.Path {
		case "/orders/query":
			return newHTTPExecutorResponse(http.StatusOK, http.Header{"Content-Type": []string{"application/json"}}, []byte(`{"order_id":"order-1","status":"paid"}`)), nil
		case "/users/query":
			return newHTTPExecutorResponse(http.StatusOK, http.Header{"Content-Type": []string{"application/json"}}, []byte(`{"user_id":"user-1","tier":"gold"}`)), nil
		default:
			t.Fatalf("unexpected HTTP path: %s", r.URL.Path)
			return nil, nil
		}
	})

	functionCalls := map[string]string{}
	registry := NewStaticRegistry([]ToolDefinition{
		{
			Name:        "search_kb",
			Version:     "v1",
			Type:        ToolTypeFunction,
			InputSchema: json.RawMessage(`{"type":"object","required":["query"],"properties":{"query":{"type":"string"}}}`),
			RiskLevel:   RiskLow,
			Visibility:  observability.VisibilityUserVisible,
			Function:    &FunctionToolSpec{HandlerName: "search"},
			Permissions: ToolPermissions{AllowedAgents: []string{"agent-a"}},
		},
		{
			Name:        "calc_price",
			Version:     "v1",
			Type:        ToolTypeFunction,
			InputSchema: json.RawMessage(`{"type":"object","required":["sku"],"properties":{"sku":{"type":"string"}}}`),
			RiskLevel:   RiskLow,
			Visibility:  observability.VisibilityUserVisible,
			Function:    &FunctionToolSpec{HandlerName: "price"},
			Permissions: ToolPermissions{AllowedAgents: []string{"agent-a"}},
		},
		{
			Name:        "query_order",
			Version:     "v1",
			Type:        ToolTypeHTTP,
			InputSchema: json.RawMessage(`{"type":"object","required":["order_id"],"properties":{"order_id":{"type":"string"}}}`),
			RiskLevel:   RiskLow,
			Visibility:  observability.VisibilityUserVisible,
			HTTP:        &HTTPToolSpec{Method: http.MethodPost, URL: "https://tools.example.test/orders/query", ResponseMode: "json"},
			Permissions: ToolPermissions{AllowedAgents: []string{"agent-a"}},
		},
		{
			Name:        "query_user",
			Version:     "v1",
			Type:        ToolTypeHTTP,
			InputSchema: json.RawMessage(`{"type":"object","required":["user_id"],"properties":{"user_id":{"type":"string"}}}`),
			RiskLevel:   RiskLow,
			Visibility:  observability.VisibilityUserVisible,
			HTTP:        &HTTPToolSpec{Method: http.MethodPost, URL: "https://tools.example.test/users/query", ResponseMode: "json"},
			Permissions: ToolPermissions{AllowedAgents: []string{"agent-a"}},
		},
	})
	gateway := NewGateway(GatewayConfig{
		Registry:        registry,
		SchemaValidator: BasicSchemaValidator{},
		EventStore:      eventStore,
		Executors: []ToolExecutor{
			NewFunctionExecutor(map[string]FunctionTool{
				"search": func(_ context.Context, call FunctionCall) (*FunctionResult, error) {
					functionCalls["search"] = string(call.Arguments)
					return &FunctionResult{Data: json.RawMessage(`{"items":["doc-a"]}`), MimeType: "application/json"}, nil
				},
				"price": func(_ context.Context, call FunctionCall) (*FunctionResult, error) {
					functionCalls["price"] = string(call.Arguments)
					return &FunctionResult{Data: json.RawMessage(`{"sku":"sku-1","price":42}`), MimeType: "application/json"}, nil
				},
			}),
			NewHTTPExecutor(client),
		},
		IDGenerator: fixedIDGenerator{},
		Clock:       fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})

	cases := []struct {
		toolCallID string
		toolName   string
		args       json.RawMessage
		want       string
	}{
		{toolCallID: "tc-search", toolName: "search_kb", args: json.RawMessage(`{"query":"hotel"}`), want: `{"items":["doc-a"]}`},
		{toolCallID: "tc-price", toolName: "calc_price", args: json.RawMessage(`{"sku":"sku-1"}`), want: `{"sku":"sku-1","price":42}`},
		{toolCallID: "tc-order", toolName: "query_order", args: json.RawMessage(`{"order_id":"order-1"}`), want: `{"order_id":"order-1","status":"paid"}`},
		{toolCallID: "tc-user", toolName: "query_user", args: json.RawMessage(`{"user_id":"user-1"}`), want: `{"user_id":"user-1","tier":"gold"}`},
	}
	for _, tc := range cases {
		req := baseToolRequest(tc.args)
		req.ToolCallID = tc.toolCallID
		req.ToolName = tc.toolName
		result, err := gateway.Invoke(testTraceContext(), req)
		if err != nil {
			t.Fatalf("invoke %s: %v", tc.toolName, err)
		}
		if result.Status != ToolCallSucceeded {
			t.Fatalf("%s status = %s", tc.toolName, result.Status)
		}
		if string(result.ResultPreview) != tc.want {
			t.Fatalf("%s preview = %s, want %s", tc.toolName, result.ResultPreview, tc.want)
		}
	}

	if functionCalls["search"] != `{"query":"hotel"}` || functionCalls["price"] != `{"sku":"sku-1"}` {
		t.Fatalf("function handlers were not both invoked: %#v", functionCalls)
	}
	if httpHits["/orders/query"] != `{"order_id":"order-1"}` || httpHits["/users/query"] != `{"user_id":"user-1"}` {
		t.Fatalf("HTTP tools were not both invoked: %#v", httpHits)
	}
	if len(eventStore.events) != len(cases)*2 {
		t.Fatalf("expected started/completed for each tool, got %d events", len(eventStore.events))
	}
	for i := 0; i < len(eventStore.events); i += 2 {
		if eventStore.events[i].EventType != observability.EventToolCallStarted || eventStore.events[i+1].EventType != observability.EventToolCallCompleted {
			t.Fatalf("unexpected event pair at %d: %s, %s", i, eventStore.events[i].EventType, eventStore.events[i+1].EventType)
		}
	}
}

func TestGatewayOffloadsLargeHTTPResponse(t *testing.T) {
	const rawResponse = `{"text":"abcdefghijklmnopqrstuvwxyz-http-response","count":1}`
	artifactStore, ctx := newScopedArtifactStore(t)
	eventStore := &recordingEventStore{}
	roundTrips := 0
	client := newHTTPExecutorClient(func(req *http.Request) (*http.Response, error) {
		roundTrips++
		if req.URL.String() != "https://tools.example.test/v1/large-search" {
			t.Fatalf("fixed URL=%q", req.URL.String())
		}
		return newHTTPExecutorResponse(http.StatusOK, http.Header{
			"Content-Type": []string{"application/json"},
		}, []byte(rawResponse)), nil
	})
	def := ToolDefinition{
		Name:        "search_http",
		Version:     "v1",
		Type:        ToolTypeHTTP,
		InputSchema: json.RawMessage(`{"type":"object","required":["query"],"properties":{"query":{"type":"string"}}}`),
		RiskLevel:   RiskLow,
		Visibility:  observability.VisibilityUserVisible,
		HTTP: &HTTPToolSpec{
			Method:       http.MethodPost,
			URL:          "https://tools.example.test/v1/large-search",
			ResponseMode: "json",
		},
		Permissions: ToolPermissions{AllowedAgents: []string{"agent-a"}},
		ResultPolicy: ToolOutputPolicy{
			ArtifactThresholdBytes: 24,
			MaxSSEPreviewBytes:     24,
			MaxModelContextBytes:   32,
			RedactSensitiveFields:  true,
		},
	}
	gateway := NewGateway(GatewayConfig{
		Registry:        NewStaticRegistry([]ToolDefinition{def}),
		SchemaValidator: BasicSchemaValidator{},
		EventStore:      eventStore,
		ArtifactStore:   artifactStore,
		Executors:       []ToolExecutor{NewHTTPExecutor(client)},
		IDGenerator:     fixedIDGenerator{},
		Clock:           fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.ToolName = def.Name
	req.ToolVersion = def.Version

	result, err := gateway.Invoke(ctx, req)
	if err != nil {
		t.Fatalf("invoke large HTTP tool: %v", err)
	}
	if result.Status != ToolCallSucceeded || roundTrips != 1 {
		t.Fatalf("status=%s round_trips=%d", result.Status, roundTrips)
	}
	if result.ResultRef == "" || len(result.Usage.ArtifactRefs) != 1 || result.Usage.ArtifactRefs[0] != result.ResultRef {
		t.Fatalf("HTTP result artifact layering=%#v", result)
	}
	if string(result.ResultPreview) == rawResponse || string(result.ModelContextResult) == rawResponse {
		t.Fatalf("large HTTP response remained fully inline: preview=%s model=%s", result.ResultPreview, result.ModelContextResult)
	}
	if got := eventTypes(eventStore.events); len(got) != 3 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolArtifactCreated || got[2] != observability.EventToolCallCompleted {
		t.Fatalf("large HTTP lifecycle=%#v", got)
	}
	if eventStore.events[1].PayloadRef != result.ResultRef || eventStore.events[2].PayloadRef != result.ResultRef {
		t.Fatalf("artifact/completed refs=%q/%q want=%q", eventStore.events[1].PayloadRef, eventStore.events[2].PayloadRef, result.ResultRef)
	}
	meta, content := readArtifactForTest(t, ctx, artifactStore, result.ResultRef)
	if string(content) != rawResponse || meta.MimeType != "application/json" || meta.SizeBytes != int64(len(rawResponse)) || meta.Hash == "" {
		t.Fatalf("authoritative HTTP artifact meta=%#v content=%s", meta, content)
	}
	var completed map[string]any
	if err := json.Unmarshal(eventStore.events[2].PayloadPreview, &completed); err != nil {
		t.Fatalf("decode completed HTTP payload: %v", err)
	}
	if completed["result_ref"] != meta.ArtifactRef || completed["mime_type"] != meta.MimeType || completed["hash"] != meta.Hash || completed["size_bytes"] != float64(meta.SizeBytes) {
		t.Fatalf("completed HTTP artifact metadata=%#v want=%#v", completed, meta)
	}
}

func TestGatewayBlocksHighRiskHTTPBeforeRoundTrip(t *testing.T) {
	eventStore := &recordingEventStore{}
	roundTrips := 0
	client := newHTTPExecutorClient(func(_ *http.Request) (*http.Response, error) {
		roundTrips++
		return newHTTPExecutorResponse(http.StatusOK, http.Header{"Content-Type": []string{"application/json"}}, []byte(`{"deleted":true}`)), nil
	})
	def := ToolDefinition{
		Name:        "delete_http",
		Version:     "v1",
		Type:        ToolTypeHTTP,
		InputSchema: json.RawMessage(`{"type":"object","required":["target"],"properties":{"target":{"type":"string"}}}`),
		RiskLevel:   RiskHigh,
		Visibility:  observability.VisibilityUserVisible,
		HTTP: &HTTPToolSpec{
			Method:       http.MethodDelete,
			URL:          "https://tools.example.test/v1/delete",
			ResponseMode: "json",
			Write:        true,
		},
		Permissions: ToolPermissions{AllowedAgents: []string{"agent-a"}},
	}
	gateway := NewGateway(GatewayConfig{
		Registry:        NewStaticRegistry([]ToolDefinition{def}),
		SchemaValidator: BasicSchemaValidator{},
		EventStore:      eventStore,
		Executors:       []ToolExecutor{NewHTTPExecutor(client)},
		IDGenerator:     fixedIDGenerator{},
		Clock:           fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})
	req := baseToolRequest(json.RawMessage(`{"target":"record-1"}`))
	req.ToolName = def.Name
	req.ToolVersion = def.Version
	req.Policy.RiskLevel = RiskHigh

	result, err := gateway.Invoke(testTraceContext(), req)
	if err != nil {
		t.Fatalf("high-risk HTTP canonical failure: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeControlRequired, false)
	if roundTrips != 0 {
		t.Fatalf("high-risk HTTP reached RoundTrip %d times", roundTrips)
	}
	if got := eventTypes(eventStore.events); len(got) != 1 || got[0] != observability.EventToolCallFailed {
		t.Fatalf("high-risk HTTP lifecycle=%#v, want pre-execution failed only", got)
	}
}

func TestGatewayRejectsInvalidArgumentsWithoutExecutingAndEmitsFailed(t *testing.T) {
	eventStore := &recordingEventStore{}
	called := false
	gateway := newTestGateway(eventStore, map[string]FunctionTool{
		"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
			called = true
			return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
		},
	}, ToolDefinition{
		Name:         "search_kb",
		Version:      "v1",
		Type:         ToolTypeFunction,
		InputSchema:  json.RawMessage(`{"type":"object","required":["query"],"properties":{"query":{"type":"string"}}}`),
		RiskLevel:    RiskLow,
		Visibility:   observability.VisibilityUserVisible,
		Function:     &FunctionToolSpec{HandlerName: "search"},
		ResultPolicy: ToolOutputPolicy{},
		Permissions:  ToolPermissions{AllowedAgents: []string{"agent-a"}},
	})

	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"limit":3}`)))
	if err != nil {
		t.Fatalf("schema failure should be returned as ToolCallResult, got error: %v", err)
	}
	if called {
		t.Fatalf("executor should not be called after schema validation failure")
	}
	assertFailedResult(t, result, ErrorTypeSchemaValidationFailed, false)
	if !result.Failure.Retryable || result.Failure.ModelGuidance.RetryBudget != 1 {
		t.Fatalf("schema failure recovery = %#v", result.Failure)
	}
	if !strings.Contains(string(result.ModelContextResult), "/query") ||
		!strings.Contains(string(result.ModelContextResult), `"retry_budget":1`) {
		t.Fatalf("schema repair context = %s", result.ModelContextResult)
	}
	assertOnlyFailedEvent(t, eventStore, ErrorTypeSchemaValidationFailed)
}

func TestGatewayRejectsDisallowedAgentWithoutExecuting(t *testing.T) {
	eventStore := &recordingEventStore{}
	called := false
	gateway := newTestGateway(eventStore, map[string]FunctionTool{
		"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
			called = true
			return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
		},
	}, ToolDefinition{
		Name:        "search_kb",
		Version:     "v1",
		Type:        ToolTypeFunction,
		InputSchema: json.RawMessage(`{"type":"object","required":["query"],"properties":{"query":{"type":"string"}}}`),
		RiskLevel:   RiskLow,
		Visibility:  observability.VisibilityUserVisible,
		Function:    &FunctionToolSpec{HandlerName: "search"},
		Permissions: ToolPermissions{AllowedAgents: []string{"agent-b"}},
	})

	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("permission failure should be returned as ToolCallResult, got error: %v", err)
	}
	if called {
		t.Fatalf("executor should not be called after permission denial")
	}
	assertFailedResult(t, result, ErrorTypePermissionDenied, false)
	assertOnlyFailedEvent(t, eventStore, ErrorTypePermissionDenied)
}

func TestGatewayRejectsHighRiskToolWithoutControlPort(t *testing.T) {
	eventStore := &recordingEventStore{}
	called := false
	gateway := newTestGateway(eventStore, map[string]FunctionTool{
		"delete": func(context.Context, FunctionCall) (*FunctionResult, error) {
			called = true
			return &FunctionResult{Data: json.RawMessage(`{"deleted":true}`)}, nil
		},
	}, ToolDefinition{
		Name:        "search_kb",
		Version:     "v1",
		Type:        ToolTypeFunction,
		InputSchema: json.RawMessage(`{"type":"object","required":["query"],"properties":{"query":{"type":"string"}}}`),
		RiskLevel:   RiskHigh,
		Visibility:  observability.VisibilityUserVisible,
		Function:    &FunctionToolSpec{HandlerName: "delete"},
		Permissions: ToolPermissions{AllowedAgents: []string{"agent-a"}},
	})

	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("control-required failure should be returned as ToolCallResult, got error: %v", err)
	}
	if called {
		t.Fatalf("executor should not be called for high risk tool without control")
	}
	assertFailedResult(t, result, ErrorTypeControlRequired, false)
	assertOnlyFailedEvent(t, eventStore, ErrorTypeControlRequired)
}

func TestGatewayDoesNotExecuteWhenStartedEventAppendFails(t *testing.T) {
	eventStore := &recordingEventStore{err: errors.New("event store down")}
	called := false
	gateway := newTestGateway(eventStore, map[string]FunctionTool{
		"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
			called = true
			return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
		},
	}, defaultSearchDefinition())

	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err == nil {
		t.Fatalf("started event append failure should return infrastructure error")
	}
	if result != nil {
		t.Fatalf("started event append failure should not return result: %#v", result)
	}
	if called {
		t.Fatalf("executor must not run when started event append fails")
	}
}

func TestGatewayRequiresEventStoreBeforeExecution(t *testing.T) {
	called := false
	gateway := NewGateway(GatewayConfig{
		Registry: NewStaticRegistry([]ToolDefinition{defaultSearchDefinition()}),
		Executors: []ToolExecutor{NewFunctionExecutor(map[string]FunctionTool{
			"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
				called = true
				return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
			},
		})},
	})
	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if result != nil || !IsErrorType(err, ErrorTypeInternal) || called {
		t.Fatalf("result=%#v err=%v called=%v", result, err, called)
	}
}

func TestGatewayReturnsErrorWhenFailedEventCannotPersist(t *testing.T) {
	store := &recordingEventStore{err: errors.New("event store down")}
	gateway := newTestGateway(store, nil, defaultSearchDefinition())
	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"missing":true}`)))
	if result != nil || err == nil {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestGatewayRejectsNilEventAppendResult(t *testing.T) {
	called := false
	store := &recordingEventStore{nilResult: true}
	gateway := newTestGateway(store, map[string]FunctionTool{
		"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
			called = true
			return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
		},
	}, defaultSearchDefinition())

	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if result != nil || !IsErrorType(err, ErrorTypeInternal) || called {
		t.Fatalf("result=%#v err=%v called=%v", result, err, called)
	}
}

func TestGatewayRejectsEmptyEventAppendResultBeforeExecution(t *testing.T) {
	assertGatewayRejectsEventAppendResultBeforeExecution(t, &EventAppendResult{})
}

func TestGatewayRejectsInvalidEventAppendResultBeforeExecution(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*observability.AgentEvent)
	}{
		{name: "noncanonical schema", mutate: func(event *observability.AgentEvent) { event.SchemaVersion = "tool_event.v1" }},
		{name: "nonpositive sequence", mutate: func(event *observability.AgentEvent) { event.Sequence = 0 }},
		{name: "zero created at", mutate: func(event *observability.AgentEvent) { event.CreatedAt = time.Time{} }},
		{name: "empty event id", mutate: func(event *observability.AgentEvent) { event.EventID = "" }},
		{name: "empty trace id", mutate: func(event *observability.AgentEvent) { event.TraceID = "" }},
		{name: "mismatched run id", mutate: func(event *observability.AgentEvent) { event.RunID = "run-other" }},
		{name: "mismatched session id", mutate: func(event *observability.AgentEvent) { event.SessionID = "sess-other" }},
		{name: "mismatched step id", mutate: func(event *observability.AgentEvent) { event.StepID = "step-other" }},
		{name: "mismatched agent id", mutate: func(event *observability.AgentEvent) { event.AgentID = "agent-other" }},
		{name: "noncanonical event type", mutate: func(event *observability.AgentEvent) { event.EventType = observability.EventType("tool_warning") }},
		{name: "mismatched event type", mutate: func(event *observability.AgentEvent) { event.EventType = observability.EventToolCallCompleted }},
		{name: "empty idempotency key", mutate: func(event *observability.AgentEvent) { event.IdempotencyKey = "" }},
		{name: "mismatched idempotency key", mutate: func(event *observability.AgentEvent) { event.IdempotencyKey = "run-1:tc-other:started" }},
		{name: "noncanonical visibility", mutate: func(event *observability.AgentEvent) { event.Visibility = observability.EventVisibility("public") }},
		{name: "mismatched visibility", mutate: func(event *observability.AgentEvent) { event.Visibility = observability.VisibilityDebug }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := validPersistedStartedEvent()
			tt.mutate(&event)
			assertGatewayRejectsEventAppendResultBeforeExecution(t, &EventAppendResult{Event: event})
		})
	}
}

func TestGatewayAcceptsReplaySafeEventAppendResult(t *testing.T) {
	submitted := validPersistedStartedEvent()
	submitted.Sequence = 0
	persisted := submitted
	persisted.EventID = "evt-first-persisted"
	persisted.TraceID = "trace-first-attempt"
	persisted.Sequence = 7
	persisted.CreatedAt = persisted.CreatedAt.Add(-time.Minute)
	gateway := NewGateway(GatewayConfig{
		EventStore: &recordingEventStore{appendResult: &EventAppendResult{Event: persisted}},
	})

	result, err := gateway.appendEvent(context.Background(), submitted)
	if err != nil {
		t.Fatalf("append replay-safe result: %v", err)
	}
	if result.EventID != persisted.EventID || result.TraceID != persisted.TraceID || result.Sequence != persisted.Sequence {
		t.Fatalf("persisted event = %#v", result)
	}
}

func TestNormalizeToolArtifactEventSetsStableIdempotencyKey(t *testing.T) {
	event, err := NormalizeToolEvent(fixedIDGenerator{}, ToolEvent{
		EventType:      ToolEventArtifactCreated,
		RunID:          "run-1",
		ToolCallID:     "tc-1",
		ToolName:       "search_kb",
		ToolVersion:    "v1",
		ToolType:       ToolTypeFunction,
		PayloadPreview: json.RawMessage(`{"tool_call_id":"tc-1","tool_name":"search_kb","tool_version":"v1","tool_type":"function"}`),
	})
	if err != nil {
		t.Fatalf("normalize artifact tool event: %v", err)
	}
	if event.IdempotencyKey != "run-1:tc-1:artifact" {
		t.Fatalf("idempotency key = %q", event.IdempotencyKey)
	}
}

func TestGatewayRejectsNilRegistryWithoutPanic(t *testing.T) {
	gateway := NewGateway(GatewayConfig{
		SchemaValidator: BasicSchemaValidator{},
		EventStore:      &recordingEventStore{},
		Executors:       []ToolExecutor{NewFunctionExecutor(nil)},
		IDGenerator:     fixedIDGenerator{},
		Clock:           fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("nil registry should return an error, not panic: %v", recovered)
		}
	}()
	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if result != nil {
		t.Fatalf("nil registry should not return a result: %#v", result)
	}
	if !IsErrorType(err, ErrorTypeInternal) {
		t.Fatalf("expected internal_error, got %v", err)
	}
}

func TestGatewayEmitsFailedWhenExecutorMissing(t *testing.T) {
	eventStore := &recordingEventStore{}
	gateway := NewGateway(GatewayConfig{
		Registry:        NewStaticRegistry([]ToolDefinition{defaultSearchDefinition()}),
		SchemaValidator: BasicSchemaValidator{},
		EventStore:      eventStore,
		IDGenerator:     fixedIDGenerator{},
		Clock:           fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})

	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("missing executor should be returned as ToolCallResult, got error: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeToolNotFound, false)
	if got := eventTypes(eventStore.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolCallFailed {
		t.Fatalf("expected started then failed, got %#v", got)
	}
	assertGatewayResultEventsMatchStore(t, result, eventStore.events)
}

func TestFunctionExecutorRecoversPanic(t *testing.T) {
	eventStore := &recordingEventStore{}
	gateway := newTestGateway(eventStore, map[string]FunctionTool{
		"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
			panic("handler exploded")
		},
	}, defaultSearchDefinition())

	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("function panic should be returned as ToolCallResult, got error: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeInternal, true)
	if got := eventTypes(eventStore.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolCallFailed {
		t.Fatalf("expected started then failed, got %#v", got)
	}
}

func TestGatewayIdempotencyReturnsCachedResultAndDetectsConflict(t *testing.T) {
	eventStore := &recordingEventStore{}
	calls := 0
	idempotency := NewMemoryIdempotencyStore()
	gateway := NewGateway(GatewayConfig{
		Registry:        NewStaticRegistry([]ToolDefinition{defaultSearchDefinition()}),
		SchemaValidator: BasicSchemaValidator{},
		EventStore:      eventStore,
		Idempotency:     idempotency,
		Executors: []ToolExecutor{NewFunctionExecutor(map[string]FunctionTool{
			"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
				calls++
				return &FunctionResult{Data: json.RawMessage(`{"count":1}`), MimeType: "application/json"}, nil
			},
		})},
		IDGenerator: fixedIDGenerator{},
		Clock:       fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})

	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.IdempotencyKey = "idem-1"
	first, err := gateway.Invoke(testTraceContext(), req)
	if err != nil {
		t.Fatalf("first invoke: %v", err)
	}
	second, err := gateway.Invoke(testTraceContext(), req)
	if err != nil {
		t.Fatalf("second invoke: %v", err)
	}
	if calls != 1 {
		t.Fatalf("same idempotency key and arguments should execute once, got %d calls", calls)
	}
	if string(first.ResultPreview) != string(second.ResultPreview) || second.ToolCallID != first.ToolCallID {
		t.Fatalf("second result should replay first result: first=%#v second=%#v", first, second)
	}
	eventsBeforeConflict := len(eventStore.events)

	conflictReq := baseToolRequest(json.RawMessage(`{"query":"airport"}`))
	conflictReq.Policy.IdempotencyKey = "idem-1"
	conflict, err := gateway.Invoke(testTraceContext(), conflictReq)
	if conflict != nil || !IsErrorType(err, ErrorTypeIdempotencyConflict) {
		t.Fatalf("idempotency conflict result=%#v err=%v", conflict, err)
	}
	if calls != 1 {
		t.Fatalf("conflicting idempotency key should not execute, got %d calls", calls)
	}
	if len(eventStore.events) != eventsBeforeConflict {
		t.Fatalf("idempotency conflict must not append canonical events: before=%d after=%d", eventsBeforeConflict, len(eventStore.events))
	}
}

func TestGatewayTimeoutEmitsFailedResult(t *testing.T) {
	eventStore := &recordingEventStore{}
	gateway := newTestGateway(eventStore, map[string]FunctionTool{
		"search": func(ctx context.Context, _ FunctionCall) (*FunctionResult, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}, defaultSearchDefinition())
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.Timeout = time.Nanosecond

	result, err := gateway.Invoke(testTraceContext(), req)
	if err != nil {
		t.Fatalf("timeout should be returned as ToolCallResult, got error: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeTimeout, true)
	if len(eventStore.events) != 2 || eventStore.events[0].EventType != observability.EventToolCallStarted || eventStore.events[1].EventType != observability.EventToolCallFailed {
		t.Fatalf("expected started then failed events, got %#v", eventTypes(eventStore.events))
	}
}

func TestGatewayCancelPropagatesToExecutor(t *testing.T) {
	eventStore := &recordingEventStore{}
	gateway := newTestGateway(eventStore, map[string]FunctionTool{
		"search": func(ctx context.Context, _ FunctionCall) (*FunctionResult, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}, defaultSearchDefinition())
	ctx, cancel := context.WithCancel(testTraceContext())
	cancel()

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("cancel should be returned as ToolCallResult, got error: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeCancelled, true)
}

func assertGatewayEventSequence(t *testing.T, events []observability.AgentEvent, want []observability.EventType) {
	t.Helper()
	if got := eventTypes(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("event types=%#v, want %#v", got, want)
	}
	for i := range events {
		if events[i].Sequence != int64(i+1) {
			t.Fatalf("event[%d] sequence=%d", i, events[i].Sequence)
		}
	}
}

func assertGatewayResultEventsMatchStore(t *testing.T, result *ToolCallResult, persisted []observability.AgentEvent) {
	t.Helper()
	if result == nil || len(result.Events) != len(persisted) {
		t.Fatalf("result events=%#v persisted=%#v", result, persisted)
	}
	for i := range persisted {
		if result.Events[i].Sequence != persisted[i].Sequence || result.Events[i].EventID != persisted[i].EventID || result.Events[i].EventType != persisted[i].EventType {
			t.Fatalf("result event[%d]=%#v persisted=%#v", i, result.Events[i], persisted[i])
		}
	}
}

type contextEmittingTestExecutor struct {
	raw          *ToolRawResult
	err          error
	contextCalls int
	legacyCalls  int
}

func (e *contextEmittingTestExecutor) Type() ToolType {
	return ToolTypeFunction
}

func (e *contextEmittingTestExecutor) Execute(context.Context, *ToolDefinition, ToolCallRequest) (*ToolRawResult, error) {
	e.legacyCalls++
	return e.raw, e.err
}

func (e *contextEmittingTestExecutor) ExecuteWithContext(_ context.Context, _ *ToolDefinition, _ ToolCallRequest, toolCtx ToolContext) (*ToolRawResult, error) {
	e.contextCalls++
	if toolCtx == nil {
		return nil, errors.New("missing ToolContext")
	}
	if err := toolCtx.EmitProgress(context.Background(), ToolProgressEvent{Message: "working"}); err != nil {
		return nil, err
	}
	return e.raw, e.err
}

type recordingEventStore struct {
	events       []observability.AgentEvent
	err          error
	nilResult    bool
	appendResult *EventAppendResult
}

func (s *recordingEventStore) AppendEvent(_ context.Context, event observability.AgentEvent) (*EventAppendResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.nilResult {
		return nil, nil
	}
	if s.appendResult != nil {
		return s.appendResult, nil
	}
	event.Sequence = int64(len(s.events) + 1)
	s.events = append(s.events, event)
	return &EventAppendResult{Event: event}, nil
}

type fixedIDGenerator struct{}

func (fixedIDGenerator) NewTraceID() string   { return "trace-generated" }
func (fixedIDGenerator) NewSpanID() string    { return "span-generated" }
func (fixedIDGenerator) NewRunID() string     { return "run-generated" }
func (fixedIDGenerator) NewEventID() string   { return "evt-generated" }
func (fixedIDGenerator) NewRequestID() string { return "req-generated" }

type fixedClock struct {
	now time.Time
}

func (c fixedClock) Now() time.Time {
	return c.now
}

func newTestGateway(eventStore *recordingEventStore, handlers map[string]FunctionTool, def ToolDefinition) *DefaultGateway {
	return NewGateway(GatewayConfig{
		Registry:        NewStaticRegistry([]ToolDefinition{def}),
		SchemaValidator: BasicSchemaValidator{},
		EventStore:      eventStore,
		Executors:       []ToolExecutor{NewFunctionExecutor(handlers)},
		IDGenerator:     fixedIDGenerator{},
		Clock:           fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})
}

func defaultSearchDefinition() ToolDefinition {
	return ToolDefinition{
		Name:        "search_kb",
		Version:     "v1",
		Type:        ToolTypeFunction,
		InputSchema: json.RawMessage(`{"type":"object","required":["query"],"properties":{"query":{"type":"string"}}}`),
		RiskLevel:   RiskLow,
		Timeout:     time.Second,
		Visibility:  observability.VisibilityUserVisible,
		Function:    &FunctionToolSpec{HandlerName: "search"},
		Permissions: ToolPermissions{AllowedAgents: []string{"agent-a"}},
	}
}

func TestNewProductionGatewayRejectsDevelopmentPersistence(t *testing.T) {
	productionEvents := productionEventStore{recordingEventStore: &recordingEventStore{}}
	base := GatewayConfig{
		Registry: NewStaticRegistry([]ToolDefinition{defaultSearchDefinition()}), EventStore: productionEvents,
		TerminalCommitter: productionTerminalCommitter{},
	}
	if _, err := NewProductionGateway(base); !errors.Is(err, ErrProductionDependencyInvalid) {
		t.Fatalf("nil idempotency error = %v", err)
	}
	base.Idempotency = NewMemoryIdempotencyStore()
	if _, err := NewProductionGateway(base); !errors.Is(err, ErrProductionDependencyInvalid) {
		t.Fatalf("memory idempotency error = %v", err)
	}
	base.Idempotency = productionIdempotencyStore{MemoryIdempotencyStore: NewMemoryIdempotencyStore()}
	base.EventStore = NewMemoryEventStore()
	if _, err := NewProductionGateway(base); !errors.Is(err, ErrProductionDependencyInvalid) {
		t.Fatalf("memory event store error = %v", err)
	}
	base.EventStore = productionEventStore{recordingEventStore: &recordingEventStore{}}
	base.SchemaValidator = BasicSchemaValidator{}
	if _, err := NewProductionGateway(base); !errors.Is(err, ErrProductionDependencyInvalid) {
		t.Fatalf("basic schema validator error = %v", err)
	}
}

func TestNewProductionGatewayAcceptsDurablePorts(t *testing.T) {
	events := productionEventStore{recordingEventStore: &recordingEventStore{}}
	gateway, err := NewProductionGateway(GatewayConfig{
		Registry:          NewStaticRegistry([]ToolDefinition{defaultSearchDefinition()}),
		EventStore:        events,
		Idempotency:       productionIdempotencyStore{MemoryIdempotencyStore: NewMemoryIdempotencyStore()},
		TerminalCommitter: productionTerminalCommitter{},
	})
	if err != nil || gateway == nil {
		t.Fatalf("production gateway = %#v, error = %v", gateway, err)
	}
}

// productionIdempotencyStore stands in for a durable adapter in constructor
// tests; behavior is covered independently by the idempotency conformance suite.
type productionIdempotencyStore struct {
	*MemoryIdempotencyStore
}

func (productionIdempotencyStore) ProductionReady() bool { return true }

type productionEventStore struct {
	*recordingEventStore
}

func (productionEventStore) ProductionReady() bool { return true }

type productionTerminalCommitter struct{}

func (productionTerminalCommitter) ProductionReady() bool { return true }

func (productionTerminalCommitter) CommitTerminal(_ context.Context, event observability.AgentEvent, _ ToolTerminalBinding) (*EventAppendResult, error) {
	return &EventAppendResult{Event: event}, nil
}

func testTraceContext() context.Context {
	return observability.WithTraceContext(context.Background(), observability.TraceContext{
		TraceID:   "trace-1",
		SpanID:    "span-parent",
		TenantID:  "tenant-a",
		UserID:    "user-a",
		SessionID: "sess-1",
		RunID:     "run-1",
		AgentID:   "agent-a",
	})
}

func baseToolRequest(arguments json.RawMessage) ToolCallRequest {
	return ToolCallRequest{
		ToolCallID:  "tc-1",
		TenantID:    "tenant-a",
		UserID:      "user-a",
		SessionID:   "sess-1",
		RunID:       "run-1",
		StepID:      "step-1",
		AgentID:     "agent-a",
		ToolName:    "search_kb",
		ToolVersion: "v1",
		Arguments:   arguments,
		Caller:      ToolCaller{Type: "runtime", AgentID: "agent-a"},
		Policy:      ToolCallPolicy{RiskLevel: RiskLow},
	}
}

func assertFailedResult(t *testing.T, result *ToolCallResult, errorType ErrorType, executed bool) {
	t.Helper()
	if result == nil {
		t.Fatalf("expected failed result, got nil")
	}
	if result.Status != ToolCallFailed {
		t.Fatalf("expected failed status, got %s", result.Status)
	}
	if result.Failure == nil {
		t.Fatalf("expected failure payload")
	}
	if result.Failure.ErrorType != string(errorType) {
		t.Fatalf("error type = %s, want %s", result.Failure.ErrorType, errorType)
	}
	if result.Failure.Executed != executed {
		t.Fatalf("executed = %v, want %v", result.Failure.Executed, executed)
	}
	if result.Failure.ModelGuidance.Instruction == "" {
		t.Fatalf("failure should include model guidance")
	}
}

func assertOnlyFailedEvent(t *testing.T, eventStore *recordingEventStore, errorType ErrorType) {
	t.Helper()
	if len(eventStore.events) != 1 {
		t.Fatalf("expected one failed event, got %d", len(eventStore.events))
	}
	event := eventStore.events[0]
	if event.EventType != observability.EventToolCallFailed {
		t.Fatalf("expected failed event, got %s", event.EventType)
	}
	if event.Error == nil || event.Error.Type != canonicalEventErrorType(errorType) {
		t.Fatalf("unexpected event error: %#v", event.Error)
	}
}

func assertGatewayRejectsEventAppendResultBeforeExecution(t *testing.T, appendResult *EventAppendResult) {
	t.Helper()
	called := false
	store := &recordingEventStore{appendResult: appendResult}
	gateway := newTestGateway(store, map[string]FunctionTool{
		"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
			called = true
			return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
		},
	}, defaultSearchDefinition())

	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if result != nil || !IsErrorType(err, ErrorTypeInternal) || called {
		t.Fatalf("result=%#v err=%v called=%v", result, err, called)
	}
}

func validPersistedStartedEvent() observability.AgentEvent {
	return observability.AgentEvent{
		EventID:        "evt-generated",
		SchemaVersion:  observability.AgentEventSchemaVersion,
		Sequence:       1,
		IdempotencyKey: "run-1:tc-1:started",
		TraceID:        "trace-1",
		SessionID:      "sess-1",
		RunID:          "run-1",
		StepID:         "step-1",
		AgentID:        "agent-a",
		EventType:      observability.EventToolCallStarted,
		Visibility:     observability.VisibilityUserVisible,
		CreatedAt:      time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC),
	}
}

func eventTypes(events []observability.AgentEvent) []observability.EventType {
	types := make([]observability.EventType, 0, len(events))
	for _, event := range events {
		types = append(types, event.EventType)
	}
	return types
}
