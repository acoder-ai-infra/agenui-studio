package runtimestore_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/runtimestore"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	"github.com/AGenUI/agenui-studio/harness/internal/storage/memory"
)

func TestBridgeBeginResumeAcceptsControlClaimAndAppendsOnce(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	bridge := runtimestore.NewBridge(stores)
	run := &storage.Run{RunID: "run-resume", SessionID: "s1", TenantID: "t1", Status: storage.RunStatusCreated}
	if err := stores.Runs.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err := stores.Runs.CompareAndSetStatus(ctx, run.RunID, storage.RunStatusCreated, storage.RunStatusRunning, storage.RunMutation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := stores.Runs.CompareAndSetStatus(ctx, run.RunID, storage.RunStatusRunning, storage.RunStatusWaitingControl, storage.RunMutation{}); err != nil {
		t.Fatal(err)
	}
	token := "resume-secret"
	hash := sha256.Sum256([]byte(token))
	controlRequest := &storage.ControlRequest{
		RequestID: "control-1", RunID: run.RunID, TenantID: "t1", CheckpointID: "checkpoint-1",
		Status: "pending", ResumeTokenHash: "sha256:" + hex.EncodeToString(hash[:]),
	}
	if err := stores.Controls.Create(ctx, controlRequest); err != nil {
		t.Fatal(err)
	}
	if _, err := stores.Controls.CompareAndAnswer(ctx, controlRequest.RequestID, "pending", "answered", "response-ref"); err != nil {
		t.Fatal(err)
	}
	const attemptID = "attempt-1"
	event := observability.AgentEvent{
		EventID: "event-resume", IdempotencyKey: "resume-once", RunID: run.RunID,
		EventType: observability.EventResumeAccepted, Visibility: observability.VisibilityInternal,
		Payload: agentruntime.JSONPayload(map[string]string{"attempt_id": attemptID}),
	}
	request := agentruntime.ResumeRequest{
		RunID: run.RunID, SessionID: run.SessionID, CheckpointID: controlRequest.CheckpointID,
		ControlRequestID: controlRequest.RequestID, ResumeToken: token,
	}
	if err := bridge.BeginResume(ctx, request, attemptID, event); err != nil {
		t.Fatal(err)
	}
	if err := bridge.BeginResume(ctx, request, attemptID, event); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	events, err := stores.Events.Query(ctx, storage.EventQuery{RunID: run.RunID})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventType != observability.EventResumeAccepted {
		t.Fatalf("resume events = %#v", events)
	}
}

func ctxT(tenant string) context.Context {
	return observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: tenant, TraceID: "trace_x"})
}

// R-001: StartRun -> AppendEvent -> CompleteRun drives created->running->completed
// and events flow through the EventStore with allocated sequences.
func TestBridgeRunLifecycle(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	b := runtimestore.NewBridge(stores)

	req := agentruntime.RunRequest{
		SessionID: "s1", RunID: "run1", TenantID: "t1",
		Definition: agentruntime.AgentDefinition{AgentID: "a1"},
		Trace:      observability.TraceContext{TraceID: "trace_x"},
	}
	if _, err := b.StartRun(ctx, req); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	run, _ := stores.Runs.Get(ctx, "run1")
	if run.Status != storage.RunStatusRunning {
		t.Fatalf("want running, got %s", run.Status)
	}

	res, err := b.AppendEvent(ctx, observability.AgentEvent{RunID: "run1", EventType: observability.EventRunStarted})
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if res.Sequence != 1 {
		t.Fatalf("want sequence 1, got %d", res.Sequence)
	}

	if err := b.CompleteRun(ctx, "run1"); err != nil {
		t.Fatalf("CompleteRun: %v", err)
	}
	run, _ = stores.Runs.Get(ctx, "run1")
	if run.Status != storage.RunStatusCompleted {
		t.Fatalf("want completed, got %s", run.Status)
	}
}

func TestBridgeBindRuntimeCapabilitiesUsesCAS(t *testing.T) {
	t.Run("different snapshots cannot overwrite", func(t *testing.T) {
		bridge, ctx, runID := newCapabilityBridge(t)
		bindings := []agentruntime.RuntimeCapabilityBinding{{Hash: "sha256:capability-a"}, {Hash: "sha256:capability-b"}}
		start := make(chan struct{})
		errs := make(chan error, len(bindings))
		var wg sync.WaitGroup
		for _, binding := range bindings {
			binding := binding
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs <- bridge.BindRuntimeCapabilities(ctx, runID, binding)
			}()
		}
		close(start)
		wg.Wait()
		close(errs)

		success, drift := 0, 0
		for err := range errs {
			switch {
			case err == nil:
				success++
			case errors.Is(err, agentruntime.ErrRuntimeCapabilityDrift):
				drift++
			default:
				t.Fatalf("unexpected bind error: %v", err)
			}
		}
		if success != 1 || drift != 1 {
			t.Fatalf("success=%d drift=%d, want 1/1", success, drift)
		}
	})

	t.Run("same snapshot is idempotent", func(t *testing.T) {
		bridge, ctx, runID := newCapabilityBridge(t)
		binding := agentruntime.RuntimeCapabilityBinding{Hash: "sha256:same-capability"}
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs <- bridge.BindRuntimeCapabilities(ctx, runID, binding)
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("same binding should be idempotent: %v", err)
			}
		}
	})
}

