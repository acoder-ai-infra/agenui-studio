package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storagewrite"
)

func TestRuntimeServiceRunCompletes(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime(
		observability.AgentEvent{EventType: EventAgentStarted, Visibility: observability.VisibilityDebug},
		observability.AgentEvent{EventType: EventAgentTextDelta, Visibility: observability.VisibilityUserVisible, Payload: JSONPayload(map[string]string{"text": "hello"})},
	)
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	collected := collect(events)
	if len(collected) != 10 {
		t.Fatalf("unexpected event count: %d %#v", len(collected), collected)
	}
	if collected[0].EventType != EventRuntimeStepStarted || collected[0].StepID == "" {
		t.Fatalf("first event should start model context step: %#v", collected[0])
	}
	if eventIndex(collected, EventModelContextBuilt) < eventIndex(collected, EventRuntimeStepStarted) {
		t.Fatalf("model_context_built should follow step start: %#v", collected)
	}
	if eventIndex(collected, EventRunStarted) < eventIndex(collected, EventModelContextBuilt) {
		t.Fatalf("run_started should follow model context build: %#v", collected)
	}
	if collected[len(collected)-1].EventType != EventRunCompleted {
		t.Fatalf("last event should be run_completed: %#v", collected[len(collected)-1])
	}
	if collected[len(collected)-2].EventType != EventFinalResponse {
		t.Fatalf("final_response should precede run_completed: %#v", collected[len(collected)-2:])
	}

	run, ok := state.Run("run_1")
	if !ok {
		t.Fatal("run state missing")
	}
	if run.Status != RunStatusCompleted {
		t.Fatalf("run should be completed: %#v", run)
	}
	wantPersisted := 0
	for _, event := range collected {
		if !observability.IsEphemeralDelta(event.EventType) {
			wantPersisted++
		}
	}
	if got := state.Events("run_1"); len(got) != wantPersisted {
		t.Fatalf("persisted events = %d, want %d", len(got), wantPersisted)
	}
}

