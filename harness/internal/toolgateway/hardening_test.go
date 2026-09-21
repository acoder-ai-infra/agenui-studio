package toolgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"go.uber.org/zap/zapcore"
)

func TestGatewayRejectsTrustedTraceIdentityMismatchBeforeReplay(t *testing.T) {
	eventStore := &recordingEventStore{}
	calls := 0
	gateway := NewGateway(GatewayConfig{
		Registry:        NewStaticRegistry([]ToolDefinition{defaultSearchDefinition()}),
		SchemaValidator: BasicSchemaValidator{},
		EventStore:      eventStore,
		Idempotency:     NewMemoryIdempotencyStore(),
		Executors: []ToolExecutor{NewFunctionExecutor(map[string]FunctionTool{
			"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
				calls++
				return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
			},
		})},
		IDGenerator: fixedIDGenerator{},
		Clock:       fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.IdempotencyKey = "identity-replay"

	first, err := gateway.Invoke(testTraceContext(), req)
	if err != nil || first.Status != ToolCallSucceeded {
		t.Fatalf("prime cached result: result=%#v err=%v", first, err)
	}
	req.SessionID = "sess-untrusted"
	result, err := gateway.Invoke(testTraceContext(), req)
	if result != nil || !IsErrorType(err, ErrorTypePermissionDenied) {
		t.Fatalf("trusted identity mismatch result=%#v err=%v, want direct permission_denied", result, err)
	}
	if calls != 1 {
		t.Fatalf("identity mismatch replayed or executed tool, calls=%d", calls)
	}
	if len(eventStore.events) != 2 {
		t.Fatalf("identity mismatch must not append events, got %#v", eventTypes(eventStore.events))
	}
}

func TestGatewayRejectsMissingRequiredExecutionIdentity(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*ToolCallRequest)
		errorType ErrorType
	}{
		{
			name: "missing step",
			mutate: func(req *ToolCallRequest) {
				req.StepID = ""
			},
			errorType: ErrorTypeInternal,
		},
		{
			name: "missing tool name",
			mutate: func(req *ToolCallRequest) {
				req.ToolName = ""
			},
			errorType: ErrorTypeInternal,
		},
		{
			name: "caller agent conflict",
			mutate: func(req *ToolCallRequest) {
				req.Caller.AgentID = "agent-untrusted"
			},
			errorType: ErrorTypePermissionDenied,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eventStore := &recordingEventStore{}
			called := false
			gateway := newTestGateway(eventStore, map[string]FunctionTool{
				"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
					called = true
					return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
				},
			}, defaultSearchDefinition())
			req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
			tt.mutate(&req)

			result, err := gateway.Invoke(testTraceContext(), req)
			if result != nil || !IsErrorType(err, tt.errorType) {
				t.Fatalf("result=%#v err=%v, want direct %s", result, err, tt.errorType)
			}
			if called || len(eventStore.events) != 0 {
				t.Fatalf("invalid identity dispatched=%v events=%#v", called, eventTypes(eventStore.events))
			}
		})
	}
}

func TestDefaultPolicyEngineDeniesEmptyAgentBinding(t *testing.T) {
	def := defaultSearchDefinition()
	def.Permissions.AllowedAgents = nil
	decision, err := (DefaultPolicyEngine{}).EvaluateToolCall(
		testTraceContext(),
		&def,
		baseToolRequest(json.RawMessage(`{"query":"hotel"}`)),
	)
	if err != nil {
		t.Fatalf("evaluate policy: %v", err)
	}
	if decision == nil || decision.Decision != DecisionBlock || decision.ReasonCode != string(ErrorTypePermissionDenied) {
		t.Fatalf("empty binding decision=%#v, want permission block", decision)
	}
}

func TestDefaultPolicyEngineAllowsWildcardAgentBinding(t *testing.T) {
	def := defaultSearchDefinition()
	def.Permissions.AllowedAgents = []string{"*"}
	decision, err := (DefaultPolicyEngine{}).EvaluateToolCall(
		testTraceContext(),
		&def,
		baseToolRequest(json.RawMessage(`{"query":"hotel"}`)),
	)
	if err != nil {
		t.Fatalf("evaluate policy: %v", err)
	}
	if decision == nil || decision.Decision != DecisionAllow {
		t.Fatalf("wildcard binding decision=%#v, want allow", decision)
	}
}

func TestGatewayFailsClosedWhenRequestRequiresApproval(t *testing.T) {
	eventStore := &recordingEventStore{}
	called := false
	gateway := newTestGateway(eventStore, map[string]FunctionTool{
		"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
			called = true
			return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
		},
	}, defaultSearchDefinition())
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.RequireApproval = true

	result, err := gateway.Invoke(testTraceContext(), req)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if called {
		t.Fatalf("approval-required request must not execute without a control port")
	}
	assertFailedResult(t, result, ErrorTypeControlRequired, false)
	assertOnlyFailedEvent(t, eventStore, ErrorTypeControlRequired)
}

func TestGatewayRejectsInvalidTenantScope(t *testing.T) {
	eventStore := &recordingEventStore{}
	called := false
	def := defaultSearchDefinition()
	def.Permissions.TenantScope = "organization"
	gateway := newTestGateway(eventStore, map[string]FunctionTool{
		"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
			called = true
			return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
		},
	}, def)

	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if called {
		t.Fatalf("tool with an unknown tenant scope must not execute")
	}
	assertFailedResult(t, result, ErrorTypePermissionDenied, false)
}

func TestGatewayEnforcesTenantScopeIdentityShape(t *testing.T) {
	tests := []struct {
		name       string
		scope      string
		tenantID   string
		userID     string
		wantAllow  bool
		wantDirect ErrorType
	}{
		{name: "empty scope requires tenant", scope: "", userID: "user-a", wantDirect: ErrorTypeInternal},
		{name: "empty scope accepts tenant", scope: "", tenantID: "tenant-a", wantAllow: true},
		{name: "tenant scope requires tenant", scope: "tenant", userID: "user-a", wantDirect: ErrorTypeInternal},
		{name: "tenant scope accepts tenant", scope: "tenant", tenantID: "tenant-a", wantAllow: true},
		{name: "user scope requires tenant", scope: "user", userID: "user-a", wantDirect: ErrorTypeInternal},
		{name: "user scope requires user", scope: "user", tenantID: "tenant-a"},
		{name: "user scope accepts tenant and user", scope: "user", tenantID: "tenant-a", userID: "user-a", wantAllow: true},
		{name: "system scope requires tenant", scope: "system", wantDirect: ErrorTypeInternal},
		{name: "system scope accepts tenant", scope: "system", tenantID: "tenant-a", wantAllow: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			eventStore := &recordingEventStore{}
			def := defaultSearchDefinition()
			def.Permissions.TenantScope = tt.scope
			gateway := newTestGateway(eventStore, map[string]FunctionTool{
				"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
					called = true
					return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
				},
			}, def)
			req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
			req.TenantID = tt.tenantID
			req.UserID = tt.userID

			result, err := gateway.Invoke(traceContextWithIdentity(tt.tenantID, tt.userID), req)
			if tt.wantDirect != "" {
				if result != nil || !IsErrorType(err, tt.wantDirect) {
					t.Fatalf("result=%#v err=%v, want direct %s", result, err, tt.wantDirect)
				}
				if called || len(eventStore.events) != 0 {
					t.Fatalf("missing identity dispatched=%v events=%#v", called, eventTypes(eventStore.events))
				}
				return
			}
			if err != nil {
				t.Fatalf("invoke: %v", err)
			}
			if tt.wantAllow {
				if result.Status != ToolCallSucceeded || !called {
					t.Fatalf("expected allowed invocation, result=%#v called=%v", result, called)
				}
				return
			}
			if called {
				t.Fatalf("scope identity failure must prevent execution")
			}
			assertFailedResult(t, result, ErrorTypePermissionDenied, false)
		})
	}
}

func TestGatewayRejectsTenantScopeIdentityMismatch(t *testing.T) {
	tests := []struct {
		name        string
		scope       string
		traceTenant string
		traceUser   string
	}{
		{name: "default tenant mismatch", traceTenant: "tenant-b", traceUser: "user-a"},
		{name: "tenant mismatch", scope: "tenant", traceTenant: "tenant-b", traceUser: "user-a"},
		{name: "user tenant mismatch", scope: "user", traceTenant: "tenant-b", traceUser: "user-a"},
		{name: "user identity mismatch", scope: "user", traceTenant: "tenant-a", traceUser: "user-b"},
		{name: "system tenant mismatch", scope: "system", traceTenant: "tenant-b", traceUser: "user-a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			eventStore := &recordingEventStore{}
			def := defaultSearchDefinition()
			def.Permissions.TenantScope = tt.scope
			gateway := newTestGateway(eventStore, map[string]FunctionTool{
				"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
					called = true
					return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
				},
			}, def)
			result, err := gateway.Invoke(
				traceContextWithIdentity(tt.traceTenant, tt.traceUser),
				baseToolRequest(json.RawMessage(`{"query":"hotel"}`)),
			)
			if result != nil || !IsErrorType(err, ErrorTypePermissionDenied) {
				t.Fatalf("mismatch result=%#v err=%v, want direct permission_denied", result, err)
			}
			if called || len(eventStore.events) != 0 {
				t.Fatalf("mismatched trusted identity dispatched=%v events=%#v", called, eventTypes(eventStore.events))
			}
		})
	}
}

func TestGatewayStillEnforcesRequiredPermissionScopes(t *testing.T) {
	def := defaultSearchDefinition()
	def.Permissions.RequiredScopes = []string{"hotel:search"}
	result, err := newTestGateway(&recordingEventStore{}, nil, def).Invoke(
		testTraceContext(),
		baseToolRequest(json.RawMessage(`{"query":"hotel"}`)),
	)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	assertFailedResult(t, result, ErrorTypePermissionDenied, false)
}

func TestGatewaySanitizesArgumentsPreview(t *testing.T) {
	store := &recordingEventStore{}
	gateway := newTestGateway(store, map[string]FunctionTool{
		"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
			return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
		},
	}, defaultSearchDefinition())
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.ArgumentsPreview = map[string]any{
		"query":    "hotel",
		"password": "secret",
		"nested": map[string]any{
			"token":         "abc",
			"authorization": "Bearer x",
		},
	}

	_, err := gateway.Invoke(testTraceContext(), req)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	payload := string(store.events[0].PayloadPreview)
	if strings.Contains(payload, "secret") || strings.Contains(payload, "abc") || strings.Contains(payload, "Bearer x") || !strings.Contains(payload, "[REDACTED]") {
		t.Fatalf("unsafe started payload: %s", payload)
	}
}

