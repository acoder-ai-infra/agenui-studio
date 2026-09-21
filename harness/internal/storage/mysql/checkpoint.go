package mysql

import (
	"context"
	"database/sql"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// checkpointStore is the MySQL CheckpointStore. Create relies on the primary-key
// constraint as the conflict backstop; LatestByRun returns the newest by
// created_at. expires_at is DATETIME(3) NULL. Semantics mirror memory/sqlite.
type checkpointStore struct {
	db     *sql.DB
	schema physicalSchema
}

func (c *checkpointStore) Create(ctx context.Context, ck *storage.CheckpointMeta) error {
	if ck == nil || ck.CheckpointID == "" || ck.RunID == "" {
		return storage.NewError(storage.ErrInvalidArgument, "checkpoint_id and run_id required")
	}
	if ck.StateRef == "" {
		return storage.NewError(storage.ErrInvalidArgument, "state_ref required")
	}
	scope := storage.ScopeFromLenient(ctx)
	if ck.TenantID == "" {
		ck.TenantID = scope.TenantID
	}
	if err := storage.ValidateCheckpointIdentifiers(ck); err != nil {
		return err
	}
	if err := scope.EnforceTenant(ck.TenantID); err != nil {
		return err
	}
	if ck.CreatedAt.IsZero() {
		ck.CreatedAt = time.Now()
	}
	_, err := c.db.ExecContext(ctx, `INSERT INTO checkpoint_meta
		(checkpoint_id, run_id, tenant_id, runtime, type, state_ref, event_sequence, created_reason, created_at, expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		c.schema.id("checkpoint_meta", "checkpoint_id", ck.CheckpointID), ck.RunID, ck.TenantID, nullStr(ck.Runtime), ck.Type, ck.StateRef,
		ck.EventSequence, nullStr(ck.CreatedReason), ck.CreatedAt, nullTime(ck.ExpiresAt),
	)
	if err != nil && isDuplicate(err) {
		return storage.NewError(storage.ErrConflict, "checkpoint already exists: "+ck.CheckpointID)
	}
	return err
}

func (c *checkpointStore) Get(ctx context.Context, checkpointID string) (*storage.CheckpointMeta, error) {
	return c.get(ctx, c.db, "checkpoint_id=?", checkpointID)
}

func (c *checkpointStore) LatestByRun(ctx context.Context, runID string) (*storage.CheckpointMeta, error) {
	return c.get(ctx, c.db, "run_id=? ORDER BY created_at DESC LIMIT 1", runID)
}

func (c *checkpointStore) CompareAndSwapState(ctx context.Context, checkpointID, expectedStateRef string, replacement *storage.CheckpointMeta) (bool, error) {
	if replacement == nil || replacement.StateRef == "" || replacement.CheckpointID != checkpointID {
		return false, storage.NewError(storage.ErrInvalidArgument, "valid replacement checkpoint required")
	}
	if err := storage.ValidateCheckpointIdentifiers(replacement); err != nil {
		return false, err
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(replacement.TenantID); err != nil {
		return false, err
	}
	result, err := c.db.ExecContext(ctx, `UPDATE checkpoint_meta SET state_ref=?, event_sequence=?, created_reason=?, expires_at=?
		WHERE checkpoint_id=? AND tenant_id=? AND run_id=? AND runtime <=> ? AND type=? AND state_ref=?`,
		replacement.StateRef, replacement.EventSequence, nullStr(replacement.CreatedReason), nullTime(replacement.ExpiresAt),
		checkpointID, replacement.TenantID, replacement.RunID, nullStr(replacement.Runtime), replacement.Type, expectedStateRef)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

func (c *checkpointStore) Delete(ctx context.Context, checkpointID string) error {
	current, err := c.Get(ctx, checkpointID)
	if storage.IsErrorCode(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, "DELETE FROM checkpoint_meta WHERE checkpoint_id=? AND tenant_id=?", checkpointID, current.TenantID)
	return err
}

func (c *checkpointStore) get(ctx context.Context, q queryRower, where string, args ...any) (*storage.CheckpointMeta, error) {
	var logicalCheckpointID string
	if where == "checkpoint_id=?" && len(args) == 1 {
		if id, ok := args[0].(string); ok {
			logicalCheckpointID = id
			args[0] = c.schema.id("checkpoint_meta", "checkpoint_id", id)
		}
	}
	var (
		ck              storage.CheckpointMeta
		runtime, reason sql.NullString
		created         time.Time
		expires         sql.NullTime
	)
	err := q.QueryRowContext(ctx, `SELECT checkpoint_id, run_id, tenant_id, runtime, type, state_ref,
		event_sequence, created_reason, created_at, expires_at FROM checkpoint_meta WHERE `+where, args...).Scan(
		&ck.CheckpointID, &ck.RunID, &ck.TenantID, &runtime, &ck.Type, &ck.StateRef,
		&ck.EventSequence, &reason, &created, &expires,
	)
	if err == sql.ErrNoRows {
		return nil, storage.NewError(storage.ErrNotFound, "checkpoint not found")
	}
	if err != nil {
		return nil, err
	}
	if logicalCheckpointID != "" {
		ck.CheckpointID = logicalCheckpointID
	}
	ck.Runtime, ck.CreatedReason = runtime.String, reason.String
	ck.CreatedAt = created
	if expires.Valid {
		ck.ExpiresAt = expires.Time
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(ck.TenantID); err != nil {
		return nil, err
	}
	return &ck, nil
}

var _ storage.CheckpointStore = (*checkpointStore)(nil)