// TestRuntimeServiceChildResumeReachesStateClaim 验证 child resume 不再在
// 校验层 fail-closed：请求进入常规 claim 链（此处因无 runtime state 而以
// claim 失败结束，而非 ErrChildResumeUnsupported）。
func TestRuntimeServiceChildResumeReachesStateClaim(t *testing.T) {
	state := NewInMemoryStateManager()
	service := NewRuntimeService(NewMockRuntime(), state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	req := ResumeRequest{
		SessionID: "session_1", RunID: "child_run_1", ParentRunID: "parent_run_1",
		CheckpointID: "checkpoint_1", ControlRequestID: "control_1", ResumeToken: "token_1",
	}

	_, err := service.Resume(context.Background(), req)
	if err == nil {
		t.Fatal("resume without runtime state must still fail at claim")
	}
	if errors.Is(err, ErrChildResumeUnsupported) {
		t.Fatalf("child resume must not be rejected by ParentRunID validation any more, got %v", err)
	}
}

func TestRuntimeServicePersistsAdapterNestedStepLifecycle(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime(
		observability.AgentEvent{
			StepID:     "internal_agent_step_1",
			EventType:  EventRuntimeStepStarted,
			Visibility: observability.VisibilityDebug,
			Payload: JSONPayload(map[string]string{
				"kind":           string(StepKindRuntimeInternalAgent),
				"name":           "planner",
				"parent_step_id": "runtime_adapter_parent",
			}),
		},
		observability.AgentEvent{
			StepID:     "internal_agent_step_1",
			EventType:  EventRuntimeStepCompleted,
			Visibility: observability.VisibilityDebug,
		},
		observability.AgentEvent{EventType: EventAgentTextDelta, Visibility: observability.VisibilityUserVisible, Payload: JSONPayload(map[string]string{"text": "ok"})},
	)
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	collected := collect(events)
	step, ok := state.Step("run_1", "internal_agent_step_1")
	if !ok || step.Status != StepStatusCompleted || step.Kind != StepKindRuntimeInternalAgent || step.Name != "planner" || step.ParentStepID != "runtime_adapter_parent" {
		t.Fatalf("adapter nested step was not persisted: %#v", step)
	}
	var started, completed observability.AgentEvent
	for _, event := range collected {
		if event.StepID != step.StepID {
			continue
		}
		switch event.EventType {
		case EventRuntimeStepStarted:
			started = event
		case EventRuntimeStepCompleted:
			completed = event
		}
	}
	if started.SpanID == "" || completed.SpanID != started.SpanID || started.ParentSpanID == "" {
		t.Fatalf("adapter nested step span is incomplete: started=%#v completed=%#v", started, completed)
	}
}

func TestRuntimeServiceCancelPropagatesToActiveRuntimeAndEmitsTerminalEvent(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := &cancelCapturingRuntime{MockRuntime: *NewMockRuntime(observability.AgentEvent{
		EventType:  EventAgentTextDelta,
		Visibility: observability.VisibilityUserVisible,
	})}
	runtime.DelayPerEvent = time.Second
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if err := service.Cancel(context.Background(), CancelRequest{SessionID: "session_1", RunID: "run_1", Reason: "user cancelled"}); err != nil {
		t.Fatalf("cancel failed: %v", err)
	}
	collected := collect(events)
	if eventIndex(collected, EventRunCancelled) < 0 {
		t.Fatalf("run_cancelled event missing: %#v", collected)
	}
	cancelledIndex := eventIndex(collected, EventRuntimeStepCancelled)
	if cancelledIndex < 0 {
		t.Fatalf("runtime_step_cancelled event missing: %#v", collected)
	}
	cancelledStep, ok := state.Step("run_1", collected[cancelledIndex].StepID)
	if !ok || cancelledStep.Status != StepStatusCancelled {
		t.Fatalf("runtime adapter step should be cancelled: %#v", cancelledStep)
	}
	if got := runtime.CancelCalls(); got != 1 {
		t.Fatalf("runtime cancel calls = %d, want 1", got)
	}
	run, ok := state.Run("run_1")
	if !ok || run.Status != RunStatusCancelled {
		t.Fatalf("run should be cancelled: %#v", run)
	}
}

func TestRuntimeServiceTimeoutExpiresRun(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime(observability.AgentEvent{
		EventType:  EventAgentTextDelta,
		Visibility: observability.VisibilityUserVisible,
	})
	runtime.DelayPerEvent = time.Second
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.Writer = contextRejectingWriter{next: service.Writer}
	req := testRunRequest()
	req.Definition.Timeout = 10 * time.Millisecond

	events, err := service.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	collected := collect(events)
	if eventIndex(collected, EventRunExpired) < 0 {
		t.Fatalf("run_expired event missing: %#v", collected)
	}
	if eventIndex(collected, EventRuntimeStepFailed) < 0 || eventIndex(collected, EventRuntimeStepCancelled) >= 0 {
		t.Fatalf("timeout should fail the step rather than mark user cancellation: %#v", collected)
	}
	run, ok := state.Run("run_1")
	if !ok || run.Status != RunStatusExpired {
		t.Fatalf("run should be expired: %#v", run)
	}
}

func TestRuntimeServiceCancellationReleasesRunWhenConsumerStopsReading(t *testing.T) {
	state := NewInMemoryStateManager()
	script := make([]observability.AgentEvent, 128)
	for i := range script {
		script[i] = observability.AgentEvent{
			EventType:  EventAgentTextDelta,
			Visibility: observability.VisibilityUserVisible,
			Payload:    JSONPayload(map[string]int{"index": i}),
		}
	}
	service := NewRuntimeService(NewMockRuntime(script...), state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	ctx, cancel := context.WithCancel(context.Background())
	events, err := service.Run(ctx, testRunRequest())
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	// Leave the output unread until its buffer is full, then simulate an SSE
	// disconnect. Runtime cleanup must not depend on the consumer draining it.
	time.Sleep(10 * time.Millisecond)
	cancel()
	deadline := time.Now().Add(time.Second)
	for service.active.get("run_1") != nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if service.active.get("run_1") != nil {
		t.Fatal("active run was retained after consumer cancellation")
	}
	select {
	case _, ok := <-events:
		if ok {
			for range events {
			}
		}
	case <-time.After(time.Second):
		t.Fatal("runtime output did not close after consumer cancellation")
	}
}

func TestRuntimeServiceCancelIsIdempotentUnderConcurrency(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := &cancelCapturingRuntime{MockRuntime: *NewMockRuntime()}
	runtime.DelayPerEvent = time.Second
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- service.Cancel(context.Background(), CancelRequest{SessionID: "session_1", RunID: "run_1", Reason: "user cancelled"})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("cancel failed: %v", err)
		}
	}
	_ = collect(events)
	if got := runtime.CancelCalls(); got != 1 {
		t.Fatalf("runtime cancel calls = %d, want 1", got)
	}
}

func TestRuntimeServiceWritesRunAndEventsThroughStoragePlan(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime(observability.AgentEvent{EventType: EventAgentTextDelta, Visibility: observability.VisibilityUserVisible})
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	spy := &storagePlanSpy{next: service.Writer}
	service.Writer = spy

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	_ = collect(events)

	reasons := spy.Reasons()
	want := []string{
		"runtime.start_run",
		"runtime.bind_runtime",
		"runtime.start_step.model_context.",
		"runtime.append_event.model_context_built",
		"runtime.complete_step.model_context.",
		"runtime.bind_capabilities",
		"runtime.start_step.runtime_adapter.",
		"runtime.append_event.run_started",
		"runtime.complete_step.runtime_adapter.",
		"runtime.finalize",
	}
	assertReasonPrefixes(t, reasons, want)
}

