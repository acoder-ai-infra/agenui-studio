package mysql

import (
	"context"
	"database/sql"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// controlStatusPending / controlStatusExpired mirror internal/control's status
// strings without importing that package (control -> storage is the only allowed
// direction).
const (
	controlStatusPending = "pending"
	controlStatusExpired = "expired"
)

// controlStore is the MySQL ControlRequestStore. CompareAndAnswer does a
// transactional CAS + version bump (FOR UPDATE); ExpirePending batch-expires.
// created_at is DATETIME(3) NOT NULL, expires_at DATETIME(3) NULL.
type controlStore struct {
	db     *sql.DB
	schema physicalSchema
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
		c.schema.id("control_requests", "request_id", cr.RequestID), cr.RunID, cr.TenantID, nullStr(cr.CheckpointID), cr.Type, cr.Status,
		nullStr(cr.ResumeTokenHash), nullStr(cr.ToolUseID), nullStr(cr.PromptPreview),
		nullStr(cr.ResponseRef), cr.SchemaVersion, cr.CreatedAt, nullTime(cr.ExpiresAt), cr.Version,
	)
	if err != nil && isDuplicate(err) {
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

// CompareAndAnswer moves status from expectFrom to `to` under a row lock,
// bumping version and writing response_ref. Succeeds at most once.
func (c *controlStore) CompareAndAnswer(ctx context.Context, requestID, expectFrom, to, responseRef string) (*storage.ControlRequest, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var status, tenant string
	physicalRequestID := c.schema.id("control_requests", "request_id", requestID)
	if err := tx.QueryRowContext(ctx, "SELECT status, tenant_id FROM control_requests WHERE request_id=? FOR UPDATE", physicalRequestID).Scan(&status, &tenant); err == sql.ErrNoRows {
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
		to, nullStr(responseRef), physicalRequestID); err != nil {
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

// ExpirePending moves all pending requests with expires_at <= now to expired,
// returning the affected requests so the caller can drive their runs terminal.
func (c *controlStore) ExpirePending(ctx context.Context, now time.Time) ([]*storage.ControlRequest, error) {
	scope := storage.ScopeFromLenient(ctx)
	sb := `SELECT request_id, run_id, tenant_id, checkpoint_id, type, status, resume_token_hash,
		tool_use_id, prompt_preview, response_ref, schema_version, created_at, expires_at, version
		FROM control_requests WHERE status=? AND expires_at IS NOT NULL AND expires_at<=?`
	args := []any{controlStatusPending, now}
	if scope.TenantID != "" {
		sb += " AND tenant_id=?"
		args = append(args, scope.TenantID)
	}
	rows, err := c.db.QueryContext(ctx, sb, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []*storage.ControlRequest
	for rows.Next() {
		cr, err := scanControl(rows)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, cr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var expired []*storage.ControlRequest
	for _, cr := range candidates {
		result, err := c.db.ExecContext(ctx, `UPDATE control_requests SET status=?, version=version+1 WHERE request_id=? AND status=? AND expires_at IS NOT NULL AND expires_at<=?`,
			controlStatusExpired, c.schema.id("control_requests", "request_id", cr.RequestID), controlStatusPending, now)
		if err != nil {
			return nil, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if affected != 1 {
			continue
		}
		cr.Status = controlStatusExpired
		cr.Version++
		expired = append(expired, cr)
	}
	return expired, nil
}

func (c *controlStore) get(ctx context.Context, q queryRower, where string, args ...any) (*storage.ControlRequest, error) {
	var logicalRequestID string
	if where == "request_id=?" && len(args) == 1 {
		if id, ok := args[0].(string); ok {
			logicalRequestID = id
			args[0] = c.schema.id("control_requests", "request_id", id)
		}
	}
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
	if logicalRequestID != "" {
		cr.RequestID = logicalRequestID
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(cr.TenantID); err != nil {
		return nil, err
	}
	return cr, nil
}

func scanControl(s scanner) (*storage.ControlRequest, error) {
	var (
		cr                                       storage.ControlRequest
		checkpoint, resumeToken, toolUse, prompt sql.NullString
		responseRef                              sql.NullString
		created                                  time.Time
		expires                                  sql.NullTime
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
	cr.CreatedAt = created
	if expires.Valid {
		cr.ExpiresAt = expires.Time
	}
	return &cr, nil
}

var _ storage.ControlRequestStore = (*controlStore)(nil)
