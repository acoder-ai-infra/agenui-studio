package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/scheduler"
)

func TestRuntimeDispatchHandlerExecutesFrozenRequest(t *testing.T) {
	store := NewInMemoryRunRequestStore()
	req := testRunRequest("run_worker")
	ref, _ := store.Put(context.Background(), req)
	runtime := &runtimeRunnerStub{events: []observability.AgentEvent{{EventType: observability.EventRunCompleted}}}
	handler := NewRuntimeDispatchHandler(store, runtime)
	err := handler(context.Background(), scheduler.RunDispatch{
		RunID: req.RunID, SessionID: req.SessionID, AgentID: req.Definition.AgentID, RequestRef: ref, Trace: stableRunTrace(req),
	})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.got.RunID != req.RunID || runtime.got.AgentBindingID != req.AgentBindingID {
		t.Fatalf("runtime request = %#v", runtime.got)
	}
}

func TestRuntimeDispatchHandlerRejectsIdentityMismatch(t *testing.T) {
	store := NewInMemoryRunRequestStore()
	req := testRunRequest("run_worker_mismatch")
	ref, _ := store.Put(context.Background(), req)
	err := NewRuntimeDispatchHandler(store, &runtimeRunnerStub{})(context.Background(), scheduler.RunDispatch{
		RunID: "different", SessionID: req.SessionID, AgentID: req.Definition.AgentID, RequestRef: ref, Trace: stableRunTrace(req),
	})
	if !errors.Is(err, ErrDispatchRequestMismatch) {
		t.Fatalf("handler error = %v", err)
	}
}

func TestRuntimeDispatchHandlerMapsFailedTerminalEvent(t *testing.T) {
	store := NewInMemoryRunRequestStore()
	req := testRunRequest("run_worker_failed")
	ref, _ := store.Put(context.Background(), req)
	runtime := &runtimeRunnerStub{events: []observability.AgentEvent{{
		EventType: observability.EventRunFailed,
		Error:     &observability.EventError{Code: "MODEL_UPSTREAM", Type: observability.EventErrorUpstream, Message: "secret detail", Retryable: true},
	}}}
	err := NewRuntimeDispatchHandler(store, runtime)(context.Background(), scheduler.RunDispatch{
		RunID: req.RunID, SessionID: req.SessionID, AgentID: req.Definition.AgentID, RequestRef: ref, Trace: stableRunTrace(req),
	})
	var handlerErr scheduler.HandlerError
	if !errors.As(err, &handlerErr) || handlerErr.Type != scheduler.ErrorTypeTransient || handlerErr.Retryable || handlerErr.Error() != "runtime run failed: MODEL_UPSTREAM" {
		t.Fatalf("handler error = %#v", err)
	}
}

