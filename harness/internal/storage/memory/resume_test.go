package memory

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

const (
	resumeTestRunID        = "run_resume"
	resumeTestSessionID    = "session_resume"
	resumeTestCheckpointID = "checkpoint_resume"
	resumeTestControlID    = "control_resume"
	resumeTestTokenHash    = "sha256:resume-token"
)

type memoryResumeFixture struct {
	backend *Backend
	store   *resumeStore
}

func newMemoryResumeFixture(t *testing.T) memoryResumeFixture {
	t.Helper()
	ctx := tenantCtx("tenant_resume")
	backend := New()
	if err := backend.runs.Create(ctx, &storage.Run{
		RunID:     resumeTestRunID,
		SessionID: resumeTestSessionID,
		TenantID:  "tenant_resume",
		Status:    storage.RunStatusWaitingControl,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := backend.controls.Create(ctx, &storage.ControlRequest{
		RequestID:       resumeTestControlID,
		RunID:           resumeTestRunID,
		TenantID:        "tenant_resume",
		CheckpointID:    resumeTestCheckpointID,
		Type:            "ask_user",
		Status:          "answered",
		ResumeTokenHash: resumeTestTokenHash,
	}); err != nil {
		t.Fatalf("create control: %v", err)
	}
	return memoryResumeFixture{backend: backend, store: backend.resumes}
}

func memoryResumeClaim(attemptID, eventID string) storage.ResumeClaimCommand {
	return storage.ResumeClaimCommand{
		RunID:            resumeTestRunID,
		SessionID:        resumeTestSessionID,
		CheckpointID:     resumeTestCheckpointID,
		ControlRequestID: resumeTestControlID,
		ResumeTokenHash:  resumeTestTokenHash,
		AttemptID:        attemptID,
		Event:            memoryResumeEvent(observability.EventResumeAccepted, attemptID, eventID),
	}
}

func memoryResumeFailure(attemptID, eventID string, retryable bool) storage.ResumeFailureCommand {
	return storage.ResumeFailureCommand{
		RunID:        resumeTestRunID,
		AttemptID:    attemptID,
		Event:        memoryResumeEvent(observability.EventResumeFailed, attemptID, eventID),
		ErrorCode:    "RESUME_FAILED",
		ErrorMessage: "resume failed",
		Retryable:    retryable,
	}
}

func memoryResumeEvent(eventType observability.EventType, attemptID, eventID string) observability.AgentEvent {
	payload, _ := json.Marshal(map[string]string{"attempt_id": attemptID})
	return observability.AgentEvent{
		EventID:   eventID,
		RunID:     resumeTestRunID,
		EventType: eventType,
		Payload:   payload,
	}
}

func TestResumeStoreWaitPersistsBindingAndIsIdempotent(t *testing.T) {
	ctx := tenantCtx("tenant_resume")
	backend := New()
	if err := backend.runs.Create(ctx, &storage.Run{RunID: resumeTestRunID, SessionID: resumeTestSessionID, TenantID: "tenant_resume", Status: storage.RunStatusRunning}); err != nil {
		t.Fatal(err)
	}
	if err := backend.ckpts.Create(ctx, &storage.CheckpointMeta{CheckpointID: resumeTestCheckpointID, RunID: resumeTestRunID, TenantID: "tenant_resume", Type: "run", StateRef: "artifact://checkpoint"}); err != nil {
		t.Fatal(err)
	}
	cmd := storage.ResumeWaitCommand{
		Control: &storage.ControlRequest{RequestID: resumeTestControlID, RunID: resumeTestRunID, TenantID: "tenant_resume", CheckpointID: resumeTestCheckpointID, Type: "ask_user", Status: "pending", ResumeTokenHash: resumeTestTokenHash, SchemaVersion: storage.ControlRequestSchemaVersion},
		Event:   observability.AgentEvent{EventID: "event_wait", RunID: resumeTestRunID, EventType: observability.EventControlRequestCreated},
	}
	if err := backend.resumes.Wait(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	if err := backend.resumes.Wait(ctx, cmd); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	run, err := backend.runs.Get(ctx, resumeTestRunID)
	if err != nil || run.Status != storage.RunStatusWaitingControl {
		t.Fatalf("run=%#v err=%v", run, err)
	}
	events, err := backend.events.Query(ctx, storage.EventQuery{RunID: resumeTestRunID, Limit: 10})
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%#v err=%v", events, err)
	}
}

func TestResumeStoreConcurrentClaimHasSingleOwner(t *testing.T) {
	fixture := newMemoryResumeFixture(t)
	ctx := tenantCtx("tenant_resume")
	start := make(chan struct{})
	type claimResult struct {
		attemptID string
		err       error
	}
	results := make(chan claimResult, 2)
	var wg sync.WaitGroup
	for _, attemptID := range []string{"attempt_a", "attempt_b"} {
		attemptID := attemptID
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- claimResult{
				attemptID: attemptID,
				err:       fixture.store.Claim(ctx, memoryResumeClaim(attemptID, "event_"+attemptID)),
			}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var winner string
	held := 0
	for result := range results {
		switch {
		case result.err == nil:
			winner = result.attemptID
		case storage.IsErrorCode(result.err, storage.ErrResumeClaimHeld):
			held++
		default:
			t.Fatalf("claim %s: %v", result.attemptID, result.err)
		}
	}
	if winner == "" || held != 1 {
		t.Fatalf("want one winner and one held claim, winner=%q held=%d", winner, held)
	}
	run, err := fixture.backend.runs.Get(ctx, resumeTestRunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != storage.RunStatusResuming || run.ResumeAttemptID != winner {
		t.Fatalf("claimed run = %+v", run)
	}
	events, err := fixture.backend.events.Query(ctx, storage.EventQuery{RunID: resumeTestRunID})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventType != observability.EventResumeAccepted {
		t.Fatalf("accepted events = %+v", events)
	}
}

func TestResumeStoreSameAttemptAndEventIsIdempotent(t *testing.T) {
	fixture := newMemoryResumeFixture(t)
	ctx := tenantCtx("tenant_resume")
	claim := memoryResumeClaim("attempt_owner", "event_claim")
	if err := fixture.store.Claim(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Claim(ctx, claim); err != nil {
		t.Fatalf("idempotent claim: %v", err)
	}
	run, err := fixture.backend.runs.Get(ctx, resumeTestRunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Version != 2 || run.ResumeAttemptID != "attempt_owner" {
		t.Fatalf("idempotent claim changed run: %+v", run)
	}
	events, err := fixture.backend.events.Query(ctx, storage.EventQuery{RunID: resumeTestRunID})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("idempotent claim duplicated event: %+v", events)
	}
}

func TestResumeStoreOtherOwnerIsHeldUntilExplicitRelease(t *testing.T) {
	fixture := newMemoryResumeFixture(t)
	ctx := tenantCtx("tenant_resume")
	if err := fixture.store.Claim(ctx, memoryResumeClaim("attempt_owner", "event_claim_owner")); err != nil {
		t.Fatalf("owner claim: %v", err)
	}
	if err := fixture.store.Claim(ctx, memoryResumeClaim("attempt_other", "event_claim_other")); !storage.IsErrorCode(err, storage.ErrResumeClaimHeld) {
		t.Fatalf("other owner claim: want resume_claim_held, got %v", err)
	}

	otherFailure := memoryResumeFailure("attempt_other", "event_failure_other", true)
	if err := fixture.store.Fail(ctx, otherFailure); !storage.IsErrorCode(err, storage.ErrResumeClaimLost) {
		t.Fatalf("other owner fail: want resume_claim_lost, got %v", err)
	}
	if err := fixture.store.Activate(ctx, storage.ResumeActivationCommand{
		RunID: resumeTestRunID, AttemptID: "attempt_other",
	}); !storage.IsErrorCode(err, storage.ErrResumeClaimLost) {
		t.Fatalf("other owner activate: want resume_claim_lost, got %v", err)
	}

	run, err := fixture.backend.runs.Get(ctx, resumeTestRunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != storage.RunStatusResuming || run.ResumeAttemptID != "attempt_owner" {
		t.Fatalf("owned run = %+v", run)
	}
	events, err := fixture.backend.events.Query(ctx, storage.EventQuery{RunID: resumeTestRunID})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("other owner must not append a fake event: %+v", events)
	}
	for _, event := range events {
		if event.EventID == otherFailure.Event.EventID || event.EventType != observability.EventResumeAccepted {
			t.Fatalf("unexpected event after fencing: %+v", event)
		}
	}
	if err := fixture.store.Activate(ctx, storage.ResumeActivationCommand{
		RunID: resumeTestRunID, AttemptID: "attempt_owner",
	}); err != nil {
		t.Fatalf("owner activate: %v", err)
	}
}

func TestResumeStoreRetryableFailureClearsTerminalStateAndReleasesClaim(t *testing.T) {
	fixture := newMemoryResumeFixture(t)
	ctx := tenantCtx("tenant_resume")
	fixture.backend.runs.mu.Lock()
	run := fixture.backend.runs.byID[resumeTestRunID]
	run.EndedAt = time.Now().Add(-time.Hour)
	run.ErrorCode = "STALE_ERROR"
	run.ErrorMessage = "stale error"
	fixture.backend.runs.mu.Unlock()

	if err := fixture.store.Claim(ctx, memoryResumeClaim("attempt_1", "event_claim_1")); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := fixture.store.Fail(ctx, memoryResumeFailure("attempt_1", "event_failure_1", true)); err != nil {
		t.Fatalf("retryable fail: %v", err)
	}
	run, err := fixture.backend.runs.Get(ctx, resumeTestRunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != storage.RunStatusWaitingControl || run.ResumeAttemptID != "" ||
		!run.EndedAt.IsZero() || run.ErrorCode != "" || run.ErrorMessage != "" {
		t.Fatalf("released run = %+v", run)
	}
	if err := fixture.store.Claim(ctx, memoryResumeClaim("attempt_2", "event_claim_2")); err != nil {
		t.Fatalf("claim after retryable failure: %v", err)
	}
}

func TestResumeStoreTerminalFailureOverwritesPreviousError(t *testing.T) {
	fixture := newMemoryResumeFixture(t)
	ctx := tenantCtx("tenant_resume")
	fixture.backend.runs.mu.Lock()
	run := fixture.backend.runs.byID[resumeTestRunID]
	oldEndedAt := time.Now().Add(-time.Hour)
	run.EndedAt = oldEndedAt
	run.ErrorCode = "STALE_ERROR"
	run.ErrorMessage = "stale error"
	fixture.backend.runs.mu.Unlock()

	if err := fixture.store.Claim(ctx, memoryResumeClaim("attempt_1", "event_claim_1")); err != nil {
		t.Fatalf("claim: %v", err)
	}
	failure := memoryResumeFailure("attempt_1", "event_failure_1", false)
	failure.ErrorCode = "CURRENT_ERROR"
	failure.ErrorMessage = "current error"
	if err := fixture.store.Fail(ctx, failure); err != nil {
		t.Fatalf("terminal fail: %v", err)
	}
	run, err := fixture.backend.runs.Get(ctx, resumeTestRunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != storage.RunStatusFailed || run.ResumeAttemptID != "" ||
		run.EndedAt.IsZero() || run.EndedAt.Equal(oldEndedAt) ||
		run.ErrorCode != failure.ErrorCode || run.ErrorMessage != failure.ErrorMessage {
		t.Fatalf("failed run = %+v", run)
	}
}

func TestResumeStoreWrongAttemptCannotReuseFailureEventID(t *testing.T) {
	fixture := newMemoryResumeFixture(t)
	ctx := tenantCtx("tenant_resume")
	if err := fixture.store.Claim(ctx, memoryResumeClaim("attempt_old", "event_claim_old")); err != nil {
		t.Fatalf("old claim: %v", err)
	}
	if err := fixture.store.Fail(ctx, memoryResumeFailure("attempt_old", "event_failure_shared", true)); err != nil {
		t.Fatalf("old failure: %v", err)
	}
	if err := fixture.store.Claim(ctx, memoryResumeClaim("attempt_new", "event_claim_new")); err != nil {
		t.Fatalf("new claim: %v", err)
	}

	// EventID 已存在不能跳过 fencing；事件属于 attempt_old，intruder 无权重放。
	intruder := memoryResumeFailure("attempt_intruder", "event_failure_shared", true)
	if err := fixture.store.Fail(ctx, intruder); err == nil {
		t.Fatal("wrong attempt reused EventID should not be treated as an idempotent success")
	}
	run, err := fixture.backend.runs.Get(ctx, resumeTestRunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != storage.RunStatusResuming || run.ResumeAttemptID != "attempt_new" {
		t.Fatalf("wrong attempt changed current owner: %+v", run)
	}
}

func TestResumeStoreRejectsModifiedPayloadReplay(t *testing.T) {
	fixture := newMemoryResumeFixture(t)
	ctx := tenantCtx("tenant_resume")
	claim := memoryResumeClaim("attempt_1", "event_claim")
	if err := fixture.store.Claim(ctx, claim); err != nil {
		t.Fatal(err)
	}
	claim.Event.Payload, _ = json.Marshal(map[string]string{"attempt_id": "attempt_1", "tampered": "true"})
	if err := fixture.store.Claim(ctx, claim); !storage.IsErrorCode(err, storage.ErrConflict) {
		t.Fatalf("modified replay want conflict, got %v", err)
	}
}

func TestResumeStoreIdempotentReplayStillEnforcesTenant(t *testing.T) {
	fixture := newMemoryResumeFixture(t)
	ctx := tenantCtx("tenant_resume")
	if err := fixture.store.Claim(ctx, memoryResumeClaim("attempt_1", "event_claim")); err != nil {
		t.Fatal(err)
	}
	failure := memoryResumeFailure("attempt_1", "event_failure", true)
	if err := fixture.store.Fail(ctx, failure); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Fail(tenantCtx("other_tenant"), failure); !storage.IsErrorCode(err, storage.ErrTenantMismatch) {
		t.Fatalf("cross-tenant replay want tenant_mismatch, got %v", err)
	}
}

func TestRunCASClearsResumeClaimOnEveryExit(t *testing.T) {
	for _, target := range []storage.RunStatus{
		storage.RunStatusRunning,
		storage.RunStatusWaitingControl,
		storage.RunStatusCompleted,
		storage.RunStatusFailed,
		storage.RunStatusCancelled,
		storage.RunStatusExpired,
	} {
		t.Run(string(target), func(t *testing.T) {
			fixture := newMemoryResumeFixture(t)
			ctx := tenantCtx("tenant_resume")
			if err := fixture.store.Claim(ctx, memoryResumeClaim("attempt", "event_claim")); err != nil {
				t.Fatal(err)
			}
			run, err := fixture.backend.runs.CompareAndSetStatus(ctx, resumeTestRunID, storage.RunStatusResuming, target, storage.RunMutation{})
			if err != nil {
				t.Fatal(err)
			}
			if run.ResumeAttemptID != "" {
				t.Fatalf("claim leaked after resuming -> %s: %+v", target, run)
			}
		})
	}
}
