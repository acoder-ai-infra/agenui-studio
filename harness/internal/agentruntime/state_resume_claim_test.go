package agentruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestInMemoryResumeClaimRejectsSecondOwnerAndFencesWrongOwner(t *testing.T) {
	state := NewInMemoryStateManager()
	runReq := testRunRequest()
	prepareWaitingRun(t, state, runReq)
	resume := resumeClaimRequest(runReq)

	if err := state.BeginResume(context.Background(), resume, "attempt_owner", resumeAcceptedEvent(runReq.RunID, "event_owner")); err != nil {
		t.Fatalf("claim owner attempt: %v", err)
	}
	claimed, _ := state.Run(runReq.RunID)
	if claimed.Status != RunStatusResuming || claimed.ResumeAttemptID != "attempt_owner" {
		t.Fatalf("resume owner not persisted: %#v", claimed)
	}

	if err := state.BeginResume(context.Background(), resume, "attempt_other", resumeAcceptedEvent(runReq.RunID, "event_other")); !errors.Is(err, ErrResumeAlreadyClaimed) {
		t.Fatalf("second owner must be rejected: %v", err)
	}
	if err := state.FailResumeAttempt(context.Background(), runReq.RunID, "attempt_other", resumeFailedEvent(runReq.RunID, "event_other_failed"), errors.New("other failed"), true); !errors.Is(err, ErrResumeClaimLost) {
		t.Fatalf("wrong owner released current claim: %v", err)
	}
	if err := state.ActivateResume(context.Background(), runReq.RunID, "attempt_other"); !errors.Is(err, ErrResumeClaimLost) {
		t.Fatalf("wrong owner activated current claim: %v", err)
	}
	current, _ := state.Run(runReq.RunID)
	if current.Status != RunStatusResuming || current.ResumeAttemptID != "attempt_owner" {
		t.Fatalf("wrong owner changed current claim: %#v", current)
	}

	if err := state.ActivateResume(context.Background(), runReq.RunID, "attempt_owner"); err != nil {
		t.Fatalf("activate owner: %v", err)
	}
	activated, _ := state.Run(runReq.RunID)
	if activated.Status != RunStatusRunning || activated.ResumeAttemptID != "" || activated.PendingControlRequestID != "" {
		t.Fatalf("activation did not clear resume claim: %#v", activated)
	}
}

func TestInMemoryResumeClaimRetryableCleanupReleasesOnlyCurrentOwner(t *testing.T) {
	state := NewInMemoryStateManager()
	runReq := testRunRequest()
	prepareWaitingRun(t, state, runReq)
	resume := resumeClaimRequest(runReq)

	if err := state.BeginResume(context.Background(), resume, "attempt_1", resumeAcceptedEvent(runReq.RunID, "event_1")); err != nil {
		t.Fatal(err)
	}
	if err := state.FailResumeAttempt(
		context.Background(),
		runReq.RunID,
		"attempt_1",
		resumeFailedEvent(runReq.RunID, "event_failed_1"),
		errors.New("temporary dependency failure"),
		true,
	); err != nil {
		t.Fatalf("release retryable claim: %v", err)
	}
	released, _ := state.Run(runReq.RunID)
	if released.Status != RunStatusWaitingControl || released.ResumeAttemptID != "" {
		t.Fatalf("retryable cleanup kept claim: %#v", released)
	}

	if err := state.BeginResume(context.Background(), resume, "attempt_2", resumeAcceptedEvent(runReq.RunID, "event_2")); err != nil {
		t.Fatalf("claim after cleanup: %v", err)
	}
	if err := state.ActivateResume(context.Background(), runReq.RunID, "attempt_2"); err != nil {
		t.Fatalf("activate after cleanup: %v", err)
	}
}

func TestInMemoryResumeClaimSameAttemptAndEventIsIdempotent(t *testing.T) {
	state := NewInMemoryStateManager()
	runReq := testRunRequest()
	prepareWaitingRun(t, state, runReq)
	resume := resumeClaimRequest(runReq)
	event := resumeAcceptedEvent(runReq.RunID, "event_1")

	if err := state.BeginResume(context.Background(), resume, "attempt_1", event); err != nil {
		t.Fatal(err)
	}
	if err := state.BeginResume(context.Background(), resume, "attempt_1", event); err != nil {
		t.Fatalf("same claim replay must be idempotent: %v", err)
	}
	mutated := event
	mutated.Payload = JSONPayload(map[string]string{"attempt_id": "different"})
	if err := state.BeginResume(context.Background(), resume, "attempt_1", mutated); !errors.Is(err, ErrResumeAlreadyClaimed) {
		t.Fatalf("mutated event replay must be rejected, got %v", err)
	}
	if got := len(state.Events(runReq.RunID)); got != 1 {
		t.Fatalf("idempotent replay appended %d events", got)
	}
}

