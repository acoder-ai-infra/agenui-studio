package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	artifactstore "github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storagewrite"
)

func TestRuntimeFinalizationPersistsSmallAssistantResponseInOrder(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime(
		observability.AgentEvent{EventType: EventAgentTextDelta, Payload: JSONPayload(map[string]string{"text": "你"})},
		observability.AgentEvent{EventType: EventAgentTextDelta, Payload: JSONPayload(map[string]string{"text": "好"})},
	)
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	spy := &storagePlanSpy{next: service.Writer}
	service.Writer = spy

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	collected := collect(events)
	final := eventByType(t, collected, EventFinalResponse)
	var index FinalResponseIndex
	if err := json.Unmarshal(final.Payload, &index); err != nil {
		t.Fatalf("decode final response: %v", err)
	}
	if index.Preview != "你好" || index.SizeBytes != len("你好") || index.ContentHash != sha256Hex("你好") {
		t.Fatalf("unexpected final response index: %#v", index)
	}
	if final.PayloadRef != index.ContentRef || index.MessageID == "" {
		t.Fatalf("final response refs are not aligned: event=%#v index=%#v", final, index)
	}
	if eventIndex(collected, EventFinalResponse) != len(collected)-2 || eventIndex(collected, EventRunCompleted) != len(collected)-1 {
		t.Fatalf("terminal ordering changed: %#v", collected)
	}

	plan := planByReason(t, spy.Plans(), "runtime.finalize")
	wantStores := []storagewrite.StoreName{
		storagewrite.StoreMessage,
		storagewrite.StoreEvent,
		storagewrite.StoreEvent,
		storagewrite.StoreRun,
	}
	if got := writeStores(plan.RequiredWrites); !reflect.DeepEqual(got, wantStores) {
		t.Fatalf("finalization write order = %#v, want %#v", got, wantStores)
	}
	message, ok := plan.RequiredWrites[0].Payload.(AssistantMessageWrite)
	if !ok || message.Content != "你好" || message.ContentRef != index.ContentRef {
		t.Fatalf("assistant message write changed: %#v", plan.RequiredWrites[0])
	}
}

func TestRuntimeFinalizationUsesArtifactForLargeResponse(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime(observability.AgentEvent{
		EventType: EventAgentTextDelta,
		Payload:   JSONPayload(map[string]string{"text": "large response"}),
	})
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.Finalization.ArtifactThreshold = 4
	spy := &storagePlanSpy{next: service.Writer}
	service.Writer = spy

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	collected := collect(events)
	final := eventByType(t, collected, EventFinalResponse)
	plan := planByReason(t, spy.Plans(), "runtime.finalize")
	wantStores := []storagewrite.StoreName{
		storagewrite.StoreArtifact,
		storagewrite.StoreMessage,
		storagewrite.StoreEvent,
		storagewrite.StoreEvent,
		storagewrite.StoreRun,
	}
	if got := writeStores(plan.RequiredWrites); !reflect.DeepEqual(got, wantStores) {
		t.Fatalf("large response write order = %#v, want %#v", got, wantStores)
	}
	artifact, ok := plan.RequiredWrites[0].Payload.(FinalArtifactWrite)
	if !ok || string(artifact.Content) != "large response" || artifact.ContentHash != sha256Hex("large response") {
		t.Fatalf("artifact write changed: %#v", plan.RequiredWrites[0])
	}
	parts, err := artifactstore.ParseRef(plan.RequiredWrites[0].Ref)
	if err != nil || parts.TenantID != "default" || parts.SessionID != "session_1" || parts.RunID != "run_1" || parts.ArtifactID != artifact.ArtifactID {
		t.Fatalf("artifact ref does not follow canonical format: ref=%s parts=%#v err=%v", plan.RequiredWrites[0].Ref, parts, err)
	}
	message, ok := plan.RequiredWrites[1].Payload.(AssistantMessageWrite)
	if !ok || message.Content != "" || message.ContentRef != plan.RequiredWrites[0].Ref || final.PayloadRef != message.ContentRef {
		t.Fatalf("artifact and message refs are not aligned: message=%#v final=%#v", message, final)
	}
}