func TestRuntimeServiceRunErrorFailsState(t *testing.T) {
	state := NewInMemoryStateManager()
	sourceErr := errors.New("runtime unavailable")
	runtime := &MockRuntime{RunError: sourceErr}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))

	_, runErr := service.Run(context.Background(), testRunRequest())
	if runErr == nil {
		t.Fatal("expected run error")
	}
	var runtimeErr *RuntimeError
	if !errors.As(runErr, &runtimeErr) || !errors.Is(runErr, sourceErr) {
		t.Fatalf("run error should be normalized and preserve cause: %v", runErr)
	}
	run, ok := state.Run("run_1")
	if !ok {
		t.Fatal("run state missing")
	}
	if run.Status != RunStatusFailed {
		t.Fatalf("run should be failed: %#v", run)
	}
	events := state.Events("run_1")
	if eventIndex(events, EventModelContextBuilt) < 0 || eventIndex(events, EventRuntimeStepFailed) < 0 || eventIndex(events, EventRunFailed) < 0 {
		t.Fatalf("run_failed event missing: %#v", events)
	}
	failed := events[eventIndex(events, EventRunFailed)]
	if failed.Error == nil || failed.Error.Type != observability.EventErrorInternal || failed.Error.Code != "RUNTIME_ADAPTER_FAILED" {
		t.Fatalf("run_failed error not normalized: %#v", failed)
	}
	if strings.Contains(failed.Error.Message, "runtime unavailable") {
		t.Fatalf("unknown adapter detail leaked into event: %#v", failed.Error)
	}
	if strings.Contains(string(failed.Payload), "runtime unavailable") {
		t.Fatalf("unknown adapter detail leaked into payload: %s", failed.Payload)
	}
	if run.ErrorCode != "RUNTIME_ADAPTER_FAILED" {
		t.Fatalf("run error code not persisted: %#v", run)
	}
}

func TestRuntimeServiceNormalizesUnclassifiedAdapterFailureEvent(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime(observability.AgentEvent{
		EventType:  EventRunFailed,
		Visibility: observability.VisibilityDebug,
		Payload:    JSONPayload(map[string]string{"stack": "provider stack with secret"}),
		Error:      &observability.EventError{Type: "vendor_error", Code: "VENDOR_RAW", Message: "provider stack with secret"},
	})
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("run failed before stream: %v", err)
	}
	collected := collect(events)
	idx := eventIndex(collected, EventRunFailed)
	if idx < 0 {
		t.Fatalf("run_failed missing: %#v", collected)
	}
	failed := collected[idx]
	if failed.Error == nil || failed.Error.Type != observability.EventErrorInternal || failed.Error.Code != "RUNTIME_ADAPTER_FAILED" {
		t.Fatalf("unclassified adapter failure not normalized: %#v", failed)
	}
	if strings.Contains(string(failed.Payload), "provider stack") || strings.Contains(failed.Error.Message, "provider stack") {
		t.Fatalf("adapter detail leaked into normalized event: %#v", failed)
	}
}

func TestRuntimeServicePreservesClassifiedAdapterFailure(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime(observability.AgentEvent{
		EventType:  EventRunFailed,
		Visibility: observability.VisibilityDebug,
		Error:      &observability.EventError{Type: observability.EventErrorRateLimited, Code: "MODEL_RATE_LIMITED", Message: "model rate limited", Retryable: true},
	})
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("run failed before stream: %v", err)
	}
	collected := collect(events)
	idx := eventIndex(collected, EventRunFailed)
	if idx < 0 {
		t.Fatalf("run_failed missing: %#v", collected)
	}
	failed := collected[idx]
	if failed.Error == nil || failed.Error.Type != observability.EventErrorRateLimited || failed.Error.Code != "MODEL_RATE_LIMITED" || !failed.Error.Retryable {
		t.Fatalf("classified adapter failure changed: %#v", failed)
	}
	run, ok := state.Run("run_1")
	if !ok || run.Status != RunStatusFailed || run.ErrorCode != "MODEL_RATE_LIMITED" {
		t.Fatalf("classified failure not persisted: %#v", run)
	}
}

// terminalWriteBarrier 模拟真实数据库驱动（SQLite 等）对 context 取消的敏感
// 性：context 已取消时直接拒写。内存 StorePort 不感知取消，必须靠它才能在
// 单测里暴露终态落库竞态。barrier 同时把 fail_run 写入阻塞到测试发出取消信号
// 之后，使“父 Run 先取消 → child 再落库”的时序变得确定，避免用雪崩碰运气。
type terminalWriteBarrier struct {
	inner    StorageWriteExecutor
	released chan struct{}
}

