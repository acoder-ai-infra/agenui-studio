package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestRuntimeStepHooksWrapOnlyBusinessSteps(t *testing.T) {
	state := NewInMemoryStateManager()
	service := NewRuntimeService(NewMockRuntime(), state, observability.NoopLogger{}, observability.NewNoopTracer("test"))

	var mu sync.Mutex
	var calls []RuntimeHookInput
	record := func(_ context.Context, input RuntimeHookInput) (RuntimeHookOutput, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, input)
		return RuntimeHookOutput{}, nil
	}
	for _, registration := range []RuntimeHookRegistration{
		{ID: "test.before_step", Point: HookBeforeStep, Hook: RuntimeHookFunc(record)},
		{ID: "test.after_step", Point: HookAfterStep, Hook: RuntimeHookFunc(record)},
	} {
		if err := service.Hooks.Register(registration); err != nil {
			t.Fatalf("register hook: %v", err)
		}
	}

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	_ = collect(events)

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 4 {
		t.Fatalf("step hook calls = %d, want 4: %#v", len(calls), calls)
	}
	want := []struct {
		point  RuntimeHookPoint
		kind   StepKind
		status StepStatus
	}{
		{HookBeforeStep, StepKindModelContext, StepStatusCreated},
		{HookAfterStep, StepKindModelContext, StepStatusCompleted},
		{HookBeforeStep, StepKindRuntimeAdapter, StepStatusCreated},
		{HookAfterStep, StepKindRuntimeAdapter, StepStatusCompleted},
	}
	for i, expected := range want {
		if calls[i].Point != expected.point || calls[i].Step == nil || calls[i].Step.Kind != expected.kind || calls[i].Step.Status != expected.status {
			t.Fatalf("call[%d] = %#v, want point=%s kind=%s status=%s", i, calls[i], expected.point, expected.kind, expected.status)
		}
		if calls[i].Step.Kind == StepKindProcessorHook {
			t.Fatal("processor hook steps must not recursively invoke step hooks")
		}
	}
}

func TestRuntimeOnErrorHookReceivesCanonicalError(t *testing.T) {
	state := NewInMemoryStateManager()
	runtimeErr := NewRuntimeError(ErrorModel, "MODEL_UPSTREAM_FAILED", "model upstream failed").WithRetryable(true)
	runtime := NewMockRuntime(observability.AgentEvent{EventType: EventRunFailed, Error: runtimeErr.EventError()})
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))

	var calls []RuntimeHookInput
	if err := service.Hooks.Register(RuntimeHookRegistration{
		ID:    "test.on_error",
		Point: HookOnError,
		Hook: RuntimeHookFunc(func(_ context.Context, input RuntimeHookInput) (RuntimeHookOutput, error) {
			calls = append(calls, input)
			return RuntimeHookOutput{}, nil
		}),
	}); err != nil {
		t.Fatalf("register hook: %v", err)
	}

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("run should return stream before adapter failure: %v", err)
	}
	collected := collect(events)
	if len(calls) != 1 {
		t.Fatalf("on_error calls = %d, want 1", len(calls))
	}
	if calls[0].Error == nil || calls[0].Error.Code != runtimeErr.Code || calls[0].Error.Type != ErrorDependencyUnavailable {
		t.Fatalf("unexpected hook error: %#v", calls[0].Error)
	}
	if eventIndex(collected, EventRunFailed) < 0 || eventIndex(collected, EventRunCompleted) >= 0 {
		t.Fatalf("unexpected terminal events: %#v", collected)
	}
}

type recordingControlCreator struct {
	state    RuntimeStateManager
	requests []ControlRequestCreateRequest
	err      error
}

func (c *recordingControlCreator) CreateControlRequest(ctx context.Context, req ControlRequestCreateRequest) (observability.AgentEvent, error) {
	c.requests = append(c.requests, req)
	if c.err != nil {
		return observability.AgentEvent{}, c.err
	}
	if c.state != nil {
		err := c.state.EnterWaitingControl(ctx, WaitingControlRequest{
			SessionID:        req.SessionID,
			RunID:            req.RunID,
			CheckpointID:     req.CheckpointID,
			ControlRequestID: req.RequestID,
			ResumeToken:      req.ResumeToken,
			Type:             req.Type,
			Event:            req.Event,
		})
		return req.Event, err
	}
	return req.Event, nil
}