func TestRuntimeFinalizationPreservesTrustedInternalResultVisibility(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime(observability.AgentEvent{
		EventType: EventAgentTextDelta,
		Payload:   JSONPayload(map[string]string{"text": "child result"}),
	})
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.Finalization.ArtifactThreshold = 4
	spy := &storagePlanSpy{next: service.Writer}
	service.Writer = spy
	req := testRunRequest()
	req.ResultVisibility = observability.VisibilityInternal

	events, err := service.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	collected := collect(events)
	for _, eventType := range []observability.EventType{EventFinalResponse, EventRunCompleted} {
		if event := eventByType(t, collected, eventType); event.Visibility != observability.VisibilityInternal {
			t.Fatalf("%s visibility = %q, want internal", eventType, event.Visibility)
		}
	}
	plan := planByReason(t, spy.Plans(), "runtime.finalize")
	artifact, ok := plan.RequiredWrites[0].Payload.(FinalArtifactWrite)
	if !ok || artifact.Visibility != observability.VisibilityInternal {
		t.Fatalf("child artifact visibility changed: %#v", plan.RequiredWrites[0])
	}
	message, ok := plan.RequiredWrites[1].Payload.(AssistantMessageWrite)
	if !ok || message.Visibility != observability.VisibilityInternal {
		t.Fatalf("child message visibility changed: %#v", plan.RequiredWrites[1])
	}
}

func TestRuntimeFinalizationAdapterSnapshotOverridesStreamedDeltas(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime(
		observability.AgentEvent{EventType: EventAgentTextDelta, Payload: JSONPayload(map[string]string{"text": "partial"})},
		observability.AgentEvent{EventType: EventFinalResponse, Payload: JSONPayload(map[string]string{"content": "complete"})},
	)
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	collected := collect(events)
	if got := eventTypeCount(collected, EventFinalResponse); got != 1 {
		t.Fatalf("final_response count = %d, want 1", got)
	}
	var index FinalResponseIndex
	if err := json.Unmarshal(eventByType(t, collected, EventFinalResponse).Payload, &index); err != nil {
		t.Fatalf("decode final response: %v", err)
	}
	if index.Preview != "complete" || index.SizeBytes != len("complete") {
		t.Fatalf("adapter final snapshot did not override deltas: %#v", index)
	}
}

func TestRuntimeFinalizationFailureFailsRunWithoutTerminalSuccess(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime(observability.AgentEvent{EventType: EventAgentTextDelta, Payload: JSONPayload(map[string]string{"text": "hello"})})
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	ports := NewRuntimeStateStorePorts(state)
	message := storagewrite.NewMemoryPort(storagewrite.StoreMessage)
	message.Fail(storagewrite.OperationInsert, errors.New("message unavailable"))
	ports[storagewrite.StoreMessage] = message
	ports[storagewrite.StoreArtifact] = storagewrite.NewMemoryPort(storagewrite.StoreArtifact)
	service.Writer = storagewrite.NewExecutor(ports)

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("run should enter stream before finalization: %v", err)
	}
	collected := collect(events)
	if eventIndex(collected, EventFinalResponse) >= 0 || eventIndex(collected, EventRunCompleted) >= 0 {
		t.Fatalf("success events must be suppressed after finalization failure: %#v", collected)
	}
	failed := eventByType(t, collected, EventRunFailed)
	if failed.Error == nil || failed.Error.Code != "FINALIZATION_WRITE_FAILED" || !failed.Error.Retryable {
		t.Fatalf("finalization error not normalized: %#v", failed)
	}
	run, ok := state.Run("run_1")
	if !ok || run.Status != RunStatusFailed || run.ErrorCode != "FINALIZATION_WRITE_FAILED" {
		t.Fatalf("run must fail when final response is not durable: %#v", run)
	}
}

func TestRuntimeFinalizationRejectsResponseBeyondCaptureLimit(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime(observability.AgentEvent{EventType: EventAgentTextDelta, Payload: JSONPayload(map[string]string{"text": "hello"})})
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.Finalization.MaxContentBytes = 4

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("run should enter stream before finalization: %v", err)
	}
	collected := collect(events)
	failed := eventByType(t, collected, EventRunFailed)
	if failed.Error == nil || failed.Error.Code != "FINAL_RESPONSE_TOO_LARGE" {
		t.Fatalf("capture limit error changed: %#v", failed)
	}
	if eventIndex(collected, EventFinalResponse) >= 0 || eventIndex(collected, EventRunCompleted) >= 0 {
		t.Fatalf("oversized response must not complete: %#v", collected)
	}
}

