package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// runStore 是 SQLite RunStore。状态变更用事务 + 乐观 version 自增(CAS)并校验迁移。
// 连接数固定为 1,写事务串行,无需 SELECT ... FOR UPDATE。
type runStore struct {
	db *sql.DB
}

func (r *runStore) Create(ctx context.Context, run *storage.Run) error {
	if run == nil || run.RunID == "" || run.SessionID == "" {
		return storage.NewError(storage.ErrInvalidArgument, "run_id and session_id required")
	}
	scope := storage.ScopeFromLenient(ctx)
	if run.TenantID == "" {
		run.TenantID = scope.TenantID
	}
	if err := storage.ValidateRunIdentifiers(run); err != nil {
		return err
	}
	if err := scope.EnforceTenant(run.TenantID); err != nil {
		return err
	}
	if run.Status == "" {
		run.Status = storage.RunStatusCreated
	}
	run.Version = 1
	_, err := r.db.ExecContext(ctx, `INSERT INTO runs
		(run_id, session_id, turn_id, parent_run_id, tenant_id, agent_id, runtime, runtime_binding, status, trace_id,
		 config_snapshot_ref, context_snapshot_ref, agent_binding_id, resume_attempt_id,
		 started_at, ended_at, version)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		run.RunID, run.SessionID, nullStr(run.TurnID), nullStr(run.ParentRunID), run.TenantID,
		nullStr(run.AgentID), nullStr(run.Runtime), rawStr(run.RuntimeBinding), string(run.Status), nullStr(run.TraceID),
		nullStr(run.ConfigSnapshotRef), nullStr(run.ContextSnapshotRef), nullStr(run.AgentBindingID),
		nullStr(run.ResumeAttemptID),
		tsVal(run.StartedAt), tsVal(run.EndedAt), run.Version,
	)
	return err
}

func (r *runStore) Get(ctx context.Context, runID string) (*storage.Run, error) {
	return r.get(ctx, r.db, runID)
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (r *runStore) get(ctx context.Context, q queryRower, runID string) (*storage.Run, error) {
	var (
		run                                                                          storage.Run
		turn, parent, agent, runtime, trace, cfg, ctxRef, binding, resumeAttempt, ec sql.NullString
		runtimeBinding                                                               sql.NullString
		em                                                                           sql.NullString
		status                                                                       string
		started, ended                                                               int64
	)
	err := q.QueryRowContext(ctx, `SELECT run_id, session_id, turn_id, parent_run_id, tenant_id, agent_id,
		runtime, runtime_binding, status, trace_id, config_snapshot_ref, context_snapshot_ref, agent_binding_id,
		resume_attempt_id,
		started_at, ended_at, error_code, error_message, version FROM runs WHERE run_id=?`, runID).Scan(
		&run.RunID, &run.SessionID, &turn, &parent, &run.TenantID, &agent, &runtime, &runtimeBinding, &status, &trace,
		&cfg, &ctxRef, &binding, &resumeAttempt, &started, &ended, &ec, &em, &run.Version,
	)
	if err == sql.ErrNoRows {
		return nil, storage.NewError(storage.ErrNotFound, "run not found: "+runID)
	}
	if err != nil {
		return nil, err
	}
	run.TurnID, run.ParentRunID, run.AgentID, run.Runtime = turn.String, parent.String, agent.String, runtime.String
	run.RuntimeBinding = json.RawMessage(runtimeBinding.String)
	run.TraceID, run.ConfigSnapshotRef, run.ContextSnapshotRef, run.AgentBindingID = trace.String, cfg.String, ctxRef.String, binding.String
	run.ResumeAttemptID = resumeAttempt.String
	run.ErrorCode, run.ErrorMessage = ec.String, em.String
	run.Status = storage.RunStatus(status)
	run.StartedAt, run.EndedAt = tsTime(started), tsTime(ended)
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(run.TenantID); err != nil {
		return nil, err
	}
	return &run, nil
}

func (r *runStore) CompareAndSetStatus(ctx context.Context, runID string, from, to storage.RunStatus, mut storage.RunMutation) (*storage.Run, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var status, tenant string
	if err := tx.QueryRowContext(ctx, "SELECT status, tenant_id FROM runs WHERE run_id=?", runID).Scan(&status, &tenant); err == sql.ErrNoRows {
		return nil, storage.NewError(storage.ErrNotFound, "run not found: "+runID)
	} else if err != nil {
		return nil, err
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(tenant); err != nil {
		return nil, err
	}
	// CAS first (matches memory backend), then validate the transition.
	if storage.RunStatus(status) != from {
		return nil, storage.NewError(storage.ErrCASMismatch, "run status is "+status+", expected "+string(from))
	}
	if err := storage.ValidateRunTransition(from, to); err != nil {
		return nil, err
	}

	started := int64(0)
	if to == storage.RunStatusRunning {
		started = time.Now().UnixNano()
	}
	ended := mut.EndedAt
	if ended.IsZero() && to.IsTerminal() {
		ended = time.Now()
	}
	clearResume := to != storage.RunStatusResuming
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status=?, version=version+1,
		started_at=CASE WHEN ?<>0 AND started_at=0 THEN ? ELSE started_at END,
		ended_at=CASE WHEN ?<>0 THEN ? ELSE ended_at END,
		error_code=CASE WHEN ?<>'' THEN ? ELSE error_code END,
		error_message=CASE WHEN ?<>'' THEN ? ELSE error_message END,
		resume_attempt_id=CASE WHEN ? THEN NULL ELSE resume_attempt_id END
		WHERE run_id=?`,
		string(to), started, started, tsVal(ended), tsVal(ended),
		mut.ErrorCode, mut.ErrorCode, mut.ErrorMessage, mut.ErrorMessage,
		clearResume, runID,
	); err != nil {
		return nil, err
	}
	run, err := r.get(ctx, tx, runID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return run, nil
}

func (r *runStore) BindContextSnapshot(ctx context.Context, runID, ref string) error {
	return r.bind(ctx, runID, "context_snapshot_ref", ref, ref)
}

func (r *runStore) BindAgentBinding(ctx context.Context, runID, bindingID string) error {
	if err := storage.ValidateAgentBindingIdentifiers(runID, bindingID); err != nil {
		return err
	}
	return r.bind(ctx, runID, "agent_binding_id", bindingID, bindingID)
}

func (r *runStore) BindAgentConfig(ctx context.Context, runID, bindingID, configSnapshotRef string) error {
	if err := storage.ValidateAgentBindingIdentifiers(runID, bindingID); err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var tenantID string
	var currentBinding, currentConfig sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT tenant_id, agent_binding_id, config_snapshot_ref FROM runs WHERE run_id=?`, runID).
		Scan(&tenantID, &currentBinding, &currentConfig)
	if err == sql.ErrNoRows {
		return storage.NewError(storage.ErrNotFound, "run not found: "+runID)
	}
	if err != nil {
		return err
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(tenantID); err != nil {
		return err
	}
	if currentBinding.Valid && currentBinding.String != bindingID {
		return storage.NewError(storage.ErrCASMismatch, "agent binding is already frozen")
	}
	if currentConfig.Valid && currentConfig.String != configSnapshotRef {
		return storage.NewError(storage.ErrCASMismatch, "config snapshot is already frozen")
	}
	if currentBinding.String == bindingID && currentConfig.String == configSnapshotRef {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET agent_binding_id=?, config_snapshot_ref=?, version=version+1 WHERE run_id=?`, bindingID, configSnapshotRef, runID); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *runStore) BindRuntimeBinding(ctx context.Context, runID string, binding json.RawMessage) error {
	return r.bind(ctx, runID, "runtime_binding", string(binding), rawStr(binding))
}

