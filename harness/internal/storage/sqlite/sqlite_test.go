package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	"github.com/AGenUI/agenui-studio/harness/internal/storage/sqlite"
)

func TestMigrateAddsRuntimeBindingToExistingRunsTable(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE runs (
		run_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, turn_id TEXT, parent_run_id TEXT,
		tenant_id TEXT NOT NULL, agent_id TEXT, runtime TEXT, status TEXT NOT NULL,
		trace_id TEXT, config_snapshot_ref TEXT, context_snapshot_ref TEXT, agent_binding_id TEXT,
		started_at INTEGER, ended_at INTEGER, error_code TEXT, error_message TEXT, version INTEGER NOT NULL DEFAULT 1
	)`); err != nil {
		t.Fatal(err)
	}
	_ = legacy.Close()

	backend, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	if err := backend.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	check, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	rows, err := check.Query(`PRAGMA table_info(runs)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		found = found || name == "runtime_binding"
	}
	if !found {
		t.Fatal("runtime_binding column was not added")
	}
}

func TestMigrateAddsModelLatencyColumnsToExistingUsageTable(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "legacy-usage.db")
	legacy, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE model_usage_records (
		usage_id TEXT PRIMARY KEY, request_id TEXT, trace_id TEXT, tenant_id TEXT NOT NULL,
		session_id TEXT, run_id TEXT, agent_id TEXT, provider TEXT, model TEXT,
		attempt INTEGER NOT NULL DEFAULT 0, fallback_applied INTEGER NOT NULL DEFAULT 0,
		prompt_tokens INTEGER NOT NULL DEFAULT 0, completion_tokens INTEGER NOT NULL DEFAULT 0,
		reasoning_tokens INTEGER NOT NULL DEFAULT 0, cache_read_tokens INTEGER NOT NULL DEFAULT 0,
		cache_write_tokens INTEGER NOT NULL DEFAULT 0, usage_source TEXT, currency TEXT,
		estimated_cost REAL NOT NULL DEFAULT 0, cache_hit INTEGER NOT NULL DEFAULT 0,
		output_ref TEXT, created_at INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		t.Fatal(err)
	}
	_ = legacy.Close()

	backend, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	if err := backend.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	check, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	rows, err := check.Query(`PRAGMA table_info(model_usage_records)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := map[string]bool{
		"total_latency_ms": false, "first_token_observed": false, "first_token_ms": false,
		"generation_duration_ms": false, "output_tokens_per_second": false,
	}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for column, found := range want {
		if !found {
			t.Fatalf("migration did not add %s", column)
		}
	}
}

func TestCheckReadyRequiresExistingSchemaWithoutMigrating(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "empty.db")
	backend, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	err = backend.CheckReady(context.Background())
	if err == nil || !strings.Contains(err.Error(), "schema not ready") {
		t.Fatalf("CheckReady empty db err=%v, want schema not ready", err)
	}
	if closeErr := backend.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	check, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var tableCount int
	if err := check.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table'`).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if tableCount != 0 {
		t.Fatalf("CheckReady must not create tables; found %d", tableCount)
	}
}

func TestCheckReadyPassesAfterExplicitMigrate(t *testing.T) {
	backend, err := sqlite.Open(filepath.Join(t.TempDir(), "ready.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	if err := backend.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := backend.CheckReady(context.Background()); err != nil {
		t.Fatalf("CheckReady after explicit migrate: %v", err)
	}
}

func tenantCtx(tenant string) context.Context {
	return observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: tenant, TraceID: "trace_x"})
}

