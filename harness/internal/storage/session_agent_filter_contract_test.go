package storage_test

import (
	"context"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	"github.com/AGenUI/agenui-studio/harness/internal/storage/memory"
	storagemysql "github.com/AGenUI/agenui-studio/harness/internal/storage/mysql"
	storagesqlite "github.com/AGenUI/agenui-studio/harness/internal/storage/sqlite"
)

func runSessionAgentFilterContract(t *testing.T, store storage.SessionStore) {
	t.Helper()
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "tenant-chat"})
	base := time.Unix(1_720_000_000, 0).UTC()
	for i, session := range []*storage.Session{
		{ID: "chat-a-new", TenantID: "tenant-chat", UserID: "user-chat", AgentID: "agent-a", Status: storage.SessionStatusActive},
		{ID: "chat-a-old", TenantID: "tenant-chat", UserID: "user-chat", AgentID: "agent-a", Status: storage.SessionStatusActive},
		{ID: "chat-b", TenantID: "tenant-chat", UserID: "user-chat", AgentID: "agent-b", Status: storage.SessionStatusActive},
		{ID: "chat-a-other-user", TenantID: "tenant-chat", UserID: "other", AgentID: "agent-a", Status: storage.SessionStatusActive},
		{ID: "chat-a-archived", TenantID: "tenant-chat", UserID: "user-chat", AgentID: "agent-a", Status: storage.SessionStatusArchived},
	} {
		session.CreatedAt = base.Add(time.Duration(i) * time.Second)
		session.UpdatedAt = session.CreatedAt
		if err := store.Create(ctx, session); err != nil {
			t.Fatalf("create %s: %v", session.ID, err)
		}
	}

	page, err := store.List(ctx, storage.SessionListQuery{UserID: "user-chat", AgentID: "agent-a", Limit: 10})
	if err != nil {
		t.Fatalf("list agent-a: %v", err)
	}
	if len(page.Items) != 2 || page.Items[0].AgentID != "agent-a" || page.Items[1].AgentID != "agent-a" {
		t.Fatalf("agent filtered sessions = %#v", page.Items)
	}
	firstPage, err := store.List(ctx, storage.SessionListQuery{UserID: "user-chat", AgentID: "agent-a", Limit: 1})
	if err != nil || len(firstPage.Items) != 1 || !firstPage.HasMore {
		t.Fatalf("first filtered page = %#v, %v", firstPage, err)
	}
	secondPage, err := store.List(ctx, storage.SessionListQuery{
		UserID: "user-chat", AgentID: "agent-a", Limit: 1,
		BeforeUpdatedAt: firstPage.NextBeforeUpdatedAt, BeforeSessionID: firstPage.NextBeforeSessionID,
	})
	if err != nil || len(secondPage.Items) != 1 || secondPage.Items[0].ID == firstPage.Items[0].ID {
		t.Fatalf("second filtered page = %#v, %v", secondPage, err)
	}
	withArchived, err := store.List(ctx, storage.SessionListQuery{UserID: "user-chat", AgentID: "agent-a", Limit: 10, IncludeArchived: true})
	if err != nil || len(withArchived.Items) != 3 {
		t.Fatalf("filtered archived page = %#v, %v", withArchived, err)
	}

	all, err := store.List(ctx, storage.SessionListQuery{UserID: "user-chat", Limit: 10})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all.Items) != 3 {
		t.Fatalf("empty AgentID changed legacy list: got %d want 3", len(all.Items))
	}

	missing, err := store.List(ctx, storage.SessionListQuery{UserID: "user-chat", AgentID: "missing", Limit: 10})
	if err != nil || len(missing.Items) != 0 {
		t.Fatalf("missing agent page = %#v, %v", missing, err)
	}
}

func TestSessionAgentFilterContract_Memory(t *testing.T) {
	runSessionAgentFilterContract(t, memory.New().Stores().Sessions)
}

