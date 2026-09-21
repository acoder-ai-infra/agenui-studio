package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestRuntimeHooksEnrichScopedDataInPriorityOrder(t *testing.T) {
	var order []string
	var mu sync.Mutex
	first := RuntimeHookFunc(func(_ context.Context, input RuntimeHookInput) (RuntimeHookOutput, error) {
		mu.Lock()
		order = append(order, "first")
		mu.Unlock()
		return hookRunData("engineering_profile", input.Run.RunID), nil
	})
	second := RuntimeHookFunc(func(_ context.Context, input RuntimeHookInput) (RuntimeHookOutput, error) {
		if _, ok := input.Run.ScopedData.RunValue("engineering_profile"); !ok {
			return RuntimeHookOutput{}, errors.New("prior hook data missing")
		}
		mu.Lock()
		order = append(order, "second")
		mu.Unlock()
		return hookRunData("feature_flags", "enabled"), nil
	})
	pipeline := mustHookPipeline(t,
		RuntimeHookRegistration{ID: "second", Point: HookBeforeBuild, Priority: 20, Hook: second},
		RuntimeHookRegistration{ID: "first", Point: HookBeforeBuild, Priority: 10, Hook: first},
	)
	state := NewInMemoryStateManager()
	runtime := &scopedDataRuntime{MockRuntime: *NewMockRuntime()}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.Hooks = pipeline

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	collected := collect(events)
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Fatalf("hook order mismatch: %#v", order)
	}
	if _, ok := runtime.seen.RunValue("engineering_profile"); !ok {
		t.Fatalf("first hook data missing: %#v", runtime.seen)
	}
	if _, ok := runtime.seen.RunValue("feature_flags"); !ok {
		t.Fatalf("second hook data missing: %#v", runtime.seen)
	}
	if len(collected) == 0 || collected[0].EventType != EventRuntimeStepStarted {
		t.Fatalf("hook step should precede context build: %#v", collected)
	}
}

func TestRuntimeHookFailClosedStopsAdapter(t *testing.T) {
	pipeline := mustHookPipeline(t, RuntimeHookRegistration{
		ID:            "required_init",
		Point:         HookBeforeBuild,
		FailurePolicy: HookFailClosed,
		Hook: RuntimeHookFunc(func(context.Context, RuntimeHookInput) (RuntimeHookOutput, error) {
			return RuntimeHookOutput{}, errors.New("internal hook stack with secret")
		}),
	})
	state := NewInMemoryStateManager()
	runtime := &countingRuntime{MockRuntime: *NewMockRuntime()}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.Hooks = pipeline

	_, err := service.Run(context.Background(), testRunRequest())
	var runtimeErr *RuntimeError
	if !errors.As(err, &runtimeErr) || runtimeErr.Code != "RUNTIME_HOOK_FAILED" {
		t.Fatalf("hook error not normalized: %v", err)
	}
	if runtime.RunCount() != 0 {
		t.Fatalf("adapter ran after fail-closed hook: %d", runtime.RunCount())
	}
	run, ok := state.Run("run_1")
	if !ok || run.Status != RunStatusFailed || run.ErrorCode != "RUNTIME_HOOK_FAILED" {
		t.Fatalf("run state mismatch: %#v", run)
	}
	for _, event := range state.Events("run_1") {
		if event.EventType == EventRunFailed && event.Error != nil && event.Error.Message == "internal hook stack with secret" {
			t.Fatalf("hook detail leaked: %#v", event)
		}
	}
}

func TestRuntimeHookFailOpenContinues(t *testing.T) {
	pipeline := mustHookPipeline(t, RuntimeHookRegistration{
		ID:            "optional_enrichment",
		Point:         HookBeforeRun,
		FailurePolicy: HookFailOpen,
		Hook: RuntimeHookFunc(func(context.Context, RuntimeHookInput) (RuntimeHookOutput, error) {
			return RuntimeHookOutput{}, errors.New("optional source unavailable")
		}),
	})
	state := NewInMemoryStateManager()
	runtime := &countingRuntime{MockRuntime: *NewMockRuntime()}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.Hooks = pipeline

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("fail-open hook blocked run: %v", err)
	}
	collected := collect(events)
	if runtime.RunCount() != 1 {
		t.Fatalf("adapter run count = %d, want 1", runtime.RunCount())
	}
	if eventIndex(collected, EventRuntimeStepFailed) < 0 || eventIndex(collected, EventRunCompleted) < 0 {
		t.Fatalf("fail-open lifecycle events missing: %#v", collected)
	}
}

