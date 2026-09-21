package mysql

import (
	"context"
	"database/sql"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// sessionStore is the MySQL SessionStore. Update uses a transactional read-lock
// + optimistic version bump (CAS); List uses keyset pagination
// (updated_at desc, id desc). Semantics mirror the memory/sqlite backends.
//
// NOTE: the sessions table's primary key column is `id` (not session_id).
// The store receives the caller-owned shared pool created by the structured
// framework/mysql DBInit, which owns driver time parsing for DATETIME(3).
type sessionStore struct {
	db     *sql.DB
	schema physicalSchema
}

func (s *sessionStore) Create(ctx context.Context, sess *storage.Session) error {
	if sess == nil || sess.ID == "" {
		return storage.NewError(storage.ErrInvalidArgument, "session id required")
	}
	scope := storage.ScopeFromLenient(ctx)
	if sess.TenantID == "" {
		sess.TenantID = scope.TenantID
	}
	if err := storage.ValidateSessionIdentifiers(sess); err != nil {
		return err
	}
	if err := scope.EnforceTenant(sess.TenantID); err != nil {
		return err
	}
	now := time.Now()
	clone := *sess
	if clone.Status == "" {
		clone.Status = storage.SessionStatusActive
	}
	if clone.CreatedAt.IsZero() {
		clone.CreatedAt = now
	}
	clone.UpdatedAt = now
	clone.Version = 1
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions
		(id, tenant_id, user_id, channel, agent_id, title, status, created_at, updated_at, version, metadata)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		s.schema.id("sessions", "id", clone.ID), clone.TenantID, clone.UserID, nullStr(clone.Channel), nullStr(clone.AgentID),
		nullStr(clone.Title), clone.Status, clone.CreatedAt, clone.UpdatedAt, clone.Version,
		metaToJSON(clone.Metadata),
	)
	if err != nil {
		if isDuplicate(err) {
			return storage.NewError(storage.ErrConflict, "session already exists: "+sess.ID)
		}
		return err
	}
	*sess = clone
	return nil
}

func (s *sessionStore) Get(ctx context.Context, id string) (*storage.Session, error) {
	return s.get(ctx, s.db, id)
}

func (s *sessionStore) get(ctx context.Context, q queryRower, id string) (*storage.Session, error) {
	var (
		sess                  storage.Session
		channel, agent, title sql.NullString
		meta                  []byte
		created, updated      time.Time
	)
	err := q.QueryRowContext(ctx, `SELECT id, tenant_id, user_id, channel, agent_id, title,
		status, created_at, updated_at, version, metadata FROM sessions WHERE id=?`, s.schema.id("sessions", "id", id)).Scan(
		&sess.ID, &sess.TenantID, &sess.UserID, &channel, &agent, &title,
		&sess.Status, &created, &updated, &sess.Version, &meta,
	)
	if err == sql.ErrNoRows {
		return nil, storage.NewError(storage.ErrNotFound, "session not found: "+id)
	}
	if err != nil {
		return nil, err
	}
	sess.ID = id
	sess.Channel, sess.AgentID, sess.Title = channel.String, agent.String, title.String
	sess.CreatedAt, sess.UpdatedAt = created, updated
	sess.Metadata = jsonToMeta(meta)
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(sess.TenantID); err != nil {
		return nil, err
	}
	return &sess, nil
}

