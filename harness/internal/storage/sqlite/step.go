package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// stepStore 是 SQLite StepStore。主键 (run_id, step_id) 实现 upsert。
// Upsert 时若 StartedAt 为零则保留已有值或填入当前时间。语义对齐 memory 后端。
type stepStore struct {
	db *sql.DB
}

func (s *stepStore) Upsert(ctx context.Context, step *storage.Step) error {
	if step == nil || step.StepID == "" || step.RunID == "" {
		return storage.NewError(storage.ErrInvalidArgument, "step_id and run_id required")
	}
	if err := storage.ValidateStepIdentifiers(step); err != nil {
		return err
	}
	if step.StartedAt.IsZero() {
		// 查已有记录的 started_at;若不存在则用当前时间
		var existing int64
		err := s.db.QueryRowContext(ctx, "SELECT started_at FROM steps WHERE run_id=? AND step_id=?", step.RunID, step.StepID).Scan(&existing)
		if err == sql.ErrNoRows {
			step.StartedAt = time.Now()
		} else if err != nil {
			return err
		} else {
			step.StartedAt = tsTime(existing)
		}
	}
	// Step 结构体没有 TenantID 字段(对齐 memory 后端);schema 中 tenant_id 列存 NULL。
	_, err := s.db.ExecContext(ctx, `INSERT INTO steps
		(step_id, run_id, parent_step_id, tenant_id, step_type, name, status, started_at, ended_at)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(run_id, step_id) DO UPDATE SET
			parent_step_id=COALESCE(excluded.parent_step_id, steps.parent_step_id),
			step_type=CASE WHEN excluded.step_type <> '' THEN excluded.step_type ELSE steps.step_type END,
			name=COALESCE(excluded.name, steps.name),
			status=excluded.status,
			started_at=excluded.started_at,
			ended_at=CASE WHEN excluded.ended_at > 0 THEN excluded.ended_at ELSE steps.ended_at END`,
		step.StepID, step.RunID, nullStr(step.ParentStepID), nil,
		step.StepType, nullStr(step.Name), step.Status, tsVal(step.StartedAt), tsVal(step.EndedAt),
	)
	return err
}

func (s *stepStore) ListByRun(ctx context.Context, runID string) ([]*storage.Step, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT step_id, run_id, parent_step_id,
		step_type, name, status, started_at, ended_at FROM steps WHERE run_id=? ORDER BY started_at ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*storage.Step
	for rows.Next() {
		var (
			step           storage.Step
			parent, name   sql.NullString
			started, ended int64
		)
		if err := rows.Scan(&step.StepID, &step.RunID, &parent,
			&step.StepType, &name, &step.Status, &started, &ended); err != nil {
			return nil, err
		}
		step.ParentStepID, step.Name = parent.String, name.String
		step.StartedAt, step.EndedAt = tsTime(started), tsTime(ended)
		out = append(out, &step)
	}
	return out, rows.Err()
}

var _ storage.StepStore = (*stepStore)(nil)