func TestRuntimeHooksKeepConcurrentRunDataIsolated(t *testing.T) {
	pipeline := mustHookPipeline(t, RuntimeHookRegistration{
		ID:    "run_identity",
		Point: HookBeforeBuild,
		Hook: RuntimeHookFunc(func(_ context.Context, input RuntimeHookInput) (RuntimeHookOutput, error) {
			return hookRunData("hook_run_id", input.Run.RunID), nil
		}),
	})
	state := NewInMemoryStateManager()
	runtime := &multiScopedDataRuntime{MockRuntime: *NewMockRuntime(), seen: make(map[string]ScopedData)}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.Hooks = pipeline

	const runs = 16
	var wg sync.WaitGroup
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := testRunRequest()
			req.RunID = fmt.Sprintf("run_%d", i)
			req.SessionID = fmt.Sprintf("session_%d", i)
			req.Trace.TraceID = fmt.Sprintf("trace_%d", i)
			events, err := service.Run(context.Background(), req)
			if err != nil {
				t.Errorf("run %d failed: %v", i, err)
				return
			}
			_ = collect(events)
		}(i)
	}
	wg.Wait()
	for runID, scoped := range runtime.Seen() {
		item, ok := scoped.RunValue("hook_run_id")
		if !ok || string(item.Value) != fmt.Sprintf("%q", runID) {
			t.Fatalf("scoped data crossed runs: run=%s data=%#v", runID, scoped)
		}
	}
}

func TestRuntimeHookPipelineRejectsDuplicateAndDataConflict(t *testing.T) {
	hook := RuntimeHookFunc(func(context.Context, RuntimeHookInput) (RuntimeHookOutput, error) { return RuntimeHookOutput{}, nil })
	pipeline := mustHookPipeline(t, RuntimeHookRegistration{ID: "same", Point: HookBeforeBuild, Hook: hook})
	if err := pipeline.Register(RuntimeHookRegistration{ID: "same", Point: HookBeforeRun, Hook: hook}); !errors.Is(err, ErrHookDuplicate) {
		t.Fatalf("expected duplicate hook error, got %v", err)
	}
	if err := pipeline.Register(RuntimeHookRegistration{ID: "unknown", Point: RuntimeHookPoint("runtime.unknown"), Hook: hook}); !errors.Is(err, ErrHookPointMissing) {
		t.Fatalf("expected invalid point error, got %v", err)
	}
	base := hookRunData("key", "base").ScopedData
	patch := hookRunData("key", "patch").ScopedData
	if _, err := mergeScopedData(base, patch, false); !errors.Is(err, ErrHookDataConflict) {
		t.Fatalf("expected scoped data conflict, got %v", err)
	}
	merged, err := mergeScopedData(base, patch, true)
	if err != nil || string(merged.Run["key"].Value) != `"patch"` {
		t.Fatalf("overwrite merge failed: %#v %v", merged, err)
	}
}

func TestRuntimeHookPanicAndTimeoutAreContained(t *testing.T) {
	tests := []struct {
		name string
		hook RuntimeHook
		want string
	}{
		{
			name: "panic",
			hook: RuntimeHookFunc(func(context.Context, RuntimeHookInput) (RuntimeHookOutput, error) {
				panic("provider panic with secret")
			}),
			want: "RUNTIME_HOOK_FAILED",
		},
		{
			name: "timeout",
			hook: RuntimeHookFunc(func(ctx context.Context, _ RuntimeHookInput) (RuntimeHookOutput, error) {
				<-ctx.Done()
				return RuntimeHookOutput{}, ctx.Err()
			}),
			want: "RUNTIME_HOOK_TIMEOUT",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pipeline := mustHookPipeline(t, RuntimeHookRegistration{ID: tt.name, Point: HookBeforeBuild, Timeout: 10 * time.Millisecond, Hook: tt.hook})
			state := NewInMemoryStateManager()
			service := NewRuntimeService(NewMockRuntime(), state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
			service.Hooks = pipeline
			_, err := service.Run(context.Background(), testRunRequest())
			var runtimeErr *RuntimeError
			if !errors.As(err, &runtimeErr) || runtimeErr.Code != tt.want {
				t.Fatalf("hook failure mismatch: %v", err)
			}
		})
	}
}