func TestGatewayTruncatesArgumentsPreview(t *testing.T) {
	store := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.ResultPolicy.MaxSSEPreviewBytes = 24
	gateway := newTestGateway(store, map[string]FunctionTool{
		"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
			return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
		},
	}, def)
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.ArgumentsPreview = map[string]any{"query": strings.Repeat("long-preview-value", 8)}

	if _, err := gateway.Invoke(testTraceContext(), req); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	payload := string(store.events[0].PayloadPreview)
	if strings.Contains(payload, strings.Repeat("long-preview-value", 8)) || !strings.Contains(payload, "...") {
		t.Fatalf("arguments preview was not safely truncated: %s", payload)
	}
}

func TestGatewayOffloadsLargeInlineArguments(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.ResultPolicy.MaxInlineBytes = 16
	def.ResultPolicy.MaxSSEPreviewBytes = 20
	arguments := json.RawMessage(`{"query":"full-hotel-search-value-that-must-not-reach-events"}`)
	executedArguments := ""
	gateway := newGatewayForHardening(eventStore, artifactStore, nil, nil, def, func(_ context.Context, call FunctionCall) (*FunctionResult, error) {
		executedArguments = string(call.Arguments)
		if got := eventTypes(eventStore.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolArtifactCreated {
			t.Fatalf("argument artifact event must be persisted before execution, got %#v", got)
		}
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})
	req := baseToolRequest(arguments)
	req.ArgumentsPreview = map[string]any{"query": "full-hotel-search-value-that-must-not-reach-events"}

	result, err := gateway.Invoke(ctx, req)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if result.Status != ToolCallSucceeded {
		t.Fatalf("status = %s", result.Status)
	}
	if executedArguments != string(arguments) {
		t.Fatalf("executor arguments = %s, want %s", executedArguments, arguments)
	}
	if got := eventTypes(eventStore.events); len(got) != 3 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolArtifactCreated || got[2] != observability.EventToolCallCompleted {
		t.Fatalf("unexpected event order: %#v", got)
	}
	started := eventStore.events[0]
	if started.DebugRef == "" {
		t.Fatalf("started event should reference the argument artifact")
	}
	for _, event := range eventStore.events {
		if strings.Contains(string(event.PayloadPreview), "full-hotel-search-value-that-must-not-reach-events") || strings.Contains(string(event.PayloadPreview), string(arguments)) {
			t.Fatalf("raw arguments leaked into %s: %s", event.EventType, event.PayloadPreview)
		}
	}
	if eventStore.events[1].DebugRef != started.DebugRef {
		t.Fatalf("argument artifact event ref = %q, want %q", eventStore.events[1].DebugRef, started.DebugRef)
	}
	if eventStore.events[1].Visibility != observability.VisibilityDebug {
		t.Fatalf("argument artifact event visibility = %q, want debug", eventStore.events[1].Visibility)
	}
	wantArtifactEventKey := "run-1:tc-1:artifact:" + hashString(started.DebugRef)[:24]
	if eventStore.events[1].IdempotencyKey != wantArtifactEventKey {
		t.Fatalf("argument artifact event idempotency key = %q, want %q", eventStore.events[1].IdempotencyKey, wantArtifactEventKey)
	}
	object, err := artifactStore.Get(ctx, started.DebugRef, artifact.GetOptions{Purpose: artifact.PurposeDebug})
	if err != nil {
		t.Fatalf("get argument artifact: %v", err)
	}
	defer object.Content.Close()
	if object.Meta.ArtifactType != artifact.ArtifactTypeDebugPayload || object.Meta.Visibility != artifact.VisibilityDebug || object.Meta.RetentionPolicy != artifact.RetentionDebugShortTTL {
		t.Fatalf("unexpected argument artifact metadata: %#v", object.Meta)
	}
	stored, err := io.ReadAll(object.Content)
	if err != nil {
		t.Fatalf("read argument artifact: %v", err)
	}
	if string(stored) != string(arguments) {
		t.Fatalf("stored arguments = %s, want %s", stored, arguments)
	}
}

func TestGatewayUsesDistinctIdempotencyKeysForArgumentAndResultArtifacts(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	eventStore := NewMemoryEventStore()
	def := defaultSearchDefinition()
	def.ResultPolicy.MaxInlineBytes = 16
	def.ResultPolicy.ArtifactThresholdBytes = 16
	gateway := NewGateway(GatewayConfig{
		Registry:        NewStaticRegistry([]ToolDefinition{def}),
		SchemaValidator: BasicSchemaValidator{},
		EventStore:      eventStore,
		ArtifactStore:   artifactStore,
		Executors: []ToolExecutor{NewFunctionExecutor(map[string]FunctionTool{
			"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
				return &FunctionResult{Data: json.RawMessage(`{"result":"large-result-that-needs-an-artifact"}`)}, nil
			},
		})},
		IDGenerator: observability.NewULIDGenerator(""),
		Clock:       fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"large-argument-that-needs-an-artifact"}`)))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	events := eventStore.Events()
	artifactKeys := make([]string, 0, 2)
	for _, event := range events {
		if event.EventType == observability.EventToolArtifactCreated {
			artifactKeys = append(artifactKeys, event.IdempotencyKey)
		}
	}
	if len(artifactKeys) != 2 {
		t.Fatalf("persisted artifact events = %d, want argument and result events; all events=%#v", len(artifactKeys), eventTypes(events))
	}
	if artifactKeys[0] == artifactKeys[1] {
		t.Fatalf("argument and result artifact events share idempotency key %q", artifactKeys[0])
	}
	if len(result.Events) != 4 {
		t.Fatalf("result events = %d, want started, two artifacts, completed", len(result.Events))
	}
}

func TestGatewayRequiresArgumentArtifactEventBeforeExecution(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	sentinel := errors.New("argument artifact event unavailable")
	eventStore := &failOnceEventStore{failType: observability.EventToolArtifactCreated, err: sentinel}
	called := false
	def := defaultSearchDefinition()
	def.ResultPolicy.MaxInlineBytes = 16
	gateway := newGatewayForHardening(eventStore, artifactStore, nil, nil, def, func(context.Context, FunctionCall) (*FunctionResult, error) {
		called = true
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"argument-value-longer-than-sixteen-bytes"}`)))
	if err != nil {
		t.Fatalf("required append failure should become a durable failed result: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeInternal, false)
	if result.Failure.Retryable {
		t.Fatalf("required argument artifact event failure must not be retryable in place")
	}
	if called {
		t.Fatalf("executor must not run before the argument artifact event is persisted")
	}
	if got := eventTypes(eventStore.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolCallFailed {
		t.Fatalf("events before failure = %#v", got)
	}
}

func TestGatewayPersistsFailedWhenArgumentArtifactEventAppendFails(t *testing.T) {
	t.Run("failed terminal persists and replays", func(t *testing.T) {
		artifactStore, ctx := newScopedArtifactStore(t)
		artifactErr := errors.New("argument artifact event unavailable")
		eventStore := &taskCFaultEventStore{faults: map[observability.EventType]*taskCEventAppendFault{
			observability.EventToolArtifactCreated: {err: artifactErr, remaining: 1},
		}}
		stepStore := &taskCStepStore{}
		idempotency := NewMemoryIdempotencyStore()
		calls := 0
		def := defaultSearchDefinition()
		def.ResultPolicy.MaxInlineBytes = 16
		gateway := newGatewayForHardening(eventStore, artifactStore, idempotency, stepStore, def, func(context.Context, FunctionCall) (*FunctionResult, error) {
			calls++
			return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
		})
		req := baseToolRequest(json.RawMessage(`{"query":"argument-value-longer-than-sixteen-bytes"}`))
		req.Policy.IdempotencyKey = "task-c-argument-event"

		result, err := gateway.Invoke(ctx, req)
		if err != nil {
			t.Fatalf("argument artifact event failure should persist failed: %v", err)
		}
		assertFailedResult(t, result, ErrorTypeInternal, false)
		if result.Failure.Retryable || calls != 0 {
			t.Fatalf("failure=%#v calls=%d", result.Failure, calls)
		}
		if got := eventTypes(eventStore.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolCallFailed {
			t.Fatalf("argument artifact append lifecycle = %#v", got)
		}
		if stepStore.startCalls != 1 || stepStore.failCalls != 1 || stepStore.completeCalls != 0 {
			t.Fatalf("step projections start/fail/complete = %d/%d/%d", stepStore.startCalls, stepStore.failCalls, stepStore.completeCalls)
		}
		assertTaskCIdempotencyStatus(t, gateway, req, req.Policy.IdempotencyKey, ToolCallFailed, true)

		replayed, err := gateway.Invoke(ctx, req)
		if err != nil {
			t.Fatalf("replay failed terminal: %v", err)
		}
		assertFailedResult(t, replayed, ErrorTypeInternal, false)
		if calls != 0 || len(eventStore.events) != 2 || stepStore.startCalls != 1 || stepStore.failCalls != 1 || stepStore.completeCalls != 0 {
			t.Fatalf("replay mutated execution state: calls=%d events=%#v steps=%d/%d/%d", calls, eventTypes(eventStore.events), stepStore.startCalls, stepStore.failCalls, stepStore.completeCalls)
		}
	})

	t.Run("failed terminal append also fails", func(t *testing.T) {
		artifactStore, ctx := newScopedArtifactStore(t)
		artifactErr := errors.New("argument artifact event unavailable")
		terminalErr := errors.New("failed terminal unavailable")
		eventStore := &taskCFaultEventStore{faults: map[observability.EventType]*taskCEventAppendFault{
			observability.EventToolArtifactCreated: {err: artifactErr, remaining: 1},
			observability.EventToolCallFailed:      {err: terminalErr, remaining: 1},
		}}
		stepStore := &taskCStepStore{}
		calls := 0
		def := defaultSearchDefinition()
		def.ResultPolicy.MaxInlineBytes = 16
		gateway := newGatewayForHardening(eventStore, artifactStore, NewMemoryIdempotencyStore(), stepStore, def, func(context.Context, FunctionCall) (*FunctionResult, error) {
			calls++
			return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
		})
		req := baseToolRequest(json.RawMessage(`{"query":"argument-value-longer-than-sixteen-bytes"}`))
		req.Policy.IdempotencyKey = "task-c-argument-terminal-unavailable"

		result, err := gateway.Invoke(ctx, req)
		if result != nil || !errors.Is(err, artifactErr) || !errors.Is(err, terminalErr) {
			t.Fatalf("result=%#v err=%v, want both append errors", result, err)
		}
		if calls != 0 || stepStore.startCalls != 1 || stepStore.failCalls != 0 || stepStore.completeCalls != 0 {
			t.Fatalf("ambiguous failure mutated execution/step: calls=%d steps=%d/%d/%d", calls, stepStore.startCalls, stepStore.failCalls, stepStore.completeCalls)
		}
		if got := eventTypes(eventStore.events); len(got) != 1 || got[0] != observability.EventToolCallStarted {
			t.Fatalf("events after both append failures = %#v", got)
		}
		assertTaskCIdempotencyStatus(t, gateway, req, req.Policy.IdempotencyKey, ToolCallRunning, false)

		replayed, replayErr := gateway.Invoke(ctx, req)
		if replayed != nil || !IsErrorType(replayErr, ErrorTypeDuplicateInflight) {
			t.Fatalf("ambiguous retry result=%#v err=%v, want duplicate_inflight", replayed, replayErr)
		}
		if calls != 0 || len(eventStore.events) != 1 || stepStore.startCalls != 1 {
			t.Fatalf("ambiguous retry mutated state: calls=%d events=%#v start=%d", calls, eventTypes(eventStore.events), stepStore.startCalls)
		}
	})
}

func TestGatewayPersistsFailedWhenResultArtifactEventAppendFails(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	artifactErr := errors.New("result artifact event unavailable")
	eventStore := &taskCFaultEventStore{faults: map[observability.EventType]*taskCEventAppendFault{
		observability.EventToolArtifactCreated: {err: artifactErr, remaining: 1},
	}}
	stepStore := &taskCStepStore{}
	calls := 0
	def := defaultSearchDefinition()
	def.ResultPolicy.ArtifactThresholdBytes = 16
	gateway := newGatewayForHardening(eventStore, artifactStore, NewMemoryIdempotencyStore(), stepStore, def, func(context.Context, FunctionCall) (*FunctionResult, error) {
		calls++
		return &FunctionResult{Data: json.RawMessage(`{"result":"large-result-that-requires-an-artifact"}`)}, nil
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.IdempotencyKey = "task-c-result-event"

	result, err := gateway.Invoke(ctx, req)
	if err != nil {
		t.Fatalf("result artifact event failure should persist failed: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeInternal, true)
	if result.Failure.Retryable || calls != 1 {
		t.Fatalf("failure=%#v calls=%d", result.Failure, calls)
	}
	if got := eventTypes(eventStore.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolCallFailed {
		t.Fatalf("result artifact append lifecycle = %#v", got)
	}
	if stepStore.startCalls != 1 || stepStore.failCalls != 1 || stepStore.completeCalls != 0 {
		t.Fatalf("step projections start/fail/complete = %d/%d/%d", stepStore.startCalls, stepStore.failCalls, stepStore.completeCalls)
	}
	assertTaskCIdempotencyStatus(t, gateway, req, req.Policy.IdempotencyKey, ToolCallFailed, true)

	replayed, err := gateway.Invoke(ctx, req)
	if err != nil {
		t.Fatalf("replay result artifact failure: %v", err)
	}
	assertFailedResult(t, replayed, ErrorTypeInternal, true)
	if calls != 1 || len(eventStore.events) != 2 || stepStore.startCalls != 1 || stepStore.failCalls != 1 || stepStore.completeCalls != 0 {
		t.Fatalf("replay mutated state: calls=%d events=%#v steps=%d/%d/%d", calls, eventTypes(eventStore.events), stepStore.startCalls, stepStore.failCalls, stepStore.completeCalls)
	}
}

func TestGatewayRetainsPartialResultRefWhenLaterArtifactEventAppendFails(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	laterArtifactErr := errors.New("later partial artifact event unavailable")
	eventStore := &taskCFaultEventStore{faults: map[observability.EventType]*taskCEventAppendFault{
		observability.EventToolArtifactCreated: {err: laterArtifactErr, skip: 1, remaining: 1},
	}}
	def := defaultSearchDefinition()
	def.ResultPolicy = ToolOutputPolicy{ArtifactThresholdBytes: 4096, RedactSensitiveFields: true}
	gateway := newRawResultGateway(eventStore, artifactStore, def, &ToolRawResult{
		Data:    json.RawMessage(`{"items":[1,2],"password":"partial-event-secret"}`),
		Debug:   json.RawMessage(`{"wire":"raw-debug"}`),
		Partial: true,
	})

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("later partial artifact event failure should return durable failed result: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeInternal, true)
	if result.Failure.PartialResultRef == "" {
		t.Fatalf("partial result ref was lost after later artifact event failure: %#v", result.Failure)
	}
	if got := eventTypes(eventStore.events); len(got) != 3 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolArtifactCreated || got[2] != observability.EventToolCallFailed {
		t.Fatalf("later partial artifact failure lifecycle = %#v", got)
	}
	if eventStore.events[1].PayloadRef != result.Failure.PartialResultRef {
		t.Fatalf("persisted partial artifact ref=%q failure ref=%q", eventStore.events[1].PayloadRef, result.Failure.PartialResultRef)
	}
}

func TestGatewayDoesNotAppendCompetingFailedAfterCompletedAppendError(t *testing.T) {
	completedErr := errors.New("completed persistence outcome is ambiguous")
	eventStore := &taskCFaultEventStore{faults: map[observability.EventType]*taskCEventAppendFault{
		observability.EventToolCallCompleted: {err: completedErr, afterPersist: true, remaining: 1},
	}}
	stepStore := &taskCStepStore{}
	calls := 0
	gateway := newGatewayForHardening(eventStore, nil, NewMemoryIdempotencyStore(), stepStore, defaultSearchDefinition(), func(context.Context, FunctionCall) (*FunctionResult, error) {
		calls++
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.IdempotencyKey = "task-c-completed-ambiguous"

	result, err := gateway.Invoke(testTraceContext(), req)
	if result != nil || !errors.Is(err, completedErr) {
		t.Fatalf("result=%#v err=%v, want ambiguous completed error", result, err)
	}
	if calls != 1 {
		t.Fatalf("executor calls=%d, want 1", calls)
	}
	if got := eventTypes(eventStore.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolCallCompleted {
		t.Fatalf("ambiguous completed lifecycle = %#v", got)
	}
	if stepStore.startCalls != 1 || stepStore.failCalls != 0 || stepStore.completeCalls != 0 {
		t.Fatalf("ambiguous completed step projections = %d/%d/%d", stepStore.startCalls, stepStore.failCalls, stepStore.completeCalls)
	}
	assertTaskCIdempotencyStatus(t, gateway, req, req.Policy.IdempotencyKey, ToolCallRunning, false)

	replayed, replayErr := gateway.Invoke(testTraceContext(), req)
	if replayed != nil || !IsErrorType(replayErr, ErrorTypeDuplicateInflight) {
		t.Fatalf("ambiguous completed retry result=%#v err=%v, want duplicate_inflight", replayed, replayErr)
	}
	if calls != 1 || len(eventStore.events) != 2 || stepStore.startCalls != 1 || stepStore.failCalls != 0 || stepStore.completeCalls != 0 {
		t.Fatalf("ambiguous retry mutated state: calls=%d events=%#v steps=%d/%d/%d", calls, eventTypes(eventStore.events), stepStore.startCalls, stepStore.failCalls, stepStore.completeCalls)
	}
}

func TestGatewayFailsClosedForHighRiskWithoutControlPortEvenWhenRequestClaimsApproval(t *testing.T) {
	eventStore := &recordingEventStore{}
	called := false
	def := defaultSearchDefinition()
	def.RiskLevel = RiskHigh
	gateway := newTestGateway(eventStore, map[string]FunctionTool{
		"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
			called = true
			return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
		},
	}, def)
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.RiskLevel = RiskHigh
	req.Policy.RequireApproval = true

	result, err := gateway.Invoke(testTraceContext(), req)
	if err != nil {
		t.Fatalf("high risk without control port should return failed result, got error: %v", err)
	}
	if called {
		t.Fatalf("high risk tool must not execute without control port")
	}
	assertFailedResult(t, result, ErrorTypeControlRequired, false)
	assertOnlyFailedEvent(t, eventStore, ErrorTypeControlRequired)
}

func TestGatewayRejectsInvalidOutputSchema(t *testing.T) {
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.OutputSchema = json.RawMessage(`{"type":"object","required":["count"],"properties":{"count":{"type":"integer"}}}`)
	def.ResultPolicy.RequireOutputSchema = true
	gateway := newTestGateway(eventStore, map[string]FunctionTool{
		"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
			return &FunctionResult{
				Data:     json.RawMessage(`{"count":"one"}`),
				MimeType: "application/json",
			}, nil
		},
	}, def)

	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("invalid output schema should return failed result, got error: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeSchemaValidationFailed, true)
	if got := eventTypes(eventStore.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolCallFailed {
		t.Fatalf("expected started then failed, got %#v", got)
	}
}

func TestGatewayRedactsSensitiveFieldsFromInlineResults(t *testing.T) {
	def := defaultSearchDefinition()
	def.ResultPolicy.RedactSensitiveFields = true
	gateway := newTestGateway(&recordingEventStore{}, map[string]FunctionTool{
		"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
			return &FunctionResult{
				Data:     json.RawMessage(`{"username":"alice","password":"secret","nested":{"token":"abc","api_key":"key-1"}}`),
				MimeType: "application/json",
			}, nil
		},
	}, def)

	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("invoke redaction tool: %v", err)
	}
	for _, raw := range []json.RawMessage{result.ResultPreview, result.ModelContextResult} {
		text := string(raw)
		if containsAny(text, "secret", "abc", "key-1") {
			t.Fatalf("sensitive values should be redacted from %s", text)
		}
		if !containsAny(text, "[REDACTED]") {
			t.Fatalf("redacted result should include marker, got %s", text)
		}
	}
}

func TestGatewayRetriesRetryableIdempotentTool(t *testing.T) {
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.Retry = RetryPolicy{MaxAttempts: 2, Idempotent: true}
	attempts := 0
	gateway := newTestGateway(eventStore, map[string]FunctionTool{
		"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
			attempts++
			if attempts == 1 {
				return nil, NewToolError(ErrorTypeUpstreamError, "temporary upstream error", true, nil)
			}
			return &FunctionResult{Data: json.RawMessage(`{"count":1}`), MimeType: "application/json"}, nil
		},
	}, def)

	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.MaxRetries = 1
	result, err := gateway.Invoke(testTraceContext(), req)
	if err != nil {
		t.Fatalf("retryable idempotent tool should eventually succeed: %v", err)
	}
	if result.Status != ToolCallSucceeded {
		t.Fatalf("status = %s", result.Status)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if result.Usage.RetryCount != 1 {
		t.Fatalf("retry count = %d, want 1", result.Usage.RetryCount)
	}
	var completed map[string]any
	if err := json.Unmarshal(eventStore.events[len(eventStore.events)-1].PayloadPreview, &completed); err != nil {
		t.Fatalf("decode completed payload: %v", err)
	}
	if completed["retry_count"].(float64) != 1 {
		t.Fatalf("completed retry_count = %#v", completed["retry_count"])
	}
}

func TestGatewayDoesNotRetryWhenRequestRetryBudgetIsZero(t *testing.T) {
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.Retry = RetryPolicy{MaxAttempts: 3, Idempotent: true}
	attempts := 0
	gateway := newTestGateway(eventStore, map[string]FunctionTool{
		"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
			attempts++
			return nil, NewToolError(ErrorTypeUpstreamError, "temporary", true, nil)
		},
	}, def)
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))

	result, err := gateway.Invoke(testTraceContext(), req)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeUpstreamError, true)
	if attempts != 1 || result.Usage.RetryCount != 0 {
		t.Fatalf("attempts=%d retry_count=%d, want 1/0", attempts, result.Usage.RetryCount)
	}
}

func TestGatewayCapsRetriesByRequestAndDefinition(t *testing.T) {
	for _, tt := range []struct {
		name          string
		definition    RetryPolicy
		requestBudget int
		wantAttempts  int
	}{
		{name: "request cap", definition: RetryPolicy{MaxAttempts: 5, Idempotent: true}, requestBudget: 1, wantAttempts: 2},
		{name: "definition cap", definition: RetryPolicy{MaxAttempts: 2, Idempotent: true}, requestBudget: 5, wantAttempts: 2},
		{name: "negative request", definition: RetryPolicy{MaxAttempts: 5, Idempotent: true}, requestBudget: -1, wantAttempts: 1},
		{name: "zero definition attempts", definition: RetryPolicy{MaxAttempts: 0, Idempotent: true}, requestBudget: 5, wantAttempts: 1},
		{name: "negative definition attempts", definition: RetryPolicy{MaxAttempts: -2, Idempotent: true}, requestBudget: 5, wantAttempts: 1},
		{name: "non idempotent", definition: RetryPolicy{MaxAttempts: 5}, requestBudget: 5, wantAttempts: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			def := defaultSearchDefinition()
			def.Retry = tt.definition
			attempts := 0
			gateway := newTestGateway(&recordingEventStore{}, map[string]FunctionTool{
				"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
					attempts++
					return nil, NewToolError(ErrorTypeUpstreamError, "temporary", true, nil)
				},
			}, def)
			req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
			req.Policy.MaxRetries = tt.requestBudget

			result, err := gateway.Invoke(testTraceContext(), req)
			if err != nil {
				t.Fatalf("invoke: %v", err)
			}
			assertFailedResult(t, result, ErrorTypeUpstreamError, true)
			if attempts != tt.wantAttempts || result.Usage.RetryCount != tt.wantAttempts-1 {
				t.Fatalf("attempts=%d retry_count=%d, want %d/%d", attempts, result.Usage.RetryCount, tt.wantAttempts, tt.wantAttempts-1)
			}
		})
	}
}

func TestGatewayDoesNotRetryCancellation(t *testing.T) {
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.Retry = RetryPolicy{MaxAttempts: 3, Idempotent: true}
	attempts := 0
	gateway := newTestGateway(eventStore, map[string]FunctionTool{
		"search": func(ctx context.Context, _ FunctionCall) (*FunctionResult, error) {
			attempts++
			return nil, ctx.Err()
		},
	}, def)
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.MaxRetries = 2
	ctx, cancel := context.WithCancel(testTraceContext())
	cancel()

	result, err := gateway.Invoke(ctx, req)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeCancelled, true)
	if attempts != 1 || result.Failure.Retryable || result.Usage.RetryCount != 0 {
		t.Fatalf("attempts=%d retryable=%v retry_count=%d", attempts, result.Failure.Retryable, result.Usage.RetryCount)
	}
}

func TestGatewayDoesNotRetryWhenContextCancelsBetweenAttempts(t *testing.T) {
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.Retry = RetryPolicy{MaxAttempts: 3, Idempotent: true}
	attempts := 0
	ctx, cancel := context.WithCancel(testTraceContext())
	gateway := newTestGateway(eventStore, map[string]FunctionTool{
		"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
			attempts++
			cancel()
			return nil, NewToolError(ErrorTypeUpstreamError, "ignored cancellation", true, nil)
		},
	}, def)
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.MaxRetries = 2

	result, err := gateway.Invoke(ctx, req)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeCancelled, true)
	if attempts != 1 || result.Failure.Retryable || result.Usage.RetryCount != 0 {
		t.Fatalf("attempts=%d retryable=%v retry_count=%d", attempts, result.Failure.Retryable, result.Usage.RetryCount)
	}
}

func TestGatewayForcesCancelledToolErrorNonRetryable(t *testing.T) {
	def := defaultSearchDefinition()
	def.Retry = RetryPolicy{MaxAttempts: 3, Idempotent: true}
	attempts := 0
	gateway := newTestGateway(&recordingEventStore{}, map[string]FunctionTool{
		"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
			attempts++
			return nil, NewToolError(ErrorTypeCancelled, "misclassified cancellation", true, nil)
		},
	}, def)
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.MaxRetries = 2

	result, err := gateway.Invoke(testTraceContext(), req)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeCancelled, true)
	if attempts != 1 || result.Failure.Retryable || result.Usage.RetryCount != 0 {
		t.Fatalf("attempts=%d retryable=%v retry_count=%d", attempts, result.Failure.Retryable, result.Usage.RetryCount)
	}
}