func TestRuntimeServicePersistsOneAttemptIDAcrossResumeStoreActions(t *testing.T) {
	tests := []struct {
		name       string
		assembler  RuntimeContextAssembler
		wantAction RunStoreAction
		wantError  bool
	}{
		{name: "activation", wantAction: RunStoreActionActivateResume},
		{name: "cleanup", assembler: failingAssembler{err: errors.New("context unavailable")}, wantAction: RunStoreActionFailResume, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := NewInMemoryStateManager()
			runReq := testRunRequest()
			prepareWaitingRun(t, state, runReq)
			service := NewRuntimeService(NewMockRuntime(), state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
			service.IDs = fixedIDGenerator{}
			if tt.assembler != nil {
				service.Assembler = tt.assembler
			}
			spy := &storagePlanSpy{next: service.Writer}
			service.Writer = spy

			events, err := service.Resume(context.Background(), resumeClaimRequest(runReq))
			if tt.wantError {
				if err == nil {
					t.Fatal("expected resume error")
				}
			} else {
				if err != nil {
					t.Fatalf("resume: %v", err)
				}
				_ = collect(events)
			}

			const wantAttemptID = "resume_request_fixed"
			seenBegin, seenTerminal := false, false
			for _, plan := range spy.Plans() {
				for _, write := range plan.RequiredWrites {
					payload, ok := write.Payload.(RunStoreWrite)
					if !ok || (payload.Action != RunStoreActionBeginResume && payload.Action != tt.wantAction) {
						continue
					}
					if payload.ResumeAttemptID != wantAttemptID {
						t.Fatalf("action %s attempt_id=%q", payload.Action, payload.ResumeAttemptID)
					}
					seenBegin = seenBegin || payload.Action == RunStoreActionBeginResume
					seenTerminal = seenTerminal || payload.Action == tt.wantAction
				}
			}
			if !seenBegin || !seenTerminal {
				t.Fatalf("missing resume store actions: begin=%v terminal=%v plans=%#v", seenBegin, seenTerminal, spy.Plans())
			}
		})
	}
}

func TestRuntimeServiceClaimHeldDoesNotReachRuntimeAdapter(t *testing.T) {
	memory := NewInMemoryStateManager()
	runReq := testRunRequest()
	prepareWaitingRun(t, memory, runReq)
	resume := resumeClaimRequest(runReq)
	if err := memory.BeginResume(
		context.Background(),
		resume,
		"attempt_existing",
		resumeAcceptedEvent(runReq.RunID, "event_existing"),
	); err != nil {
		t.Fatalf("prepare existing owner: %v", err)
	}
	runtime := &countingResumeRuntime{MockRuntime: *NewMockRuntime()}
	service := NewRuntimeService(runtime, memory, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.IDs = fixedIDGenerator{}

	_, err := service.Resume(context.Background(), resume)
	var runtimeErr *RuntimeError
	if !errors.As(err, &runtimeErr) || runtimeErr.Code != "RESUME_ALREADY_CLAIMED" || runtimeErr.Retryable {
		t.Fatalf("claim-held error=%v runtime_error=%#v", err, runtimeErr)
	}
	snapshot, _ := memory.Run(runReq.RunID)
	if snapshot.Status != RunStatusResuming || snapshot.ResumeAttemptID != "attempt_existing" {
		t.Fatalf("claim holder was changed: %#v", snapshot)
	}
	if runtime.ResumeCount() != 0 {
		t.Fatal("rejected attempt reached runtime adapter")
	}
}

func TestRuntimeServiceActivateUnknownCommitDoesNotBypassFencing(t *testing.T) {
	memory := NewInMemoryStateManager()
	runReq := testRunRequest()
	prepareWaitingRun(t, memory, runReq)
	state := &committedActivateErrorState{InMemoryStateManager: memory}
	runtime := &countingResumeRuntime{MockRuntime: *NewMockRuntime()}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.IDs = fixedIDGenerator{}

	_, err := service.Resume(context.Background(), resumeClaimRequest(runReq))
	var runtimeErr *RuntimeError
	if !errors.As(err, &runtimeErr) || runtimeErr.Code != "RUNTIME_STATE_WRITE_FAILED" || !runtimeErr.Retryable {
		t.Fatalf("unexpected activate error=%v runtime_error=%#v", err, runtimeErr)
	}
	snapshot, _ := memory.Run(runReq.RunID)
	if snapshot.Status != RunStatusRunning {
		t.Fatalf("unknown commit was overwritten by unfenced FailRun: %#v", snapshot)
	}
	if runtime.ResumeCount() != 0 {
		t.Fatal("runtime adapter must not run after uncertain activation result")
	}
}

func resumeClaimRequest(req RunRequest) ResumeRequest {
	return ResumeRequest{
		SessionID:        req.SessionID,
		RunID:            req.RunID,
		Definition:       req.Definition,
		CheckpointID:     "checkpoint_1",
		ControlRequestID: "control_1",
		ResumeToken:      "resume_token_1",
		Trace:            req.Trace,
	}
}

func resumeAcceptedEvent(runID, eventID string) observability.AgentEvent {
	return observability.AgentEvent{EventID: eventID, EventType: EventResumeAccepted, RunID: runID}
}

func resumeFailedEvent(runID, eventID string) observability.AgentEvent {
	return observability.AgentEvent{EventID: eventID, EventType: EventResumeFailed, RunID: runID}
}

type committedActivateErrorState struct {
	*InMemoryStateManager
}

func (s *committedActivateErrorState) ActivateResume(ctx context.Context, runID, attemptID string) error {
	if err := s.InMemoryStateManager.ActivateResume(ctx, runID, attemptID); err != nil {
		return err
	}
	return errors.New("activation result unknown after commit")
}