func TestSessionAgentFilterContract_SQLite(t *testing.T) {
	backend, err := storagesqlite.Open(filepath.Join(t.TempDir(), "agent-filter.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	if err := backend.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	runSessionAgentFilterContract(t, backend.Stores().Sessions)
}

func TestSessionAgentFilterContract_MySQLQuery(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	query := regexp.QuoteMeta("SELECT id, tenant_id, user_id, channel, agent_id, title, status,") +
		`[\s\S]*FROM sessions WHERE 1=1 AND tenant_id=\? AND user_id=\? AND agent_id=\? AND status=\?[\s\S]*ORDER BY updated_at DESC, id DESC LIMIT \?`
	mock.ExpectQuery(query).
		WithArgs("tenant-chat", "user-chat", "agent-a", storage.SessionStatusActive, 11).
		WillReturnRows(sqlmock.NewRows([]string{"id", "tenant_id", "user_id", "channel", "agent_id", "title", "status", "created_at", "updated_at", "version", "metadata"}))

	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "tenant-chat"})
	store := storagemysql.New(db).Stores().Sessions
	if _, err := store.List(ctx, storage.SessionListQuery{UserID: "user-chat", AgentID: "agent-a", Limit: 10}); err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func openTurnForAgent(sessionID, agentID string) storage.OpenTurnCommand {
	return storage.OpenTurnCommand{
		Session: storage.Session{ID: sessionID, TenantID: "tenant-chat", UserID: "user-chat", AgentID: agentID},
		Run:     storage.Run{RunID: "run-" + agentID, SessionID: sessionID, TurnID: "turn-" + agentID, TenantID: "tenant-chat", AgentID: agentID},
		Message: storage.Message{ID: "message-" + agentID, SessionID: sessionID, TurnID: "turn-" + agentID, RunID: "run-" + agentID, TenantID: "tenant-chat", Role: "user", ContentPreview: "hello"},
		Events:  []observability.AgentEvent{{RunID: "run-" + agentID, SessionID: sessionID, AgentID: agentID, EventType: observability.EventRunCreated, Visibility: observability.VisibilityDebug}},
	}
}

func runOpenTurnAgentBindingContract(t *testing.T, stores storage.Stores) {
	t.Helper()
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "tenant-chat", UserID: "user-chat"})
	if err := stores.Sessions.Create(ctx, &storage.Session{
		ID: "bound-session", TenantID: "tenant-chat", UserID: "user-chat", AgentID: "agent-a", Status: storage.SessionStatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := stores.Turns.Commit(ctx, openTurnForAgent("bound-session", "agent-b")); !storage.IsErrorCode(err, storage.ErrPermissionDenied) {
		t.Fatalf("cross-agent Commit() error = %v, want permission denied", err)
	}
	if runs, err := stores.Runs.ListBySession(ctx, "bound-session"); err != nil || len(runs) != 0 {
		t.Fatalf("cross-agent commit persisted runs=%#v err=%v", runs, err)
	}
}

func TestOpenTurnAgentBindingContract_Memory(t *testing.T) {
	runOpenTurnAgentBindingContract(t, memory.New().Stores())
}

func TestOpenTurnAgentBindingContract_SQLite(t *testing.T) {
	backend, err := storagesqlite.Open(filepath.Join(t.TempDir(), "agent-binding.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	if err := backend.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	runOpenTurnAgentBindingContract(t, backend.Stores())
}

func TestOpenTurnAgentBindingContract_MySQL(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, tenant_id, user_id, channel, agent_id, title,
		status, created_at, updated_at, version, metadata FROM sessions WHERE id=?`)).
		WithArgs("bound-session").
		WillReturnRows(sqlmock.NewRows([]string{"id", "tenant_id", "user_id", "channel", "agent_id", "title", "status", "created_at", "updated_at", "version", "metadata"}).
			AddRow("bound-session", "tenant-chat", "user-chat", nil, "agent-a", nil, storage.SessionStatusActive, now, now, 1, nil))
	mock.ExpectRollback()
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "tenant-chat", UserID: "user-chat"})
	_, err = storagemysql.New(db).Stores().Turns.Commit(ctx, openTurnForAgent("bound-session", "agent-b"))
	if !storage.IsErrorCode(err, storage.ErrPermissionDenied) {
		t.Fatalf("cross-agent Commit() error = %v, want permission denied", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