func TestRuntimeOnInterruptTransitionsRunAndAdapterStepToWaiting(t *testing.T) {
	state := NewInMemoryStateManager()
	binding := WaitingControlRequest{
		SessionID:        "session_1",
		RunID:            "run_1",
		CheckpointID:     "checkpoint_1",
		ControlRequestID: "control_1",
		ResumeToken:      "secret_resume_token",
	}
	runtime := &interruptMockRuntime{
		MockRuntime: *NewMockRuntime(
			observability.AgentEvent{EventType: observability.EventCheckpointCreated, Visibility: observability.VisibilityInternal, Payload: JSONPayload(map[string]string{"checkpoint_id": binding.CheckpointID})},
			observability.AgentEvent{EventType: observability.EventControlRequestCreated, Visibility: observability.VisibilityUserVisible, Payload: JSONPayload(map[string]string{"request_id": binding.ControlRequestID, "type": "ask_user", "checkpoint_id": binding.CheckpointID})},
		),
		binding: binding,
	}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	controlCreator := &recordingControlCreator{state: state}
	service.ControlRequests = controlCreator

	var interrupt RuntimeHookInterrupt
	if err := service.Hooks.Register(RuntimeHookRegistration{
		ID:    "test.on_interrupt",
		Point: HookOnInterrupt,
		Hook: RuntimeHookFunc(func(_ context.Context, input RuntimeHookInput) (RuntimeHookOutput, error) {
			if input.Interrupt != nil {
				interrupt = *input.Interrupt
			}
			return RuntimeHookOutput{}, nil
		}),
	}); err != nil {
		t.Fatalf("register hook: %v", err)
	}

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	collected := collect(events)
	run, ok := state.Run(binding.RunID)
	if !ok || run.Status != RunStatusWaitingControl || run.CheckpointID != binding.CheckpointID || run.PendingControlRequestID != binding.ControlRequestID {
		t.Fatalf("run not waiting for control: %#v", run)
	}
	if interrupt.CheckpointID != binding.CheckpointID || interrupt.ControlRequestID != binding.ControlRequestID || interrupt.ControlType != "ask_user" {
		t.Fatalf("unexpected interrupt hook input: %#v", interrupt)
	}
	if len(controlCreator.requests) != 1 {
		t.Fatalf("control create calls = %d, want 1", len(controlCreator.requests))
	}
	controlReq := controlCreator.requests[0]
	if controlReq.RequestID != binding.ControlRequestID || controlReq.ResumeToken != binding.ResumeToken || controlReq.Type != "ask_user" || controlReq.CheckpointID != binding.CheckpointID {
		t.Fatalf("unexpected control create request: %#v", controlReq)
	}
	if eventIndex(collected, EventFinalResponse) >= 0 || eventIndex(collected, EventRunCompleted) >= 0 {
		t.Fatalf("interrupted run must not be finalized: %#v", collected)
	}
	adapterStepID := ""
	for _, event := range collected {
		if event.EventType != EventRuntimeStepStarted {
			continue
		}
		var payload struct {
			StepID string   `json:"step_id"`
			Kind   StepKind `json:"kind"`
		}
		_ = json.Unmarshal(event.Payload, &payload)
		if payload.Kind == StepKindRuntimeAdapter {
			adapterStepID = payload.StepID
		}
	}
	step, ok := state.Step(binding.RunID, adapterStepID)
	if !ok || step.Status != StepStatusWaitingControl {
		t.Fatalf("adapter step not waiting for control: %#v", step)
	}
}

