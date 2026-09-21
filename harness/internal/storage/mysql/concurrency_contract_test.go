package mysql

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

func TestIdempotencyExpiredClaimUsesConditionalCAS(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO idempotency_keys (id, expires_at, created_at) VALUES (?,?,?)`).
		WithArgs("tenant-a|tool|key", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(errors.New("Error 1062: Duplicate entry"))
	mock.ExpectExec(`UPDATE idempotency_keys SET expires_at=?, created_at=? WHERE id=? AND expires_at IS NOT NULL AND expires_at<=?`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "tenant-a|tool|key", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	claimed, err := (&idempotencyStore{db: db}).Consume(context.Background(), storage.IdemKey{
		TenantID: "tenant-a", Namespace: "tool", Key: "key", TTL: time.Minute,
	})
	if err != nil || !claimed {
		t.Fatalf("Consume() = (%v, %v), want claimed", claimed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestIdempotencyDeletedDuplicateRetriesInsert(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	insert := `INSERT INTO idempotency_keys (id, expires_at, created_at) VALUES (?,?,?)`
	mock.ExpectExec(insert).WillReturnError(errors.New("Error 1062: Duplicate entry"))
	mock.ExpectExec(`UPDATE idempotency_keys SET expires_at=?, created_at=? WHERE id=? AND expires_at IS NOT NULL AND expires_at<=?`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT 1 FROM idempotency_keys WHERE id=?`).
		WithArgs("tenant-a|tool|key").WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(insert).WillReturnResult(sqlmock.NewResult(1, 1))

	claimed, err := (&idempotencyStore{db: db}).Consume(context.Background(), storage.IdemKey{
		TenantID: "tenant-a", Namespace: "tool", Key: "key", TTL: time.Minute,
	})
	if err != nil || !claimed {
		t.Fatalf("Consume() = (%v, %v), want claimed after bounded retry", claimed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestExpirePendingDoesNotOverwriteConcurrentAnswer(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now()
	rows := sqlmock.NewRows([]string{
		"request_id", "run_id", "tenant_id", "checkpoint_id", "type", "status", "resume_token_hash",
		"tool_use_id", "prompt_preview", "response_ref", "schema_version", "created_at", "expires_at", "version",
	}).AddRow("ctrl-1", "run-1", "tenant-a", nil, "ask_user", "pending", "hash", nil, "question", nil,
		storage.ControlRequestSchemaVersion, now.Add(-time.Minute), now.Add(-time.Second), int64(1))
	mock.ExpectQuery(`SELECT request_id, run_id, tenant_id, checkpoint_id, type, status, resume_token_hash,
		tool_use_id, prompt_preview, response_ref, schema_version, created_at, expires_at, version
		FROM control_requests WHERE status=? AND expires_at IS NOT NULL AND expires_at<=? AND tenant_id=?`).
		WithArgs("pending", now, "tenant-a").WillReturnRows(rows)
	mock.ExpectExec(`UPDATE control_requests SET status=?, version=version+1 WHERE request_id=? AND status=? AND expires_at IS NOT NULL AND expires_at<=?`).
		WithArgs("expired", "ctrl-1", "pending", now).WillReturnResult(sqlmock.NewResult(0, 0))

	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "tenant-a"})
	expired, err := (&controlStore{db: db}).ExpirePending(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 0 {
		t.Fatalf("ExpirePending() returned %d rows, want zero CAS winners", len(expired))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunBindingRejectsCrossTenantBeforeUpdate(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT tenant_id, context_snapshot_ref FROM runs WHERE run_id=? FOR UPDATE`).
		WithArgs("run-1").WillReturnRows(sqlmock.NewRows([]string{"tenant_id", "context_snapshot_ref"}).AddRow("tenant-a", nil))
	mock.ExpectRollback()

	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "tenant-b"})
	err = (&runStore{db: db}).BindContextSnapshot(ctx, "run-1", "artifact://snapshot")
	if !storage.IsErrorCode(err, storage.ErrTenantMismatch) {
		t.Fatalf("BindContextSnapshot() error = %v, want tenant mismatch", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunAgentConfigBindingIsImmutableAndIdempotent(t *testing.T) {
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "tenant-a"})
	for _, test := range []struct {
		name         string
		binding      any
		config       any
		wantUpdate   bool
		wantConflict bool
	}{
		{name: "first bind", binding: nil, config: nil, wantUpdate: true},
		{name: "complete prebound agent", binding: "binding-1", config: nil, wantUpdate: true},
		{name: "same binding replay", binding: "binding-1", config: "config-1"},
		{name: "different binding rejected", binding: "binding-other", config: "config-1", wantConflict: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT tenant_id, agent_binding_id, config_snapshot_ref FROM runs WHERE run_id=? FOR UPDATE`).
				WithArgs("run-1").WillReturnRows(sqlmock.NewRows([]string{"tenant_id", "agent_binding_id", "config_snapshot_ref"}).
				AddRow("tenant-a", test.binding, test.config))
			if test.wantUpdate {
				mock.ExpectExec(`UPDATE runs SET agent_binding_id=?, config_snapshot_ref=?, version=version+1 WHERE run_id=?`).
					WithArgs("binding-1", "config-1", "run-1").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit()
			} else {
				mock.ExpectRollback()
			}

			err = (&runStore{db: db}).BindAgentConfig(ctx, "run-1", "binding-1", "config-1")
			if test.wantConflict != storage.IsErrorCode(err, storage.ErrCASMismatch) {
				t.Fatalf("BindAgentConfig() error = %v, wantConflict=%v", err, test.wantConflict)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStepUpsertRejectsCrossTenantRun(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "tenant-b"})
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT tenant_id FROM runs WHERE run_id=? FOR UPDATE`).
		WithArgs("run-1").WillReturnRows(sqlmock.NewRows([]string{"tenant_id"}).AddRow("tenant-a"))
	mock.ExpectRollback()

	err = (&stepStore{db: db}).Upsert(ctx, &storage.Step{StepID: "step-1", RunID: "run-1", Status: "running"})
	if !storage.IsErrorCode(err, storage.ErrTenantMismatch) {
		t.Fatalf("Upsert() error = %v, want tenant mismatch", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestStepListScopesQueryByRunTenant(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "tenant-a"})
	mock.ExpectQuery(`SELECT s.step_id, s.run_id, s.parent_step_id,
		s.step_type, s.name, s.status, s.started_at, s.ended_at
		FROM steps s JOIN runs r ON r.run_id=s.run_id
		WHERE s.run_id=? AND (?='' OR r.tenant_id=?) ORDER BY s.started_at ASC`).
		WithArgs("run-1", "tenant-a", "tenant-a").
		WillReturnRows(sqlmock.NewRows([]string{"step_id", "run_id", "parent_step_id", "step_type", "name", "status", "started_at", "ended_at"}))

	if _, err := (&stepStore{db: db}).ListByRun(ctx, "run-1"); err != nil {
		t.Fatalf("ListByRun() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
