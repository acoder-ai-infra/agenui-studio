package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

const (
	sqliteResumeRunID        = "run_resume"
	sqliteResumeSessionID    = "session_resume"
	sqliteResumeCheckpointID = "checkpoint_resume"
	sqliteResumeControlID    = "control_resume"
	sqliteResumeTokenHash    = "sha256:resume-token"
)

type sqliteResumeFixture struct {
	backend *Backend
	store   *resumeStore
}

func TestSQLiteRuntimeBindingCASAcrossBackends(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "runtime-binding-cas.db")
	first, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err := first.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	ctx := sqliteResumeTenantCtx()
	expected := json.RawMessage(`{"schema_version":"base"}`)
	if err := first.RunStore().Create(ctx, &storage.Run{
		RunID: "run_binding_cas", SessionID: "session_binding", TenantID: "tenant_resume",
		Status: storage.RunStatusRunning, RuntimeBinding: expected,
	}); err != nil {
		t.Fatal(err)
	}
	replacements := []json.RawMessage{
		json.RawMessage(`{"schema_version":"winner_a"}`),
		json.RawMessage(`{"schema_version":"winner_b"}`),
	}
	type result struct {
		swapped bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for index, backend := range []*Backend{first, second} {
		index, backend := index, backend
		go func() {
			<-start
			swapped, err := backend.RunStore().CompareAndSetRuntimeBinding(ctx, "run_binding_cas", expected, replacements[index])
			results <- result{swapped: swapped, err: err}
		}()
	}
	close(start)
	winners := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.swapped {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("CAS winners=%d, want 1", winners)
	}
	run, err := first.RunStore().Get(ctx, "run_binding_cas")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(run.RuntimeBinding, replacements[0]) && !bytes.Equal(run.RuntimeBinding, replacements[1]) {
		t.Fatalf("unexpected persisted binding: %s", run.RuntimeBinding)
	}
}

