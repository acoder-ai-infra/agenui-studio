package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// sessionStore 是 SQLite SessionStore。Update 用事务 + 乐观 version 自增(CAS),
// List 走 keyset 分页(updated_at desc, session_id desc)。语义对齐 memory 后端。
type sessionStore struct {
	db *sql.DB
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
	if _, err := s.db.ExecContext(ctx, `INSERT INTO sessions
		(session_id, tenant_id, user_id, channel, agent_id, title, status, created_at, updated_at, version, metadata)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		clone.ID, clone.TenantID, nullStr(clone.UserID), nullStr(clone.Channel), nullStr(clone.AgentID),
		nullStr(clone.Title), clone.Status, tsVal(clone.CreatedAt), tsVal(clone.UpdatedAt), clone.Version,
		mapToJSON(clone.Metadata),
	); err != nil {
		if isUniqueViolation(err) {
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
		sess                        storage.Session
		user, channel, agent, title sql.NullString
		meta                        sql.NullString
		created, updated            int64
	)
	err := q.QueryRowContext(ctx, `SELECT session_id, tenant_id, user_id, channel, agent_id, title,
		status, created_at, updated_at, version, metadata FROM sessions WHERE session_id=?`, id).Scan(
		&sess.ID, &sess.TenantID, &user, &channel, &agent, &title,
		&sess.Status, &created, &updated, &sess.Version, &meta,
	)
	if err == sql.ErrNoRows {
		return nil, storage.NewError(storage.ErrNotFound, "session not found: "+id)
	}
	if err != nil {
		return nil, err
	}
	sess.UserID, sess.Channel, sess.AgentID, sess.Title = user.String, channel.String, agent.String, title.String
	sess.CreatedAt, sess.UpdatedAt = tsTime(created), tsTime(updated)
	sess.Metadata = jsonToMap(meta.String)
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
		curCreated int64
	)
	if err := tx.QueryRowContext(ctx, "SELECT tenant_id, version, created_at FROM sessions WHERE session_id=?", sess.ID).
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
	clone.CreatedAt = tsTime(curCreated)
	clone.UpdatedAt = time.Now()
	clone.Version = curVersion + 1
	if clone.Status == "" {
		clone.Status = storage.SessionStatusActive
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET user_id=?, channel=?, agent_id=?, title=?,
		status=?, updated_at=?, version=?, metadata=? WHERE session_id=?`,
		nullStr(clone.UserID), nullStr(clone.Channel), nullStr(clone.AgentID), nullStr(clone.Title),
		clone.Status, tsVal(clone.UpdatedAt), clone.Version, mapToJSON(clone.Metadata), clone.ID,
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
	if err := tx.QueryRowContext(ctx, "SELECT tenant_id FROM sessions WHERE session_id=?", id).Scan(&tenant); err == sql.ErrNoRows {
		return storage.NewError(storage.ErrNotFound, "session not found: "+id)
	} else if err != nil {
		return err
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(tenant); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE sessions SET status=?, updated_at=?, version=version+1 WHERE session_id=?",
		status, tsVal(time.Now()), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *sessionStore) List(ctx context.Context, q storage.SessionListQuery) (storage.SessionPage, error) {
	scope := storage.ScopeFromLenient(ctx)
	sb := `SELECT session_id, tenant_id, user_id, channel, agent_id, title, status,
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
	// keyset cursor: only applied when BeforeUpdatedAt is set (mirrors memory).
	if !q.BeforeUpdatedAt.IsZero() {
		cur := tsVal(q.BeforeUpdatedAt)
		sb += " AND (updated_at < ? OR (updated_at = ? AND session_id < ?))"
		args = append(args, cur, cur, q.BeforeSessionID)
	}
	sb += " ORDER BY updated_at DESC, session_id DESC"
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
			sess                        storage.Session
			user, channel, agent, title sql.NullString
			meta                        sql.NullString
			created, updated            int64
		)
		if err := rows.Scan(&sess.ID, &sess.TenantID, &user, &channel, &agent, &title,
			&sess.Status, &created, &updated, &sess.Version, &meta); err != nil {
			return storage.SessionPage{}, err
		}
		sess.UserID, sess.Channel, sess.AgentID, sess.Title = user.String, channel.String, agent.String, title.String
		sess.CreatedAt, sess.UpdatedAt = tsTime(created), tsTime(updated)
		sess.Metadata = jsonToMap(meta.String)
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

func mapToJSON(m map[string]string) any {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return string(b)
}

func jsonToMap(s string) map[string]string {
	if s == "" {
		return nil
	}
	var m map[string]string
	if json.Unmarshal([]byte(s), &m) != nil {
		return nil
	}
	return m
}

var _ storage.SessionStore = (*sessionStore)(nil)