func newCapabilityBridge(t *testing.T) (*runtimestore.Bridge, context.Context, string) {
	t.Helper()
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	const runID = "run-capabilities"
	if err := stores.Runs.Create(ctx, &storage.Run{
		RunID: runID, SessionID: "s1", TenantID: "t1", Status: storage.RunStatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	bridge := runtimestore.NewBridge(stores)
	if err := bridge.BindRuntime(ctx, runID, agentruntime.RuntimeBinding{
		SchemaVersion:  agentruntime.RuntimeBindingSchemaVersion,
		Runtime:        agentruntime.RuntimeTypeNative,
		RuntimeVersion: "builtin-v1", AdapterVersion: "v1",
		Governance:          agentruntime.RuntimeGovernanceManaged,
		AgentDefinitionHash: "sha256:definition",
	}); err != nil {
		t.Fatal(err)
	}
	return bridge, ctx, runID
}

func TestBridgeStepLifecyclePreservesMetadataAndRecordsEndTime(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	b := runtimestore.NewBridge(stores)

	started, err := b.StartStep(ctx, "run1", agentruntime.StepStart{
		StepID: "step1", Kind: agentruntime.StepKindModelContext, Name: "build_context", ParentStepID: "parent1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.CompleteStep(ctx, "run1", "step1"); err != nil {
		t.Fatal(err)
	}
	steps, err := stores.Steps.ListByRun(ctx, "run1")
	if err != nil || len(steps) != 1 {
		t.Fatalf("steps: %v %+v", err, steps)
	}
	step := steps[0]
	if step.StepType != string(agentruntime.StepKindModelContext) || step.Name != "build_context" || step.ParentStepID != "parent1" {
		t.Fatalf("step metadata was not preserved: %+v", step)
	}
	if step.StartedAt.IsZero() || !step.StartedAt.Equal(started.StartedAt) || step.EndedAt.IsZero() || step.EndedAt.Before(step.StartedAt) {
		t.Fatalf("step timestamps are invalid: %+v", step)
	}
}

func TestBridgeFailRunPersistsRuntimeErrorCode(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	b := runtimestore.NewBridge(stores)
	req := agentruntime.RunRequest{SessionID: "s1", RunID: "run1", TenantID: "t1"}
	if _, err := b.StartRun(ctx, req); err != nil {
		t.Fatal(err)
	}
	runtimeErr := agentruntime.NewRuntimeError(agentruntime.ErrorContextOverflow, "MODEL_CONTEXT_BUDGET_EXCEEDED", "model context exceeds input budget")
	if err := b.FailRun(ctx, req.RunID, runtimeErr); err != nil {
		t.Fatal(err)
	}
	run, err := stores.Runs.Get(ctx, req.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != storage.RunStatusFailed || run.ErrorCode != runtimeErr.Code || run.ErrorMessage == "" {
		t.Fatalf("failed run facts are incomplete: %+v", run)
	}
}

// StartRun is idempotent when the run is already running.
func TestBridgeStartRunIdempotent(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	b := runtimestore.NewBridge(stores)
	req := agentruntime.RunRequest{SessionID: "s1", RunID: "run1", TenantID: "t1"}
	if _, err := b.StartRun(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := b.StartRun(ctx, req); err != nil {
		t.Fatalf("second StartRun should be idempotent: %v", err)
	}
}

// A Run cancelled before asynchronous dispatch starts must never be revived.
func TestBridgeStartRunRejectsTerminalRun(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	b := runtimestore.NewBridge(stores)
	run := &storage.Run{RunID: "run-cancelled", SessionID: "s1", TenantID: "t1", Status: storage.RunStatusCreated}
	if err := stores.Runs.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := b.CancelRun(ctx, agentruntime.CancelRequest{RunID: run.RunID, SessionID: run.SessionID, Reason: "cancel before dispatch"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.StartRun(ctx, agentruntime.RunRequest{RunID: run.RunID, SessionID: run.SessionID, TenantID: run.TenantID}); !errors.Is(err, agentruntime.ErrRunTerminal) {
		t.Fatalf("StartRun error = %v, want ErrRunTerminal", err)
	}
	stored, err := stores.Runs.Get(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != storage.RunStatusCancelled {
		t.Fatalf("terminal run was revived: status=%s", stored.Status)
	}
}

// D4: both backends satisfy the interface via Select.
func TestSelectBackends(t *testing.T) {
	stores := memory.New().Stores()
	if runtimestore.Select(runtimestore.StateBackendMemory, stores) == nil {
		t.Fatal("memory backend nil")
	}
	if runtimestore.Select(runtimestore.StateBackendStorage, stores) == nil {
		t.Fatal("storage backend nil")
	}
}

var _ agentruntime.RuntimeStateManager = (*runtimestore.Bridge)(nil)