func newDB(t *testing.T) *sqlite.Backend {
	t.Helper()
	b, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := b.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestSQLiteBackendCapabilities(t *testing.T) {
	b := newDB(t)
	if err := storage.RequireCapabilities(b, storage.CapCAS, storage.CapTransaction, storage.CapOrderedAppend, storage.CapIdempotency, storage.CapTenantIsolation); err != nil {
		t.Fatalf("missing capability: %v", err)
	}
}

func TestSQLiteCheckpointAndControlRequestLifecycle(t *testing.T) {
	ctx := tenantCtx("t1")
	b := newDB(t)
	stores := b.Stores()
	if err := stores.Runs.Create(ctx, &storage.Run{RunID: "run-control", SessionID: "s1", TenantID: "t1", Status: storage.RunStatusRunning}); err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1_700_000_000, 0).UTC()
	for i, checkpointID := range []string{"checkpoint-1", "checkpoint-2"} {
		if err := stores.Checkpoints.Create(ctx, &storage.CheckpointMeta{
			CheckpointID: checkpointID, RunID: "run-control", TenantID: "t1",
			Runtime: "eino", Type: "run", StateRef: "artifact://t1/" + checkpointID,
			EventSequence: int64(i + 1), CreatedAt: base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("create checkpoint %s: %v", checkpointID, err)
		}
	}
	latest, err := stores.Checkpoints.LatestByRun(ctx, "run-control")
	if err != nil || latest.CheckpointID != "checkpoint-2" || latest.StateRef == "" {
		t.Fatalf("latest checkpoint=%#v err=%v", latest, err)
	}

	request := &storage.ControlRequest{
		RequestID: "control-1", RunID: "run-control", TenantID: "t1",
		CheckpointID: latest.CheckpointID, Type: "ask_user", Status: "pending",
		ResumeTokenHash: "sha256:token", SchemaVersion: storage.ControlRequestSchemaVersion,
		CreatedAt: base,
	}
	if err := stores.Controls.Create(ctx, request); err != nil {
		t.Fatal(err)
	}

	const contenders = 8
	var successes atomic.Int32
	var mismatches atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answered, answerErr := stores.Controls.CompareAndAnswer(ctx, request.RequestID, "pending", "answered", "artifact://response")
			switch {
			case answerErr == nil:
				if answered.Status != "answered" || answered.Version != 2 {
					t.Errorf("invalid answered request: %#v", answered)
				}
				successes.Add(1)
			case storage.IsErrorCode(answerErr, storage.ErrCASMismatch):
				mismatches.Add(1)
			default:
				t.Errorf("unexpected answer error: %v", answerErr)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 || mismatches.Load() != contenders-1 {
		t.Fatalf("control CAS successes=%d mismatches=%d", successes.Load(), mismatches.Load())
	}
	persisted, err := stores.Controls.Get(ctx, request.RequestID)
	if err != nil || persisted.Status != "answered" || persisted.ResponseRef != "artifact://response" || persisted.CheckpointID != latest.CheckpointID {
		t.Fatalf("persisted control=%#v err=%v", persisted, err)
	}
	if _, err := stores.Controls.Get(tenantCtx("other"), request.RequestID); !storage.IsErrorCode(err, storage.ErrTenantMismatch) {
		t.Fatalf("cross-tenant control get must fail closed: %v", err)
	}
}

func TestSQLiteEventStoreSequenceAndIdempotency(t *testing.T) {
	ctx := tenantCtx("t1")
	b := newDB(t)
	_ = b.RunStore().Create(ctx, &storage.Run{RunID: "run1", SessionID: "s1", TenantID: "t1"})
	es := b.EventStore()

	// monotonic sequence
	for i := 0; i < 5; i++ {
		if _, err := es.Append(ctx, observability.AgentEvent{RunID: "run1", EventType: observability.EventAgentTextDelta}); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := es.Query(ctx, storage.EventQuery{RunID: "run1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 5 {
		t.Fatalf("want 5 events, got %d", len(evs))
	}
	for i, ev := range evs {
		if ev.Sequence != int64(i+1) {
			t.Fatalf("event %d sequence = %d", i, ev.Sequence)
		}
	}

	// event_id idempotency
	r1, _ := es.Append(ctx, observability.AgentEvent{RunID: "run1", EventID: "evt_fixed", EventType: observability.EventRunStarted})
	r2, _ := es.Append(ctx, observability.AgentEvent{RunID: "run1", EventID: "evt_fixed", EventType: observability.EventRunStarted})
	if !r2.Idempotent || r1.Event.Sequence != r2.Event.Sequence {
		t.Fatalf("event_id idempotency failed: %+v / %+v", r1, r2)
	}

	// idempotency_key
	_, _ = es.Append(ctx, observability.AgentEvent{RunID: "run1", IdempotencyKey: "k1", EventType: observability.EventAgentTextDelta})
	r4, _ := es.Append(ctx, observability.AgentEvent{RunID: "run1", IdempotencyKey: "k1", EventType: observability.EventAgentTextDelta})
	if !r4.Idempotent {
		t.Fatalf("idempotency_key dedup failed")
	}

	// SequenceOf + after_sequence
	seq, err := es.SequenceOf(ctx, "run1", "evt_fixed")
	if err != nil || seq != r1.Event.Sequence {
		t.Fatalf("SequenceOf = %d err=%v", seq, err)
	}
	after, _ := es.Query(ctx, storage.EventQuery{RunID: "run1", AfterSequence: seq})
	for _, ev := range after {
		if ev.Sequence <= seq {
			t.Fatalf("after_sequence returned seq %d <= %d", ev.Sequence, seq)
		}
	}
}

func TestSQLiteEventStoreVisibilityFilter(t *testing.T) {
	ctx := tenantCtx("t1")
	b := newDB(t)
	_ = b.RunStore().Create(ctx, &storage.Run{RunID: "run1", SessionID: "s1", TenantID: "t1"})
	es := b.EventStore()
	_, _ = es.Append(ctx, observability.AgentEvent{RunID: "run1", EventType: observability.EventAgentTextDelta, Visibility: observability.VisibilityUserVisible})
	_, _ = es.Append(ctx, observability.AgentEvent{RunID: "run1", EventType: observability.EventModelTokenDelta, Visibility: observability.VisibilityDebug})

	user, _ := es.Query(ctx, storage.EventQuery{RunID: "run1", Visibilities: []observability.EventVisibility{observability.VisibilityUserVisible}})
	if len(user) != 1 {
		t.Fatalf("user-visible view = %d", len(user))
	}
}

func TestSQLiteRunStoreCAS(t *testing.T) {
	ctx := tenantCtx("t1")
	b := newDB(t)
	rs := b.RunStore()
	if err := rs.Create(ctx, &storage.Run{RunID: "run1", SessionID: "s1", TenantID: "t1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.CompareAndSetStatus(ctx, "run1", storage.RunStatusCreated, storage.RunStatusRunning, storage.RunMutation{}); err != nil {
		t.Fatalf("created->running: %v", err)
	}
	if _, err := rs.CompareAndSetStatus(ctx, "run1", storage.RunStatusCreated, storage.RunStatusCompleted, storage.RunMutation{}); !storage.IsErrorCode(err, storage.ErrCASMismatch) {
		t.Fatalf("want cas_mismatch, got %v", err)
	}
	run, _ := rs.CompareAndSetStatus(ctx, "run1", storage.RunStatusRunning, storage.RunStatusCompleted, storage.RunMutation{})
	if run.Status != storage.RunStatusCompleted || run.EndedAt.IsZero() {
		t.Fatalf("completed run = %+v", run)
	}
}

func TestSQLiteStepPatchPreservesMetadata(t *testing.T) {
	ctx := tenantCtx("t1")
	steps := newDB(t).StepStore()
	if err := steps.Upsert(ctx, &storage.Step{
		StepID: "step1", RunID: "run1", ParentStepID: "parent1", StepType: "model_context", Name: "build_context", Status: "running",
	}); err != nil {
		t.Fatal(err)
	}
	endedAt := time.Now()
	if err := steps.Upsert(ctx, &storage.Step{StepID: "step1", RunID: "run1", Status: "completed", EndedAt: endedAt}); err != nil {
		t.Fatal(err)
	}
	got, err := steps.ListByRun(ctx, "run1")
	if err != nil || len(got) != 1 {
		t.Fatalf("steps: %v %+v", err, got)
	}
	if got[0].StepType != "model_context" || got[0].Name != "build_context" || got[0].ParentStepID != "parent1" || got[0].Status != "completed" {
		t.Fatalf("step patch overwrote metadata: %+v", got[0])
	}
	if got[0].EndedAt.IsZero() {
		t.Fatalf("step end time was not persisted: %+v", got[0])
	}
}

func TestSQLiteTenantIsolation(t *testing.T) {
	b := newDB(t)
	_ = b.RunStore().Create(tenantCtx("t1"), &storage.Run{RunID: "run1", SessionID: "s1"})
	if _, err := b.RunStore().Get(tenantCtx("t2"), "run1"); !storage.IsErrorCode(err, storage.ErrTenantMismatch) {
		t.Fatalf("want tenant_mismatch, got %v", err)
	}
}

func TestSQLiteModelUsageStore(t *testing.T) {
	ctx := tenantCtx("t1")
	b := newDB(t)
	usage := b.Stores().Usage
	if err := usage.Record(ctx, &storage.ModelUsageRecord{
		ID:                    "usage1",
		TenantID:              "t1",
		SessionID:             "s1",
		RunID:                 "run1",
		AgentID:               "agent1",
		Provider:              "mock",
		Model:                 "mock-model",
		PromptTokens:          10,
		CompletionTokens:      5,
		ReasoningTokens:       2,
		UsageSource:           "gateway",
		Currency:              "USD",
		EstimatedCost:         0.25,
		TotalLatencyMS:        1200,
		FirstTokenObserved:    true,
		FirstTokenMS:          200,
		GenerationDurationMS:  1000,
		OutputTokensPerSecond: 5,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	records, err := usage.List(ctx, storage.ModelUsageQuery{TenantID: "t1", RunID: "run1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(records) != 1 || records[0].SessionID != "s1" || records[0].PromptTokens != 10 ||
		!records[0].FirstTokenObserved || records[0].FirstTokenMS != 200 || records[0].TotalLatencyMS != 1200 ||
		records[0].GenerationDurationMS != 1000 || records[0].OutputTokensPerSecond != 5 {
		t.Fatalf("records = %+v", records)
	}
	summary, err := usage.Summary(ctx, storage.ModelUsageQuery{TenantID: "t1"})
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.Records != 1 || summary.PromptTokens != 10 || summary.CompletionTokens != 5 || summary.ReasoningTokens != 2 || summary.EstimatedCost != 0.25 {
		t.Fatalf("summary = %+v", summary)
	}
}