func TestGatewayPersistsCancelledTerminalWithDetachedContext(t *testing.T) {
	eventStore := &contextSensitiveEventStore{}
	def := defaultSearchDefinition()
	def.Retry = RetryPolicy{MaxAttempts: 3, Idempotent: true}
	ctx, cancel := context.WithCancel(testTraceContext())
	gateway := newGatewayForHardening(eventStore, nil, nil, nil, def, func(context.Context, FunctionCall) (*FunctionResult, error) {
		cancel()
		return nil, context.Canceled
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.MaxRetries = 2

	result, err := gateway.Invoke(ctx, req)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeCancelled, true)
	if result.Failure.Retryable || result.Usage.RetryCount != 0 {
		t.Fatalf("retryable=%v retry_count=%d", result.Failure.Retryable, result.Usage.RetryCount)
	}
	if got := eventTypes(eventStore.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolCallFailed {
		t.Fatalf("cancelled lifecycle=%#v", got)
	}
	if !eventStore.failedContextHadDeadline {
		t.Fatalf("cancelled terminal persistence context must be bounded")
	}
}

func TestFailedEventCarriesActualRetryCount(t *testing.T) {
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.Retry = RetryPolicy{MaxAttempts: 3, Idempotent: true}
	attempts := 0
	gateway := newTestGateway(eventStore, map[string]FunctionTool{
		"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
			attempts++
			return nil, NewToolError(ErrorTypeUpstreamError, "temporary", true, nil)
		},
	}, def)
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.MaxRetries = 2

	result, err := gateway.Invoke(testTraceContext(), req)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeUpstreamError, true)
	if attempts != 3 || result.Usage.RetryCount != 2 {
		t.Fatalf("attempts=%d retry_count=%d", attempts, result.Usage.RetryCount)
	}
	var payload map[string]any
	if err := json.Unmarshal(eventStore.events[len(eventStore.events)-1].PayloadPreview, &payload); err != nil {
		t.Fatalf("decode failed payload: %v", err)
	}
	if payload["retry_count"] != float64(2) {
		t.Fatalf("failed retry_count=%#v, want 2", payload["retry_count"])
	}
}

