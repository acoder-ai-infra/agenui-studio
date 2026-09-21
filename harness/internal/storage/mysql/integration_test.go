//go:build mysql_integration

// Live MySQL integration test for the storage prototype.
//
// It is excluded from default builds/CI. To run it:
//
//	HARNESS_MYSQL_TEST_HOST=127.0.0.1 \
//	HARNESS_MYSQL_TEST_PORT=3306 \
//	HARNESS_MYSQL_TEST_USER=root \
//	HARNESS_MYSQL_TEST_PASSWORD='' \
//	HARNESS_MYSQL_TEST_DATABASE=harness_test \
//	  go test -tags mysql_integration ./internal/storage/mysql/
//
// It verifies the EventStore invariants (E-003 idempotency, E-004 concurrent
// sequence) and RunStore CAS (R-001) against a real transactional MySQL.
package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	frameworkmysql "github.com/AGenUI/agenui-studio/harness/framework/mysql"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

func TestMySQLIntegrationConfig(t *testing.T) {
	t.Run("maps structured environment", func(t *testing.T) {
		cfg, configured, err := ledgerMySQLConfig(mapLookup(map[string]string{
			"HARNESS_MYSQL_TEST_HOST":     "mysql.test",
			"HARNESS_MYSQL_TEST_PORT":     "3307",
			"HARNESS_MYSQL_TEST_USER":     "ledger_user",
			"HARNESS_MYSQL_TEST_PASSWORD": "fixture-secret",
			"HARNESS_MYSQL_TEST_DATABASE": "harness_test",
		}))
		if err != nil {
			t.Fatalf("ledgerMySQLConfig() error = %v", err)
		}
		if !configured {
			t.Fatal("ledgerMySQLConfig() configured = false, want true")
		}
		if cfg.Name != "ledger-test" || cfg.DBName != "harness_test" || cfg.Host != "mysql.test" || cfg.Port != 3307 {
			t.Fatalf("fixture identity/address was not preserved: name=%q database=%q host=%q port=%d", cfg.Name, cfg.DBName, cfg.Host, cfg.Port)
		}
		if cfg.User != "ledger_user" || cfg.Password != "fixture-secret" {
			t.Fatal("fixture credentials were not preserved")
		}
		if cfg.Charset != "utf8mb4" || cfg.MaxOpenConns != 4 || cfg.MaxIdleConns != 4 {
			t.Fatalf("fixture defaults = (charset %q, maxOpen %d, maxIdle %d), want (utf8mb4, 4, 4)", cfg.Charset, cfg.MaxOpenConns, cfg.MaxIdleConns)
		}
	})

	t.Run("accepts explicitly empty password", func(t *testing.T) {
		cfg, configured, err := ledgerMySQLConfig(mapLookup(map[string]string{
			"HARNESS_MYSQL_TEST_HOST":     "127.0.0.1",
			"HARNESS_MYSQL_TEST_PORT":     "3306",
			"HARNESS_MYSQL_TEST_USER":     "root",
			"HARNESS_MYSQL_TEST_PASSWORD": "",
			"HARNESS_MYSQL_TEST_DATABASE": "harness_test",
		}))
		if err != nil || !configured {
			t.Fatalf("ledgerMySQLConfig() = (_, %t, %v), want configured without error", configured, err)
		}
		if cfg.Password != "" {
			t.Fatal("ledgerMySQLConfig() did not preserve the explicitly empty password")
		}
	})

	t.Run("all variables unset disables fixture", func(t *testing.T) {
		_, configured, err := ledgerMySQLConfig(mapLookup(nil))
		if err != nil || configured {
			t.Fatalf("ledgerMySQLConfig() = (_, %t, %v), want (_, false, nil)", configured, err)
		}
	})

	t.Run("rejects partial and invalid configuration with a fixed redacted error", func(t *testing.T) {
		const wantError = "invalid ledger MySQL fixture configuration"
		tests := []struct {
			name string
			env  map[string]string
		}{
			{name: "partial", env: map[string]string{"HARNESS_MYSQL_TEST_PASSWORD": "fixture-secret"}},
			{name: "password unset", env: map[string]string{
				"HARNESS_MYSQL_TEST_HOST": "mysql.test", "HARNESS_MYSQL_TEST_PORT": "3307",
				"HARNESS_MYSQL_TEST_USER": "ledger_user", "HARNESS_MYSQL_TEST_DATABASE": "harness_test",
			}},
			{name: "invalid port", env: map[string]string{
				"HARNESS_MYSQL_TEST_HOST": "mysql.test", "HARNESS_MYSQL_TEST_PORT": "not-a-port",
				"HARNESS_MYSQL_TEST_USER": "ledger_user", "HARNESS_MYSQL_TEST_PASSWORD": "fixture-secret",
				"HARNESS_MYSQL_TEST_DATABASE": "harness_test",
			}},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				_, configured, err := ledgerMySQLConfig(mapLookup(test.env))
				if !configured || !errors.Is(err, errInvalidLedgerMySQLFixture) {
					t.Fatalf("ledgerMySQLConfig() = (_, %t, %v), want configured with fixed error", configured, err)
				}
				if err.Error() != wantError || strings.Contains(err.Error(), "fixture-secret") {
					t.Fatalf("ledgerMySQLConfig() error = %q, want fixed redacted error", err)
				}
			})
		}
	})
}