func (r *runStore) CompareAndSetRuntimeBinding(ctx context.Context, runID string, expected, replacement json.RawMessage) (bool, error) {
	scope := storage.ScopeFromLenient(ctx)
	expectedValue := rawStr(expected)
	query := `UPDATE runs SET runtime_binding=?,version=version+1
		WHERE run_id=? AND status=? AND ((? IS NULL AND COALESCE(runtime_binding,'')='') OR runtime_binding=?)`
	args := []any{rawStr(replacement), runID, string(storage.RunStatusRunning), expectedValue, expectedValue}
	if scope.TenantID != "" {
		query += " AND tenant_id=?"
		args = append(args, scope.TenantID)
	}
	result, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return false, err
	} else if affected == 1 {
		return true, nil
	}
	run, err := r.Get(ctx, runID)
	if err != nil {
		return false, err
	}
	if run.Status != storage.RunStatusRunning {
		return false, storage.NewError(storage.ErrIllegalTransition, "runtime binding can only be changed while running")
	}
	return false, nil
}

func (r *runStore) bind(ctx context.Context, runID, col, desired string, val any) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var tenantID string
	var current sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT tenant_id, "+col+" FROM runs WHERE run_id=?", runID).Scan(&tenantID, &current)
	if err == sql.ErrNoRows {
		return storage.NewError(storage.ErrNotFound, "run not found: "+runID)
	}
	if err != nil {
		return err
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(tenantID); err != nil {
		return err
	}
	if current.Valid && current.String != "" {
		if current.String == desired {
			return nil
		}
		return storage.NewError(storage.ErrCASMismatch, col+" is already frozen")
	}
	if _, err := tx.ExecContext(ctx, "UPDATE runs SET "+col+"=?, version=version+1 WHERE run_id=?", val, runID); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *runStore) ListBySession(ctx context.Context, sessionID string) ([]*storage.Run, error) {
	scope := storage.ScopeFromLenient(ctx)
	rows, err := r.db.QueryContext(ctx, "SELECT run_id FROM runs WHERE session_id=? AND (?='' OR tenant_id=?)",
		sessionID, scope.TenantID, scope.TenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []*storage.Run
	for _, id := range ids {
		run, err := r.get(ctx, r.db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, nil
}

var _ storage.RunStore = (*runStore)(nil)