func TestRuntimeDispatchHandlerDrainsEventsAfterTerminal(t *testing.T) {
	store := NewInMemoryRunRequestStore()
	req := testRunRequest("run_worker_drain")
	ref, _ := store.Put(context.Background(), req)
	drained := make(chan struct{})
	runtime := runtimeRunnerFunc(func(_ context.Context, got agentruntime.RunRequest) (<-chan observability.AgentEvent, error) {
		out := make(chan observability.AgentEvent)
		go func() {
			defer close(drained)
			defer close(out)
			out <- canonicalRuntimeEvent(got, observability.EventRunCompleted)
			out <- canonicalRuntimeEvent(got, observability.EventAgentTextDelta)
		}()
		return out, nil
	})
	err := NewRuntimeDispatchHandler(store, runtime)(context.Background(), scheduler.RunDispatch{
		RunID: req.RunID, SessionID: req.SessionID, AgentID: req.Definition.AgentID, RequestRef: ref, Trace: stableRunTrace(req),
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("runtime event producer remained blocked after terminal event")
	}
}

func TestRuntimeDispatchHandlerRestoresDeliveryTraceOnExecutionCopy(t *testing.T) {
	store := NewInMemoryRunRequestStore()
	req := testRunRequest("run_delivery_trace")
	req.UserID = "user_1"
	req.TenantID = "tenant_1"
	ref, err := store.Put(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	delivery := stableRunTrace(req)
	delivery.RequestID = "request_attempt_2"
	delivery.SpanID = "span_attempt_2"
	delivery.Baggage = map[string]string{"attempt": "2"}
	ctx := observability.WithTraceContext(context.Background(), delivery)
	runtime := &runtimeRunnerStub{events: []observability.AgentEvent{{EventType: observability.EventRunCompleted}}}
	if err := NewRuntimeDispatchHandler(store, runtime)(ctx, scheduler.RunDispatch{
		RunID: req.RunID, SessionID: req.SessionID, AgentID: req.Definition.AgentID, RequestRef: ref, Trace: stableRunTrace(req),
	}); err != nil {
		t.Fatal(err)
	}
	if runtime.got.Trace.RequestID != "request_attempt_2" || runtime.got.Trace.SpanID != "span_attempt_2" || runtime.got.Trace.Baggage["attempt"] != "2" {
		t.Fatalf("delivery trace = %#v", runtime.got.Trace)
	}
	delivery.Baggage["attempt"] = "mutated"
	if runtime.got.Trace.Baggage["attempt"] != "2" {
		t.Fatal("runtime retained caller-owned trace baggage")
	}
	frozen, err := store.Get(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.Trace.RequestID != "" || frozen.Trace.SpanID != "" || frozen.Trace.Baggage != nil {
		t.Fatalf("delivery trace polluted frozen request: %#v", frozen.Trace)
	}
}

func TestRuntimeDispatchHandlerDoesNotExposeFrozenRequestToRuntimeMutation(t *testing.T) {
	store := NewInMemoryRunRequestStore()
	req := testRunRequest("run_runtime_mutation")
	req.Input = []agentruntime.Message{{Role: "user", Content: "original"}}
	req.Metadata = map[string]string{"binding_hash": "original"}
	ref, err := store.Put(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	runtime := runtimeRunnerFunc(func(_ context.Context, got agentruntime.RunRequest) (<-chan observability.AgentEvent, error) {
		got.Input[0].Content = "mutated"
		got.Metadata["binding_hash"] = "mutated"
		out := make(chan observability.AgentEvent, 1)
		out <- canonicalRuntimeEvent(got, observability.EventRunCompleted)
		close(out)
		return out, nil
	})
	if err := NewRuntimeDispatchHandler(store, runtime)(context.Background(), scheduler.RunDispatch{
		RunID: req.RunID, SessionID: req.SessionID, AgentID: req.Definition.AgentID, RequestRef: ref, Trace: stableRunTrace(req),
	}); err != nil {
		t.Fatal(err)
	}
	frozen, err := store.Get(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.Input[0].Content != "original" || frozen.Metadata["binding_hash"] != "original" {
		t.Fatalf("frozen request mutated: %#v", frozen)
	}
}

func TestRuntimeDispatchHandlerFailsClosedForInvalidEventProtocol(t *testing.T) {
	tests := []struct {
		name      string
		events    []observability.AgentEvent
		nilEvents bool
	}{
		{name: "nil stream", nilEvents: true},
		{name: "no terminal", events: []observability.AgentEvent{{EventType: observability.EventAgentTextDelta}}},
		{name: "control and terminal", events: []observability.AgentEvent{{EventType: observability.EventControlRequestCreated}, {EventType: observability.EventRunCompleted}}},
		{name: "duplicate terminal", events: []observability.AgentEvent{{EventType: observability.EventRunCompleted}, {EventType: observability.EventRunCompleted}}},
		{name: "foreign terminal", events: []observability.AgentEvent{{EventType: observability.EventRunCompleted, RunID: "other_run"}}},
		{name: "undeclared runtime", events: []observability.AgentEvent{{EventType: observability.EventRunCompleted, Runtime: string(agentruntime.RuntimeTypeNative)}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := NewInMemoryRunRequestStore()
			req := testRunRequest("run_protocol_" + test.name)
			ref, err := store.Put(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			runtime := &runtimeRunnerStub{events: test.events, nilEvents: test.nilEvents}
			err = NewRuntimeDispatchHandler(store, runtime)(context.Background(), scheduler.RunDispatch{
				RunID: req.RunID, SessionID: req.SessionID, AgentID: req.Definition.AgentID, RequestRef: ref, Trace: stableRunTrace(req),
			})
			if err == nil {
				t.Fatal("handler succeeded")
			}
		})
	}
}

func TestRuntimeDispatchHandlerAcceptsControlWithoutTerminal(t *testing.T) {
	store := NewInMemoryRunRequestStore()
	req := testRunRequest("run_control")
	ref, _ := store.Put(context.Background(), req)
	runtime := &runtimeRunnerStub{events: []observability.AgentEvent{{EventType: observability.EventControlRequestCreated}}}
	if err := NewRuntimeDispatchHandler(store, runtime)(context.Background(), scheduler.RunDispatch{
		RunID: req.RunID, SessionID: req.SessionID, AgentID: req.Definition.AgentID, RequestRef: ref, Trace: stableRunTrace(req),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeDispatchHandlerAcceptsDeclaredFallbackRuntime(t *testing.T) {
	store := NewInMemoryRunRequestStore()
	req := testRunRequest("run_runtime_fallback")
	req.Definition.Runtime = agentruntime.RuntimeSpec{
		Type:       agentruntime.RuntimeTypeAuto,
		Preferred:  agentruntime.RuntimeTypeEino,
		Candidates: []agentruntime.RuntimeType{agentruntime.RuntimeTypeMock},
	}
	ref, _ := store.Put(context.Background(), req)
	runtime := &runtimeRunnerStub{events: []observability.AgentEvent{{
		EventType: observability.EventRunCompleted,
		Runtime:   string(agentruntime.RuntimeTypeMock),
	}}}
	if err := NewRuntimeDispatchHandler(store, runtime)(context.Background(), scheduler.RunDispatch{
		RunID: req.RunID, SessionID: req.SessionID, AgentID: req.Definition.AgentID, RequestRef: ref, Trace: stableRunTrace(req),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestScheduledDispatchReconstructsCanonicalRuntimeRequest(t *testing.T) {
	s := scheduler.NewInMemoryScheduler(scheduler.Config{LeaseTTL: time.Second})
	store := NewInMemoryRunRequestStore()
	d := NewRunDispatcher(nil, s, Policy{}, nil, nil, store)
	req := testRunRequest("run_scheduled_runtime")
	req.AgentBindingID = "binding_1"
	req.ContextSnapshotRef = "context-snapshot://ctx_1"
	if _, err := d.Dispatch(context.Background(), DispatchRunRequest{
		Run: req, Policy: DispatchPolicy{ExecutionMode: ExecutionModeSingleAgent},
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &runtimeRunnerStub{events: []observability.AgentEvent{{EventType: observability.EventRunCompleted}}}
	worker := scheduler.NewWorker(s, "worker_1", nil, nil)
	worker.HeartbeatInterval = 5 * time.Millisecond
	if err := worker.ExecuteOnce(context.Background(), NewRuntimeDispatchHandler(store, runtime)); err != nil {
		t.Fatal(err)
	}
	if runtime.got.RunID != req.RunID || runtime.got.AgentBindingID != req.AgentBindingID || runtime.got.ContextSnapshotRef != req.ContextSnapshotRef {
		t.Fatalf("reconstructed request = %#v", runtime.got)
	}
	dispatch, ok := s.DispatchByRun(req.RunID)
	if !ok || dispatch.Status != scheduler.DispatchCompleted {
		t.Fatalf("dispatch = %#v, exists=%v", dispatch, ok)
	}
}

type runtimeRunnerStub struct {
	got       agentruntime.RunRequest
	events    []observability.AgentEvent
	err       error
	nilEvents bool
}

type runtimeRunnerFunc func(context.Context, agentruntime.RunRequest) (<-chan observability.AgentEvent, error)

func (f runtimeRunnerFunc) Run(ctx context.Context, req agentruntime.RunRequest) (<-chan observability.AgentEvent, error) {
	return f(ctx, req)
}

func (r *runtimeRunnerStub) Run(_ context.Context, req agentruntime.RunRequest) (<-chan observability.AgentEvent, error) {
	r.got = req
	if r.err != nil {
		return nil, r.err
	}
	if r.nilEvents {
		return nil, nil
	}
	out := make(chan observability.AgentEvent, len(r.events))
	for i, event := range r.events {
		if event.EventID == "" {
			event.EventID = fmt.Sprintf("event_%d", i)
		}
		if event.SchemaVersion == "" {
			event.SchemaVersion = observability.AgentEventSchemaVersion
		}
		if event.TraceID == "" {
			event.TraceID = req.Trace.TraceID
		}
		if event.SessionID == "" {
			event.SessionID = req.SessionID
		}
		if event.RunID == "" {
			event.RunID = req.RunID
		}
		if event.AgentID == "" {
			event.AgentID = req.Definition.AgentID
		}
		if event.AgentType == "" {
			event.AgentType = req.Definition.AgentType
		}
		if event.Runtime == "" {
			event.Runtime = string(req.Definition.Runtime.Type)
		}
		out <- event
	}
	close(out)
	return out, nil
}

func canonicalRuntimeEvent(req agentruntime.RunRequest, eventType observability.EventType) observability.AgentEvent {
	return observability.AgentEvent{
		EventID:       "event_" + string(eventType),
		SchemaVersion: observability.AgentEventSchemaVersion,
		TraceID:       req.Trace.TraceID,
		SessionID:     req.SessionID,
		RunID:         req.RunID,
		AgentID:       req.Definition.AgentID,
		AgentType:     req.Definition.AgentType,
		Runtime:       string(req.Definition.Runtime.Type),
		EventType:     eventType,
	}
}