func TestRuntimeOnInterruptFailClosedDoesNotEnterWaiting(t *testing.T) {
	state := NewInMemoryStateManager()
	binding := WaitingControlRequest{
		SessionID:        "session_1",
		RunID:            "run_1",
		CheckpointID:     "checkpoint_1",
		ControlRequestID: "control_1",
		ResumeToken:      "secret_resume_token",
	}
	runtime := &interruptMockRuntime{
		MockRuntime: *NewMockRuntime(observability.AgentEvent{
			EventType:  observability.EventControlRequestCreated,
			Visibility: observability.VisibilityUserVisible,
			Payload:    JSONPayload(map[string]string{"request_id": binding.ControlRequestID, "type": "ask_user", "checkpoint_id": binding.CheckpointID}),
		}),
		binding: binding,
	}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	if err := service.Hooks.Register(RuntimeHookRegistration{
		ID:    "test.on_interrupt.block",
		Point: HookOnInterrupt,
		Hook: RuntimeHookFunc(func(context.Context, RuntimeHookInput) (RuntimeHookOutput, error) {
			return RuntimeHookOutput{}, errors.New("interrupt policy unavailable")
		}),
	}); err != nil {
		t.Fatalf("register hook: %v", err)
	}

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("run failed before stream: %v", err)
	}
	collected := collect(events)
	run, ok := state.Run(binding.RunID)
	if !ok || run.Status != RunStatusFailed {
		t.Fatalf("fail-closed interrupt hook must fail run: %#v", run)
	}
	if eventIndex(collected, observability.EventControlRequestCreated) >= 0 || eventIndex(collected, EventRunCompleted) >= 0 {
		t.Fatalf("blocked interrupt must not be exposed or completed: %#v", collected)
	}
}

// TestRuntimeChildInterruptCreatesControlRequest 验证 platform child run 的
// interrupt 与父 Run 走同一条 canonical control 链路：control 事实建在
// child run 上（waiting_control + control_request_created 事件），由 Agent
// Gateway 内部消费后上交父 Run（方案 §6.5）。
func TestRuntimeChildInterruptCreatesControlRequest(t *testing.T) {
	state := NewInMemoryStateManager()
	binding := WaitingControlRequest{
		SessionID:        "session_1",
		RunID:            "run_1",
		CheckpointID:     "checkpoint_child_1",
		ControlRequestID: "control_child_1",
		ResumeToken:      "child_resume_token",
	}
	runtime := &interruptMockRuntime{
		MockRuntime: *NewMockRuntime(
			observability.AgentEvent{EventType: observability.EventCheckpointCreated, Visibility: observability.VisibilityInternal, Payload: JSONPayload(map[string]string{"checkpoint_id": binding.CheckpointID})},
			observability.AgentEvent{EventType: observability.EventControlRequestCreated, Visibility: observability.VisibilityUserVisible, Payload: JSONPayload(map[string]string{"request_id": binding.ControlRequestID, "type": "ask_user", "checkpoint_id": binding.CheckpointID})},
		),
		binding: binding,
	}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	controlCreator := &recordingControlCreator{}
	service.ControlRequests = controlCreator
	req := testRunRequest()
	req.ParentRunID = "parent_run_1"

	events, err := service.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("child run failed before stream: %v", err)
	}
	collected := collect(events)
	if len(controlCreator.requests) != 1 || controlCreator.requests[0].RequestID != binding.ControlRequestID {
		t.Fatalf("child interrupt must create a control request on the child run: %#v", controlCreator.requests)
	}
	if eventIndex(collected, observability.EventControlRequestCreated) < 0 {
		t.Fatalf("child control request event missing from child stream: %#v", collected)
	}
	if failed := eventIndex(collected, EventRunFailed); failed >= 0 {
		t.Fatalf("child interrupt must not fail the run any more: %#v", collected[failed])
	}
}

type interruptMockRuntime struct {
	MockRuntime
	binding WaitingControlRequest
}

func (r *interruptMockRuntime) TakeInterruptBinding(_ context.Context, event observability.AgentEvent) (WaitingControlRequest, error) {
	if event.RunID != r.binding.RunID {
		return WaitingControlRequest{}, ErrInterruptBindingMissing
	}
	return r.binding, nil
}