func TestGatewayEmitsFailedWhenToolRegistryLookupFails(t *testing.T) {
	eventStore := &recordingEventStore{}
	gateway := NewGateway(GatewayConfig{
		Registry:        NewStaticRegistry(nil),
		SchemaValidator: BasicSchemaValidator{},
		EventStore:      eventStore,
		IDGenerator:     fixedIDGenerator{},
		Clock:           fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})

	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("registry lookup failure should be returned as ToolCallResult, got error: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeToolNotFound, false)
	assertOnlyFailedEvent(t, eventStore, ErrorTypeToolNotFound)
}

func TestGatewayLogsSafeLifecycle(t *testing.T) {
	const (
		argumentSentinel    = "ARG_SECRET"
		resultSentinel      = "RESULT_SECRET"
		debugSentinel       = "DEBUG_SECRET"
		credentialSentinel  = "CREDENTIAL_SECRET"
		stackSentinel       = "STACK_SECRET"
		projectionSentinel  = "PROJECTION_SECRET"
		idempotencySentinel = "IDEMPOTENCY_SECRET"
	)
	sentinels := []string{
		argumentSentinel,
		resultSentinel,
		debugSentinel,
		credentialSentinel,
		stackSentinel,
		projectionSentinel,
		idempotencySentinel,
	}

	t.Run("success", func(t *testing.T) {
		artifactStore, ctx := newScopedArtifactStore(t)
		def := defaultSearchDefinition()
		def.OutputSchema = json.RawMessage(`{"type":"object"}`)
		def.ResultPolicy = ToolOutputPolicy{
			ArtifactThresholdBytes: 16,
			MaxSSEPreviewBytes:     32,
			MaxModelContextBytes:   64,
			RedactSensitiveFields:  true,
		}
		fixture := newLifecycleGatewayFixture(t, lifecycleGatewayOptions{
			definition:          def,
			artifactStore:       artifactStore,
			enableToolEventSink: true,
			handler: func(ctx context.Context, call FunctionCall) (*FunctionResult, error) {
				if call.ToolContext == nil {
					t.Fatal("function call is missing ToolContext")
				}
				if err := call.ToolContext.EmitDebug(ctx, ToolDebugEvent{
					Message: "debug sentinel",
					Data:    json.RawMessage(`{"debug":"` + debugSentinel + `"}`),
				}); err != nil {
					t.Fatalf("emit debug through DefaultToolEventSink: %v", err)
				}
				return &FunctionResult{
					Data:     json.RawMessage(`{"value":"` + resultSentinel + `"}`),
					MimeType: "application/json",
				}, nil
			},
		})
		req := lifecycleToolRequest(argumentSentinel, credentialSentinel)

		result, err := fixture.gateway.Invoke(ctx, req)
		if err != nil || result == nil || result.Status != ToolCallSucceeded {
			t.Fatalf("success result=%#v err=%v", result, err)
		}
		if result.ResultRef == "" {
			t.Fatal("success result must expose the offloaded artifact ref")
		}
		debugArtifacts := 0
		for _, meta := range listToolCallArtifacts(t, ctx, artifactStore) {
			if meta.ArtifactType == artifact.ArtifactTypeDebugPayload && meta.Visibility == artifact.VisibilityDebug {
				debugArtifacts++
			}
		}
		if debugArtifacts != 1 {
			t.Fatalf("debug artifacts = %d, want 1", debugArtifacts)
		}
		assertLifecycleLogFields(t, fixture.logger, "tool call started", lifecycleCommonWant())
		assertLifecycleLogFields(t, fixture.logger, "tool permission decision", map[string]any{"permission_decision": string(DecisionAllow)})
		assertLifecycleLogFields(t, fixture.logger, "tool schema decision", map[string]any{"schema_phase": "input", "schema_decision": "allow"})
		assertLifecycleLogFields(t, fixture.logger, "tool schema decision", map[string]any{"schema_phase": "output", "schema_decision": "allow"})
		assertLifecycleLogFields(t, fixture.logger, "tool call completed", map[string]any{
			"status": string(ToolCallSucceeded), "success": true, "duration_ms": int64(0), "retry_count": 0,
			"artifact_count": 1, "artifact_ref": result.ResultRef,
		})
		assertLifecycleSpanEventFields(t, fixture.tracer, "tool.call.started", lifecycleCommonWant())
		assertLifecycleSpanEventFields(t, fixture.tracer, "tool.call.completed", map[string]any{
			"status": string(ToolCallSucceeded), "success": true, "duration_ms": int64(0), "artifact_ref": result.ResultRef,
		})
		assertLifecycleTraceEnded(t, fixture.tracer, "search_kb")
		assertSafeLifecycleObservability(t, fixture.logger, fixture.tracer, sentinels...)
	})

	t.Run("http response and configured credential remain safe", func(t *testing.T) {
		const endpoint = "https://tools.example.test/safe-observability"
		roundTrips := 0
		receivedAuthorization := ""
		receivedBody := ""
		client := newHTTPExecutorClient(func(req *http.Request) (*http.Response, error) {
			roundTrips++
			receivedAuthorization = req.Header.Get("Authorization")
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read HTTP request body: %v", err)
			}
			receivedBody = string(body)
			return newHTTPExecutorResponse(http.StatusOK, http.Header{
				"Content-Type": []string{"application/json"},
			}, []byte(`{"value":"`+resultSentinel+`"}`)), nil
		})
		def := ToolDefinition{
			Name:         "safe_http",
			Version:      "v1",
			Type:         ToolTypeHTTP,
			InputSchema:  json.RawMessage(`{"type":"object"}`),
			OutputSchema: json.RawMessage(`{"type":"object"}`),
			RiskLevel:    RiskLow,
			Visibility:   observability.VisibilityUserVisible,
			Permissions:  ToolPermissions{AllowedAgents: []string{"agent-a"}},
			HTTP: &HTTPToolSpec{
				Method: http.MethodPost, URL: endpoint, ResponseMode: "json",
				Headers: map[string]string{"Authorization": credentialSentinel},
			},
		}
		fixture := newLifecycleGatewayFixture(t, lifecycleGatewayOptions{
			definition: def,
			executors:  []ToolExecutor{NewHTTPExecutor(client)},
		})
		req := lifecycleToolRequest(argumentSentinel, credentialSentinel)
		req.ToolName = def.Name
		req.ToolVersion = def.Version

		result, err := fixture.gateway.Invoke(testTraceContext(), req)
		if err != nil || result == nil || result.Status != ToolCallSucceeded {
			t.Fatalf("HTTP result=%#v err=%v", result, err)
		}
		if roundTrips != 1 || receivedAuthorization != credentialSentinel || !strings.Contains(receivedBody, argumentSentinel) {
			t.Fatalf("HTTP route calls=%d authorization=%q body=%q", roundTrips, receivedAuthorization, receivedBody)
		}
		assertLifecycleLogFields(t, fixture.logger, "tool call completed", map[string]any{
			"tool_type": string(ToolTypeHTTP), "status": string(ToolCallSucceeded), "success": true,
		})
		assertLifecycleTraceEnded(t, fixture.tracer, def.Name)
		assertSafeLifecycleObservability(t, fixture.logger, fixture.tracer, sentinels...)
	})

	t.Run("retry then success", func(t *testing.T) {
		def := defaultSearchDefinition()
		def.Retry = RetryPolicy{MaxAttempts: 2, Idempotent: true}
		attempts := 0
		fixture := newLifecycleGatewayFixture(t, lifecycleGatewayOptions{
			definition: def,
			handler: func(context.Context, FunctionCall) (*FunctionResult, error) {
				attempts++
				if attempts == 1 {
					return nil, NewToolError(ErrorTypeUpstreamError, stackSentinel, true, errors.New(stackSentinel))
				}
				return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
			},
		})
		req := lifecycleToolRequest(argumentSentinel, credentialSentinel)
		req.Policy.MaxRetries = 1

		result, err := fixture.gateway.Invoke(testTraceContext(), req)
		if err != nil || result == nil || result.Status != ToolCallSucceeded || attempts != 2 || result.Usage.RetryCount != 1 {
			t.Fatalf("retry result=%#v attempts=%d err=%v", result, attempts, err)
		}
		assertLifecycleLogFields(t, fixture.logger, "tool call retry", map[string]any{"retry_count": 1, "error_type": string(ErrorTypeUpstreamError)})
		assertLifecycleLogFields(t, fixture.logger, "tool call completed", map[string]any{"status": string(ToolCallSucceeded), "retry_count": 1})
		assertLifecycleSpanEventFields(t, fixture.tracer, "tool.call.retry", map[string]any{"retry_count": 1, "error_type": string(ErrorTypeUpstreamError)})
		assertSafeLifecycleObservability(t, fixture.logger, fixture.tracer, sentinels...)
	})

	t.Run("executor failure", func(t *testing.T) {
		fixture := newLifecycleGatewayFixture(t, lifecycleGatewayOptions{
			handler: func(context.Context, FunctionCall) (*FunctionResult, error) {
				return nil, NewToolError(ErrorTypeUpstreamError, stackSentinel, false, errors.New(stackSentinel))
			},
		})
		result, err := fixture.gateway.Invoke(testTraceContext(), lifecycleToolRequest(argumentSentinel, credentialSentinel))
		if err != nil {
			t.Fatalf("executor failure should be canonical: %v", err)
		}
		assertFailedResult(t, result, ErrorTypeUpstreamError, true)
		assertLifecycleLogFields(t, fixture.logger, "tool call failed", map[string]any{
			"status": string(ToolCallFailed), "success": false, "error_type": string(ErrorTypeUpstreamError),
		})
		assertLifecycleSpanEventFields(t, fixture.tracer, "tool.call.failed", map[string]any{"error_type": string(ErrorTypeUpstreamError)})
		assertLifecycleSafeRecordedErrors(t, fixture.logger, fixture.tracer, ErrorTypeUpstreamError)
		assertSafeLifecycleObservability(t, fixture.logger, fixture.tracer, sentinels...)
	})

	t.Run("cached success replay", func(t *testing.T) {
		calls := 0
		fixture := newLifecycleGatewayFixture(t, lifecycleGatewayOptions{
			handler: func(context.Context, FunctionCall) (*FunctionResult, error) {
				calls++
				return &FunctionResult{Data: json.RawMessage(`{"value":"` + resultSentinel + `"}`)}, nil
			},
		})
		req := lifecycleToolRequest(argumentSentinel, credentialSentinel)
		first, err := fixture.gateway.Invoke(testTraceContext(), req)
		if err != nil {
			t.Fatalf("prime success replay: %v", err)
		}
		eventsBefore := len(fixture.events.events)
		fixture.logger.Reset()
		second, err := fixture.gateway.Invoke(testTraceContext(), req)
		if err != nil || !reflect.DeepEqual(first, second) || calls != 1 || len(fixture.events.events) != eventsBefore {
			t.Fatalf("success replay first=%#v second=%#v calls=%d events=%d/%d err=%v", first, second, calls, eventsBefore, len(fixture.events.events), err)
		}
		assertLifecycleLogFields(t, fixture.logger, "tool call started", lifecycleCommonWant())
		assertLifecycleLogFields(t, fixture.logger, "tool call completed", map[string]any{"status": string(ToolCallSucceeded), "success": true})
		assertSafeLifecycleObservability(t, fixture.logger, fixture.tracer, sentinels...)
	})

	t.Run("cached failed replay", func(t *testing.T) {
		calls := 0
		fixture := newLifecycleGatewayFixture(t, lifecycleGatewayOptions{
			handler: func(context.Context, FunctionCall) (*FunctionResult, error) {
				calls++
				return nil, NewToolError(ErrorTypeUpstreamError, stackSentinel, false, errors.New(stackSentinel))
			},
		})
		req := lifecycleToolRequest(argumentSentinel, credentialSentinel)
		first, err := fixture.gateway.Invoke(testTraceContext(), req)
		if err != nil {
			t.Fatalf("prime failed replay: %v", err)
		}
		eventsBefore := len(fixture.events.events)
		fixture.logger.Reset()
		second, err := fixture.gateway.Invoke(testTraceContext(), req)
		if err != nil || !reflect.DeepEqual(first, second) || calls != 1 || len(fixture.events.events) != eventsBefore {
			t.Fatalf("failed replay first=%#v second=%#v calls=%d events=%d/%d err=%v", first, second, calls, eventsBefore, len(fixture.events.events), err)
		}
		assertLifecycleLogFields(t, fixture.logger, "tool call started", lifecycleCommonWant())
		assertLifecycleLogFields(t, fixture.logger, "tool call failed", map[string]any{"status": string(ToolCallFailed), "error_type": string(ErrorTypeUpstreamError)})
		assertSafeLifecycleObservability(t, fixture.logger, fixture.tracer, sentinels...)
	})

	t.Run("post identity direct infrastructure error", func(t *testing.T) {
		stepStore := &lifecycleFaultStepStore{startErr: errors.New(stackSentinel)}
		calls := 0
		fixture := newLifecycleGatewayFixture(t, lifecycleGatewayOptions{
			stepStore: stepStore,
			handler: func(context.Context, FunctionCall) (*FunctionResult, error) {
				calls++
				return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
			},
		})
		result, err := fixture.gateway.Invoke(testTraceContext(), lifecycleToolRequest(argumentSentinel, credentialSentinel))
		if result != nil || err == nil || calls != 0 || len(fixture.events.events) != 0 {
			t.Fatalf("direct error result=%#v calls=%d events=%#v err=%v", result, calls, eventTypes(fixture.events.events), err)
		}
		assertLifecycleLogFields(t, fixture.logger, "tool call failed", map[string]any{
			"status": string(ToolCallFailed), "success": false, "error_type": string(ErrorTypeInternal),
		})
		assertSafeLifecycleObservability(t, fixture.logger, fixture.tracer, sentinels...)
	})

	t.Run("idempotency store without release support", func(t *testing.T) {
		calls := 0
		fixture := newLifecycleGatewayFixture(t, lifecycleGatewayOptions{
			idempotency: &getPutOnlyIdempotencyStore{records: make(map[string]IdempotencyRecord)},
			handler: func(context.Context, FunctionCall) (*FunctionResult, error) {
				calls++
				return &FunctionResult{Data: json.RawMessage(`{"value":"` + resultSentinel + `"}`)}, nil
			},
		})
		result, err := fixture.gateway.Invoke(testTraceContext(), lifecycleToolRequest(argumentSentinel, credentialSentinel))
		if result != nil || !IsErrorType(err, ErrorTypeInternal) || calls != 0 || len(fixture.events.events) != 0 {
			t.Fatalf("unsupported idempotency result=%#v calls=%d events=%#v err=%v", result, calls, eventTypes(fixture.events.events), err)
		}
		assertLifecycleLogMessageCount(t, fixture.logger, "tool call started", 1)
		assertLifecycleLogMessageCount(t, fixture.logger, "tool call failed", 1)
		assertLifecycleLogFields(t, fixture.logger, "tool call started", map[string]any{
			"trace_id": "trace-1", "span_id": "span-tool-call", "tool_call_id": "tc-1", "tool_name": "search_kb",
		})
		assertLifecycleLogFields(t, fixture.logger, "tool call failed", map[string]any{
			"status": string(ToolCallFailed), "success": false, "error_type": string(ErrorTypeInternal),
		})
		assertLifecycleSpanEventCount(t, fixture.tracer, "tool.call.started", 1)
		assertLifecycleSpanEventCount(t, fixture.tracer, "tool.call.failed", 1)
		assertLifecycleTraceEnded(t, fixture.tracer, "search_kb")
		assertSafeLifecycleObservability(t, fixture.logger, fixture.tracer, sentinels...)
	})

	t.Run("permission block", func(t *testing.T) {
		def := defaultSearchDefinition()
		def.Permissions.AllowedAgents = []string{"agent-b"}
		fixture := newLifecycleGatewayFixture(t, lifecycleGatewayOptions{definition: def})
		result, err := fixture.gateway.Invoke(testTraceContext(), lifecycleToolRequest(argumentSentinel, credentialSentinel))
		if err != nil {
			t.Fatalf("permission block should be canonical: %v", err)
		}
		assertFailedResult(t, result, ErrorTypePermissionDenied, false)
		assertLifecycleLogFields(t, fixture.logger, "tool permission decision", map[string]any{"permission_decision": string(DecisionBlock)})
		assertLifecycleLogFields(t, fixture.logger, "tool call failed", map[string]any{"error_type": string(ErrorTypePermissionDenied)})
		assertSafeLifecycleObservability(t, fixture.logger, fixture.tracer, sentinels...)
	})

	t.Run("input schema block", func(t *testing.T) {
		fixture := newLifecycleGatewayFixture(t, lifecycleGatewayOptions{})
		req := lifecycleToolRequest(argumentSentinel, credentialSentinel)
		req.Arguments = json.RawMessage(`{"query":7,"secret":"` + argumentSentinel + `"}`)
		result, err := fixture.gateway.Invoke(testTraceContext(), req)
		if err != nil {
			t.Fatalf("input schema block should be canonical: %v", err)
		}
		assertFailedResult(t, result, ErrorTypeSchemaValidationFailed, false)
		assertLifecycleLogFields(t, fixture.logger, "tool schema decision", map[string]any{"schema_phase": "input", "schema_decision": "block"})
		assertSafeLifecycleObservability(t, fixture.logger, fixture.tracer, sentinels...)
	})

	t.Run("output schema block", func(t *testing.T) {
		def := defaultSearchDefinition()
		def.OutputSchema = json.RawMessage(`{"type":"object","required":["count"],"properties":{"count":{"type":"integer"}}}`)
		fixture := newLifecycleGatewayFixture(t, lifecycleGatewayOptions{
			definition: def,
			handler: func(context.Context, FunctionCall) (*FunctionResult, error) {
				return &FunctionResult{Data: json.RawMessage(`{"value":"` + resultSentinel + `"}`)}, nil
			},
		})
		result, err := fixture.gateway.Invoke(testTraceContext(), lifecycleToolRequest(argumentSentinel, credentialSentinel))
		if err != nil {
			t.Fatalf("output schema block should be canonical: %v", err)
		}
		assertFailedResult(t, result, ErrorTypeSchemaValidationFailed, true)
		assertLifecycleLogFields(t, fixture.logger, "tool schema decision", map[string]any{"schema_phase": "output", "schema_decision": "block"})
		assertSafeLifecycleObservability(t, fixture.logger, fixture.tracer, sentinels...)
	})

	projectionCases := []struct {
		name       string
		status     ToolCallStatus
		projection string
		operation  string
		fault      error
	}{
		{name: "completed step projection degraded", status: ToolCallSucceeded, projection: "step_store", operation: "complete_tool_step", fault: errors.New(projectionSentinel)},
		{name: "failed step projection degraded", status: ToolCallFailed, projection: "step_store", operation: "fail_tool_step", fault: errors.New(projectionSentinel)},
		{name: "completed idempotency projection degraded", status: ToolCallSucceeded, projection: "idempotency_store", operation: "put_terminal", fault: errors.New(idempotencySentinel)},
		{name: "failed idempotency projection degraded", status: ToolCallFailed, projection: "idempotency_store", operation: "put_terminal", fault: errors.New(idempotencySentinel)},
	}
	for _, tc := range projectionCases {
		t.Run(tc.name, func(t *testing.T) {
			handler := lifecycleHandlerForStatus(tc.status, stackSentinel)
			baselineOptions := lifecycleGatewayOptions{handler: handler}
			faultOptions := lifecycleGatewayOptions{handler: lifecycleHandlerForStatus(tc.status, stackSentinel)}
			if tc.projection == "step_store" {
				baselineOptions.stepStore = &lifecycleFaultStepStore{}
				faultStep := &lifecycleFaultStepStore{}
				if tc.status == ToolCallSucceeded {
					faultStep.completeErr = tc.fault
				} else {
					faultStep.failErr = tc.fault
				}
				faultOptions.stepStore = faultStep
			} else {
				baselineOptions.idempotency = NewMemoryIdempotencyStore()
				faultOptions.idempotency = &lifecycleFailTerminalIdempotencyStore{
					MemoryIdempotencyStore: NewMemoryIdempotencyStore(),
					status:                 tc.status,
					err:                    tc.fault,
				}
			}
			baseline := newLifecycleGatewayFixture(t, baselineOptions)
			degraded := newLifecycleGatewayFixture(t, faultOptions)
			req := lifecycleToolRequest(argumentSentinel, credentialSentinel)
			req.Policy.RiskLevel = RiskMedium
			wantResult, wantErr := baseline.gateway.Invoke(testTraceContext(), req)
			gotResult, gotErr := degraded.gateway.Invoke(testTraceContext(), req)
			if wantErr != nil || gotErr != nil || !reflect.DeepEqual(wantResult, gotResult) || !reflect.DeepEqual(baseline.events.events, degraded.events.events) {
				t.Fatalf("canonical outcome changed: want_result=%#v got_result=%#v want_events=%#v got_events=%#v want_err=%v got_err=%v", wantResult, gotResult, eventTypes(baseline.events.events), eventTypes(degraded.events.events), wantErr, gotErr)
			}
			assertSingleLifecycleTerminal(t, degraded.events.events, tc.status)
			degradationWant := lifecycleCommonWant()
			for key, value := range map[string]any{
				"fallback_type": "secondary_projection_degraded",
				"from":          tc.projection,
				"to":            "event_store_canonical_terminal",
				"reason":        "projection_write_failed",
				"impact":        "canonical_terminal_preserved_reconciliation_required",
				"projection":    tc.projection,
				"operation":     tc.operation,
			} {
				degradationWant[key] = value
			}
			assertLifecycleLogFields(t, degraded.logger, "tool secondary projection degraded", degradationWant)
			assertSafeLifecycleObservability(t, degraded.logger, degraded.tracer, sentinels...)
		})
	}
}