func newSQLiteResumeFixture(t *testing.T) sqliteResumeFixture {
	t.Helper()
	ctx := sqliteResumeTenantCtx()
	backend, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	if err := backend.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := backend.RunStore().Create(ctx, &storage.Run{
		RunID:     sqliteResumeRunID,
		SessionID: sqliteResumeSessionID,
		TenantID:  "tenant_resume",
		Status:    storage.RunStatusWaitingControl,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := backend.ControlRequestStore().Create(ctx, &storage.ControlRequest{
		RequestID:       sqliteResumeControlID,
		RunID:           sqliteResumeRunID,
		TenantID:        "tenant_resume",
		CheckpointID:    sqliteResumeCheckpointID,
		Type:            "ask_user",
		Status:          "answered",
		ResumeTokenHash: sqliteResumeTokenHash,
	}); err != nil {
		t.Fatalf("create control: %v", err)
	}
	store := &resumeStore{
		db:     backend.db,
		events: &eventStore{db: backend.db},
	}
	return sqliteResumeFixture{backend: backend, store: store}
}

func sqliteResumeTenantCtx() context.Context {
	return observability.WithTraceContext(context.Background(), observability.TraceContext{
		TenantID: "tenant_resume",
		TraceID:  "trace_resume",
	})
}

func sqliteResumeClaim(attemptID, eventID string) storage.ResumeClaimCommand {
	return storage.ResumeClaimCommand{
		RunID:            sqliteResumeRunID,
		SessionID:        sqliteResumeSessionID,
		CheckpointID:     sqliteResumeCheckpointID,
		ControlRequestID: sqliteResumeControlID,
		ResumeTokenHash:  sqliteResumeTokenHash,
		AttemptID:        attemptID,
		Event:            sqliteResumeEvent(observability.EventResumeAccepted, attemptID, eventID),
	}
}

func sqliteResumeFailure(attemptID, eventID string, retryable bool) storage.ResumeFailureCommand {
	return storage.ResumeFailureCommand{
		RunID:        sqliteResumeRunID,
		AttemptID:    attemptID,
		Event:        sqliteResumeEvent(observability.EventResumeFailed, attemptID, eventID),
		ErrorCode:    "RESUME_FAILED",
		ErrorMessage: "resume failed",
		Retryable:    retryable,
	}
}

func sqliteResumeEvent(eventType observability.EventType, attemptID, eventID string) observability.AgentEvent {
	payload, _ := json.Marshal(map[string]string{"attempt_id": attemptID})
	return observability.AgentEvent{
		EventID:   eventID,
		RunID:     sqliteResumeRunID,
		EventType: eventType,
		Payload:   payload,
	}
}

func TestSQLiteResumeStoreWaitPersistsBindingAndIsIdempotent(t *testing.T) {
	ctx := sqliteResumeTenantCtx()
	backend, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	if err := backend.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := backend.RunStore().Create(ctx, &storage.Run{RunID: sqliteResumeRunID, SessionID: sqliteResumeSessionID, TenantID: "tenant_resume", Status: storage.RunStatusRunning}); err != nil {
		t.Fatal(err)
	}
	if err := backend.CheckpointStore().Create(ctx, &storage.CheckpointMeta{CheckpointID: sqliteResumeCheckpointID, RunID: sqliteResumeRunID, TenantID: "tenant_resume", Type: "run", StateRef: "artifact://checkpoint"}); err != nil {
		t.Fatal(err)
	}
	cmd := storage.ResumeWaitCommand{
		Control: &storage.ControlRequest{RequestID: sqliteResumeControlID, RunID: sqliteResumeRunID, TenantID: "tenant_resume", CheckpointID: sqliteResumeCheckpointID, Type: "ask_user", Status: "pending", ResumeTokenHash: sqliteResumeTokenHash, SchemaVersion: storage.ControlRequestSchemaVersion},
		Event:   observability.AgentEvent{EventID: "event_wait", RunID: sqliteResumeRunID, EventType: observability.EventControlRequestCreated},
	}
	if err := backend.ResumeStore().Wait(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	if err := backend.ResumeStore().Wait(ctx, cmd); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	run, err := backend.RunStore().Get(ctx, sqliteResumeRunID)
	if err != nil || run.Status != storage.RunStatusWaitingControl {
		t.Fatalf("run=%#v err=%v", run, err)
	}
	events, err := backend.EventStore().Query(ctx, storage.EventQuery{RunID: sqliteResumeRunID, Limit: 10})
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%#v err=%v", events, err)
	}
}

func TestSQLiteResumeStoreOtherOwnerIsHeldUntilExplicitRelease(t *testing.T) {
	fixture := newSQLiteResumeFixture(t)
	ctx := sqliteResumeTenantCtx()
	if err := fixture.store.Claim(ctx, sqliteResumeClaim("attempt_owner", "event_claim_owner")); err != nil {
		t.Fatalf("owner claim: %v", err)
	}
	if err := fixture.store.Claim(ctx, sqliteResumeClaim("attempt_other", "event_claim_other")); !storage.IsErrorCode(err, storage.ErrResumeClaimHeld) {
		t.Fatalf("other owner claim: want resume_claim_held, got %v", err)
	}

	otherFailure := sqliteResumeFailure("attempt_other", "event_failure_other", true)
	if err := fixture.store.Fail(ctx, otherFailure); !storage.IsErrorCode(err, storage.ErrResumeClaimLost) {
		t.Fatalf("other owner fail: want resume_claim_lost, got %v", err)
	}
	if err := fixture.store.Activate(ctx, storage.ResumeActivationCommand{
		RunID: sqliteResumeRunID, AttemptID: "attempt_other",
	}); !storage.IsErrorCode(err, storage.ErrResumeClaimLost) {
		t.Fatalf("other owner activate: want resume_claim_lost, got %v", err)
	}
	run, err := fixture.backend.RunStore().Get(ctx, sqliteResumeRunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != storage.RunStatusResuming || run.ResumeAttemptID != "attempt_owner" {
		t.Fatalf("owned run = %+v", run)
	}
	events, err := fixture.backend.EventStore().Query(ctx, storage.EventQuery{RunID: sqliteResumeRunID})
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
		RunID: sqliteResumeRunID, AttemptID: "attempt_owner",
	}); err != nil {
		t.Fatalf("owner activate: %v", err)
	}
}

func TestSQLiteResumeStoreRetryableFailureClearsTerminalState(t *testing.T) {
	fixture := newSQLiteResumeFixture(t)
	ctx := sqliteResumeTenantCtx()
	if _, err := fixture.backend.db.ExecContext(ctx, `UPDATE runs
		SET ended_at=?,error_code=?,error_message=? WHERE run_id=?`,
		tsVal(time.Now().Add(-time.Hour)), "STALE_ERROR", "stale error", sqliteResumeRunID); err != nil {
		t.Fatalf("seed stale terminal state: %v", err)
	}
	if err := fixture.store.Claim(ctx, sqliteResumeClaim("attempt_1", "event_claim_1")); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := fixture.store.Fail(ctx, sqliteResumeFailure("attempt_1", "event_failure_1", true)); err != nil {
		t.Fatalf("retryable fail: %v", err)
	}
	run, err := fixture.backend.RunStore().Get(ctx, sqliteResumeRunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != storage.RunStatusWaitingControl || run.ResumeAttemptID != "" ||
		!run.EndedAt.IsZero() || run.ErrorCode != "" || run.ErrorMessage != "" {
		t.Fatalf("released run = %+v", run)
	}
}

func TestSQLiteResumeStoreTerminalFailureOverwritesPreviousError(t *testing.T) {
	fixture := newSQLiteResumeFixture(t)
	ctx := sqliteResumeTenantCtx()
	oldEndedAt := time.Now().Add(-time.Hour)
	if _, err := fixture.backend.db.ExecContext(ctx, `UPDATE runs
		SET ended_at=?,error_code=?,error_message=? WHERE run_id=?`,
		tsVal(oldEndedAt), "STALE_ERROR", "stale error", sqliteResumeRunID); err != nil {
		t.Fatalf("seed stale terminal state: %v", err)
	}
	if err := fixture.store.Claim(ctx, sqliteResumeClaim("attempt_1", "event_claim_1")); err != nil {
		t.Fatalf("claim: %v", err)
	}
	failure := sqliteResumeFailure("attempt_1", "event_failure_1", false)
	failure.ErrorCode = "CURRENT_ERROR"
	failure.ErrorMessage = "current error"
	if err := fixture.store.Fail(ctx, failure); err != nil {
		t.Fatalf("terminal fail: %v", err)
	}
	run, err := fixture.backend.RunStore().Get(ctx, sqliteResumeRunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != storage.RunStatusFailed || run.ResumeAttemptID != "" ||
		run.EndedAt.IsZero() || run.EndedAt.Equal(oldEndedAt) ||
		run.ErrorCode != failure.ErrorCode || run.ErrorMessage != failure.ErrorMessage {
		t.Fatalf("failed run = %+v", run)
	}
}

func TestSQLiteResumeStoreClaimRollsBackWhenEventAppendFails(t *testing.T) {
	fixture := newSQLiteResumeFixture(t)
	ctx := sqliteResumeTenantCtx()
	if _, err := fixture.backend.db.Exec(`CREATE TRIGGER reject_resume_event
		BEFORE INSERT ON agent_events
		WHEN NEW.event_type = 'resume_accepted'
		BEGIN
			SELECT RAISE(ABORT, 'reject resume event');
		END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if err := fixture.store.Claim(ctx, sqliteResumeClaim("attempt_1", "event_claim_1")); err == nil {
		t.Fatal("claim should fail when resume event is rejected")
	}
	run, err := fixture.backend.RunStore().Get(ctx, sqliteResumeRunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != storage.RunStatusWaitingControl || run.ResumeAttemptID != "" || run.Version != 1 {
		t.Fatalf("run update was not rolled back: %+v", run)
	}
	events, err := fixture.backend.EventStore().Query(ctx, storage.EventQuery{RunID: sqliteResumeRunID})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("event should not be persisted: %+v", events)
	}
}

func TestSQLiteResumeStoreWrongAttemptCannotReuseFailureEventID(t *testing.T) {
	fixture := newSQLiteResumeFixture(t)
	ctx := sqliteResumeTenantCtx()
	if err := fixture.store.Claim(ctx, sqliteResumeClaim("attempt_old", "event_claim_old")); err != nil {
		t.Fatalf("old claim: %v", err)
	}
	if err := fixture.store.Fail(ctx, sqliteResumeFailure("attempt_old", "event_failure_shared", true)); err != nil {
		t.Fatalf("old failure: %v", err)
	}
	if err := fixture.store.Claim(ctx, sqliteResumeClaim("attempt_new", "event_claim_new")); err != nil {
		t.Fatalf("new claim: %v", err)
	}
	intruder := sqliteResumeFailure("attempt_intruder", "event_failure_shared", true)
	if err := fixture.store.Fail(ctx, intruder); err == nil {
		t.Fatal("wrong attempt reused EventID should not be treated as an idempotent success")
	}
	run, err := fixture.backend.RunStore().Get(ctx, sqliteResumeRunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != storage.RunStatusResuming || run.ResumeAttemptID != "attempt_new" {
		t.Fatalf("wrong attempt changed current owner: %+v", run)
	}
}

func TestSQLiteResumeStoreRejectsModifiedPayloadReplay(t *testing.T) {
	fixture := newSQLiteResumeFixture(t)
	ctx := sqliteResumeTenantCtx()
	claim := sqliteResumeClaim("attempt_1", "event_claim")
	if err := fixture.store.Claim(ctx, claim); err != nil {
		t.Fatal(err)
	}
	claim.Event.Payload, _ = json.Marshal(map[string]string{"attempt_id": "attempt_1", "tampered": "true"})
	if err := fixture.store.Claim(ctx, claim); !storage.IsErrorCode(err, storage.ErrConflict) {
		t.Fatalf("modified replay want conflict, got %v", err)
	}
}

func TestSQLiteResumeEventStableIdentityRoundTrips(t *testing.T) {
	fixture := newSQLiteResumeFixture(t)
	ctx := sqliteResumeTenantCtx()
	claim := sqliteResumeClaim("attempt_1", "event_claim")
	claim.Event.SessionID = sqliteResumeSessionID
	claim.Event.AgentID = "agent_1"
	claim.Event.AgentType = "single_agent"
	claim.Event.Runtime = "native"
	claim.Event.TraceID = "trace_1"
	claim.Event.SpanID = "span_1"
	claim.Event.ParentSpanID = "parent_1"
	if err := fixture.store.Claim(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Claim(ctx, claim); err != nil {
		t.Fatalf("stable replay: %v", err)
	}
	events, err := fixture.backend.EventStore().Query(ctx, storage.EventQuery{RunID: sqliteResumeRunID})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].ParentSpanID != "parent_1" || events[0].AgentType != "single_agent" || events[0].Runtime != "native" {
		t.Fatalf("stable identity was not persisted: %+v", events)
	}
}

func TestSQLiteResumeStoreIdempotentReplayStillEnforcesTenant(t *testing.T) {
	fixture := newSQLiteResumeFixture(t)
	ctx := sqliteResumeTenantCtx()
	if err := fixture.store.Claim(ctx, sqliteResumeClaim("attempt_1", "event_claim")); err != nil {
		t.Fatal(err)
	}
	failure := sqliteResumeFailure("attempt_1", "event_failure", true)
	if err := fixture.store.Fail(ctx, failure); err != nil {
		t.Fatal(err)
	}
	otherTenant := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "other_tenant"})
	if err := fixture.store.Fail(otherTenant, failure); !storage.IsErrorCode(err, storage.ErrTenantMismatch) {
		t.Fatalf("cross-tenant replay want tenant_mismatch, got %v", err)
	}
}

func TestSQLiteMigrateAddsResumeOwnerToLegacyRuns(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "legacy-resume.db")
	legacy, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE runs (
		run_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, turn_id TEXT, parent_run_id TEXT,
		tenant_id TEXT NOT NULL, agent_id TEXT, runtime TEXT, status TEXT NOT NULL,
		trace_id TEXT, config_snapshot_ref TEXT, context_snapshot_ref TEXT, agent_binding_id TEXT,
		started_at INTEGER, ended_at INTEGER, error_code TEXT, error_message TEXT,
		version INTEGER NOT NULL DEFAULT 1
	)`); err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE agent_events (
		event_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, sequence INTEGER NOT NULL, tenant_id TEXT,
		session_id TEXT, step_id TEXT, agent_id TEXT, trace_id TEXT, span_id TEXT,
		event_type TEXT NOT NULL, visibility TEXT NOT NULL, schema_version TEXT NOT NULL,
		idempotency_key TEXT, payload TEXT, payload_preview TEXT, payload_ref TEXT,
		usage TEXT, debug_ref TEXT, error TEXT, created_at INTEGER NOT NULL,
		UNIQUE(run_id, sequence), UNIQUE(tenant_id, idempotency_key)
	)`); err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	backend, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	if err := backend.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := backend.db.Query(`PRAGMA table_info(runs)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		found[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"resume_attempt_id"} {
		if !found[column] {
			t.Fatalf("%s column was not added", column)
		}
	}
	eventRows, err := backend.db.Query(`PRAGMA table_info(agent_events)`)
	if err != nil {
		t.Fatal(err)
	}
	defer eventRows.Close()
	eventColumns := map[string]bool{}
	for eventRows.Next() {
		var cid int
		var name, typ string
		var notNull, primaryKey int
		var defaultValue any
		if err := eventRows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		eventColumns[name] = true
	}
	for _, column := range []string{"parent_span_id", "agent_type", "runtime"} {
		if !eventColumns[column] {
			t.Fatalf("%s event column was not added", column)
		}
	}
}

func TestSQLiteRunCASClearsResumeClaimOnEveryExit(t *testing.T) {
	for _, target := range []storage.RunStatus{
		storage.RunStatusRunning,
		storage.RunStatusWaitingControl,
		storage.RunStatusCompleted,
		storage.RunStatusFailed,
		storage.RunStatusCancelled,
		storage.RunStatusExpired,
	} {
		t.Run(string(target), func(t *testing.T) {
			fixture := newSQLiteResumeFixture(t)
			ctx := sqliteResumeTenantCtx()
			if err := fixture.store.Claim(ctx, sqliteResumeClaim("attempt", "event_claim")); err != nil {
				t.Fatal(err)
			}
			run, err := fixture.backend.RunStore().CompareAndSetStatus(ctx, sqliteResumeRunID, storage.RunStatusResuming, target, storage.RunMutation{})
			if err != nil {
				t.Fatal(err)
			}
			if run.ResumeAttemptID != "" {
				t.Fatalf("claim leaked after resuming -> %s: %+v", target, run)
			}
		})
	}
}