func (w *terminalWriteBarrier) Execute(ctx context.Context, plan storagewrite.Plan) (storagewrite.Result, error) {
	if plan.Reason == "runtime.fail_run" {
		select {
		case <-w.released:
		case <-time.After(2 * time.Second):
			return storagewrite.Result{}, errors.New("terminal write barrier timeout")
		}
	}
	if err := ctx.Err(); err != nil {
		return storagewrite.Result{}, err
	}
	return w.inner.Execute(ctx, plan)
}

// TestRuntimeServiceFailRunSurvivesConsumerCancellation 锁定父子 Run 终态竞态：
// 父 Run（Agent Gateway 的 inline collector）收到 run_failed 后会立即结束并取消
// 继承下来的 context，此时 child 的 RunStore 状态仍必须落到 failed，而不是永久
// 停留在 running。
func TestRuntimeServiceFailRunSurvivesConsumerCancellation(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime(observability.AgentEvent{
		EventType:  EventRunFailed,
		Visibility: observability.VisibilityDebug,
		Error:      &observability.EventError{Type: observability.EventErrorRateLimited, Code: "MODEL_RATE_LIMITED", Message: "model rate limited"},
	})
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	barrier := &terminalWriteBarrier{inner: service.Writer, released: make(chan struct{})}
	service.Writer = barrier

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := service.Run(ctx, testRunRequest())
	if err != nil {
		t.Fatalf("run failed before stream: %v", err)
	}

	// 复刻 collectChildResult：收到 run_failed 立即向父 Run 返回并停止消费，
	// 父 Run 随即取消 context。
	var sawFailed bool
	for event := range events {
		if event.EventType == EventRunFailed {
			sawFailed = true
			break
		}
	}
	if !sawFailed {
		t.Fatal("run_failed event missing from child stream")
	}
	cancel()
	close(barrier.released)

	deadline := time.Now().Add(3 * time.Second)
	for {
		run, ok := state.Run("run_1")
		if ok && run.Status != RunStatusRunning {
			if run.Status != RunStatusFailed {
				t.Fatalf("child run should be failed after parent cancellation: %#v", run)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child run stuck in non-terminal status after parent cancellation: %#v", run)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRuntimeServiceCompletesAdapterStepBeforeAdapterRunCompleted(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime(observability.AgentEvent{EventType: EventRunCompleted, Visibility: observability.VisibilityDebug})
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	collected := collect(events)
	if len(collected) < 2 {
		t.Fatalf("unexpected events: %#v", collected)
	}
	stepCompleted := collected[len(collected)-3]
	finalResponse := collected[len(collected)-2]
	runCompleted := collected[len(collected)-1]
	if stepCompleted.EventType != EventRuntimeStepCompleted || !strings.HasPrefix(stepCompleted.StepID, string(StepKindRuntimeAdapter)+"_") {
		t.Fatalf("adapter step must complete before run_completed: %#v", collected)
	}
	if runCompleted.EventType != EventRunCompleted {
		t.Fatalf("last event should be run_completed: %#v", collected)
	}
	if finalResponse.EventType != EventFinalResponse {
		t.Fatalf("final_response must follow adapter completion: %#v", collected)
	}
}

func TestRuntimeServiceFallbacksToCandidateRuntime(t *testing.T) {
	state := NewInMemoryStateManager()
	primary := &MockRuntime{NameValue: string(RuntimeTypeEino), Unhealthy: true, HealthReason: "eino unavailable"}
	fallback := &MockRuntime{NameValue: string(RuntimeTypeMock), Script: []observability.AgentEvent{
		{EventType: EventAgentTextDelta, Visibility: observability.VisibilityUserVisible, Payload: JSONPayload(map[string]string{"text": "fallback"})},
	}}
	service := NewRuntimeService(primary, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.RegisterRuntime(RuntimeTypeMock, fallback)
	req := testRunRequest()
	req.Definition.Runtime = RuntimeSpec{
		Type:       RuntimeTypeAuto,
		Preferred:  RuntimeTypeEino,
		Candidates: []RuntimeType{RuntimeTypeMock},
		Mode:       RuntimeModeReact,
	}

	events, err := service.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	collected := collect(events)
	fallbackIdx := eventIndex(collected, EventFallback)
	modelContextIdx := eventIndex(collected, EventModelContextBuilt)
	if fallbackIdx < 0 {
		t.Fatalf("fallback event missing: %#v", collected)
	}
	if modelContextIdx < 0 || fallbackIdx > modelContextIdx {
		t.Fatalf("fallback event should precede model context build: %#v", collected)
	}
	if textIdx := eventIndex(collected, EventAgentTextDelta); textIdx < 0 || collected[textIdx].Runtime != string(RuntimeTypeMock) {
		t.Fatalf("fallback runtime event not normalized: %#v", collected)
	}
	decisions := state.Fallbacks()
	if len(decisions) != 1 {
		t.Fatalf("fallback decision missing: %#v", decisions)
	}
	if decisions[0].FromRuntime != RuntimeTypeEino || decisions[0].ToRuntime != RuntimeTypeMock {
		t.Fatalf("unexpected fallback decision: %#v", decisions[0])
	}
}

func TestRuntimeServiceFailsWhenAllRuntimeCandidatesUnavailable(t *testing.T) {
	state := NewInMemoryStateManager()
	primary := &MockRuntime{NameValue: string(RuntimeTypeEino), Unhealthy: true, HealthReason: "eino unavailable"}
	service := NewRuntimeService(primary, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	req := testRunRequest()
	req.Definition.Runtime = RuntimeSpec{
		Type:       RuntimeTypeAuto,
		Preferred:  RuntimeTypeEino,
		Candidates: []RuntimeType{RuntimeTypeGoogleADK},
		Mode:       RuntimeModeReact,
	}

	if _, err := service.Run(context.Background(), req); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("expected runtime unavailable, got %v", err)
	}
	run, ok := state.Run(req.RunID)
	if !ok {
		t.Fatal("run state missing")
	}
	if run.Status != RunStatusFailed {
		t.Fatalf("run should fail when all runtimes unavailable: %#v", run)
	}
	if eventIndex(state.Events(req.RunID), EventRunFailed) < 0 {
		t.Fatalf("run_failed event missing: %#v", state.Events(req.RunID))
	}
}

func TestRuntimeServiceFailureBeforeStreamUsesStoragePlan(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := &MockRuntime{RunError: errors.New("runtime unavailable")}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	spy := &storagePlanSpy{next: service.Writer}
	service.Writer = spy

	if _, err := service.Run(context.Background(), testRunRequest()); err == nil {
		t.Fatal("expected run error")
	}
	reasons := spy.Reasons()
	want := []string{
		"runtime.start_run",
		"runtime.bind_runtime",
		"runtime.start_step.model_context.",
		"runtime.append_event.model_context_built",
		"runtime.complete_step.model_context.",
		"runtime.bind_capabilities",
		"runtime.start_step.runtime_adapter.",
		"runtime.fail_step.runtime_adapter.",
		"runtime.fail_run_before_stream",
	}
	assertReasonPrefixes(t, reasons, want)
}

func TestRuntimeServiceStoragePlanUsesPreparedTraceContext(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime(observability.AgentEvent{EventType: EventAgentTextDelta, Visibility: observability.VisibilityUserVisible})
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.IDs = fixedIDGenerator{}
	spy := &storagePlanSpy{next: service.Writer}
	service.Writer = spy

	req := testRunRequest()
	req.Trace = observability.TraceContext{}
	req.TenantID = "tenant_1"
	events, err := service.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	_ = collect(events)

	for _, plan := range spy.Plans() {
		if plan.TraceID != "trace_fixed" {
			t.Fatalf("plan trace should use prepared context trace: %#v", plan)
		}
		if plan.TenantID != "tenant_1" {
			t.Fatalf("plan tenant mismatch: %#v", plan)
		}
	}
}

func TestRuntimeServicePassesModelContextPackageToRuntime(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := &modelContextCapturingRuntime{MockRuntime: *NewMockRuntime()}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	req := testRunRequest()
	req.Input = []Message{{Role: "user", Content: "hello"}}
	req.Definition.ToolRefs = []string{"poi.search"}
	req.Definition.SubAgentRefs = []string{"hotel_agent"}

	events, err := service.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	_ = collect(events)

	if runtime.pkg.PackageID == "" {
		t.Fatal("model context package missing from runtime context")
	}
	if runtime.pkg.SchemaVersion != ModelContextPackageSchemaVersion {
		t.Fatalf("unexpected package schema: %#v", runtime.pkg)
	}
	if !hasModelMessage(runtime.pkg.Messages.ConversationWindow, "user", "hello") {
		t.Fatalf("current user input not materialized: %#v", runtime.pkg.Messages)
	}
	if len(runtime.pkg.Capabilities.Tools) != 1 || runtime.pkg.Capabilities.Tools[0] != "poi.search" {
		t.Fatalf("tool refs missing: %#v", runtime.pkg.Capabilities)
	}
	if len(runtime.pkg.Capabilities.SubAgents) != 1 || runtime.pkg.Capabilities.SubAgents[0] != "hotel_agent" {
		t.Fatalf("sub agent refs missing: %#v", runtime.pkg.Capabilities)
	}
}

func TestRuntimeServiceStripsPlatformSubAgentsFromChildRun(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := &modelContextCapturingRuntime{MockRuntime: *NewMockRuntime()}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	req := testRunRequest()
	req.ParentRunID = "parent_run"
	req.Input = []Message{{Role: "user", Content: "child task"}}
	req.Definition.SubAgentRefs = []string{"forbidden_grandchild"}

	events, err := service.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	_ = collect(events)

	if len(runtime.pkg.Capabilities.SubAgents) != 0 {
		t.Fatalf("child model context exposed nested delegation: %#v", runtime.pkg.Capabilities)
	}
}

func TestRuntimeServiceContextBuildFailureFailsRun(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime()
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.Assembler = failingAssembler{err: errors.New("context unavailable")}

	if _, err := service.Run(context.Background(), testRunRequest()); err == nil {
		t.Fatal("expected context build error")
	}
	run, ok := state.Run("run_1")
	if !ok {
		t.Fatal("run state missing")
	}
	if run.Status != RunStatusFailed {
		t.Fatalf("run should be failed: %#v", run)
	}
	events := state.Events("run_1")
	if eventIndex(events, EventRuntimeStepFailed) < 0 || eventIndex(events, EventModelContextBuildFailed) < 0 || eventIndex(events, EventRunFailed) < 0 {
		t.Fatalf("model_context_build_failed event missing: %#v", events)
	}
}

func TestRuntimeServicePassesScopedData(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := &scopedDataRuntime{MockRuntime: *NewMockRuntime()}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	req := testRunRequest()
	req.ScopedData = ScopedData{
		Run: map[string]ScopedDataItem{
			"engineering_profile": {
				Source:     "init_processor",
				Visibility: string(observability.VisibilityInternal),
				Value:      JSONPayload(map[string]string{"platform": "ios"}),
			},
		},
	}

	events, err := service.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	_ = collect(events)

	item, ok := runtime.seen.RunValue("engineering_profile")
	if !ok {
		t.Fatal("scoped data missing")
	}
	var decoded map[string]string
	if err := json.Unmarshal(item.Value, &decoded); err != nil {
		t.Fatalf("invalid scoped data: %v", err)
	}
	if decoded["platform"] != "ios" {
		t.Fatalf("unexpected scoped data: %#v", decoded)
	}
}

func TestAgentDefinitionEffectiveDataPassingMode(t *testing.T) {
	cases := []struct {
		name string
		mode RuntimeMode
		want DataPassingMode
	}{
		{name: "react", mode: RuntimeModeReact, want: DataPassingMessages},
		{name: "direct", mode: RuntimeModeDirect, want: DataPassingMessages},
		{name: "workflow", mode: RuntimeModeWorkflow, want: DataPassingState},
		{name: "graph", mode: RuntimeModeGraph, want: DataPassingState},
		{name: "plan_execute", mode: RuntimeModePlanExecute, want: DataPassingPlanResult},
		{name: "deep_agent", mode: RuntimeModeDeepAgent, want: DataPassingTask},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := AgentDefinition{Runtime: RuntimeSpec{Mode: tc.mode}}
			if got := def.EffectiveDataPassingMode(); got != tc.want {
				t.Fatalf("unexpected data passing mode: got %s want %s", got, tc.want)
			}
		})
	}
}

func TestMockRuntimeValidatesWorkflowDefinition(t *testing.T) {
	runtime := NewMockRuntime()
	def := AgentDefinition{
		AgentID: "agent_1",
		Version: "v1",
		Runtime: RuntimeSpec{Type: RuntimeTypeMock, Mode: RuntimeModeWorkflow},
		Workflow: &WorkflowDefinition{
			EntryNode: "start",
			Nodes:     []WorkflowNode{{NodeID: "start", NodeType: "agent", AgentID: "agent_1"}},
		},
	}
	if err := runtime.ValidateConfig(context.Background(), def); err != nil {
		t.Fatalf("workflow definition should be valid: %v", err)
	}

	def.Workflow = nil
	if err := runtime.ValidateConfig(context.Background(), def); err == nil {
		t.Fatal("expected missing workflow definition error")
	}
}

func TestInMemoryStateManagerConcurrentEventReadWrite(t *testing.T) {
	state := NewInMemoryStateManager()
	req := testRunRequest()
	if _, err := state.StartRun(context.Background(), req); err != nil {
		t.Fatalf("start run failed: %v", err)
	}

	const writers = 8
	const eventsPerWriter = 50
	var wg sync.WaitGroup
	for writer := 0; writer < writers; writer++ {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			for i := 0; i < eventsPerWriter; i++ {
				_, err := state.AppendEvent(context.Background(), observability.AgentEvent{
					RunID:      req.RunID,
					EventType:  EventAgentTextDelta,
					Visibility: observability.VisibilityDebug,
					Payload:    JSONPayload(map[string]string{"writer": fmt.Sprint(writer), "index": fmt.Sprint(i)}),
				})
				if err != nil {
					t.Errorf("append event failed: %v", err)
				}
				_ = state.Events(req.RunID)
			}
		}(writer)
	}
	wg.Wait()

	events := state.Events(req.RunID)
	expected := writers * eventsPerWriter
	if len(events) != expected {
		t.Fatalf("unexpected event count: got %d want %d", len(events), expected)
	}
	seen := make(map[string]struct{}, len(events))
	for _, event := range events {
		if event.EventID == "" {
			t.Fatal("event id missing")
		}
		if _, ok := seen[event.EventID]; ok {
			t.Fatalf("duplicate event id: %s", event.EventID)
		}
		seen[event.EventID] = struct{}{}
	}
}

func TestInMemoryStateManagerRejectsTerminalRunTransition(t *testing.T) {
	state := NewInMemoryStateManager()
	req := testRunRequest()
	if _, err := state.StartRun(context.Background(), req); err != nil {
		t.Fatalf("start run failed: %v", err)
	}
	if err := state.CompleteRun(context.Background(), req.RunID); err != nil {
		t.Fatalf("complete run failed: %v", err)
	}
	if err := state.FailRun(context.Background(), req.RunID, errors.New("late failure")); !errors.Is(err, ErrInvalidRunTransition) {
		t.Fatalf("expected invalid run transition, got %v", err)
	}
	if _, err := state.AppendEvent(context.Background(), observability.AgentEvent{
		RunID:      req.RunID,
		EventType:  EventAgentTextDelta,
		Visibility: observability.VisibilityDebug,
	}); !errors.Is(err, ErrRunTerminal) {
		t.Fatalf("expected terminal run event guard, got %v", err)
	}
}

func TestInMemoryStateManagerStepTransitionGuards(t *testing.T) {
	state := NewInMemoryStateManager()
	if err := state.CompleteStep(context.Background(), "run_missing", "step_missing"); !errors.Is(err, ErrStepNotFound) {
		t.Fatalf("expected missing step error, got %v", err)
	}
	step, err := state.StartStep(context.Background(), "run_1", StepStart{StepID: "step_1", Kind: StepKindModelCall})
	if err != nil {
		t.Fatalf("start step failed: %v", err)
	}
	if step.Status != StepStatusRunning {
		t.Fatalf("step should be running: %#v", step)
	}
	if err := state.CompleteStep(context.Background(), "run_1", "step_1"); err != nil {
		t.Fatalf("complete step failed: %v", err)
	}
	if err := state.CompleteStep(context.Background(), "run_1", "step_1"); err != nil {
		t.Fatalf("duplicate complete should be idempotent: %v", err)
	}
	if err := state.FailStep(context.Background(), "run_1", "step_1", errors.New("late failure")); !errors.Is(err, ErrInvalidStepTransition) {
		t.Fatalf("expected invalid step transition, got %v", err)
	}
	if err := state.CancelStep(context.Background(), "run_1", "step_1"); !errors.Is(err, ErrInvalidStepTransition) {
		t.Fatalf("expected terminal cancellation rejection, got %v", err)
	}
	if _, err := state.StartStep(context.Background(), "run_1", StepStart{StepID: "step_1", Kind: StepKindModelCall}); !errors.Is(err, ErrInvalidStepTransition) {
		t.Fatalf("expected terminal step restart rejection, got %v", err)
	}
	if _, err := state.StartStep(context.Background(), "run_1", StepStart{StepID: "step_cancel", Kind: StepKindRuntimeAdapter}); err != nil {
		t.Fatalf("start cancellable step failed: %v", err)
	}
	if err := state.CancelStep(context.Background(), "run_1", "step_cancel"); err != nil {
		t.Fatalf("cancel step failed: %v", err)
	}
	if err := state.CancelStep(context.Background(), "run_1", "step_cancel"); err != nil {
		t.Fatalf("duplicate cancel should be idempotent: %v", err)
	}
}

func TestInMemoryStateManagerConcurrentStepWritesAreIsolated(t *testing.T) {
	state := NewInMemoryStateManager()
	const steps = 32
	var wg sync.WaitGroup
	for i := 0; i < steps; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			stepID := fmt.Sprintf("step_%d", i)
			if _, err := state.StartStep(context.Background(), "run_1", StepStart{StepID: stepID, Kind: StepKindModelCall}); err != nil {
				t.Errorf("start step failed: %v", err)
				return
			}
			if i%2 == 0 {
				if err := state.CompleteStep(context.Background(), "run_1", stepID); err != nil {
					t.Errorf("complete step failed: %v", err)
				}
				return
			}
			if err := state.FailStep(context.Background(), "run_1", stepID, errors.New("step failed")); err != nil {
				t.Errorf("fail step failed: %v", err)
			}
		}(i)
	}
	wg.Wait()

	for i := 0; i < steps; i++ {
		stepID := fmt.Sprintf("step_%d", i)
		step, ok := state.Step("run_1", stepID)
		if !ok {
			t.Fatalf("step missing: %s", stepID)
		}
		want := StepStatusFailed
		if i%2 == 0 {
			want = StepStatusCompleted
		}
		if step.Status != want {
			t.Fatalf("step status mismatch for %s: got %s want %s", stepID, step.Status, want)
		}
	}
}