func TestGatewayStartsToolCallSpan(t *testing.T) {
	tracer := &recordingTraceProvider{}
	gateway := newTestGateway(&recordingEventStore{}, map[string]FunctionTool{
		"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
			return &FunctionResult{Data: json.RawMessage(`{"count":1}`), MimeType: "application/json"}, nil
		},
	}, defaultSearchDefinition())
	gateway.traceProvider = tracer

	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("invoke with tracer: %v", err)
	}
	if result.Status != ToolCallSucceeded {
		t.Fatalf("status = %s", result.Status)
	}
	if tracer.startedName != "tool.call" {
		t.Fatalf("span name = %q", tracer.startedName)
	}
	if !tracer.span.ended {
		t.Fatalf("span should be ended")
	}
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func traceContextWithIdentity(tenantID, userID string) context.Context {
	return observability.WithTraceContext(context.Background(), observability.TraceContext{
		TraceID:   "trace-1",
		SpanID:    "span-parent",
		TenantID:  tenantID,
		UserID:    userID,
		SessionID: "sess-1",
		RunID:     "run-1",
		AgentID:   "agent-a",
	})
}

func newGatewayForHardening(
	eventStore EventStore,
	artifactStore artifact.ArtifactStore,
	idempotency IdempotencyStore,
	stepStore StepStore,
	def ToolDefinition,
	handler FunctionTool,
) *DefaultGateway {
	return NewGateway(GatewayConfig{
		Registry:        NewStaticRegistry([]ToolDefinition{def}),
		SchemaValidator: BasicSchemaValidator{},
		EventStore:      eventStore,
		ArtifactStore:   artifactStore,
		StepStore:       stepStore,
		Idempotency:     idempotency,
		Executors:       []ToolExecutor{NewFunctionExecutor(map[string]FunctionTool{"search": handler})},
		IDGenerator:     fixedIDGenerator{},
		Clock:           fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})
}

