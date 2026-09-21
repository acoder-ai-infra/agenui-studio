package dispatcher

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/executionmode"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/scheduler"
)

func TestDispatcherInlineDirectAction(t *testing.T) {
	runtime := agentruntime.NewMockRuntime(observability.AgentEvent{
		EventType:  agentruntime.EventAgentTextDelta,
		Visibility: observability.VisibilityUserVisible,
		Payload:    agentruntime.JSONPayload(map[string]string{"text": "ok"}),
	})
	state := agentruntime.NewInMemoryStateManager()
	service := agentruntime.NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	d := NewRunDispatcher(service, scheduler.NewInMemoryScheduler(scheduler.Config{}), Policy{InlineMaxDuration: 2 * time.Second}, observability.NoopLogger{}, observability.NewNoopTracer("test"))

	result, err := d.Dispatch(context.Background(), DispatchRunRequest{
		Run: testRunRequest("run_inline"),
		Policy: DispatchPolicy{
			ExecutionMode:     ExecutionModeDirectAction,
			EstimatedDuration: time.Second,
		},
	})
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if result.Mode != ModeInline || result.Events == nil || result.Dispatch != nil {
		t.Fatalf("unexpected result: %#v", result)
	}
	if events := collect(result.Events); len(events) == 0 {
		t.Fatal("inline events missing")
	}
	run, ok := state.Run("run_inline")
	if !ok || run.Status != agentruntime.RunStatusCompleted {
		t.Fatalf("runtime should execute inline run, got %#v", run)
	}
}

func TestDispatcherSchedulesReadyExecution(t *testing.T) {
	for _, mode := range []ExecutionMode{ExecutionModeSingleAgent} {
		t.Run(string(mode), func(t *testing.T) {
			s := scheduler.NewInMemoryScheduler(scheduler.Config{})
			d := NewRunDispatcher(nil, s, Policy{InlineMaxDuration: 2 * time.Second}, observability.NoopLogger{}, observability.NewNoopTracer("test"))
			runID := "run_scheduled_" + string(mode)

			result, err := d.Dispatch(context.Background(), DispatchRunRequest{
				Run: testRunRequest(runID),
				Policy: DispatchPolicy{
					ExecutionMode:     mode,
					EstimatedDuration: time.Second,
				},
				Priority: scheduler.PriorityHigh,
			})
			if err != nil {
				t.Fatalf("dispatch failed: %v", err)
			}
			if result.Mode != ModeScheduled || result.Dispatch == nil || result.Events != nil {
				t.Fatalf("unexpected result: %#v", result)
			}
			if result.Dispatch.RunID != runID || result.Dispatch.Priority != scheduler.PriorityHigh {
				t.Fatalf("unexpected dispatch: %#v", result.Dispatch)
			}
		})
	}
}

func TestDispatcherInlineCheckpointCapableSingleAgent(t *testing.T) {
	runtime := agentruntime.NewMockRuntime(observability.AgentEvent{
		EventType:  agentruntime.EventAgentTextDelta,
		Visibility: observability.VisibilityUserVisible,
		Payload:    agentruntime.JSONPayload(map[string]string{"text": "ok"}),
	})
	state := agentruntime.NewInMemoryStateManager()
	service := agentruntime.NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	d := NewRunDispatcher(service, nil, Policy{InlineMaxDuration: 2 * time.Second}, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	d.InlineModes[ExecutionModeSingleAgent] = true

	result, err := d.Dispatch(context.Background(), DispatchRunRequest{
		Run: testRunRequest("run_inline_single_checkpoint"),
		Policy: DispatchPolicy{
			ExecutionMode:     ExecutionModeSingleAgent,
			EstimatedDuration: time.Second,
			HasSideEffect:     true,
			ResumeRequired:    true,
		},
	})
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if result.Mode != ModeInline || result.Events == nil || result.Dispatch != nil {
		t.Fatalf("unexpected result: %#v", result)
	}
	if events := collect(result.Events); len(events) == 0 {
		t.Fatal("inline events missing")
	}
}

func TestDispatcherRejectsModeWithoutInstalledExecutor(t *testing.T) {
	for _, mode := range []ExecutionMode{ExecutionModeDeepAgent, ExecutionModeWorkflow, ExecutionModeGraph} {
		t.Run(string(mode), func(t *testing.T) {
			s := scheduler.NewInMemoryScheduler(scheduler.Config{})
			d := NewRunDispatcher(nil, s, Policy{}, nil, nil)
			_, err := d.Dispatch(context.Background(), DispatchRunRequest{
				Run:    testRunRequest("run_not_ready_" + string(mode)),
				Policy: DispatchPolicy{ExecutionMode: mode},
			})
			if !errors.Is(err, ErrExecutorNotReady) {
				t.Fatalf("Dispatch() error = %v, want executor not ready", err)
			}
			if _, exists := s.DispatchByRun("run_not_ready_" + string(mode)); exists {
				t.Fatal("unready execution mode reached Scheduler")
			}
		})
	}
}

func TestDispatcherPersistsFrozenRequestBeforeScheduling(t *testing.T) {
	s := scheduler.NewInMemoryScheduler(scheduler.Config{})
	store := NewInMemoryRunRequestStore()
	d := NewRunDispatcher(nil, s, Policy{}, nil, nil, store)
	req := testRunRequest("run_frozen_before_queue")
	req.Metadata = map[string]string{"source": "original"}
	result, err := d.Dispatch(context.Background(), DispatchRunRequest{
		Run: req, Policy: DispatchPolicy{ExecutionMode: ExecutionModeSingleAgent},
	})
	if err != nil {
		t.Fatal(err)
	}
	req.Metadata["source"] = "mutated"
	got, err := store.Get(context.Background(), result.Dispatch.RequestRef)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["source"] != "original" {
		t.Fatalf("queued request was not frozen: %#v", got.Metadata)
	}
}

func TestDispatcherRejectsUnsupportedExecutionModeBeforeDispatch(t *testing.T) {
	for _, mode := range []ExecutionMode{"", "workflow_graph", "state_graph", "plan_execute", "remote_a2a", "unknown"} {
		mode := mode
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			s := scheduler.NewInMemoryScheduler(scheduler.Config{})
			d := NewRunDispatcher(nil, s, Policy{}, nil, nil)
			_, err := d.Dispatch(context.Background(), DispatchRunRequest{
				Run:    testRunRequest("run_invalid_" + string(mode)),
				Policy: DispatchPolicy{ExecutionMode: mode},
			})
			if !errors.Is(err, executionmode.ErrUnsupported) {
				t.Fatalf("Dispatch() error = %v, want unsupported execution mode", err)
			}
			if _, exists := s.DispatchByRun("run_invalid_" + string(mode)); exists {
				t.Fatal("unsupported mode reached Scheduler")
			}
		})
	}
}

