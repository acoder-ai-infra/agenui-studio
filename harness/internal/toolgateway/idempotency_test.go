package toolgateway

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestGatewayIdempotencyUsesResolvedArtifactContent(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	firstRef := putScopedArtifact(t, artifactStore, "tenant-a", "sess-1", "run-1", "debug_payload", `{"query":"hotel"}`).ArtifactRef
	secondRef := putScopedArtifact(t, artifactStore, "tenant-a", "sess-1", "run-1", "debug_payload", `{"query":"hotel"}`).ArtifactRef
	var calls atomic.Int32
	gateway := newGatewayForHardening(&recordingEventStore{}, artifactStore, nil, nil, defaultSearchDefinition(), func(context.Context, FunctionCall) (*FunctionResult, error) {
		calls.Add(1)
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})
	firstReq := baseToolRequest(nil)
	firstReq.ArgumentsRef = firstRef
	firstReq.Policy.IdempotencyKey = "resolved-content"
	first, err := gateway.Invoke(ctx, firstReq)
	if err != nil || first.Status != ToolCallSucceeded {
		t.Fatalf("first result=%#v err=%v", first, err)
	}
	secondReq := firstReq
	secondReq.ArgumentsRef = secondRef
	second, err := gateway.Invoke(ctx, secondReq)
	if err != nil || second.Status != ToolCallSucceeded {
		t.Fatalf("same resolved content result=%#v err=%v", second, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("equal resolved content executed %d times", calls.Load())
	}
}

func TestGatewayRechecksPolicyBeforeReturningCachedResult(t *testing.T) {
	def := defaultSearchDefinition()
	registry := &mutableToolRegistry{def: def}
	eventStore := &recordingEventStore{}
	var calls atomic.Int32
	gateway := NewGateway(GatewayConfig{
		Registry:        registry,
		SchemaValidator: BasicSchemaValidator{},
		EventStore:      eventStore,
		Idempotency:     NewMemoryIdempotencyStore(),
		Executors: []ToolExecutor{NewFunctionExecutor(map[string]FunctionTool{
			"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
				calls.Add(1)
				return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
			},
		})},
		IDGenerator: fixedIDGenerator{},
		Clock:       fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.IdempotencyKey = "policy-recheck"
	first, err := gateway.Invoke(testTraceContext(), req)
	if err != nil || first.Status != ToolCallSucceeded {
		t.Fatalf("first result=%#v err=%v", first, err)
	}
	blocked := def
	blocked.Permissions.AllowedAgents = []string{"agent-b"}
	registry.def = blocked
	req.ToolCallID = "tc-policy-2"
	second, err := gateway.Invoke(testTraceContext(), req)
	if err != nil {
		t.Fatalf("policy denial should be a failed result for the new call: %v", err)
	}
	assertFailedResult(t, second, ErrorTypePermissionDenied, false)
	if calls.Load() != 1 {
		t.Fatalf("policy recheck calls=%d, want 1", calls.Load())
	}
}

func TestGatewayScopesIdempotencyByTenantUserAndAgent(t *testing.T) {
	def := defaultSearchDefinition()
	def.Permissions.AllowedAgents = []string{"agent-a", "agent-b"}
	var calls atomic.Int32
	gateway := NewGateway(GatewayConfig{
		Registry:        NewStaticRegistry([]ToolDefinition{def}),
		SchemaValidator: BasicSchemaValidator{},
		EventStore:      &recordingEventStore{},
		Idempotency:     NewMemoryIdempotencyStore(),
		Executors: []ToolExecutor{NewFunctionExecutor(map[string]FunctionTool{
			"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
				calls.Add(1)
				return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
			},
		})},
		IDGenerator: fixedIDGenerator{},
		Clock:       fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})
	firstReq := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	firstReq.Policy.IdempotencyKey = "shared-logical-key"
	if result, err := gateway.Invoke(testTraceContext(), firstReq); err != nil || result.Status != ToolCallSucceeded {
		t.Fatalf("first scope result=%#v err=%v", result, err)
	}

	secondReq := firstReq
	secondReq.ToolCallID = "tc-scope-2"
	secondReq.UserID = "user-b"
	secondReq.AgentID = "agent-b"
	secondReq.Caller.AgentID = "agent-b"
	secondCtx := observability.WithTraceContext(context.Background(), observability.TraceContext{
		TraceID: "trace-2", SpanID: "span-2", TenantID: "tenant-a", UserID: "user-b",
		SessionID: "sess-1", RunID: "run-1", AgentID: "agent-b",
	})
	if result, err := gateway.Invoke(secondCtx, secondReq); err != nil || result.Status != ToolCallSucceeded {
		t.Fatalf("second scope result=%#v err=%v", result, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("security scopes shared cached data, calls=%d", calls.Load())
	}
}

func TestGatewayIdempotencyRejectsChangedDefinitionIdentity(t *testing.T) {
	def := defaultSearchDefinition()
	registry := &mutableToolRegistry{def: def}
	var calls atomic.Int32
	gateway := NewGateway(GatewayConfig{
		Registry: registry, SchemaValidator: BasicSchemaValidator{}, EventStore: &recordingEventStore{}, Idempotency: NewMemoryIdempotencyStore(),
		Executors: []ToolExecutor{NewFunctionExecutor(map[string]FunctionTool{
			"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
				calls.Add(1)
				return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
			},
		})}, IDGenerator: fixedIDGenerator{}, Clock: fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.IdempotencyKey = "definition-change"
	if result, err := gateway.Invoke(testTraceContext(), req); err != nil || result.Status != ToolCallSucceeded {
		t.Fatalf("first result=%#v err=%v", result, err)
	}
	changed := def
	changed.ResultPolicy.MaxSSEPreviewBytes = 32
	registry.def = changed
	result, err := gateway.Invoke(testTraceContext(), req)
	if result != nil || !IsErrorType(err, ErrorTypeIdempotencyConflict) {
		t.Fatalf("changed definition result=%#v err=%v, want direct conflict", result, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("changed definition executed tool, calls=%d", calls.Load())
	}
}

func TestGatewayPreClaimFailureDoesNotAppendCompetingTerminal(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*ToolDefinition, *ToolCallRequest)
		errorType ErrorType
	}{
		{
			name: "policy",
			mutate: func(def *ToolDefinition, _ *ToolCallRequest) {
				def.Permissions.AllowedAgents = []string{"agent-b"}
			},
			errorType: ErrorTypePermissionDenied,
		},
		{
			name: "schema",
			mutate: func(def *ToolDefinition, _ *ToolCallRequest) {
				def.InputSchema = json.RawMessage(`{"type":"object","required":["missing"]}`)
			},
			errorType: ErrorTypeSchemaValidationFailed,
		},
		{
			name: "artifact",
			mutate: func(_ *ToolDefinition, req *ToolCallRequest) {
				req.Arguments = nil
				req.ArgumentsRef = "artifact://missing"
			},
			errorType: ErrorTypeArtifactError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := defaultSearchDefinition()
			registry := &mutableToolRegistry{def: def}
			eventStore := &recordingEventStore{}
			var calls atomic.Int32
			gateway := NewGateway(GatewayConfig{
				Registry: registry, SchemaValidator: BasicSchemaValidator{}, EventStore: eventStore,
				Executors: []ToolExecutor{NewFunctionExecutor(map[string]FunctionTool{
					"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
						calls.Add(1)
						return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
					},
				})}, IDGenerator: fixedIDGenerator{}, Clock: fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
			})
			req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
			req.Policy.IdempotencyKey = "preclaim-terminal-" + tt.name
			first, err := gateway.Invoke(testTraceContext(), req)
			if err != nil || first.Status != ToolCallSucceeded {
				t.Fatalf("first result=%#v err=%v", first, err)
			}
			eventsBeforeFailure := len(eventStore.events)
			changed := def
			tt.mutate(&changed, &req)
			registry.def = changed

			second, err := gateway.Invoke(testTraceContext(), req)
			if second != nil || !IsErrorType(err, tt.errorType) {
				t.Fatalf("second result=%#v err=%v, want direct %s", second, err, tt.errorType)
			}
			if calls.Load() != 1 {
				t.Fatalf("pre-claim failure dispatched executor, calls=%d", calls.Load())
			}
			if len(eventStore.events) != eventsBeforeFailure {
				t.Fatalf("pre-claim failure appended competing terminal: before=%d after=%d", eventsBeforeFailure, len(eventStore.events))
			}
		})
	}
}