type taskCEventAppendFault struct {
	err          error
	afterPersist bool
	skip         int
	remaining    int
}

type taskCFaultEventStore struct {
	recordingEventStore
	faults map[observability.EventType]*taskCEventAppendFault
}

func (s *taskCFaultEventStore) AppendEvent(ctx context.Context, event observability.AgentEvent) (*EventAppendResult, error) {
	fault := s.faults[event.EventType]
	if fault != nil && fault.remaining > 0 && fault.skip > 0 {
		fault.skip--
		return s.recordingEventStore.AppendEvent(ctx, event)
	}
	if fault != nil && fault.remaining > 0 && !fault.afterPersist {
		fault.remaining--
		return nil, fault.err
	}
	result, err := s.recordingEventStore.AppendEvent(ctx, event)
	if err != nil {
		return nil, err
	}
	if fault != nil && fault.remaining > 0 {
		fault.remaining--
		return nil, fault.err
	}
	return result, nil
}

type taskCStepStore struct {
	startCalls    int
	completeCalls int
	failCalls     int
}

func (s *taskCStepStore) StartToolStep(_ context.Context, req StartToolStepRequest) (*StepSnapshot, error) {
	s.startCalls++
	return &StepSnapshot{StepID: req.StepID, RunID: req.RunID, Status: "running"}, nil
}

