package mysql

import (
	"context"
	"database/sql"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// stepStore is the MySQL StepStore. Tenant ownership is derived from the
// immutable parent Run in the same database; Step does not duplicate tenant_id.
type stepStore struct {
	db     *sql.DB
	schema physicalSchema
}

func (s *stepStore) Upsert(ctx context.Context, step *storage.Step) error {
	if step == nil || step.StepID == "" || step.RunID == "" {
		return storage.NewError(storage.ErrInvalidArgument, "step_id and run_id required")
	}
	if err := storage.ValidateStepIdentifiers(step); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var tenantID string
	physicalRunID := s.schema.id("runs", "run_id", step.RunID)
	// 串行化同一 Run 下的 Step 写入，保证序号分配和幂等写入一致。
	if err := tx.QueryRowContext(ctx, `SELECT tenant_id FROM runs WHERE run_id=? FOR UPDATE`, physicalRunID).Scan(&tenantID); err == sql.ErrNoRows {
		return storage.NewError(storage.ErrNotFound, "run not found: "+step.RunID)
	} else if err != nil {
		return err
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(tenantID); err != nil {
		return err
	}
	physicalStepRunID := s.schema.id("steps", "run_id", step.RunID)
	physicalStepID := s.schema.id("steps", "step_id", step.StepID)
	var (
		existingParent, existingName sql.NullString
		existingType, existingStatus string
		existingStarted              time.Time
		existingEnded                sql.NullTime
	)
	err = tx.QueryRowContext(ctx, `SELECT parent_step_id, step_type, name, status, started_at, ended_at
		FROM steps WHERE run_id=? AND step_id=? FOR UPDATE`, physicalStepRunID, physicalStepID).Scan(
		&existingParent, &existingType, &existingName, &existingStatus, &existingStarted, &existingEnded,
	)
	switch err {
	case sql.ErrNoRows:
		if step.StartedAt.IsZero() {
			step.StartedAt = time.Now()
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO steps
			(step_id, run_id, parent_step_id, step_type, name, status, started_at, ended_at)
			VALUES (?,?,?,?,?,?,?,?)`,
			physicalStepID, physicalStepRunID, s.schema.nullID("steps", "parent_step_id", step.ParentStepID),
			step.StepType, nullStr(step.Name), step.Status, step.StartedAt, nullTime(step.EndedAt),
		)
	case nil:
		if step.ParentStepID == "" {
			step.ParentStepID = existingParent.String
		}
		if step.StepType == "" {
			step.StepType = existingType
		}
		if step.Name == "" {
			step.Name = existingName.String
		}
		if step.Status == "" {
			step.Status = existingStatus
		}
		if step.StartedAt.IsZero() {
			step.StartedAt = existingStarted
		}
		if step.EndedAt.IsZero() && existingEnded.Valid {
			step.EndedAt = existingEnded.Time
		}
		_, err = tx.ExecContext(ctx, `UPDATE steps SET parent_step_id=?, step_type=?, name=?, status=?, started_at=?, ended_at=?
			WHERE run_id=? AND step_id=?`,
			s.schema.nullID("steps", "parent_step_id", step.ParentStepID), step.StepType, nullStr(step.Name), step.Status,
			step.StartedAt, nullTime(step.EndedAt), physicalStepRunID, physicalStepID,
		)
	default:
		return err
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *stepStore) ListByRun(ctx context.Context, runID string) ([]*storage.Step, error) {
	scope := storage.ScopeFromLenient(ctx)
	rows, err := s.db.QueryContext(ctx, `SELECT s.step_id, s.run_id, s.parent_step_id,
		s.step_type, s.name, s.status, s.started_at, s.ended_at
		FROM steps s JOIN runs r ON r.run_id=s.run_id
		WHERE s.run_id=? AND (?='' OR r.tenant_id=?) ORDER BY s.started_at ASC`,
		s.schema.id("steps", "run_id", runID), scope.TenantID, scope.TenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*storage.Step
	for rows.Next() {
		var (
			step         storage.Step
			parent, name sql.NullString
			started      time.Time
			ended        sql.NullTime
		)
		if err := rows.Scan(&step.StepID, &step.RunID, &parent,
			&step.StepType, &name, &step.Status, &started, &ended); err != nil {
			return nil, err
		}
		step.ParentStepID, step.Name = parent.String, name.String
		step.StartedAt = started
		if ended.Valid {
			step.EndedAt = ended.Time
		}
		out = append(out, &step)
	}
	return out, rows.Err()
}

var _ storage.StepStore = (*stepStore)(nil)