var errInvalidLedgerMySQLFixture = errors.New("invalid ledger MySQL fixture configuration")

func ledgerMySQLConfig(lookup func(string) (string, bool)) (frameworkmysql.Config, bool, error) {
	keys := [...]string{
		"HARNESS_MYSQL_TEST_HOST",
		"HARNESS_MYSQL_TEST_PORT",
		"HARNESS_MYSQL_TEST_USER",
		"HARNESS_MYSQL_TEST_PASSWORD",
		"HARNESS_MYSQL_TEST_DATABASE",
	}
	values := make(map[string]string, len(keys))
	present := make(map[string]bool, len(keys))
	configured := false
	for _, key := range keys {
		value, ok := lookup(key)
		values[key] = value
		present[key] = ok
		configured = configured || ok
	}
	if !configured {
		return frameworkmysql.Config{}, false, nil
	}
	for _, key := range keys {
		if !present[key] || (key != "HARNESS_MYSQL_TEST_PASSWORD" && strings.TrimSpace(values[key]) == "") {
			return frameworkmysql.Config{}, true, errInvalidLedgerMySQLFixture
		}
	}
	port, err := strconv.Atoi(strings.TrimSpace(values["HARNESS_MYSQL_TEST_PORT"]))
	if err != nil || port < 1 || port > 65535 {
		return frameworkmysql.Config{}, true, errInvalidLedgerMySQLFixture
	}

	return frameworkmysql.Config{
		Name:         "ledger-test",
		DBName:       strings.TrimSpace(values["HARNESS_MYSQL_TEST_DATABASE"]),
		User:         strings.TrimSpace(values["HARNESS_MYSQL_TEST_USER"]),
		Password:     values["HARNESS_MYSQL_TEST_PASSWORD"],
		Host:         strings.TrimSpace(values["HARNESS_MYSQL_TEST_HOST"]),
		Port:         port,
		Charset:      "utf8mb4",
		MaxOpenConns: 4,
		MaxIdleConns: 4,
	}, true, nil
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func openTestDB(t *testing.T) *Backend {
	t.Helper()

	config, configured, err := ledgerMySQLConfig(os.LookupEnv)
	if err != nil {
		t.Fatalf("MySQL integration fixture configuration is invalid: %v", err)
	}
	if !configured {
		t.Skip("HARNESS_MYSQL_TEST_HOST/PORT/USER/PASSWORD/DATABASE not set")
	}
	ctx := context.Background()
	if err := frameworkmysql.DBInit(config); err != nil {
		t.Fatalf("framework/mysql.DBInit() error = %v", err)
	}
	db := frameworkmysql.GetDB(config.Name)
	if db == nil {
		t.Fatalf("framework/mysql.GetDB(%q) returned nil", config.Name)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("MySQL integration DB Close() error = %v", err)
		}
	})

	b := New(db)
	if err := b.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return b
}