func (s *taskCStepStore) CompleteToolStep(context.Context, CompleteToolStepRequest) error {
	s.completeCalls++
	return nil
}

func (s *taskCStepStore) FailToolStep(context.Context, FailToolStepRequest) error {
	s.failCalls++
	return nil
}

func assertTaskCIdempotencyStatus(
	t *testing.T,
	gateway *DefaultGateway,
	req ToolCallRequest,
	logicalKey string,
	want ToolCallStatus,
	wantTerminal bool,
) {
	t.Helper()
	for _, key := range []string{
		scopedToolCallIdempotencyStoreKey(req),
		scopedIdempotencyStoreKey(req, logicalKey),
	} {
		record, found, err := gateway.idempotency.Get(context.Background(), key)
		if err != nil || !found || record == nil {
			t.Fatalf("idempotency key=%s found=%v record=%#v err=%v", key, found, record, err)
		}
		if record.Status != want {
			t.Fatalf("idempotency key=%s status=%s, want %s", key, record.Status, want)
		}
		if wantTerminal {
			if record.Result == nil || record.Result.Status != want ||
				(want == ToolCallFailed && record.Failure == nil) ||
				(want != ToolCallFailed && record.Failure != nil) {
				t.Fatalf("idempotency terminal key=%s record=%#v", key, record)
			}
		} else if record.Result != nil || record.Failure != nil {
			t.Fatalf("running idempotency key=%s has terminal projection: %#v", key, record)
		}
	}
}

type failOnceEventStore struct {
	events   []observability.AgentEvent
	failType observability.EventType
	err      error
	failed   bool
}

func (s *failOnceEventStore) AppendEvent(_ context.Context, event observability.AgentEvent) (*EventAppendResult, error) {
	if event.EventType == s.failType && !s.failed {
		s.failed = true
		return nil, s.err
	}
	event.Sequence = int64(len(s.events) + 1)
	s.events = append(s.events, event)
	return &EventAppendResult{Event: event}, nil
}

type lifecycleGatewayOptions struct {
	definition          ToolDefinition
	handler             FunctionTool
	executors           []ToolExecutor
	artifactStore       artifact.ArtifactStore
	stepStore           StepStore
	idempotency         IdempotencyStore
	schemaValidator     SchemaValidator
	enableToolEventSink bool
}

type lifecycleGatewayFixture struct {
	gateway *DefaultGateway
	logger  *recordingLifecycleLogger
	tracer  *recordingTraceProvider
	events  *recordingEventStore
}

func newLifecycleGatewayFixture(t *testing.T, options lifecycleGatewayOptions) lifecycleGatewayFixture {
	t.Helper()
	def := options.definition
	if def.Name == "" {
		def = defaultSearchDefinition()
	}
	handler := options.handler
	if handler == nil {
		handler = func(context.Context, FunctionCall) (*FunctionResult, error) {
			return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
		}
	}
	handlerName := "search"
	if def.Function != nil && def.Function.HandlerName != "" {
		handlerName = def.Function.HandlerName
	}
	logger := newRecordingLifecycleLogger()
	tracer := &recordingTraceProvider{}
	events := &recordingEventStore{}
	idempotency := options.idempotency
	if idempotency == nil {
		idempotency = NewMemoryIdempotencyStore()
	}
	validator := options.schemaValidator
	if validator == nil {
		validator = BasicSchemaValidator{}
	}
	executors := options.executors
	if len(executors) == 0 {
		executors = []ToolExecutor{NewFunctionExecutor(map[string]FunctionTool{handlerName: handler})}
	}
	var toolEventSink ToolEventSink
	if options.enableToolEventSink {
		toolEventSink = NewDefaultToolEventSink(ToolEventSinkConfig{
			EventStore: events, ArtifactStore: options.artifactStore, Logger: logger,
			IDGenerator: fixedIDGenerator{}, Clock: fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
		})
	}
	gateway := NewGateway(GatewayConfig{
		Registry:        NewStaticRegistry([]ToolDefinition{def}),
		SchemaValidator: validator,
		EventStore:      events,
		ArtifactStore:   options.artifactStore,
		StepStore:       options.stepStore,
		Idempotency:     idempotency,
		TraceProvider:   tracer,
		Logger:          logger,
		ToolEventSink:   toolEventSink,
		Executors:       executors,
		IDGenerator:     fixedIDGenerator{},
		Clock:           fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})
	return lifecycleGatewayFixture{gateway: gateway, logger: logger, tracer: tracer, events: events}
}

func lifecycleToolRequest(argumentSentinel, credentialSentinel string) ToolCallRequest {
	req := baseToolRequest(json.RawMessage(`{"query":"` + argumentSentinel + `"}`))
	req.Metadata = map[string]string{
		"authorization": credentialSentinel,
		"debug":         credentialSentinel,
	}
	return req
}

func lifecycleCommonWant() map[string]any {
	return map[string]any{
		"trace_id":       "trace-1",
		"span_id":        "span-tool-call",
		"parent_span_id": "span-parent",
		"session_id":     "sess-1",
		"run_id":         "run-1",
		"step_id":        "step-1",
		"agent_id":       "agent-a",
		"tool_call_id":   "tc-1",
		"tool_name":      "search_kb",
		"tool_version":   "v1",
		"tool_type":      string(ToolTypeFunction),
		"risk_level":     string(RiskLow),
	}
}

func lifecycleHandlerForStatus(status ToolCallStatus, stackSentinel string) FunctionTool {
	if status == ToolCallFailed {
		return func(context.Context, FunctionCall) (*FunctionResult, error) {
			return nil, NewToolError(ErrorTypeUpstreamError, stackSentinel, false, errors.New(stackSentinel))
		}
	}
	return func(context.Context, FunctionCall) (*FunctionResult, error) {
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	}
}

type lifecycleFaultStepStore struct {
	mu          sync.Mutex
	startErr    error
	completeErr error
	failErr     error
	startCalls  int
	complete    int
	failed      int
}

func (s *lifecycleFaultStepStore) StartToolStep(_ context.Context, req StartToolStepRequest) (*StepSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startCalls++
	if s.startErr != nil {
		return nil, s.startErr
	}
	return &StepSnapshot{StepID: req.StepID, RunID: req.RunID, Status: "running"}, nil
}

func (s *lifecycleFaultStepStore) CompleteToolStep(context.Context, CompleteToolStepRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.complete++
	return s.completeErr
}

func (s *lifecycleFaultStepStore) FailToolStep(context.Context, FailToolStepRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed++
	return s.failErr
}

type lifecycleFailTerminalIdempotencyStore struct {
	*MemoryIdempotencyStore
	status ToolCallStatus
	err    error
}

func (s *lifecycleFailTerminalIdempotencyStore) Put(ctx context.Context, record IdempotencyRecord) error {
	if record.Status == s.status && isTerminalIdempotencyStatus(record.Status) {
		return s.err
	}
	return s.MemoryIdempotencyStore.Put(ctx, record)
}

type lifecycleLogEntry struct {
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Error   string         `json:"error,omitempty"`
	Fields  map[string]any `json:"fields"`
}

type recordingLifecycleLogState struct {
	mu      sync.Mutex
	entries []lifecycleLogEntry
}

type recordingLifecycleLogger struct {
	state  *recordingLifecycleLogState
	fields []observability.Field
}

func newRecordingLifecycleLogger() *recordingLifecycleLogger {
	return &recordingLifecycleLogger{state: &recordingLifecycleLogState{}}
}

func (l *recordingLifecycleLogger) Debug(_ context.Context, message string, fields ...observability.Field) {
	l.record("debug", message, nil, fields)
}

func (l *recordingLifecycleLogger) Info(_ context.Context, message string, fields ...observability.Field) {
	l.record("info", message, nil, fields)
}

func (l *recordingLifecycleLogger) Warn(_ context.Context, message string, fields ...observability.Field) {
	l.record("warn", message, nil, fields)
}