type scopedDataRuntime struct {
	MockRuntime
	seen ScopedData
}

func (r *scopedDataRuntime) Run(ctx context.Context, req RunRequest) (<-chan observability.AgentEvent, error) {
	data, ok := ScopedDataFrom(ctx)
	if !ok {
		return nil, errors.New("scoped data missing from context")
	}
	r.seen = data
	return r.MockRuntime.Run(ctx, req)
}

type modelContextCapturingRuntime struct {
	MockRuntime
	pkg ModelContextPackage
}

func (r *modelContextCapturingRuntime) Run(ctx context.Context, req RunRequest) (<-chan observability.AgentEvent, error) {
	pkg, ok := ModelContextPackageFrom(ctx)
	if !ok {
		return nil, ErrModelContextPackageMissing
	}
	r.pkg = pkg
	return r.MockRuntime.Run(ctx, req)
}

type failingAssembler struct {
	err error
}

func (a failingAssembler) Build(context.Context, RuntimeContextAssemblyRequest) (ModelContextPackage, error) {
	return ModelContextPackage{}, a.err
}

func TestJSONPayload(t *testing.T) {
	payload := JSONPayload(map[string]string{"text": "hello"})
	var decoded map[string]string
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("invalid payload: %v", err)
	}
	if decoded["text"] != "hello" {
		t.Fatalf("unexpected payload: %#v", decoded)
	}
}

