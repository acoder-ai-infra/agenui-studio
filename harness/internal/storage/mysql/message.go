package mysql

import (
	"context"
	"database/sql"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// messageStore is the MySQL MessageStore. Append inserts one message; List uses
// keyset pagination (created_at asc, id asc). Semantics mirror memory/sqlite.
// The messages primary key column is `id`; created_at is DATETIME(3).
type messageStore struct {
	db     *sql.DB
	schema physicalSchema
}

func (m *messageStore) Append(ctx context.Context, msg *storage.Message) error {
	if msg == nil || msg.ID == "" || msg.SessionID == "" {
		return storage.NewError(storage.ErrInvalidArgument, "message id and session_id required")
	}
	scope := storage.ScopeFromLenient(ctx)
	if msg.TenantID == "" {
		msg.TenantID = scope.TenantID
	}
	if err := storage.ValidateMessageIdentifiers(msg); err != nil {
		return err
	}
	if err := scope.EnforceTenant(msg.TenantID); err != nil {
		return err
	}
	if msg.Visibility == "" {
		msg.Visibility = observability.VisibilityUserVisible
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	_, err := m.db.ExecContext(ctx, `INSERT INTO messages
		(id, session_id, turn_id, run_id, tenant_id, role, visibility, content_ref, content_preview, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		m.schema.id("messages", "id", msg.ID), msg.SessionID, nullStr(msg.TurnID), nullStr(msg.RunID), msg.TenantID,
		msg.Role, string(msg.Visibility), nullStr(msg.ContentRef), nullStr(msg.ContentPreview),
		msg.CreatedAt,
	)
	if err != nil && isDuplicate(err) {
		return storage.NewError(storage.ErrConflict, "message already exists: "+msg.ID)
	}
	return err
}

func (m *messageStore) Get(ctx context.Context, id string) (*storage.Message, error) {
	scope := storage.ScopeFromLenient(ctx)
	query := `SELECT id, session_id, turn_id, run_id, tenant_id, role, visibility,
		content_ref, content_preview, created_at FROM messages WHERE id=?`
	args := []any{m.schema.id("messages", "id", id)}
	if scope.TenantID != "" {
		query += " AND tenant_id=?"
		args = append(args, scope.TenantID)
	}
	message, err := scanMessage(m.db.QueryRowContext(ctx, query, args...))
	if err == sql.ErrNoRows {
		return nil, storage.NewError(storage.ErrNotFound, "message not found: "+id)
	}
	if err == nil && message != nil {
		message.ID = id
	}
	return message, err
}

func (m *messageStore) List(ctx context.Context, q storage.MessageListQuery) (storage.MessagePage, error) {
	scope := storage.ScopeFromLenient(ctx)
	sb := `SELECT id, session_id, turn_id, run_id, tenant_id, role, visibility,
		content_ref, content_preview, created_at FROM messages WHERE session_id=?`
	args := []any{q.SessionID}
	if scope.TenantID != "" {
		sb += " AND tenant_id=?"
		args = append(args, scope.TenantID)
	}
	// visibility filter: default to user_visible only.
	vis := q.Visibilities
	if len(vis) == 0 {
		vis = []observability.EventVisibility{observability.VisibilityUserVisible}
	}
	sb += " AND visibility IN (" + placeholders(len(vis)) + ")"
	for _, v := range vis {
		args = append(args, string(v))
	}
	if q.AfterMessageID != "" {
		sb += " AND (created_at > (SELECT created_at FROM messages WHERE id=?) OR (created_at = (SELECT created_at FROM messages WHERE id=?) AND id > ?))"
		physicalID := m.schema.id("messages", "id", q.AfterMessageID)
		args = append(args, physicalID, physicalID, physicalID)
	}
	if q.BeforeMessageID != "" {
		sb += " AND (created_at < (SELECT created_at FROM messages WHERE id=?) OR (created_at = (SELECT created_at FROM messages WHERE id=?) AND id < ?))"
		physicalID := m.schema.id("messages", "id", q.BeforeMessageID)
		args = append(args, physicalID, physicalID, physicalID)
	}
	// Initial history and before_message_id both select the newest matching
	// window, then reverse it to chronological order for direct rendering.
	// after_message_id remains an ascending forward-sync query.
	descending := q.BeforeMessageID != "" || q.AfterMessageID == ""
	if descending {
		sb += " ORDER BY created_at DESC, id DESC"
	} else {
		sb += " ORDER BY created_at ASC, id ASC"
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	sb += " LIMIT ?"
	args = append(args, limit+1)

	rows, err := m.db.QueryContext(ctx, sb, args...)
	if err != nil {
		return storage.MessagePage{}, err
	}
	defer rows.Close()
	var items []*storage.Message
	for rows.Next() {
		msg, err := scanMessage(rows)
		if err != nil {
			return storage.MessagePage{}, err
		}
		items = append(items, msg)
	}
	if err := rows.Err(); err != nil {
		return storage.MessagePage{}, err
	}
	page := storage.MessagePage{}
	if len(items) > limit {
		items = items[:limit]
		page.HasMore = true
	}
	if descending {
		for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
			items[i], items[j] = items[j], items[i]
		}
	}
	page.Items = items
	if n := len(items); n > 0 {
		page.NextBeforeMessageID = items[0].ID
		page.NextAfterMessageID = items[n-1].ID
	}
	return page, nil
}

func (m *messageStore) ListRecent(ctx context.Context, sessionID string, limit int) ([]*storage.Message, error) {
	if limit <= 0 {
		limit = 200
	}
	scope := storage.ScopeFromLenient(ctx)
	query := `SELECT id, session_id, turn_id, run_id, tenant_id, role, visibility,
		content_ref, content_preview, created_at FROM messages WHERE session_id=? AND visibility=?`
	args := []any{sessionID, string(observability.VisibilityUserVisible)}
	if scope.TenantID != "" {
		query += " AND tenant_id=?"
		args = append(args, scope.TenantID)
	}
	query += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*storage.Message
	for rows.Next() {
		message, scanErr := scanMessage(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, message)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

func (m *messageStore) GetByIDs(ctx context.Context, sessionID string, ids []string) ([]*storage.Message, error) {
	out := make([]*storage.Message, 0, len(ids))
	for _, id := range ids {
		message, err := m.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if message.SessionID != sessionID {
			return nil, storage.NewError(storage.ErrNotFound, "message does not belong to session: "+id)
		}
		out = append(out, message)
	}
	return out, nil
}

func scanMessage(s scanner) (*storage.Message, error) {
	var (
		msg                     storage.Message
		turn, runID, contentRef sql.NullString
		preview                 sql.NullString
		role, visibility        string
		created                 time.Time
	)
	if err := s.Scan(
		&msg.ID, &msg.SessionID, &turn, &runID, &msg.TenantID, &role, &visibility,
		&contentRef, &preview, &created,
	); err != nil {
		return nil, err
	}
	msg.TurnID, msg.RunID, msg.ContentRef = turn.String, runID.String, contentRef.String
	msg.ContentPreview = preview.String
	msg.Role = role
	msg.Visibility = observability.EventVisibility(visibility)
	msg.CreatedAt = created
	return &msg, nil
}

var _ storage.MessageStore = (*messageStore)(nil)