func TestGatewayPreClaimFailedTerminalIsReplayOnly(t *testing.T) {
	eventStore := &recordingEventStore{}
	policy := &mutablePolicyEngine{decision: DecisionBlock}
	var calls atomic.Int32
	gateway := NewGateway(GatewayConfig{
		Registry: NewStaticRegistry([]ToolDefinition{defaultSearchDefinition()}), SchemaValidator: BasicSchemaValidator{},
		EventStore: eventStore, PolicyEngine: policy,
		Executors: []ToolExecutor{NewFunctionExecutor(map[string]FunctionTool{
			"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
				calls.Add(1)
				return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
			},
		})}, IDGenerator: fixedIDGenerator{}, Clock: fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.IdempotencyKey = "preclaim-failed-terminal"
	first, err := gateway.Invoke(testTraceContext(), req)
	if err != nil {
		t.Fatalf("first denied result=%#v err=%v", first, err)
	}
	assertFailedResult(t, first, ErrorTypePermissionDenied, false)
	eventsBeforeReplay := len(eventStore.events)
	repeated, err := gateway.Invoke(testTraceContext(), req)
	if repeated != nil || !IsErrorType(err, ErrorTypePermissionDenied) {
		t.Fatalf("current denial result=%#v err=%v, want direct permission denial", repeated, err)
	}
	if calls.Load() != 0 || len(eventStore.events) != eventsBeforeReplay {
		t.Fatalf("current denial calls=%d events before=%d after=%d", calls.Load(), eventsBeforeReplay, len(eventStore.events))
	}

	policy.decision = DecisionAllow
	second, err := gateway.Invoke(testTraceContext(), req)
	if err != nil {
		t.Fatalf("failed terminal replay result=%#v err=%v", second, err)
	}
	assertFailedResult(t, second, ErrorTypePermissionDenied, false)
	if calls.Load() != 0 {
		t.Fatalf("same-call pre-claim terminal was re-executed, calls=%d", calls.Load())
	}
	if len(eventStore.events) != eventsBeforeReplay {
		t.Fatalf("failed terminal replay appended events: before=%d after=%d", eventsBeforeReplay, len(eventStore.events))
	}
}

func TestGatewayDoesNotReleasePreClaimAfterAmbiguousFailedTerminalAppend(t *testing.T) {
	sentinel := errors.New("failed terminal persistence is ambiguous")
	eventStore := &failOnceEventStore{failType: observability.EventToolCallFailed, err: sentinel}
	policy := &mutablePolicyEngine{decision: DecisionBlock}
	var calls atomic.Int32
	gateway := NewGateway(GatewayConfig{
		Registry: NewStaticRegistry([]ToolDefinition{defaultSearchDefinition()}), SchemaValidator: BasicSchemaValidator{},
		EventStore: eventStore, PolicyEngine: policy,
		Executors: []ToolExecutor{NewFunctionExecutor(map[string]FunctionTool{
			"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
				calls.Add(1)
				return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
			},
		})}, IDGenerator: fixedIDGenerator{}, Clock: fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.IdempotencyKey = "ambiguous-preclaim-terminal"
	first, err := gateway.Invoke(testTraceContext(), req)
	if first != nil || !errors.Is(err, sentinel) {
		t.Fatalf("first result=%#v err=%v, want ambiguous terminal error", first, err)
	}

	policy.decision = DecisionAllow
	second, err := gateway.Invoke(testTraceContext(), req)
	if second != nil || !IsErrorType(err, ErrorTypeDuplicateInflight) {
		t.Fatalf("second result=%#v err=%v, want retained reservation", second, err)
	}
	if calls.Load() != 0 || len(eventStore.events) != 0 {
		t.Fatalf("ambiguous terminal was retried: calls=%d events=%#v", calls.Load(), eventTypes(eventStore.events))
	}
}

func TestGatewayIdempotencyRejectsDifferentToolCallIDForSameLogicalKey(t *testing.T) {
	eventStore := &recordingEventStore{}
	var calls atomic.Int32
	gateway := newGatewayForHardening(eventStore, nil, nil, nil, defaultSearchDefinition(), func(context.Context, FunctionCall) (*FunctionResult, error) {
		calls.Add(1)
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.IdempotencyKey = "call-identity"
	first, err := gateway.Invoke(testTraceContext(), req)
	if err != nil || first.Status != ToolCallSucceeded {
		t.Fatalf("first result=%#v err=%v", first, err)
	}
	eventsBeforeConflict := len(eventStore.events)

	secondReq := req
	secondReq.ToolCallID = "tc-different"
	second, err := gateway.Invoke(testTraceContext(), secondReq)
	if second != nil || !IsErrorType(err, ErrorTypeIdempotencyConflict) {
		t.Fatalf("different ToolCallID result=%#v err=%v, want direct conflict", second, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("different ToolCallID executed tool, calls=%d", calls.Load())
	}
	if len(eventStore.events) != eventsBeforeConflict {
		t.Fatalf("different ToolCallID conflict appended canonical events: before=%d after=%d", eventsBeforeConflict, len(eventStore.events))
	}
}

func TestGatewayIdempotencyRejectsDifferentLogicalKeyForSameToolCallID(t *testing.T) {
	eventStore := &recordingEventStore{}
	var calls atomic.Int32
	gateway := newGatewayForHardening(eventStore, nil, nil, nil, defaultSearchDefinition(), func(context.Context, FunctionCall) (*FunctionResult, error) {
		calls.Add(1)
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.IdempotencyKey = "logical-key-one"
	first, err := gateway.Invoke(testTraceContext(), req)
	if err != nil || first.Status != ToolCallSucceeded {
		t.Fatalf("first result=%#v err=%v", first, err)
	}
	eventsBeforeConflict := len(eventStore.events)

	req.Policy.IdempotencyKey = "logical-key-two"
	second, err := gateway.Invoke(testTraceContext(), req)
	if second != nil || !IsErrorType(err, ErrorTypeIdempotencyConflict) {
		t.Fatalf("changed logical key result=%#v err=%v, want direct conflict", second, err)
	}
	if calls.Load() != 1 || len(eventStore.events) != eventsBeforeConflict {
		t.Fatalf("changed logical key calls=%d events before=%d after=%d", calls.Load(), eventsBeforeConflict, len(eventStore.events))
	}
}

func TestGatewayDefaultsToProcessLocalIdempotency(t *testing.T) {
	var calls atomic.Int32
	gateway := newGatewayForHardening(&recordingEventStore{}, nil, nil, nil, defaultSearchDefinition(), func(context.Context, FunctionCall) (*FunctionResult, error) {
		calls.Add(1)
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.IdempotencyKey = "default-memory"
	first, err := gateway.Invoke(testTraceContext(), req)
	if err != nil || first.Status != ToolCallSucceeded {
		t.Fatalf("first result=%#v err=%v", first, err)
	}
	second, err := gateway.Invoke(testTraceContext(), req)
	if err != nil || second.Status != ToolCallSucceeded {
		t.Fatalf("second result=%#v err=%v", second, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("nil IdempotencyStore executed %d times", calls.Load())
	}
}

func TestScopedIdempotencyStoreKeySeparatesEveryIdentityDimension(t *testing.T) {
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	base := scopedIdempotencyStoreKey(req, "logical")
	for _, tt := range []struct {
		name   string
		mutate func(*ToolCallRequest)
	}{
		{name: "tenant", mutate: func(req *ToolCallRequest) { req.TenantID = "tenant-b" }},
		{name: "user", mutate: func(req *ToolCallRequest) { req.UserID = "user-b" }},
		{name: "agent", mutate: func(req *ToolCallRequest) { req.AgentID = "agent-b" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			changed := req
			tt.mutate(&changed)
			if got := scopedIdempotencyStoreKey(changed, "logical"); got == base {
				t.Fatalf("%s identity was not scoped", tt.name)
			}
		})
	}

	left := req
	left.UserID = "u\x00a"
	left.AgentID = "g"
	right := req
	right.UserID = "u"
	right.AgentID = "a"
	if leftKey, rightKey := scopedIdempotencyStoreKey(left, "k"), scopedIdempotencyStoreKey(right, "g\x00k"); leftKey == rightKey {
		t.Fatalf("structured identities collided: %q", leftKey)
	}
}

func TestGatewayConcurrentDefaultIdempotencyExecutesOnce(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	eventStore := &recordingEventStore{}
	gateway := newGatewayForHardening(eventStore, nil, nil, nil, defaultSearchDefinition(), func(context.Context, FunctionCall) (*FunctionResult, error) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.IdempotencyKey = "default-concurrent"
	type invokeResult struct {
		result *ToolCallResult
		err    error
	}
	firstDone := make(chan invokeResult, 1)
	go func() {
		result, err := gateway.Invoke(testTraceContext(), req)
		firstDone <- invokeResult{result: result, err: err}
	}()
	<-started
	secondDone := make(chan invokeResult, 1)
	go func() {
		result, err := gateway.Invoke(testTraceContext(), req)
		secondDone <- invokeResult{result: result, err: err}
	}()

	select {
	case second := <-secondDone:
		if second.result != nil || !IsErrorType(second.err, ErrorTypeDuplicateInflight) {
			close(release)
			t.Fatalf("duplicate result=%#v err=%v", second.result, second.err)
		}
	case <-started:
		close(release)
		<-firstDone
		<-secondDone
		t.Fatalf("concurrent duplicate dispatched executor, calls=%d", calls.Load())
	case <-time.After(time.Second):
		close(release)
		t.Fatalf("concurrent duplicate did not resolve")
	}
	close(release)
	first := <-firstDone
	if first.err != nil || first.result.Status != ToolCallSucceeded {
		t.Fatalf("first result=%#v err=%v", first.result, first.err)
	}
	if calls.Load() != 1 || len(eventStore.events) != 2 {
		t.Fatalf("calls=%d events=%#v", calls.Load(), eventTypes(eventStore.events))
	}
}

func TestMemoryIdempotencyStoreClaimIsAtomic(t *testing.T) {
	store := NewMemoryIdempotencyStore()
	rec := IdempotencyRecord{
		Key:           "idem-1",
		ToolCallID:    "tc-1",
		ToolName:      "search_kb",
		ArgumentsHash: "hash-1",
		Status:        ToolCallRunning,
		CreatedAt:     time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC),
	}

	first, claimed, err := store.Claim(context.Background(), rec)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !claimed {
		t.Fatalf("first claim should reserve the key")
	}
	if first.Key != rec.Key || first.Status != ToolCallRunning {
		t.Fatalf("unexpected first claim record: %#v", first)
	}

	second, claimed, err := store.Claim(context.Background(), IdempotencyRecord{
		Key:           "idem-1",
		ToolCallID:    "tc-2",
		ToolName:      "search_kb",
		ArgumentsHash: "hash-2",
		Status:        ToolCallRunning,
		CreatedAt:     rec.CreatedAt,
	})
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if claimed {
		t.Fatalf("second claim should return existing record")
	}
	if second.ToolCallID != "tc-1" || second.ArgumentsHash != "hash-1" {
		t.Fatalf("second claim should return first record, got %#v", second)
	}
}

func TestMemoryIdempotencyStoreReleasesOnlyMatchingRunningClaim(t *testing.T) {
	store := NewMemoryIdempotencyStore()
	releaser, ok := any(store).(interface {
		Release(context.Context, string, string) error
	})
	if !ok {
		t.Fatalf("memory idempotency store must support releasing unexecuted claims")
	}
	ctx := context.Background()
	running := IdempotencyRecord{
		Key: "idem-1", ToolCallID: "tc-1", ToolName: "search_kb",
		ArgumentsHash: "hash-1", Status: ToolCallRunning,
	}
	if _, claimed, err := store.Claim(ctx, running); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	if err := releaser.Release(ctx, running.Key, "tc-other"); err != nil {
		t.Fatalf("release wrong tool call: %v", err)
	}
	if _, found, err := store.Get(ctx, running.Key); err != nil || !found {
		t.Fatalf("wrong tool call must not release claim: found=%v err=%v", found, err)
	}

	terminal := running
	terminal.Status = ToolCallSucceeded
	if err := store.Put(ctx, terminal); err != nil {
		t.Fatalf("put terminal record: %v", err)
	}
	if err := releaser.Release(ctx, terminal.Key, terminal.ToolCallID); err != nil {
		t.Fatalf("release terminal record: %v", err)
	}
	if _, found, err := store.Get(ctx, terminal.Key); err != nil || !found {
		t.Fatalf("terminal record must not be released: found=%v err=%v", found, err)
	}

	if err := store.Put(ctx, running); !IsErrorType(err, ErrorTypeIdempotencyConflict) {
		t.Fatalf("terminal overwrite error=%v, want idempotency conflict", err)
	}
	if got, found, err := store.Get(ctx, terminal.Key); err != nil || !found || got.Status != ToolCallSucceeded {
		t.Fatalf("terminal was overwritten: found=%v record=%#v err=%v", found, got, err)
	}

	releasable := running
	releasable.Key = "idem-releasable"
	if _, claimed, err := store.Claim(ctx, releasable); err != nil || !claimed {
		t.Fatalf("claim releasable: claimed=%v err=%v", claimed, err)
	}
	if err := releaser.Release(ctx, releasable.Key, releasable.ToolCallID); err != nil {
		t.Fatalf("release matching running claim: %v", err)
	}
	if _, found, err := store.Get(ctx, releasable.Key); err != nil || found {
		t.Fatalf("matching running claim should be deleted: found=%v err=%v", found, err)
	}
}

func TestGatewayReplaysRetryableFailedTerminalWithoutReexecution(t *testing.T) {
	var calls atomic.Int32
	eventStore := &recordingEventStore{}
	gateway := newGatewayForHardening(eventStore, nil, nil, nil, defaultSearchDefinition(), func(context.Context, FunctionCall) (*FunctionResult, error) {
		calls.Add(1)
		return nil, NewToolError(ErrorTypeUpstreamError, "temporary", true, nil)
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.IdempotencyKey = "failed-terminal"
	first, err := gateway.Invoke(testTraceContext(), req)
	if err != nil {
		t.Fatalf("first result=%#v err=%v", first, err)
	}
	assertFailedResult(t, first, ErrorTypeUpstreamError, true)
	eventsBeforeReplay := len(eventStore.events)
	second, err := gateway.Invoke(testTraceContext(), req)
	if err != nil {
		t.Fatalf("second result=%#v err=%v", second, err)
	}
	assertFailedResult(t, second, ErrorTypeUpstreamError, true)
	if calls.Load() != 1 || len(eventStore.events) != eventsBeforeReplay {
		t.Fatalf("failed terminal calls=%d events before=%d after=%d", calls.Load(), eventsBeforeReplay, len(eventStore.events))
	}
}

func TestGatewaySerializesFallbackIdempotencyClaim(t *testing.T) {
	store := newDelayedGetPutReleaseStore()
	var calls atomic.Int32
	gateway := newGatewayForHardening(&recordingEventStore{}, nil, store, nil, defaultSearchDefinition(), func(context.Context, FunctionCall) (*FunctionResult, error) {
		calls.Add(1)
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.IdempotencyKey = "fallback-atomic"
	start := make(chan struct{})
	type invocation struct {
		result *ToolCallResult
		err    error
	}
	done := make(chan invocation, 2)
	for range 2 {
		go func() {
			<-start
			result, err := gateway.Invoke(testTraceContext(), req)
			done <- invocation{result: result, err: err}
		}()
	}
	close(start)
	first, second := <-done, <-done
	for i, got := range []invocation{first, second} {
		if got.err != nil && !IsErrorType(got.err, ErrorTypeDuplicateInflight) {
			t.Fatalf("invoke %d result=%#v err=%v", i, got.result, got.err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("fallback Get/Put store executed %d times", calls.Load())
	}
}

func TestGatewayFailsClosedForIdempotencyStoreWithoutRelease(t *testing.T) {
	store := &getPutOnlyIdempotencyStore{records: make(map[string]IdempotencyRecord)}
	called := false
	eventStore := &recordingEventStore{}
	gateway := newGatewayForHardening(eventStore, nil, store, nil, defaultSearchDefinition(), func(context.Context, FunctionCall) (*FunctionResult, error) {
		called = true
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})
	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if result != nil || !IsErrorType(err, ErrorTypeInternal) {
		t.Fatalf("result=%#v err=%v, want direct internal error", result, err)
	}
	if called || len(eventStore.events) != 0 {
		t.Fatalf("unsupported idempotency store called=%v events=%#v", called, eventTypes(eventStore.events))
	}
}

func TestGatewayIdempotencyConflictsForDifferentArgumentsRefs(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	firstRef := putScopedArtifact(t, artifactStore, "tenant-a", "sess-1", "run-1", "debug_payload", `{"query":"hotel"}`).ArtifactRef
	secondRef := putScopedArtifact(t, artifactStore, "tenant-a", "sess-1", "run-1", "debug_payload", `{"query":"airport"}`).ArtifactRef
	calls := 0
	eventStore := &recordingEventStore{}
	gateway := newGatewayForHardening(eventStore, artifactStore, NewMemoryIdempotencyStore(), nil, defaultSearchDefinition(), func(context.Context, FunctionCall) (*FunctionResult, error) {
		calls++
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})
	firstReq := baseToolRequest(nil)
	firstReq.ArgumentsRef = firstRef
	firstReq.Policy.IdempotencyKey = "idem-ref"
	first, err := gateway.Invoke(ctx, firstReq)
	if err != nil || first.Status != ToolCallSucceeded {
		t.Fatalf("first invoke result=%#v err=%v", first, err)
	}
	eventsBeforeConflict := len(eventStore.events)

	secondReq := baseToolRequest(nil)
	secondReq.ArgumentsRef = secondRef
	secondReq.Policy.IdempotencyKey = "idem-ref"
	second, err := gateway.Invoke(ctx, secondReq)
	if second != nil || !IsErrorType(err, ErrorTypeIdempotencyConflict) {
		t.Fatalf("second invoke result=%#v err=%v", second, err)
	}
	if calls != 1 {
		t.Fatalf("different ref under one idempotency key must not replay or execute, calls=%d", calls)
	}
	if len(eventStore.events) != eventsBeforeConflict {
		t.Fatalf("idempotency conflict must not append canonical events: before=%d after=%d", eventsBeforeConflict, len(eventStore.events))
	}
}

func TestGatewayGeneratedToolCallIDIncludesArgumentsRef(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	firstRef := putScopedArtifact(t, artifactStore, "tenant-a", "sess-1", "run-1", "debug_payload", `{"query":"hotel"}`).ArtifactRef
	secondRef := putScopedArtifact(t, artifactStore, "tenant-a", "sess-1", "run-1", "debug_payload", `{"query":"airport"}`).ArtifactRef
	gateway := newGatewayForHardening(&recordingEventStore{}, artifactStore, nil, nil, defaultSearchDefinition(), func(context.Context, FunctionCall) (*FunctionResult, error) {
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})

	firstReq := baseToolRequest(nil)
	firstReq.ToolCallID = ""
	firstReq.ArgumentsRef = firstRef
	first, err := gateway.Invoke(ctx, firstReq)
	if err != nil {
		t.Fatalf("first invoke: %v", err)
	}
	secondReq := baseToolRequest(nil)
	secondReq.ToolCallID = ""
	secondReq.ArgumentsRef = secondRef
	second, err := gateway.Invoke(ctx, secondReq)
	if err != nil {
		t.Fatalf("second invoke: %v", err)
	}
	if first.ToolCallID == second.ToolCallID {
		t.Fatalf("generated tool call ids collide for distinct immutable argument refs: %q", first.ToolCallID)
	}
}

func TestGatewayDoesNotReexecuteWhenBestEffortTerminalProjectionFails(t *testing.T) {
	eventStore := &recordingEventStore{}
	idempotency := &lifecycleFailTerminalIdempotencyStore{
		MemoryIdempotencyStore: NewMemoryIdempotencyStore(),
		status:                 ToolCallSucceeded,
		err:                    errors.New("idempotency terminal unavailable"),
	}
	calls := 0
	gateway := newGatewayForHardening(eventStore, nil, idempotency, nil, defaultSearchDefinition(), func(context.Context, FunctionCall) (*FunctionResult, error) {
		calls++
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.IdempotencyKey = "terminal-journal-replay"
	first, err := gateway.Invoke(testTraceContext(), req)
	if err != nil || first == nil || first.Status != ToolCallSucceeded {
		t.Fatalf("first invoke result=%#v error=%v", first, err)
	}
	second, err := gateway.Invoke(testTraceContext(), req)
	if second != nil || !IsErrorType(err, ErrorTypeDuplicateInflight) {
		t.Fatalf("second invoke result=%#v error=%v, want running claim", second, err)
	}
	if calls != 1 {
		t.Fatalf("tool side effect replayed, calls=%d", calls)
	}
}

func TestGatewayDoesNotReexecuteWhenProductionTerminalCommitFails(t *testing.T) {
	sentinel := errors.New("terminal transaction failed")
	eventStore := &recordingEventStore{}
	calls := 0
	gateway := newGatewayForHardening(eventStore, nil, NewMemoryIdempotencyStore(), nil, defaultSearchDefinition(), func(context.Context, FunctionCall) (*FunctionResult, error) {
		calls++
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})
	gateway.terminalCommitter = failingToolTerminalCommitter{err: sentinel}
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.IdempotencyKey = "terminal-transaction-failure"

	first, err := gateway.Invoke(testTraceContext(), req)
	if first != nil || !errors.Is(err, sentinel) {
		t.Fatalf("first invoke result=%#v error=%v", first, err)
	}
	second, err := gateway.Invoke(testTraceContext(), req)
	if second != nil || !IsErrorType(err, ErrorTypeDuplicateInflight) {
		t.Fatalf("second invoke result=%#v error=%v, want running claim", second, err)
	}
	if calls != 1 {
		t.Fatalf("tool side effect replayed, calls=%d", calls)
	}
}

type failingToolTerminalCommitter struct{ err error }

func (c failingToolTerminalCommitter) CommitTerminal(context.Context, observability.AgentEvent, ToolTerminalBinding) (*EventAppendResult, error) {
	return nil, c.err
}

func TestGatewayReleasesClaimWhenStartedPersistenceFails(t *testing.T) {
	sentinel := errors.New("started event unavailable")
	eventStore := &failOnceEventStore{failType: observability.EventToolCallStarted, err: sentinel}
	stepStore := &recordingStepStore{}
	calls := 0
	gateway := newGatewayForHardening(eventStore, nil, NewMemoryIdempotencyStore(), stepStore, defaultSearchDefinition(), func(context.Context, FunctionCall) (*FunctionResult, error) {
		calls++
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.ParentStepID = "runtime-parent-step"
	req.Policy.IdempotencyKey = "idem-started"

	first, err := gateway.Invoke(testTraceContext(), req)
	if first != nil || !errors.Is(err, sentinel) {
		t.Fatalf("first invoke result=%#v err=%v, want started append failure", first, err)
	}
	second, err := gateway.Invoke(testTraceContext(), req)
	if err != nil || second.Status != ToolCallSucceeded {
		t.Fatalf("retry result=%#v err=%v", second, err)
	}
	if calls != 1 {
		t.Fatalf("retry should execute exactly once, calls=%d", calls)
	}
	if stepStore.failCalls != 1 || stepStore.lastFailure.ToolCallID != req.ToolCallID {
		t.Fatalf("started append failure should compensate the running step: calls=%d failure=%#v", stepStore.failCalls, stepStore.lastFailure)
	}
	if stepStore.lastStart.ParentStepID != req.ParentStepID {
		t.Fatalf("parent step identity was not propagated: start=%#v request=%#v", stepStore.lastStart, req)
	}
}

func TestGatewayReleasesClaimWhenStepStartFails(t *testing.T) {
	sentinel := errors.New("step store unavailable")
	stepStore := &failOnceStepStore{err: sentinel}
	calls := 0
	gateway := newGatewayForHardening(&recordingEventStore{}, nil, NewMemoryIdempotencyStore(), stepStore, defaultSearchDefinition(), func(context.Context, FunctionCall) (*FunctionResult, error) {
		calls++
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.IdempotencyKey = "idem-step"

	first, err := gateway.Invoke(testTraceContext(), req)
	if first != nil || !errors.Is(err, sentinel) {
		t.Fatalf("first invoke result=%#v err=%v, want step start failure", first, err)
	}
	second, err := gateway.Invoke(testTraceContext(), req)
	if err != nil || second.Status != ToolCallSucceeded {
		t.Fatalf("retry result=%#v err=%v", second, err)
	}
	if calls != 1 {
		t.Fatalf("retry should execute exactly once, calls=%d", calls)
	}
}

func TestGatewaySurfacesIdempotencyReleaseFailure(t *testing.T) {
	stepErr := errors.New("step store unavailable")
	releaseErr := errors.New("idempotency release unavailable")
	store := &failingReleaseIdempotencyStore{
		MemoryIdempotencyStore: NewMemoryIdempotencyStore(),
		err:                    releaseErr,
	}
	called := false
	gateway := newGatewayForHardening(&recordingEventStore{}, nil, store, &failOnceStepStore{err: stepErr}, defaultSearchDefinition(), func(context.Context, FunctionCall) (*FunctionResult, error) {
		called = true
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})
	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if result != nil || !errors.Is(err, stepErr) || !errors.Is(err, releaseErr) {
		t.Fatalf("result=%#v err=%v, want step and release errors", result, err)
	}
	if called {
		t.Fatalf("release failure path dispatched executor")
	}
}

func TestGatewayKeepsToolCallGuardWhenLogicalReleaseFails(t *testing.T) {
	stepErr := errors.New("step store unavailable")
	releaseErr := errors.New("logical idempotency release unavailable")
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.IdempotencyKey = "logical-original"
	logicalKey := scopedIdempotencyStoreKey(req, req.Policy.IdempotencyKey)
	callKey := scopedToolCallIdempotencyStoreKey(req)
	store := &selectiveFailReleaseIdempotencyStore{
		MemoryIdempotencyStore: NewMemoryIdempotencyStore(),
		failKey:                logicalKey,
		err:                    releaseErr,
	}
	eventStore := &recordingEventStore{}
	calls := 0
	gateway := newGatewayForHardening(eventStore, nil, store, &failOnceStepStore{err: stepErr}, defaultSearchDefinition(), func(context.Context, FunctionCall) (*FunctionResult, error) {
		calls++
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})

	first, err := gateway.Invoke(testTraceContext(), req)
	if first != nil || !errors.Is(err, stepErr) || !errors.Is(err, releaseErr) {
		t.Fatalf("first result=%#v err=%v, want step and logical release errors", first, err)
	}
	if calls != 0 {
		t.Fatalf("first rollback path dispatched executor %d times", calls)
	}

	retryReq := req
	retryReq.Policy.IdempotencyKey = "logical-replacement"
	second, retryErr := gateway.Invoke(testTraceContext(), retryReq)
	if second != nil || !IsErrorType(retryErr, ErrorTypeIdempotencyConflict) {
		t.Errorf("retry result=%#v err=%v, want fail-closed idempotency conflict", second, retryErr)
	}
	if calls != 0 {
		t.Errorf("changed logical key bypassed ToolCallID guard, calls=%d", calls)
	}
	if len(eventStore.events) != 0 {
		t.Errorf("guarded pre-execution retries appended events=%#v", eventTypes(eventStore.events))
	}
	releases := store.ReleaseCalls()
	if len(releases) != 1 || releases[0] != logicalKey {
		t.Errorf("release calls=%#v, want only failed inner logical key %q", releases, logicalKey)
	}
	record, found, getErr := store.Get(testTraceContext(), callKey)
	if getErr != nil || !found || record.Status != ToolCallRunning || record.LogicalKeyHash != hashString(req.Policy.IdempotencyKey) {
		t.Errorf("outer call guard record=%#v found=%v err=%v, want original running guard", record, found, getErr)
	}
}

func TestGatewayReplaysCancelledTerminalWithContextSensitiveIdempotencyStore(t *testing.T) {
	store := &contextSensitiveTerminalIdempotencyStore{MemoryIdempotencyStore: NewMemoryIdempotencyStore()}
	eventStore := &recordingEventStore{}
	ctx, cancel := context.WithCancel(testTraceContext())
	defer cancel()
	calls := 0
	gateway := newGatewayForHardening(eventStore, nil, store, nil, defaultSearchDefinition(), func(context.Context, FunctionCall) (*FunctionResult, error) {
		calls++
		cancel()
		return nil, context.Canceled
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.IdempotencyKey = "cancelled-terminal-replay"

	first, err := gateway.Invoke(ctx, req)
	if err != nil {
		t.Fatalf("first cancelled invoke: %v", err)
	}
	assertFailedResult(t, first, ErrorTypeCancelled, true)
	if got := eventTypes(eventStore.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolCallFailed {
		t.Fatalf("first cancelled lifecycle=%#v", got)
	}

	second, replayErr := gateway.Invoke(testTraceContext(), req)
	if replayErr != nil {
		t.Errorf("fresh-context replay: %v", replayErr)
	}
	if second == nil {
		t.Errorf("fresh-context replay returned nil result")
	} else {
		assertFailedResult(t, second, ErrorTypeCancelled, true)
	}
	if calls != 1 {
		t.Errorf("cancelled terminal executed %d times, want one", calls)
	}
	if len(eventStore.events) != 2 {
		t.Errorf("replay appended canonical events=%#v", eventTypes(eventStore.events))
	}
	terminalPuts, cancelledPuts, boundedPuts := store.TerminalPutStats()
	if terminalPuts != 2 || cancelledPuts != 0 || boundedPuts != 2 {
		t.Errorf("terminal Put stats total=%d cancelled=%d bounded=%d, want 2/0/2", terminalPuts, cancelledPuts, boundedPuts)
	}
	for _, key := range []string{
		scopedToolCallIdempotencyStoreKey(req),
		scopedIdempotencyStoreKey(req, req.Policy.IdempotencyKey),
	} {
		record, found, getErr := store.Get(testTraceContext(), key)
		if getErr != nil || !found || record.Status != ToolCallFailed || record.Result == nil || record.Failure == nil {
			t.Errorf("terminal record key=%q record=%#v found=%v err=%v", key, record, found, getErr)
		}
	}
}

func TestGatewayStoresFailedTerminalWhenArgumentArtifactEventPersistenceFails(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	sentinel := errors.New("argument artifact event unavailable")
	eventStore := &failOnceEventStore{failType: observability.EventToolArtifactCreated, err: sentinel}
	calls := 0
	def := defaultSearchDefinition()
	def.ResultPolicy.MaxInlineBytes = 16
	gateway := newGatewayForHardening(eventStore, artifactStore, NewMemoryIdempotencyStore(), nil, def, func(context.Context, FunctionCall) (*FunctionResult, error) {
		calls++
		return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
	})
	req := baseToolRequest(json.RawMessage(`{"query":"argument-value-longer-than-sixteen-bytes"}`))
	req.Policy.IdempotencyKey = "idem-arguments-artifact"

	first, err := gateway.Invoke(ctx, req)
	if err != nil {
		t.Fatalf("first invoke should persist failed terminal: %v", err)
	}
	assertFailedResult(t, first, ErrorTypeInternal, false)
	if got := eventTypes(eventStore.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolCallFailed {
		t.Fatalf("first lifecycle=%#v, want started then failed", got)
	}
	assertTaskCIdempotencyStatus(t, gateway, req, req.Policy.IdempotencyKey, ToolCallFailed, true)

	second, err := gateway.Invoke(ctx, req)
	if err != nil {
		t.Fatalf("replay failed terminal: %v", err)
	}
	assertFailedResult(t, second, ErrorTypeInternal, false)
	if calls != 0 || len(eventStore.events) != 2 {
		t.Fatalf("replay must not re-execute or append: calls=%d events=%#v", calls, eventTypes(eventStore.events))
	}
}

type failOnceStepStore struct {
	err    error
	failed bool
}

func (s *failOnceStepStore) StartToolStep(_ context.Context, req StartToolStepRequest) (*StepSnapshot, error) {
	if !s.failed {
		s.failed = true
		return nil, s.err
	}
	return &StepSnapshot{StepID: req.StepID, RunID: req.RunID, Status: "running"}, nil
}

func (*failOnceStepStore) CompleteToolStep(context.Context, CompleteToolStepRequest) error {
	return nil
}

func (*failOnceStepStore) FailToolStep(context.Context, FailToolStepRequest) error { return nil }

type recordingStepStore struct {
	startCalls  int
	failCalls   int
	lastStart   StartToolStepRequest
	lastFailure FailToolStepRequest
}

func (s *recordingStepStore) StartToolStep(_ context.Context, req StartToolStepRequest) (*StepSnapshot, error) {
	s.startCalls++
	s.lastStart = req
	return &StepSnapshot{StepID: req.StepID, RunID: req.RunID, Status: "running"}, nil
}

func (*recordingStepStore) CompleteToolStep(context.Context, CompleteToolStepRequest) error {
	return nil
}

func (s *recordingStepStore) FailToolStep(_ context.Context, req FailToolStepRequest) error {
	s.failCalls++
	s.lastFailure = req
	return nil
}

type mutableToolRegistry struct {
	def ToolDefinition
}

func (r *mutableToolRegistry) Get(context.Context, string, string) (*ToolDefinition, error) {
	def := cloneToolDefinition(r.def)
	return &def, nil
}

func (*mutableToolRegistry) ResolveSnapshot(context.Context, ResolveToolSnapshotRequest) (*ToolSnapshot, error) {
	return nil, errors.New("snapshot resolution is not used by this test registry")
}

type mutablePolicyEngine struct {
	decision Decision
}

func (e *mutablePolicyEngine) EvaluateToolCall(context.Context, *ToolDefinition, ToolCallRequest) (*PolicyDecision, error) {
	return &PolicyDecision{
		Decision:    e.decision,
		ReasonCode:  string(ErrorTypePermissionDenied),
		SafeMessage: "blocked by mutable policy",
	}, nil
}

type delayedGetPutReleaseStore struct {
	mu      sync.Mutex
	records map[string]IdempotencyRecord
	misses  int
	twoMiss chan struct{}
}

func newDelayedGetPutReleaseStore() *delayedGetPutReleaseStore {
	return &delayedGetPutReleaseStore{
		records: make(map[string]IdempotencyRecord),
		twoMiss: make(chan struct{}),
	}
}

func (s *delayedGetPutReleaseStore) Get(_ context.Context, key string) (*IdempotencyRecord, bool, error) {
	s.mu.Lock()
	if rec, ok := s.records[key]; ok {
		cloned := cloneIdempotencyRecord(rec)
		s.mu.Unlock()
		return &cloned, true, nil
	}
	s.misses++
	if s.misses == 2 {
		close(s.twoMiss)
	}
	twoMiss := s.twoMiss
	s.mu.Unlock()
	select {
	case <-twoMiss:
	case <-time.After(50 * time.Millisecond):
	}
	return nil, false, nil
}

func (s *delayedGetPutReleaseStore) Put(_ context.Context, rec IdempotencyRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[rec.Key] = cloneIdempotencyRecord(rec)
	return nil
}

func (s *delayedGetPutReleaseStore) Release(_ context.Context, key, toolCallID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.records[key]; ok && rec.Status == ToolCallRunning && rec.ToolCallID == toolCallID {
		delete(s.records, key)
	}
	return nil
}

type getPutOnlyIdempotencyStore struct {
	mu      sync.Mutex
	records map[string]IdempotencyRecord
}

func (s *getPutOnlyIdempotencyStore) Get(_ context.Context, key string) (*IdempotencyRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[key]
	if !ok {
		return nil, false, nil
	}
	cloned := cloneIdempotencyRecord(rec)
	return &cloned, true, nil
}

func (s *getPutOnlyIdempotencyStore) Put(_ context.Context, rec IdempotencyRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[rec.Key] = cloneIdempotencyRecord(rec)
	return nil
}

type failingReleaseIdempotencyStore struct {
	*MemoryIdempotencyStore
	err error
}

func (s *failingReleaseIdempotencyStore) Release(context.Context, string, string) error {
	return s.err
}

type selectiveFailReleaseIdempotencyStore struct {
	*MemoryIdempotencyStore
	mu           sync.Mutex
	failKey      string
	err          error
	releaseCalls []string
}

func (s *selectiveFailReleaseIdempotencyStore) Release(ctx context.Context, key, toolCallID string) error {
	s.mu.Lock()
	s.releaseCalls = append(s.releaseCalls, key)
	fail := key == s.failKey
	s.mu.Unlock()
	if fail {
		return s.err
	}
	return s.MemoryIdempotencyStore.Release(ctx, key, toolCallID)
}

func (s *selectiveFailReleaseIdempotencyStore) ReleaseCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.releaseCalls...)
}

type contextSensitiveTerminalIdempotencyStore struct {
	*MemoryIdempotencyStore
	mu                    sync.Mutex
	terminalPuts          int
	cancelledTerminalPuts int
	boundedTerminalPuts   int
}

func (s *contextSensitiveTerminalIdempotencyStore) Put(ctx context.Context, record IdempotencyRecord) error {
	if isTerminalIdempotencyStatus(record.Status) {
		_, bounded := ctx.Deadline()
		s.mu.Lock()
		s.terminalPuts++
		if bounded {
			s.boundedTerminalPuts++
		}
		if ctx.Err() != nil {
			s.cancelledTerminalPuts++
		}
		s.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return s.MemoryIdempotencyStore.Put(ctx, record)
}

func (s *contextSensitiveTerminalIdempotencyStore) TerminalPutStats() (total, cancelled, bounded int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminalPuts, s.cancelledTerminalPuts, s.boundedTerminalPuts
}