func TestMySQLResumeStoreFencesStaleOwner(t *testing.T) {
	b := openTestDB(t)
	ctx := ctxT("t1")
	stores := b.Stores()
	gen := observability.NewULIDGenerator("")
	runID := "run_" + gen.NewRunID()
	controlID := "control_" + gen.NewRunID()
	if err := stores.Runs.Create(ctx, &storage.Run{
		RunID: runID, SessionID: "session_resume", TenantID: "t1", Status: storage.RunStatusWaitingControl,
	}); err != nil {
		t.Fatal(err)
	}
	if err := stores.Controls.Create(ctx, &storage.ControlRequest{
		RequestID: controlID, RunID: runID, TenantID: "t1", CheckpointID: "checkpoint_resume",
		Type: "ask_user", Status: "answered", ResumeTokenHash: "sha256:resume-token",
	}); err != nil {
		t.Fatal(err)
	}
	resumeEvent := func(eventType observability.EventType, attemptID, eventID string) observability.AgentEvent {
		payload, _ := json.Marshal(map[string]string{"attempt_id": attemptID})
		return observability.AgentEvent{EventID: eventID, RunID: runID, EventType: eventType, Payload: payload}
	}
	claim := storage.ResumeClaimCommand{
		RunID: runID, SessionID: "session_resume", CheckpointID: "checkpoint_resume", ControlRequestID: controlID,
		ResumeTokenHash: "sha256:resume-token", AttemptID: "attempt_owner",
		Event: resumeEvent(observability.EventResumeAccepted, "attempt_owner", "event_claim_"+gen.NewRunID()),
	}
	if err := stores.Resumes.Claim(ctx, claim); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := stores.Resumes.Claim(ctx, claim); err != nil {
		t.Fatalf("idempotent claim: %v", err)
	}
	staleFailure := storage.ResumeFailureCommand{
		RunID: runID, AttemptID: "attempt_stale",
		Event:     resumeEvent(observability.EventResumeFailed, "attempt_stale", "event_failure_"+gen.NewRunID()),
		Retryable: true,
	}
	if err := stores.Resumes.Fail(ctx, staleFailure); !storage.IsErrorCode(err, storage.ErrResumeClaimLost) {
		t.Fatalf("stale failure want resume_claim_lost, got %v", err)
	}
	events, err := stores.Events.Query(ctx, storage.EventQuery{RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventType != observability.EventResumeAccepted {
		t.Fatalf("stale owner appended a false event: %+v", events)
	}
	if err := stores.Resumes.Activate(ctx, storage.ResumeActivationCommand{RunID: runID, AttemptID: "attempt_owner"}); err != nil {
		t.Fatalf("activate: %v", err)
	}
}

func TestMySQLResumeFailureReplacesTerminalFields(t *testing.T) {
	b := openTestDB(t)
	ctx := ctxT("t1")
	stores := b.Stores()
	gen := observability.NewULIDGenerator("")

	for _, test := range []struct {
		name      string
		retryable bool
	}{
		{name: "retryable_clears_stale_terminal_state", retryable: true},
		{name: "terminal_overwrites_previous_error", retryable: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			runID := "run_" + gen.NewRunID()
			controlID := "control_" + gen.NewRunID()
			attemptID := "attempt_" + gen.NewRunID()
			oldEndedAt := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
			if err := stores.Runs.Create(ctx, &storage.Run{
				RunID: runID, SessionID: "session_resume", TenantID: "t1", Status: storage.RunStatusWaitingControl,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := b.db.ExecContext(ctx, `UPDATE runs
				SET ended_at=?,error_code=?,error_message=? WHERE run_id=?`,
				oldEndedAt, "STALE_ERROR", "stale error", runID); err != nil {
				t.Fatalf("seed stale terminal state: %v", err)
			}
			if err := stores.Controls.Create(ctx, &storage.ControlRequest{
				RequestID: controlID, RunID: runID, TenantID: "t1", CheckpointID: "checkpoint_resume",
				Type: "ask_user", Status: "answered", ResumeTokenHash: "sha256:resume-token",
			}); err != nil {
				t.Fatal(err)
			}
			payload, _ := json.Marshal(map[string]string{"attempt_id": attemptID})
			if err := stores.Resumes.Claim(ctx, storage.ResumeClaimCommand{
				RunID: runID, SessionID: "session_resume", CheckpointID: "checkpoint_resume", ControlRequestID: controlID,
				ResumeTokenHash: "sha256:resume-token", AttemptID: attemptID,
				Event: observability.AgentEvent{
					EventID: "event_claim_" + gen.NewRunID(), RunID: runID,
					EventType: observability.EventResumeAccepted, Payload: payload,
				},
			}); err != nil {
				t.Fatalf("claim: %v", err)
			}
			failure := storage.ResumeFailureCommand{
				RunID: runID, AttemptID: attemptID, Retryable: test.retryable,
				ErrorCode: "CURRENT_ERROR", ErrorMessage: "current error",
				Event: observability.AgentEvent{
					EventID: "event_failure_" + gen.NewRunID(), RunID: runID,
					EventType: observability.EventResumeFailed, Payload: payload,
				},
			}
			if err := stores.Resumes.Fail(ctx, failure); err != nil {
				t.Fatalf("fail: %v", err)
			}
			run, err := stores.Runs.Get(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			if test.retryable {
				if run.Status != storage.RunStatusWaitingControl || run.ResumeAttemptID != "" ||
					!run.EndedAt.IsZero() || run.ErrorCode != "" || run.ErrorMessage != "" {
					t.Fatalf("released run = %+v", run)
				}
				return
			}
			if run.Status != storage.RunStatusFailed || run.ResumeAttemptID != "" ||
				run.EndedAt.IsZero() || run.EndedAt.Equal(oldEndedAt) ||
				run.ErrorCode != failure.ErrorCode || run.ErrorMessage != failure.ErrorMessage {
				t.Fatalf("failed run = %+v", run)
			}
		})
	}
}

func TestMySQLResumeClaimRollsBackWhenEventInsertFails(t *testing.T) {
	b := openTestDB(t)
	ctx := ctxT("t1")
	stores := b.Stores()
	gen := observability.NewULIDGenerator("")
	runID := "run_" + gen.NewRunID()
	controlID := "control_" + gen.NewRunID()
	if err := stores.Runs.Create(ctx, &storage.Run{
		RunID: runID, SessionID: "session_resume", TenantID: "t1", Status: storage.RunStatusWaitingControl,
	}); err != nil {
		t.Fatal(err)
	}
	if err := stores.Controls.Create(ctx, &storage.ControlRequest{
		RequestID: controlID, RunID: runID, TenantID: "t1", CheckpointID: "checkpoint_resume",
		Type: "ask_user", Status: "answered", ResumeTokenHash: "sha256:resume-token",
	}); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"attempt_id": "attempt_rollback"})
	err := stores.Resumes.Claim(ctx, storage.ResumeClaimCommand{
		RunID: runID, SessionID: "session_resume", CheckpointID: "checkpoint_resume", ControlRequestID: controlID,
		ResumeTokenHash: "sha256:resume-token", AttemptID: "attempt_rollback",
		Event: observability.AgentEvent{
			EventID: "event_" + gen.NewRunID(), RunID: runID, EventType: observability.EventResumeAccepted,
			Payload: payload, PayloadPreview: json.RawMessage(`{`),
		},
	})
	if err == nil {
		t.Fatal("invalid JSON event should fail")
	}
	run, getErr := stores.Runs.Get(ctx, runID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if run.Status != storage.RunStatusWaitingControl || run.ResumeAttemptID != "" || run.Version != 1 {
		t.Fatalf("claim update was not rolled back: %+v", run)
	}
}

func TestMySQLRuntimeBindingCASConcurrent(t *testing.T) {
	b := openTestDB(t)
	ctx := ctxT("t1")
	runs := b.RunStore()
	gen := observability.NewULIDGenerator("")
	runID := "run_" + gen.NewRunID()
	expected := json.RawMessage(`{"schema_version":"base"}`)
	if err := runs.Create(ctx, &storage.Run{
		RunID: runID, SessionID: "session_binding", TenantID: "t1",
		Status: storage.RunStatusRunning, RuntimeBinding: expected,
	}); err != nil {
		t.Fatal(err)
	}
	persisted, err := runs.Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	expected = persisted.RuntimeBinding
	replacements := []json.RawMessage{
		json.RawMessage(`{"schema_version":"winner_a"}`),
		json.RawMessage(`{"schema_version":"winner_b"}`),
	}
	start := make(chan struct{})
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	for _, replacement := range replacements {
		replacement := replacement
		go func() {
			<-start
			swapped, err := runs.CompareAndSetRuntimeBinding(ctx, runID, expected, replacement)
			results <- swapped
			errs <- err
		}()
	}
	close(start)
	winners := 0
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if <-results {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("CAS winners=%d, want 1", winners)
	}
}

func TestMySQLOpenTurnConcurrentSameNewSessionKey(t *testing.T) {
	b := openTestDB(t)
	stores := b.Stores()
	service := storage.NewRunService(stores, nil)
	gen := observability.NewULIDGenerator("")
	userID := "user_" + gen.NewRequestID()
	key := "open_turn_" + gen.NewRequestID()
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{
		TenantID: "t1", UserID: userID, TraceID: "trace_" + gen.NewTraceID(),
	})

	const requests = 20
	start := make(chan struct{})
	results := make(chan *storage.OpenTurnResult, requests)
	errors := make(chan error, requests)
	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := service.OpenTurn(ctx, storage.OpenTurnRequest{
				TenantID: "t1", UserID: userID, AgentID: "default-agent",
				UserContentPreview: "hello", IdempotencyKey: key,
			})
			results <- result
			errors <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errors)

	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent OpenTurn() error = %v", err)
		}
	}
	var sessionID, runID string
	for result := range results {
		if result == nil || result.Session == nil || result.Run == nil {
			t.Fatalf("concurrent OpenTurn() result = %#v", result)
		}
		if sessionID == "" {
			sessionID, runID = result.Session.ID, result.Run.RunID
			continue
		}
		if result.Session.ID != sessionID || result.Run.RunID != runID {
			t.Fatalf("same idempotency key diverged: session/run=%s/%s, want %s/%s",
				result.Session.ID, result.Run.RunID, sessionID, runID)
		}
	}
	sessions, err := stores.Sessions.List(ctx, storage.SessionListQuery{UserID: userID, Limit: 100})
	if err != nil || len(sessions.Items) != 1 || sessions.Items[0].ID != sessionID {
		t.Fatalf("sessions = %#v, error = %v; want only %q", sessions.Items, err, sessionID)
	}
	runs, err := stores.Runs.ListBySession(ctx, sessionID)
	if err != nil || len(runs) != 1 || runs[0].RunID != runID {
		t.Fatalf("runs = %#v, error = %v; want only %q", runs, err, runID)
	}
	messages, err := stores.Messages.List(ctx, storage.MessageListQuery{SessionID: sessionID, Limit: 100})
	if err != nil || len(messages.Items) != 1 {
		t.Fatalf("messages = %#v, error = %v; want exactly one", messages.Items, err)
	}
}