func testRunRequest() RunRequest {
	return RunRequest{
		SessionID: "session_1",
		RunID:     "run_1",
		Definition: AgentDefinition{
			AgentID:   "agent_1",
			AgentType: "assistant",
			Version:   "v1",
			Runtime:   RuntimeSpec{Type: RuntimeTypeMock, Mode: RuntimeModeReact},
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

func hasModelMessage(messages []ModelContextMessage, role, content string) bool {
	for _, msg := range messages {
		if msg.Role == role && msg.Content == content {
			return true
		}
	}
	return false
}

type storagePlanSpy struct {
	mu      sync.Mutex
	next    StorageWriteExecutor
	reasons []string
	plans   []storagewrite.Plan
}

func (s *storagePlanSpy) Execute(ctx context.Context, plan storagewrite.Plan) (storagewrite.Result, error) {
	s.mu.Lock()
	s.reasons = append(s.reasons, plan.Reason)
	s.plans = append(s.plans, plan)
	s.mu.Unlock()
	return s.next.Execute(ctx, plan)
}

func (s *storagePlanSpy) Reasons() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.reasons))
	copy(out, s.reasons)
	return out
}

func (s *storagePlanSpy) Plans() []storagewrite.Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]storagewrite.Plan, len(s.plans))
	copy(out, s.plans)
	return out
}