func (l *recordingLifecycleLogger) Error(_ context.Context, message string, err error, fields ...observability.Field) {
	l.record("error", message, err, fields)
}

func (l *recordingLifecycleLogger) With(fields ...observability.Field) observability.StructuredLogger {
	combined := make([]observability.Field, 0, len(l.fields)+len(fields))
	combined = append(combined, l.fields...)
	combined = append(combined, fields...)
	return &recordingLifecycleLogger{state: l.state, fields: combined}
}

func (*recordingLifecycleLogger) Sync() {}

func (l *recordingLifecycleLogger) record(level, message string, err error, fields []observability.Field) {
	allFields := make([]observability.Field, 0, len(l.fields)+len(fields))
	allFields = append(allFields, l.fields...)
	allFields = append(allFields, fields...)
	entry := lifecycleLogEntry{Level: level, Message: message, Fields: decodeLifecycleFields(allFields)}
	if err != nil {
		entry.Error = err.Error()
	}
	l.state.mu.Lock()
	l.state.entries = append(l.state.entries, entry)
	l.state.mu.Unlock()
}

func (l *recordingLifecycleLogger) Entries() []lifecycleLogEntry {
	l.state.mu.Lock()
	defer l.state.mu.Unlock()
	entries := make([]lifecycleLogEntry, len(l.state.entries))
	for i, entry := range l.state.entries {
		entries[i] = entry
		entries[i].Fields = cloneLifecycleFields(entry.Fields)
	}
	return entries
}

func (l *recordingLifecycleLogger) Reset() {
	l.state.mu.Lock()
	l.state.entries = nil
	l.state.mu.Unlock()
}

type lifecycleSpanRecord struct {
	Name   string         `json:"name"`
	Error  string         `json:"error,omitempty"`
	Fields map[string]any `json:"fields"`
}

type recordingTraceProvider struct {
	mu          sync.Mutex
	startedName string
	startFields map[string]any
	span        *recordingSpan
}

func (p *recordingTraceProvider) Start(ctx context.Context, name string, fields ...observability.Field) (context.Context, observability.Span) {
	tc := observability.MustTraceContext(ctx)
	tc.ParentSpanID = tc.SpanID
	tc.SpanID = "span-tool-call"
	ctx = observability.WithTraceContext(ctx, tc)
	span := &recordingSpan{tc: tc}
	p.mu.Lock()
	p.startedName = name
	p.startFields = decodeLifecycleFields(fields)
	p.span = span
	p.mu.Unlock()
	return ctx, span
}

type recordingSpan struct {
	mu     sync.Mutex
	tc     observability.TraceContext
	ended  bool
	events []lifecycleSpanRecord
	errors []lifecycleSpanRecord
}

func (s *recordingSpan) End() {
	s.mu.Lock()
	s.ended = true
	s.mu.Unlock()
}

func (s *recordingSpan) AddEvent(name string, fields ...observability.Field) {
	s.mu.Lock()
	s.events = append(s.events, lifecycleSpanRecord{Name: name, Fields: decodeLifecycleFields(fields)})
	s.mu.Unlock()
}

func (s *recordingSpan) RecordError(err error, fields ...observability.Field) {
	record := lifecycleSpanRecord{Fields: decodeLifecycleFields(fields)}
	if err != nil {
		record.Error = err.Error()
	}
	s.mu.Lock()
	s.errors = append(s.errors, record)
	s.mu.Unlock()
}

func (s *recordingSpan) TraceContext() observability.TraceContext {
	return s.tc
}

func (p *recordingTraceProvider) Snapshot() (string, map[string]any, *recordingSpan) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.startedName, cloneLifecycleFields(p.startFields), p.span
}

func (s *recordingSpan) Snapshot() (observability.TraceContext, bool, []lifecycleSpanRecord, []lifecycleSpanRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cloneRecords := func(records []lifecycleSpanRecord) []lifecycleSpanRecord {
		out := make([]lifecycleSpanRecord, len(records))
		for i, record := range records {
			out[i] = record
			out[i].Fields = cloneLifecycleFields(record.Fields)
		}
		return out
	}
	return s.tc, s.ended, cloneRecords(s.events), cloneRecords(s.errors)
}

func decodeLifecycleFields(fields []observability.Field) map[string]any {
	encoder := zapcore.NewMapObjectEncoder()
	for _, field := range fields {
		field.AddTo(encoder)
	}
	return cloneLifecycleFields(encoder.Fields)
}

func cloneLifecycleFields(fields map[string]any) map[string]any {
	if fields == nil {
		return nil
	}
	out := make(map[string]any, len(fields))
	for key, value := range fields {
		out[key] = value
	}
	return out
}

func assertLifecycleLogFields(t *testing.T, logger *recordingLifecycleLogger, message string, want map[string]any) {
	t.Helper()
	for _, entry := range logger.Entries() {
		if entry.Message == message && lifecycleFieldsMatch(entry.Fields, want) {
			return
		}
	}
	t.Fatalf("missing lifecycle log message=%q fields=%#v entries=%#v", message, want, logger.Entries())
}

func assertLifecycleLogMessageCount(t *testing.T, logger *recordingLifecycleLogger, message string, want int) {
	t.Helper()
	count := 0
	for _, entry := range logger.Entries() {
		if entry.Message == message {
			count++
		}
	}
	if count != want {
		t.Fatalf("lifecycle log message=%q count=%d, want %d; entries=%#v", message, count, want, logger.Entries())
	}
}

func assertLifecycleSpanEventFields(t *testing.T, tracer *recordingTraceProvider, name string, want map[string]any) {
	t.Helper()
	_, _, span := tracer.Snapshot()
	if span == nil {
		t.Fatalf("missing lifecycle span")
	}
	_, _, events, _ := span.Snapshot()
	for _, event := range events {
		if event.Name == name && lifecycleFieldsMatch(event.Fields, want) {
			return
		}
	}
	t.Fatalf("missing span event=%q fields=%#v events=%#v", name, want, events)
}

func assertLifecycleSpanEventCount(t *testing.T, tracer *recordingTraceProvider, name string, want int) {
	t.Helper()
	_, _, span := tracer.Snapshot()
	if span == nil {
		if want == 0 {
			return
		}
		t.Fatalf("missing lifecycle span for event=%q", name)
	}
	_, _, events, _ := span.Snapshot()
	count := 0
	for _, event := range events {
		if event.Name == name {
			count++
		}
	}
	if count != want {
		t.Fatalf("span event=%q count=%d, want %d; events=%#v", name, count, want, events)
	}
}

func assertLifecycleTraceEnded(t *testing.T, tracer *recordingTraceProvider, toolName string) {
	t.Helper()
	name, startFields, span := tracer.Snapshot()
	if name != "tool.call" || span == nil {
		t.Fatalf("trace name=%q span=%#v", name, span)
	}
	if !lifecycleFieldsMatch(startFields, map[string]any{
		"trace_id": "trace-1", "session_id": "sess-1", "run_id": "run-1", "step_id": "step-1",
		"agent_id": "agent-a", "tool_call_id": "tc-1", "tool_name": toolName, "parent_span_id": "span-parent",
	}) {
		t.Fatalf("span start fields=%#v", startFields)
	}
	_, ended, _, _ := span.Snapshot()
	if !ended {
		t.Fatalf("tool.call span did not end")
	}
}

func assertLifecycleSafeRecordedErrors(t *testing.T, logger *recordingLifecycleLogger, tracer *recordingTraceProvider, errorType ErrorType) {
	t.Helper()
	foundLogger := false
	for _, entry := range logger.Entries() {
		if entry.Message == "tool call failed" && strings.Contains(entry.Error, string(errorType)) {
			foundLogger = true
		}
	}
	_, _, span := tracer.Snapshot()
	foundSpan := false
	if span != nil {
		_, _, _, recordedErrors := span.Snapshot()
		for _, record := range recordedErrors {
			if strings.Contains(record.Error, string(errorType)) {
				foundSpan = true
			}
		}
	}
	if !foundLogger || !foundSpan {
		t.Fatalf("safe categorized errors logger=%v span=%v", foundLogger, foundSpan)
	}
}

func assertSafeLifecycleObservability(t *testing.T, logger *recordingLifecycleLogger, tracer *recordingTraceProvider, sentinels ...string) {
	t.Helper()
	name, startFields, span := tracer.Snapshot()
	var spanSnapshot any
	if span != nil {
		tc, ended, events, recordedErrors := span.Snapshot()
		spanSnapshot = struct {
			Trace  observability.TraceContext
			Ended  bool
			Events []lifecycleSpanRecord
			Errors []lifecycleSpanRecord
		}{tc, ended, events, recordedErrors}
	}
	encoded, err := json.Marshal(struct {
		Logs        []lifecycleLogEntry
		SpanName    string
		StartFields map[string]any
		Span        any
	}{logger.Entries(), name, startFields, spanSnapshot})
	if err != nil {
		t.Fatalf("encode observability records: %v", err)
	}
	for _, sentinel := range sentinels {
		if strings.Contains(string(encoded), sentinel) {
			t.Fatalf("unsafe sentinel %q in observability records: %s", sentinel, encoded)
		}
	}
}

func lifecycleFieldsMatch(got, want map[string]any) bool {
	for key, value := range want {
		if fmt.Sprint(got[key]) != fmt.Sprint(value) {
			return false
		}
	}
	return true
}

func assertSingleLifecycleTerminal(t *testing.T, events []observability.AgentEvent, status ToolCallStatus) {
	t.Helper()
	wantType := observability.EventToolCallCompleted
	if status == ToolCallFailed {
		wantType = observability.EventToolCallFailed
	}
	terminals := 0
	for _, event := range events {
		if event.EventType == observability.EventToolCallCompleted || event.EventType == observability.EventToolCallFailed {
			terminals++
			if event.EventType != wantType {
				t.Fatalf("terminal type=%s want=%s", event.EventType, wantType)
			}
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal count=%d events=%#v", terminals, eventTypes(events))
	}
}

type contextSensitiveEventStore struct {
	recordingEventStore
	failedContextHadDeadline bool
}

func (s *contextSensitiveEventStore) AppendEvent(ctx context.Context, event observability.AgentEvent) (*EventAppendResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if event.EventType == observability.EventToolCallFailed {
		_, s.failedContextHadDeadline = ctx.Deadline()
	}
	return s.recordingEventStore.AppendEvent(ctx, event)
}
