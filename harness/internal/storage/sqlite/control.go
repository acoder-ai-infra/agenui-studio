package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// controlStatusPending / controlStatusExpired 镜像 internal/control 的状态字符串,
// 不导入该包(control -> storage 是唯一允许方向)。
const (
	controlStatusPending = "pending"
	controlStatusExpired = "expired"
)

// controlStore 是 SQLite ControlRequestStore。Create 唯一约束兜底冲突;
// CompareAndAnswer 在事务内做 CAS + version 自增;ExpirePending 批量过期。
// 语义对齐 memory 后端。
type controlStore struct {
	db *sql.DB
}

func (c *controlStore) Create(ctx context.Context, cr *storage.ControlRequest) error {
	if cr == nil || cr.RequestID == "" || cr.RunID == "" {
		return storage.NewError(storage.ErrInvalidArgument, "request_id and run_id required")
	}
	scope := storage.ScopeFromLenient(ctx)
	if cr.TenantID == "" {
		cr.TenantID = scope.TenantID
	}
	if err := storage.ValidateControlRequestIdentifiers(cr); err != nil {
		return err
	}
	if err := scope.EnforceTenant(cr.TenantID); err != nil {
		return err
	}
	if cr.Status == "" {
		cr.Status = controlStatusPending
	}
	if cr.SchemaVersion == "" {
		cr.SchemaVersion = storage.ControlRequestSchemaVersion
	}
	if cr.CreatedAt.IsZero() {
		cr.CreatedAt = time.Now()
	}
	cr.Version = 1
	_, err := c.db.ExecContext(ctx, `INSERT INTO control_requests
		(request_id, run_id, tenant_id, checkpoint_id, type, status, resume_token_hash, tool_use_id,
		 prompt_preview, response_ref, schema_version, created_at, expires_at, version)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		cr.RequestID, cr.RunID, cr.TenantID, nullStr(cr.CheckpointID), cr.Type, cr.Status,
		nullStr(cr.ResumeTokenHash), nullStr(cr.ToolUseID), nullStr(cr.PromptPreview),
		nullStr(cr.ResponseRef), cr.SchemaVersion, tsVal(cr.CreatedAt), tsVal(cr.ExpiresAt), cr.Version,
	)
	if err != nil && isUniqueViolation(err) {
		return storage.NewError(storage.ErrConflict, "control request already exists: "+cr.RequestID)
	}
	return err
}

func (c *controlStore) Get(ctx context.Context, requestID string) (*storage.ControlRequest, error) {
	return c.get(ctx, c.db, "request_id=?", requestID)
}

func (c *controlStore) ListByRun(ctx context.Context, runID string) ([]*storage.ControlRequest, error) {
	if runID == "" {
		return nil, storage.NewError(storage.ErrInvalidArgument, "run_id required")
	}
	scope := storage.ScopeFromLenient(ctx)
	query := `SELECT request_id, run_id, tenant_id, checkpoint_id, type, status, resume_token_hash,
		tool_use_id, prompt_preview, response_ref, schema_version, created_at, expires_at, version
		FROM control_requests WHERE run_id=?`
	args := []any{runID}
	if scope.TenantID != "" {
		query += " AND tenant_id=?"
		args = append(args, scope.TenantID)
	}
	query += " ORDER BY created_at ASC, request_id ASC"
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*storage.ControlRequest
	for rows.Next() {
		cr, err := scanControl(rows)
		if err != nil {
			return nil, err
		}
		if err := scope.EnforceTenant(cr.TenantID); err != nil {
			return nil, err
		}
		out = append(out, cr)
	}
	return out, rows.Err()
}

// CompareAndAnswer 在事务内做 CAS:status 从 expectFrom 迁移到 to,
// 成功则 version+1 并写入 response_ref。至多成功一次。
func (c *controlStore) CompareAndAnswer(ctx context.Context, requestID, expectFrom, to, responseRef string) (*storage.ControlRequest, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var status, tenant string
	if err := tx.QueryRowContext(ctx, "SELECT status, tenant_id FROM control_requests WHERE request_id=?", requestID).Scan(&status, &tenant); err == sql.ErrNoRows {
		return nil, storage.NewError(storage.ErrNotFound, "control request not found: "+requestID)
	} else if err != nil {
		return nil, err
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(tenant); err != nil {
		return nil, err
	}
	if status != expectFrom {
		return nil, storage.NewError(storage.ErrCASMismatch, "control request status is "+status+", expected "+expectFrom)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE control_requests SET status=?, response_ref=?, version=version+1 WHERE request_id=?`,
		to, nullStr(responseRef), requestID); err != nil {
		return nil, err
	}
	cr, err := c.get(ctx, tx, "request_id=?", requestID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return cr, nil
}

// ExpirePending 将所有 ExpiresAt <= now 且 status=pending 的请求迁移到 expired,
// 返回受影响的请求列表(调用方可据此发事件并驱动 run 终态)。
func (c *controlStore) ExpirePending(ctx context.Context, now time.Time) ([]*storage.ControlRequest, error) {
	scope := storage.ScopeFromLenient(ctx)
	sb := `SELECT request_id, run_id, tenant_id, checkpoint_id, type, status, resume_token_hash,
		tool_use_id, prompt_preview, response_ref, schema_version, created_at, expires_at, version
		FROM control_requests WHERE status=? AND expires_at<>0 AND expires_at<=?`
	args := []any{controlStatusPending, tsVal(now)}
	if scope.TenantID != "" {
		sb += " AND tenant_id=?"
		args = append(args, scope.TenantID)
	}
	rows, err := c.db.QueryContext(ctx, sb, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var expired []*storage.ControlRequest
	var ids []string
	for rows.Next() {
		cr, err := scanControl(rows)
		if err != nil {
			return nil, err
		}
		expired = append(expired, cr)
		ids = append(ids, cr.RequestID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := c.db.ExecContext(ctx, "UPDATE control_requests SET status=?, version=version+1 WHERE request_id=?",
			controlStatusExpired, id); err != nil {
			return nil, err
		}
	}
	// 更新返回的内存对象状态
	for _, cr := range expired {
		cr.Status = controlStatusExpired
		cr.Version++
	}
	return expired, nil
}

func (c *controlStore) get(ctx context.Context, q queryRower, where string, args ...any) (*storage.ControlRequest, error) {
	row := q.QueryRowContext(ctx, `SELECT request_id, run_id, tenant_id, checkpoint_id, type, status, resume_token_hash,
		tool_use_id, prompt_preview, response_ref, schema_version, created_at, expires_at, version
		FROM control_requests WHERE `+where, args...)
	cr, err := scanControl(row)
	if err == sql.ErrNoRows {
		return nil, storage.NewError(storage.ErrNotFound, "control request not found")
	}
	if err != nil {
		return nil, err
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(cr.TenantID); err != nil {
		return nil, err
	}
	return cr, nil
}

type controlScanner interface{ Scan(dest ...any) error }

func scanControl(s controlScanner) (*storage.ControlRequest, error) {
	var (
		cr                                       storage.ControlRequest
		checkpoint, resumeToken, toolUse, prompt sql.NullString
		responseRef                              sql.NullString
		created, expires                         int64
	)
	if err := s.Scan(
		&cr.RequestID, &cr.RunID, &cr.TenantID, &checkpoint, &cr.Type, &cr.Status,
		&resumeToken, &toolUse, &prompt, &responseRef, &cr.SchemaVersion,
		&created, &expires, &cr.Version,
	); err != nil {
		return nil, err
	}
	cr.CheckpointID, cr.ResumeTokenHash, cr.ToolUseID = checkpoint.String, resumeToken.String, toolUse.String
	cr.PromptPreview, cr.ResponseRef = prompt.String, responseRef.String
	cr.CreatedAt, cr.ExpiresAt = tsTime(created), tsTime(expires)
	return &cr, nil
}

var _ storage.ControlRequestStore = (*controlStore)(nil)