func assertReasonPrefixes(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("plan count mismatch: got %#v want %#v", got, want)
	}
	for i := range want {
		if len(got[i]) < len(want[i]) || got[i][:len(want[i])] != want[i] {
			t.Fatalf("plan %d reason mismatch: got %s want prefix %s all=%#v", i, got[i], want[i], got)
		}
	}
}

type fixedIDGenerator struct{}

func (fixedIDGenerator) NewTraceID() string   { return "trace_fixed" }
func (fixedIDGenerator) NewSpanID() string    { return "span_fixed" }
func (fixedIDGenerator) NewRunID() string     { return "run_fixed" }
func (fixedIDGenerator) NewEventID() string   { return "event_fixed" }
func (fixedIDGenerator) NewRequestID() string { return "request_fixed" }

type cancelCapturingRuntime struct {
	MockRuntime
	mu          sync.Mutex
	cancelCalls int
}

type contextRejectingWriter struct {
	next StorageWriteExecutor
}

func (w contextRejectingWriter) Execute(ctx context.Context, plan storagewrite.Plan) (storagewrite.Result, error) {
	if err := ctx.Err(); err != nil {
		return storagewrite.Result{}, err
	}
	return w.next.Execute(ctx, plan)
}

func (r *cancelCapturingRuntime) Cancel(context.Context, CancelRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancelCalls++
	return nil
}

func (r *cancelCapturingRuntime) CancelCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelCalls
}