func TestRuntimeFinalizationIsIsolatedAcrossConcurrentRuns(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime(observability.AgentEvent{EventType: EventAgentTextDelta, Payload: JSONPayload(map[string]string{"text": "hello"})})
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))

	const runs = 12
	ids := make(chan string, runs)
	errs := make(chan error, runs)
	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := testRunRequest()
			req.RunID = fmt.Sprintf("run_%d", i)
			req.SessionID = fmt.Sprintf("session_%d", i)
			req.Trace.TraceID = fmt.Sprintf("trace_%d", i)
			events, err := service.Run(context.Background(), req)
			if err != nil {
				errs <- err
				return
			}
			collected := collect(events)
			final := eventByTypeConcurrent(collected, EventFinalResponse)
			if final == nil {
				errs <- errors.New("final_response missing")
				return
			}
			ids <- final.EventID
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent run failed: %v", err)
		}
	}
	seen := make(map[string]struct{}, runs)
	for id := range ids {
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate final response id: %s", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != runs {
		t.Fatalf("finalized runs = %d, want %d", len(seen), runs)
	}
}

func TestRuntimeFinalizationRunsBeforeResponseHook(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime(observability.AgentEvent{EventType: EventAgentTextDelta, Payload: JSONPayload(map[string]string{"text": "hello"})})
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	var calls atomic.Int64
	if err := service.Hooks.Register(RuntimeHookRegistration{
		ID:    "before_response",
		Point: HookBeforeResponse,
		Hook: RuntimeHookFunc(func(context.Context, RuntimeHookInput) (RuntimeHookOutput, error) {
			calls.Add(1)
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
	if calls.Load() != 1 || eventIndex(collected, EventFinalResponse) < 0 {
		t.Fatalf("before_response hook was not executed before finalization: calls=%d events=%#v", calls.Load(), collected)
	}
}

func TestRuntimeFinalizationBeforeResponseHookCanBlockCompletion(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime(observability.AgentEvent{EventType: EventAgentTextDelta, Payload: JSONPayload(map[string]string{"text": "hello"})})
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	if err := service.Hooks.Register(RuntimeHookRegistration{
		ID:            "response_guard",
		Point:         HookBeforeResponse,
		FailurePolicy: HookFailClosed,
		Hook: RuntimeHookFunc(func(context.Context, RuntimeHookInput) (RuntimeHookOutput, error) {
			return RuntimeHookOutput{}, errors.New("response blocked")
		}),
	}); err != nil {
		t.Fatalf("register hook: %v", err)
	}

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("run should enter stream before response guard: %v", err)
	}
	collected := collect(events)
	if eventIndex(collected, EventFinalResponse) >= 0 || eventIndex(collected, EventRunCompleted) >= 0 {
		t.Fatalf("blocked response must not complete: %#v", collected)
	}
	failed := eventByType(t, collected, EventRunFailed)
	if failed.Error == nil || failed.Error.Code != "RUNTIME_HOOK_FAILED" {
		t.Fatalf("response guard error changed: %#v", failed)
	}
}

func TestRuntimeRejectsInvalidFinalizationPolicyBeforeStartingRun(t *testing.T) {
	state := NewInMemoryStateManager()
	service := NewRuntimeService(NewMockRuntime(), state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.Finalization.MaxContentBytes = -1

	if _, err := service.Run(context.Background(), testRunRequest()); err == nil {
		t.Fatal("expected invalid finalization policy")
	}
	if _, ok := state.Run("run_1"); ok {
		t.Fatal("invalid policy must fail before run state is created")
	}
}

func TestPreviewTextPreservesUnicodeBoundaries(t *testing.T) {
	got := previewText("你好world", 3)
	if got != "你好w" || !utf8.ValidString(got) {
		t.Fatalf("unicode preview changed: %q", got)
	}
}

func eventByType(t *testing.T, events []observability.AgentEvent, eventType observability.EventType) observability.AgentEvent {
	t.Helper()
	event := eventByTypeConcurrent(events, eventType)
	if event == nil {
		t.Fatalf("event %s missing: %#v", eventType, events)
	}
	return *event
}

func eventByTypeConcurrent(events []observability.AgentEvent, eventType observability.EventType) *observability.AgentEvent {
	for i := range events {
		if events[i].EventType == eventType {
			return &events[i]
		}
	}
	return nil
}

func eventTypeCount(events []observability.AgentEvent, eventType observability.EventType) int {
	count := 0
	for _, event := range events {
		if event.EventType == eventType {
			count++
		}
	}
	return count
}

func planByReason(t *testing.T, plans []storagewrite.Plan, reason string) storagewrite.Plan {
	t.Helper()
	for _, plan := range plans {
		if plan.Reason == reason {
			return plan
		}
	}
	t.Fatalf("plan %s missing: %#v", reason, plans)
	return storagewrite.Plan{}
}

func writeStores(writes []storagewrite.Write) []storagewrite.StoreName {
	stores := make([]storagewrite.StoreName, 0, len(writes))
	for _, write := range writes {
		stores = append(stores, write.Store)
	}
	return stores
}