func (s *sessionStore) Update(ctx context.Context, sess *storage.Session) error {
	if sess == nil || sess.ID == "" {
		return storage.NewError(storage.ErrInvalidArgument, "session id required")
	}
	if err := storage.ValidateSessionIdentifiers(sess); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		curTenant  string
		curVersion int64
		curCreated time.Time
	)
	physicalID := s.schema.id("sessions", "id", sess.ID)
	if err := tx.QueryRowContext(ctx, "SELECT tenant_id, version, created_at FROM sessions WHERE id=? FOR UPDATE", physicalID).
		Scan(&curTenant, &curVersion, &curCreated); err == sql.ErrNoRows {
		return storage.NewError(storage.ErrNotFound, "session not found: "+sess.ID)
	} else if err != nil {
		return err
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(curTenant); err != nil {
		return err
	}
	if sess.Version != 0 && sess.Version != curVersion {
		return storage.NewError(storage.ErrCASMismatch, "session version mismatch")
	}
	clone := *sess
	clone.TenantID = curTenant
	clone.CreatedAt = curCreated
	clone.UpdatedAt = time.Now()
	clone.Version = curVersion + 1
	if clone.Status == "" {
		clone.Status = storage.SessionStatusActive
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET user_id=?, channel=?, agent_id=?, title=?,
		status=?, updated_at=?, version=?, metadata=? WHERE id=?`,
		clone.UserID, nullStr(clone.Channel), nullStr(clone.AgentID), nullStr(clone.Title),
		clone.Status, clone.UpdatedAt, clone.Version, metaToJSON(clone.Metadata), physicalID,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	*sess = clone
	return nil
}

func (s *sessionStore) Archive(ctx context.Context, id string) error {
	return s.setStatus(ctx, id, storage.SessionStatusArchived)
}

func (s *sessionStore) SoftDelete(ctx context.Context, id string) error {
	return s.setStatus(ctx, id, storage.SessionStatusDeleted)
}

func (s *sessionStore) setStatus(ctx context.Context, id, status string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var tenant string
	physicalID := s.schema.id("sessions", "id", id)
	if err := tx.QueryRowContext(ctx, "SELECT tenant_id FROM sessions WHERE id=? FOR UPDATE", physicalID).Scan(&tenant); err == sql.ErrNoRows {
		return storage.NewError(storage.ErrNotFound, "session not found: "+id)
	} else if err != nil {
		return err
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(tenant); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE sessions SET status=?, updated_at=?, version=version+1 WHERE id=?",
		status, time.Now(), physicalID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *sessionStore) List(ctx context.Context, q storage.SessionListQuery) (storage.SessionPage, error) {
	scope := storage.ScopeFromLenient(ctx)
	sb := `SELECT id, tenant_id, user_id, channel, agent_id, title, status,
		created_at, updated_at, version, metadata FROM sessions WHERE 1=1`
	var args []any
	if scope.TenantID != "" {
		sb += " AND tenant_id=?"
		args = append(args, scope.TenantID)
	}
	if q.UserID != "" {
		sb += " AND user_id=?"
		args = append(args, q.UserID)
	}
	if q.AgentID != "" {
		sb += " AND agent_id=?"
		args = append(args, q.AgentID)
	}
	if !q.IncludeArchived {
		sb += " AND status=?"
		args = append(args, storage.SessionStatusActive)
	}
	if !q.BeforeUpdatedAt.IsZero() {
		sb += " AND (updated_at < ? OR (updated_at = ? AND id < ?))"
		args = append(args, q.BeforeUpdatedAt, q.BeforeUpdatedAt, s.schema.id("sessions", "id", q.BeforeSessionID))
	}
	sb += " ORDER BY updated_at DESC, id DESC"
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	sb += " LIMIT ?"
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, sb, args...)
	if err != nil {
		return storage.SessionPage{}, err
	}
	defer rows.Close()
	var items []*storage.Session
	for rows.Next() {
		var (
			sess                  storage.Session
			channel, agent, title sql.NullString
			meta                  []byte
			created, updated      time.Time
		)
		if err := rows.Scan(&sess.ID, &sess.TenantID, &sess.UserID, &channel, &agent, &title,
			&sess.Status, &created, &updated, &sess.Version, &meta); err != nil {
			return storage.SessionPage{}, err
		}
		sess.Channel, sess.AgentID, sess.Title = channel.String, agent.String, title.String
		sess.CreatedAt, sess.UpdatedAt = created, updated
		sess.Metadata = jsonToMeta(meta)
		items = append(items, &sess)
	}
	if err := rows.Err(); err != nil {
		return storage.SessionPage{}, err
	}
	page := storage.SessionPage{}
	if len(items) > limit {
		items = items[:limit]
		page.HasMore = true
	}
	page.Items = items
	if n := len(items); n > 0 {
		page.NextBeforeUpdatedAt = items[n-1].UpdatedAt
		page.NextBeforeSessionID = items[n-1].ID
	}
	return page, nil
}

var _ storage.SessionStore = (*sessionStore)(nil)