func TestDispatcherRejectsDirectActionThatNeedsCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy DispatchPolicy
	}{
		{name: "side effect", policy: DispatchPolicy{HasSideEffect: true}},
		{name: "resume", policy: DispatchPolicy{ResumeRequired: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := scheduler.NewInMemoryScheduler(scheduler.Config{})
			d := NewRunDispatcher(nil, s, Policy{InlineMaxDuration: 2 * time.Second}, observability.NoopLogger{}, observability.NewNoopTracer("test"))
			runID := "run_direct_" + strings.ReplaceAll(tc.name, " ", "_")
			policy := tc.policy
			policy.ExecutionMode = ExecutionModeDirectAction
			policy.EstimatedDuration = time.Second

			_, err := d.Dispatch(context.Background(), DispatchRunRequest{Run: testRunRequest(runID), Policy: policy})
			if !errors.Is(err, ErrExecutorNotReady) {
				t.Fatalf("Dispatch() error = %v, want executor not ready", err)
			}
			if _, exists := s.DispatchByRun(runID); exists {
				t.Fatal("unsafe direct action reached Scheduler")
			}
		})
	}
}

func TestDispatcherSchedulesSlowDirectAction(t *testing.T) {
	s := scheduler.NewInMemoryScheduler(scheduler.Config{})
	d := NewRunDispatcher(nil, s, Policy{InlineMaxDuration: 2 * time.Second}, observability.NoopLogger{}, observability.NewNoopTracer("test"))

	result, err := d.Dispatch(context.Background(), DispatchRunRequest{
		Run: testRunRequest("run_slow"),
		Policy: DispatchPolicy{
			ExecutionMode:     ExecutionModeDirectAction,
			EstimatedDuration: 3 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if result.Mode != ModeScheduled {
		t.Fatalf("slow direct action should be scheduled: %#v", result)
	}
}

func TestDispatcherCancelsScheduledRun(t *testing.T) {
	s := scheduler.NewInMemoryScheduler(scheduler.Config{})
	d := NewRunDispatcher(nil, s, Policy{}, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	if _, err := d.Dispatch(context.Background(), DispatchRunRequest{
		Run:    testRunRequest("run_cancel"),
		Policy: DispatchPolicy{ExecutionMode: ExecutionModeSingleAgent},
	}); err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if err := d.Cancel(context.Background(), scheduler.CancelDispatchRequest{RunID: "run_cancel", Reason: "user_cancelled"}); err != nil {
		t.Fatalf("cancel failed: %v", err)
	}
	dispatch, ok := s.DispatchByRun("run_cancel")
	if !ok || dispatch.Status != scheduler.DispatchCancelled {
		t.Fatalf("scheduled run was not cancelled: %#v", dispatch)
	}
}

func TestDispatcherCancelWithoutSchedulerReturnsNotFound(t *testing.T) {
	d := NewRunDispatcher(nil, nil, Policy{}, nil, nil)
	err := d.Cancel(context.Background(), scheduler.CancelDispatchRequest{RunID: "run_inline"})
	if !errors.Is(err, scheduler.ErrDispatchNotFound) {
		t.Fatalf("Cancel() error = %v, want dispatch not found", err)
	}
}

func TestDispatcherProductionValidationRejectsInMemoryRequestStore(t *testing.T) {
	runtime := agentruntime.NewRuntimeService(agentruntime.NewMockRuntime(), agentruntime.NewInMemoryStateManager(), nil, nil)
	d := NewRunDispatcher(runtime, scheduler.NewInMemoryScheduler(scheduler.Config{}), Policy{}, nil, nil)
	if err := d.ValidateProduction(); !errors.Is(err, ErrProductionMemoryStore) {
		t.Fatalf("ValidateProduction() error = %v", err)
	}
}

func testRunRequest(runID string) agentruntime.RunRequest {
	return agentruntime.RunRequest{
		SessionID: "session_1",
		RunID:     runID,
		Definition: agentruntime.AgentDefinition{
			AgentID:   "agent_1",
			AgentType: "assistant",
			Version:   "v1",
			Runtime:   agentruntime.RuntimeSpec{Type: agentruntime.RuntimeTypeMock},
		},
		Trace: observability.TraceContext{TraceID: "trace_1"},
	}
}

func collect(ch <-chan observability.AgentEvent) []observability.AgentEvent {
	var events []observability.AgentEvent
	for event := range ch {
		events = append(events, event)
	}
	return events
}
