package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol/server"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

func TestMessageHistoryInitialAndBeforePagesAreContinuous(t *testing.T) {
	deps, stores := newDeps()
	mustCreateOwnedSessionRun(t, stores, "history", "", "acme", "u1")
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "acme", UserID: "u1"})
	base := time.Unix(1_700_000_000, 0).UTC()
	for i, id := range []string{"m1", "m2", "m3", "m4", "m5"} {
		if err := stores.Messages.Append(ctx, &storage.Message{
			ID: id, SessionID: "history", TenantID: "acme", Role: "user",
			Visibility: observability.VisibilityUserVisible, ContentPreview: id,
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}

	router := server.NewRouter(deps)
	page := func(rawURL string) struct {
		Messages []*storage.Message `json:"messages"`
		HasMore  bool               `json:"has_more"`
		Before   string             `json:"next_before_message_id"`
		After    string             `json:"next_after_message_id"`
	} {
		req := withIdentity(httptest.NewRequest(http.MethodGet, rawURL, nil), "acme", "u1")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", rawURL, rec.Code, rec.Body.String())
		}
		var out struct {
			Messages []*storage.Message `json:"messages"`
			HasMore  bool               `json:"has_more"`
			Before   string             `json:"next_before_message_id"`
			After    string             `json:"next_after_message_id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	latest := page("/api/v1/sessions/history/messages?limit=3")
	assertMessageIDs(t, latest.Messages, "m3", "m4", "m5")
	if !latest.HasMore || latest.Before != "m3" || latest.After != "m5" {
		t.Fatalf("latest cursors: %#v", latest)
	}
	older := page("/api/v1/sessions/history/messages?limit=3&before_message_id=" + latest.Before)
	assertMessageIDs(t, older.Messages, "m1", "m2")
	if older.HasMore || older.After != "m2" {
		t.Fatalf("older cursors: %#v", older)
	}
}

func TestSessionListIncludesOnlyActiveTopLevelRunStatusWhenRequested(t *testing.T) {
	deps, stores := newDeps()
	ctx := context.Background()
	for _, session := range []*storage.Session{
		{ID: "session-running", TenantID: "acme", UserID: "u1", AgentID: "agenui_agent", Title: "running", Status: storage.SessionStatusActive},
		{ID: "session-completed", TenantID: "acme", UserID: "u1", AgentID: "agenui_agent", Title: "completed", Status: storage.SessionStatusActive},
	} {
		if err := stores.Sessions.Create(ctx, session); err != nil {
			t.Fatalf("create session %s: %v", session.ID, err)
		}
	}
	for _, run := range []*storage.Run{
		{RunID: "run-running", SessionID: "session-running", TenantID: "acme", Status: storage.RunStatusRunning},
		{RunID: "run-completed", SessionID: "session-completed", TenantID: "acme", Status: storage.RunStatusCompleted},
		// A child Run never owns the conversation-level running indicator.
		{RunID: "run-child", SessionID: "session-completed", ParentRunID: "run-completed", TenantID: "acme", Status: storage.RunStatusRunning},
	} {
		if err := stores.Runs.Create(ctx, run); err != nil {
			t.Fatalf("create run %s: %v", run.RunID, err)
		}
	}

	req := withIdentity(httptest.NewRequest(
		http.MethodGet,
		"/api/v1/sessions?agent_id=agenui_agent&include_active_run=true",
		nil,
	), "acme", "u1")
	rec := httptest.NewRecorder()
	server.NewRouter(deps).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Sessions []struct {
			ID              string            `json:"id"`
			ActiveRunStatus storage.RunStatus `json:"active_run_status"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	statusBySession := make(map[string]storage.RunStatus, len(response.Sessions))
	for _, session := range response.Sessions {
		statusBySession[session.ID] = session.ActiveRunStatus
	}
	if got := statusBySession["session-running"]; got != storage.RunStatusRunning {
		t.Fatalf("running session status=%q", got)
	}
	if got := statusBySession["session-completed"]; got != "" {
		t.Fatalf("completed session unexpectedly active: %q", got)
	}
}

func assertMessageIDs(t *testing.T, messages []*storage.Message, want ...string) {
	t.Helper()
	if len(messages) != len(want) {
		t.Fatalf("message count=%d want=%d", len(messages), len(want))
	}
	for i := range want {
		if messages[i].ID != want[i] {
			t.Fatalf("message[%d]=%s want=%s", i, messages[i].ID, want[i])
		}
	}
}