func ctxT(tenant string) context.Context {
	return observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: tenant, TraceID: "trace_x"})
}

func TestMySQLEventStoreConcurrentSequence(t *testing.T) {
	b := openTestDB(t)
	ctx := ctxT("t1")
	es := b.EventStore()
	runID := "run_" + observability.NewULIDGenerator("").NewRunID()
	if err := b.RunStore().Create(ctx, &storage.Run{RunID: runID, SessionID: "s1", TenantID: "t1"}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	const n = 50
	var wg sync.WaitGroup
	appendErrors := make(chan error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := es.Append(ctx, observability.AgentEvent{RunID: runID, EventType: observability.EventAgentTextDelta})
			appendErrors <- err
		}()
	}
	wg.Wait()
	close(appendErrors)
	for err := range appendErrors {
		if err != nil {
			t.Errorf("append concurrent event: %v", err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	evs, err := es.Query(ctx, storage.EventQuery{RunID: runID})
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	if len(evs) != n {
		t.Fatalf("event count = %d, want %d", len(evs), n)
	}
	seen := map[int64]bool{}
	for _, ev := range evs {
		if seen[ev.Sequence] {
			t.Fatalf("duplicate sequence %d", ev.Sequence)
		}
		seen[ev.Sequence] = true
	}
}

func TestMySQLEventStoreIdempotency(t *testing.T) {
	b := openTestDB(t)
	ctx := ctxT("t1")
	es := b.EventStore()
	runID := "run_" + observability.NewULIDGenerator("").NewRunID()
	if err := b.RunStore().Create(ctx, &storage.Run{RunID: runID, SessionID: "s1", TenantID: "t1"}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	id := "evt_fixed_" + runID
	r1, err := es.Append(ctx, observability.AgentEvent{RunID: runID, EventID: id, EventType: observability.EventRunStarted})
	if err != nil {
		t.Fatalf("append first event: %v", err)
	}
	r2, err := es.Append(ctx, observability.AgentEvent{RunID: runID, EventID: id, EventType: observability.EventRunStarted})
	if err != nil {
		t.Fatalf("append idempotent event: %v", err)
	}
	if !r2.Idempotent || r1.Event.Sequence != r2.Event.Sequence {
		t.Fatalf("event_id idempotency failed: %+v %+v", r1, r2)
	}
}

func TestMySQLRunStoreCAS(t *testing.T) {
	b := openTestDB(t)
	ctx := ctxT("t1")
	rs := b.RunStore()
	runID := "run_" + observability.NewULIDGenerator("").NewRunID()
	if err := rs.Create(ctx, &storage.Run{RunID: runID, SessionID: "s1", TenantID: "t1"}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := rs.CompareAndSetStatus(ctx, runID, storage.RunStatusCreated, storage.RunStatusRunning, storage.RunMutation{}); err != nil {
		t.Fatalf("created->running: %v", err)
	}
	if _, err := rs.CompareAndSetStatus(ctx, runID, storage.RunStatusCreated, storage.RunStatusCompleted, storage.RunMutation{}); !storage.IsErrorCode(err, storage.ErrCASMismatch) {
		t.Fatalf("want cas_mismatch, got %v", err)
	}
}

// TestMySQLFullStores exercises the six stores added to complete the backend:
// Session (CAS + list), Message (append + list), Step (upsert), Checkpoint,
// ControlRequest (CAS answer) and Idempotency (one-time consume).
func TestMySQLFullStores(t *testing.T) {
	b := openTestDB(t)
	ctx := ctxT("t1")
	st := b.Stores()
	gen := observability.NewULIDGenerator("")

	// --- Session: create / get / update(CAS) / list ---
	sid := "sess_" + gen.NewRunID()
	sess := &storage.Session{ID: sid, TenantID: "t1", UserID: "u1"}
	if err := st.Sessions.Create(ctx, sess); err != nil {
		t.Fatalf("session create: %v", err)
	}
	got, err := st.Sessions.Get(ctx, sid)
	if err != nil || got.Version != 1 {
		t.Fatalf("session get: %v %+v", err, got)
	}
	got.Title = "hello"
	if err := st.Sessions.Update(ctx, got); err != nil {
		t.Fatalf("session update: %v", err)
	}
	if got.Version != 2 {
		t.Fatalf("session version want 2, got %d", got.Version)
	}
	stale := &storage.Session{ID: sid, TenantID: "t1", UserID: "u1", Version: 1}
	if err := st.Sessions.Update(ctx, stale); !storage.IsErrorCode(err, storage.ErrCASMismatch) {
		t.Fatalf("stale update want cas_mismatch, got %v", err)
	}
	if page, err := st.Sessions.List(ctx, storage.SessionListQuery{UserID: "u1"}); err != nil || len(page.Items) == 0 {
		t.Fatalf("session list: %v len=%d", err, len(page.Items))
	}

	// --- Message: append / list ---
	mid := "msg_" + gen.NewRunID()
	if err := st.Messages.Append(ctx, &storage.Message{ID: mid, SessionID: sid, TenantID: "t1", Role: "user", ContentPreview: "hi"}); err != nil {
		t.Fatalf("message append: %v", err)
	}
	if page, err := st.Messages.List(ctx, storage.MessageListQuery{SessionID: sid}); err != nil || len(page.Items) != 1 {
		t.Fatalf("message list: %v len=%d", err, len(page.Items))
	}

	// --- Step: upsert / list ---
	rid := "run_" + gen.NewRunID()
	if err := st.Steps.Upsert(ctx, &storage.Step{StepID: "st1", RunID: rid, StepType: "model", Status: "running"}); err != nil {
		t.Fatalf("step upsert: %v", err)
	}
	if err := st.Steps.Upsert(ctx, &storage.Step{StepID: "st1", RunID: rid, StepType: "model", Status: "completed"}); err != nil {
		t.Fatalf("step re-upsert: %v", err)
	}
	if steps, err := st.Steps.ListByRun(ctx, rid); err != nil || len(steps) != 1 || steps[0].Status != "completed" {
		t.Fatalf("step list: %v %+v", err, steps)
	}

	// --- Checkpoint: create / get / latest ---
	cid := "ckpt_" + gen.NewRunID()
	if err := st.Checkpoints.Create(ctx, &storage.CheckpointMeta{CheckpointID: cid, RunID: rid, TenantID: "t1", Type: "auto", StateRef: "artifact://x"}); err != nil {
		t.Fatalf("checkpoint create: %v", err)
	}
	if ck, err := st.Checkpoints.LatestByRun(ctx, rid); err != nil || ck.CheckpointID != cid {
		t.Fatalf("checkpoint latest: %v %+v", err, ck)
	}

	// --- ControlRequest: create / answer(CAS) ---
	crid := "ctrl_" + gen.NewRunID()
	if err := st.Controls.Create(ctx, &storage.ControlRequest{RequestID: crid, RunID: rid, TenantID: "t1", Type: "ask_user"}); err != nil {
		t.Fatalf("control create: %v", err)
	}
	if _, err := st.Controls.CompareAndAnswer(ctx, crid, "pending", "answered", "artifact://resp"); err != nil {
		t.Fatalf("control answer: %v", err)
	}
	if _, err := st.Controls.CompareAndAnswer(ctx, crid, "pending", "answered", ""); !storage.IsErrorCode(err, storage.ErrCASMismatch) {
		t.Fatalf("second answer want cas_mismatch, got %v", err)
	}

	// --- Idempotency: one-time consume ---
	key := storage.IdemKey{TenantID: "t1", Namespace: "resume", Key: gen.NewRunID()}
	if ok, err := st.Idem.Consume(ctx, key); err != nil || !ok {
		t.Fatalf("first consume want ok, got %v %v", ok, err)
	}
	if ok, err := st.Idem.Consume(ctx, key); err != nil || ok {
		t.Fatalf("replay consume want !ok, got %v %v", ok, err)
	}
}