func TestRuntimeHookFailOpenConflictPreservesExistingData(t *testing.T) {
	pipeline := mustHookPipeline(t, RuntimeHookRegistration{
		ID:            "conflicting_optional_hook",
		Point:         HookBeforeBuild,
		FailurePolicy: HookFailOpen,
		Hook: RuntimeHookFunc(func(context.Context, RuntimeHookInput) (RuntimeHookOutput, error) {
			return hookRunData("existing", "patch"), nil
		}),
	})
	state := NewInMemoryStateManager()
	runtime := &scopedDataRuntime{MockRuntime: *NewMockRuntime()}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.Hooks = pipeline
	req := testRunRequest()
	req.ScopedData = hookRunData("existing", "base").ScopedData

	events, err := service.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("fail-open conflict blocked run: %v", err)
	}
	_ = collect(events)
	item, ok := runtime.seen.RunValue("existing")
	if !ok || string(item.Value) != `"base"` {
		t.Fatalf("existing scoped data was lost: %#v", runtime.seen)
	}
}

func TestRuntimeHookRejectsInvalidScopedDataJSON(t *testing.T) {
	pipeline := mustHookPipeline(t, RuntimeHookRegistration{
		ID:    "invalid_data",
		Point: HookBeforeBuild,
		Hook: RuntimeHookFunc(func(context.Context, RuntimeHookInput) (RuntimeHookOutput, error) {
			return RuntimeHookOutput{ScopedData: ScopedData{Run: map[string]ScopedDataItem{
				"invalid": {Source: "invalid_data", Value: []byte("{")},
			}}}, nil
		}),
	})
	state := NewInMemoryStateManager()
	service := NewRuntimeService(NewMockRuntime(), state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.Hooks = pipeline

	_, err := service.Run(context.Background(), testRunRequest())
	var runtimeErr *RuntimeError
	if !errors.As(err, &runtimeErr) || runtimeErr.Code != "RUNTIME_HOOK_DATA_INVALID" {
		t.Fatalf("invalid hook data error mismatch: %v", err)
	}
}

func TestRuntimeHookSnapshotDefersDynamicRegistrationToNextRun(t *testing.T) {
	pipeline := mustHookPipeline(t)
	var once sync.Once
	var mu sync.Mutex
	afterRuns := 0
	if err := pipeline.Register(RuntimeHookRegistration{
		ID:    "dynamic_registrar",
		Point: HookBeforeBuild,
		Hook: RuntimeHookFunc(func(context.Context, RuntimeHookInput) (RuntimeHookOutput, error) {
			var registerErr error
			once.Do(func() {
				registerErr = pipeline.Register(RuntimeHookRegistration{
					ID:    "late_after_run",
					Point: HookAfterRun,
					Hook: RuntimeHookFunc(func(context.Context, RuntimeHookInput) (RuntimeHookOutput, error) {
						mu.Lock()
						afterRuns++
						mu.Unlock()
						return RuntimeHookOutput{}, nil
					}),
				})
			})
			return RuntimeHookOutput{}, registerErr
		}),
	}); err != nil {
		t.Fatalf("register dynamic hook: %v", err)
	}
	state := NewInMemoryStateManager()
	service := NewRuntimeService(NewMockRuntime(), state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.Hooks = pipeline

	first := testRunRequest()
	first.RunID = "run_first"
	first.SessionID = "session_first"
	first.Trace.TraceID = "trace_first"
	events, err := service.Run(context.Background(), first)
	if err != nil {
		t.Fatalf("first run failed: %v", err)
	}
	_ = collect(events)
	mu.Lock()
	if afterRuns != 0 {
		t.Fatalf("dynamic hook entered current run: %d", afterRuns)
	}
	mu.Unlock()

	second := testRunRequest()
	second.RunID = "run_second"
	second.SessionID = "session_second"
	second.Trace.TraceID = "trace_second"
	events, err = service.Run(context.Background(), second)
	if err != nil {
		t.Fatalf("second run failed: %v", err)
	}
	_ = collect(events)
	mu.Lock()
	defer mu.Unlock()
	if afterRuns != 1 {
		t.Fatalf("dynamic hook not applied to next run: %d", afterRuns)
	}
}

func TestRuntimeHooksExecuteBuildFallbackRunLifecycle(t *testing.T) {
	var mu sync.Mutex
	var order []RuntimeHookPoint
	record := func(point RuntimeHookPoint) RuntimeHookFunc {
		return func(_ context.Context, input RuntimeHookInput) (RuntimeHookOutput, error) {
			if input.Point != point {
				return RuntimeHookOutput{}, fmt.Errorf("hook point mismatch: %s", input.Point)
			}
			if point == HookAfterBuild && (input.Handle == nil || input.Handle.Runtime != RuntimeTypeMock) {
				return RuntimeHookOutput{}, errors.New("selected handle missing")
			}
			if point == HookOnFallback && (input.Fallback == nil || input.Fallback.FromRuntime != RuntimeTypeEino || input.Fallback.ToRuntime != RuntimeTypeMock) {
				return RuntimeHookOutput{}, errors.New("fallback decision missing")
			}
			mu.Lock()
			order = append(order, point)
			mu.Unlock()
			return RuntimeHookOutput{}, nil
		}
	}
	pipeline := mustHookPipeline(t,
		RuntimeHookRegistration{ID: "after_build", Point: HookAfterBuild, Hook: record(HookAfterBuild)},
		RuntimeHookRegistration{ID: "on_fallback", Point: HookOnFallback, Hook: record(HookOnFallback)},
		RuntimeHookRegistration{ID: "before_run", Point: HookBeforeRun, Hook: record(HookBeforeRun)},
		RuntimeHookRegistration{ID: "after_run", Point: HookAfterRun, Hook: record(HookAfterRun)},
	)
	state := NewInMemoryStateManager()
	primary := &MockRuntime{NameValue: string(RuntimeTypeEino), Unhealthy: true, HealthReason: "unavailable"}
	fallback := NewMockRuntime()
	service := NewRuntimeService(primary, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.RegisterRuntime(RuntimeTypeMock, fallback)
	service.Hooks = pipeline
	req := testRunRequest()
	req.Definition.Runtime = RuntimeSpec{Type: RuntimeTypeAuto, Preferred: RuntimeTypeEino, Candidates: []RuntimeType{RuntimeTypeMock}}

	events, err := service.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	_ = collect(events)
	want := []RuntimeHookPoint{HookAfterBuild, HookOnFallback, HookBeforeRun, HookAfterRun}
	if len(order) != len(want) {
		t.Fatalf("hook lifecycle order mismatch: %#v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("hook lifecycle order mismatch: got %#v want %#v", order, want)
		}
	}
}

func TestRuntimeAfterRunFailClosedChangesTerminalState(t *testing.T) {
	pipeline := mustHookPipeline(t, RuntimeHookRegistration{
		ID:            "final_validation",
		Point:         HookAfterRun,
		FailurePolicy: HookFailClosed,
		Hook: RuntimeHookFunc(func(context.Context, RuntimeHookInput) (RuntimeHookOutput, error) {
			return RuntimeHookOutput{}, errors.New("final validation failed")
		}),
	})
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime(observability.AgentEvent{EventType: EventRunCompleted, Visibility: observability.VisibilityDebug})
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.Hooks = pipeline

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("run failed before stream: %v", err)
	}
	collected := collect(events)
	if eventIndex(collected, EventRunFailed) < 0 {
		t.Fatalf("run_failed missing after after_run failure: %#v", collected)
	}
	if eventIndex(collected, EventRunCompleted) >= 0 {
		t.Fatalf("run_completed must be suppressed: %#v", collected)
	}
	run, ok := state.Run("run_1")
	if !ok || run.Status != RunStatusFailed || run.ErrorCode != "RUNTIME_HOOK_FAILED" {
		t.Fatalf("run terminal state mismatch: %#v", run)
	}
}

func hookRunData(key, value string) RuntimeHookOutput {
	return RuntimeHookOutput{ScopedData: ScopedData{Run: map[string]ScopedDataItem{
		key: {
			Source:     "test_hook",
			Visibility: string(observability.VisibilityInternal),
			Value:      JSONPayload(value),
		},
	}}}
}

func mustHookPipeline(t *testing.T, registrations ...RuntimeHookRegistration) *RuntimeHookPipeline {
	t.Helper()
	pipeline, err := NewRuntimeHookPipeline(registrations...)
	if err != nil {
		t.Fatalf("create hook pipeline: %v", err)
	}
	return pipeline
}

type multiScopedDataRuntime struct {
	MockRuntime
	mu   sync.Mutex
	seen map[string]ScopedData
}

func (r *multiScopedDataRuntime) Run(ctx context.Context, req RunRequest) (<-chan observability.AgentEvent, error) {
	data, ok := ScopedDataFrom(ctx)
	if !ok {
		return nil, errors.New("scoped data missing")
	}
	r.mu.Lock()
	r.seen[req.RunID] = cloneScopedData(data)
	r.mu.Unlock()
	return r.MockRuntime.Run(ctx, req)
}

func (r *multiScopedDataRuntime) Seen() map[string]ScopedData {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]ScopedData, len(r.seen))
	for runID, data := range r.seen {
		out[runID] = cloneScopedData(data)
	}
	return out
}
