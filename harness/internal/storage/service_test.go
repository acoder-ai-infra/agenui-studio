package storage_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	"github.com/AGenUI/agenui-studio/harness/internal/storage/memory"
)

type fakeBuilder struct {
	ref  string
	err  error
	reqs *[]storage.SnapshotBuildRequest
}

func (f fakeBuilder) Build(_ context.Context, req storage.SnapshotBuildRequest) (string, error) {
	if f.reqs != nil {
		*f.reqs = append(*f.reqs, req)
	}
	return f.ref, f.err
}

func ctxT(tenant string) context.Context {
	return observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: tenant, TraceID: "trace_x"})
}

func eventTypes(evs []observability.AgentEvent) []observability.EventType {
	out := make([]observability.EventType, len(evs))
	for i, e := range evs {
		out[i] = e.EventType
	}
	return out
}

// execution §1: canonical write ordering, snapshot generated before dispatch (D5).
func TestOpenTurnWriteOrdering(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	svc := storage.NewRunService(stores, fakeBuilder{ref: "artifact://ctx/run"})

	res, err := svc.OpenTurn(ctx, storage.OpenTurnRequest{
		AgentID: "travel_agent", UserContentPreview: "hi", AgentBindingID: "bind_1",
	})
	if err != nil {
		t.Fatalf("OpenTurn: %v", err)
	}
	if res.Run.Status != storage.RunStatusCreated {
		t.Fatalf("run should still be created (bridge emits run_started on dispatch), got %s", res.Run.Status)
	}
	if res.ContextSnapshotRef != "artifact://ctx/run" {
		t.Fatalf("snapshot ref not bound: %q", res.ContextSnapshotRef)
	}
	if res.Run.AgentBindingID != "bind_1" {
		t.Fatalf("agent binding not bound")
	}
	if res.Session.Title != "hi" {
		t.Fatalf("session title=%q want user prompt", res.Session.Title)
	}
	if res.UserMessage.RunID != res.Run.RunID {
		t.Fatalf("user message run_id=%q want %q", res.UserMessage.RunID, res.Run.RunID)
	}

	evs, _ := stores.Events.Query(ctx, storage.EventQuery{RunID: res.Run.RunID, Limit: 100})
	got := eventTypes(evs)
	want := []observability.EventType{
		observability.EventUserMessageReceived,
		observability.EventRunCreated,
		observability.EventContextSnapshotCreated,
		observability.EventAgentBinding,
	}
	if len(got) != len(want) {
		t.Fatalf("event order mismatch:\n got %v\nwant %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d]=%s want %s (full: %v)", i, got[i], want[i], got)
		}
	}
	// context_snapshot_created must precede agent_binding, both before any run_started.
	for _, e := range got {
		if e == observability.EventRunStarted {
			t.Fatalf("run_started must not be emitted by OpenTurn")
		}
	}
	var userPayload struct {
		MessageID      string `json:"message_id"`
		Role           string `json:"role"`
		ContentPreview string `json:"content_preview"`
		Text           string `json:"text"`
	}
	if err := json.Unmarshal(evs[0].PayloadPreview, &userPayload); err != nil {
		t.Fatalf("user_message_received payload: %v", err)
	}
	if userPayload.MessageID != res.UserMessage.ID || userPayload.Role != "user" || userPayload.ContentPreview != "hi" || userPayload.Text != "hi" {
		t.Fatalf("unexpected user_message_received payload: %+v", userPayload)
	}
}

func TestOpenTurnRejectsOversizedUserIDBeforePersistingFacts(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	svc := storage.NewRunService(stores, nil)

	_, err := svc.OpenTurn(ctx, storage.OpenTurnRequest{
		TenantID: "t1", UserID: strings.Repeat("u", 65), AgentID: "agent", UserContentPreview: "hi",
	})
	if !storage.IsErrorCode(err, storage.ErrInvalidArgument) {
		t.Fatalf("OpenTurn() error = %v, want ErrInvalidArgument", err)
	}
	page, listErr := stores.Sessions.List(ctx, storage.SessionListQuery{Limit: 10, IncludeArchived: true})
	if listErr != nil {
		t.Fatalf("List sessions: %v", listErr)
	}
	if len(page.Items) != 0 {
		t.Fatalf("oversized UserID persisted %d sessions", len(page.Items))
	}
}

func TestOpenTurnRejectsOversizedIdentifiersBeforePersistingFacts(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  storage.OpenTurnRequest
	}{
		{name: "tenant_id", req: storage.OpenTurnRequest{TenantID: strings.Repeat("t", 65), AgentID: "agent"}},
		{name: "session_id", req: storage.OpenTurnRequest{TenantID: "t1", SessionID: strings.Repeat("s", 65), AgentID: "agent"}},
		{name: "agent_id", req: storage.OpenTurnRequest{TenantID: "t1", AgentID: strings.Repeat("a", 129)}},
		{name: "agent_binding_id", req: storage.OpenTurnRequest{TenantID: "t1", AgentID: "agent", AgentBindingID: strings.Repeat("b", 129)}},
		{name: "idempotency_key", req: storage.OpenTurnRequest{TenantID: "t1", AgentID: "agent", IdempotencyKey: strings.Repeat("i", 129)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := ctxT(tc.req.TenantID)
			stores := memory.New().Stores()
			svc := storage.NewRunService(stores, nil)
			tc.req.UserContentPreview = "hi"

			_, err := svc.OpenTurn(ctx, tc.req)
			if !storage.IsErrorCode(err, storage.ErrInvalidArgument) {
				t.Fatalf("OpenTurn() error = %v, want ErrInvalidArgument", err)
			}
			page, listErr := stores.Sessions.List(ctx, storage.SessionListQuery{Limit: 10, IncludeArchived: true})
			if listErr != nil {
				t.Fatalf("List sessions: %v", listErr)
			}
			if len(page.Items) != 0 {
				t.Fatalf("oversized identifier persisted %d sessions", len(page.Items))
			}
		})
	}
}

func TestOpenTurnPreservesMaximumLengthUserIDAcrossTurns(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	svc := storage.NewRunService(stores, nil)
	userID := strings.Repeat("用", storage.MaxUserIDCharacters)

	first, err := svc.OpenTurn(ctx, storage.OpenTurnRequest{
		TenantID: "t1", UserID: userID, AgentID: "agent", UserContentPreview: "first",
	})
	if err != nil {
		t.Fatalf("first OpenTurn(): %v", err)
	}
	if first.Session.UserID != userID {
		t.Fatalf("first Session.UserID = %q, want unmodified maximum-length ID", first.Session.UserID)
	}
	if _, err := stores.Runs.CompareAndSetStatus(ctx, first.Run.RunID, storage.RunStatusCreated, storage.RunStatusRunning, storage.RunMutation{}); err != nil {
		t.Fatalf("start first Run: %v", err)
	}
	if _, err := stores.Runs.CompareAndSetStatus(ctx, first.Run.RunID, storage.RunStatusRunning, storage.RunStatusCompleted, storage.RunMutation{}); err != nil {
		t.Fatalf("complete first Run: %v", err)
	}

	second, err := svc.OpenTurn(ctx, storage.OpenTurnRequest{
		SessionID: first.Session.ID, TenantID: "t1", UserID: userID, AgentID: "agent", UserContentPreview: "second",
	})
	if err != nil {
		t.Fatalf("second OpenTurn(): %v", err)
	}
	if second.Session.ID != first.Session.ID || second.Session.UserID != userID {
		t.Fatalf("second Session = %#v, want same session with unmodified UserID", second.Session)
	}
	persisted, err := stores.Sessions.Get(ctx, first.Session.ID)
	if err != nil {
		t.Fatalf("Get Session: %v", err)
	}
	if persisted.UserID != userID {
		t.Fatalf("persisted Session.UserID = %q, want %q", persisted.UserID, userID)
	}
}

func TestOpenTurnCanDeferContextSnapshotUntilAttachmentsPersist(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	var reqs []storage.SnapshotBuildRequest
	svc := storage.NewRunService(stores, fakeBuilder{ref: "artifact://ctx/with-attachments", reqs: &reqs})

	res, err := svc.OpenTurn(ctx, storage.OpenTurnRequest{
		AgentID: "file-agent", UserContentPreview: "inspect", DeferContextSnapshot: true,
	})
	if err != nil {
		t.Fatalf("OpenTurn: %v", err)
	}
	if res.ContextSnapshotRef != "" || len(reqs) != 0 {
		t.Fatalf("snapshot built too early: ref=%q reqs=%#v", res.ContextSnapshotRef, reqs)
	}
	attachments := []storage.ContextAttachmentSnapshot{{
		Name: "notes.txt", MimeType: "text/plain", SizeBytes: 12, ArtifactRef: "artifact://file/notes", Type: "file",
	}}
	if err := svc.BuildContextSnapshot(ctx, res, attachments); err != nil {
		t.Fatalf("BuildContextSnapshot: %v", err)
	}
	if res.ContextSnapshotRef != "artifact://ctx/with-attachments" || res.Run.ContextSnapshotRef != res.ContextSnapshotRef {
		t.Fatalf("snapshot ref not bound: result=%q run=%q", res.ContextSnapshotRef, res.Run.ContextSnapshotRef)
	}
	if len(reqs) != 1 || len(reqs[0].Attachments) != 1 || reqs[0].Attachments[0].ArtifactRef != "artifact://file/notes" {
		t.Fatalf("snapshot request attachments=%#v", reqs)
	}
}

// fact-first: context build failure still leaves the run/message queryable.
func TestOpenTurnContextBuildFailureFactFirst(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	svc := storage.NewRunService(stores, fakeBuilder{err: errors.New("rag timeout")})

	res, err := svc.OpenTurn(ctx, storage.OpenTurnRequest{AgentID: "a", UserContentPreview: "hi"})
	if err == nil {
		t.Fatal("expected context build error")
	}
	if res == nil || res.Run == nil {
		t.Fatal("run must be returned even on build failure (fact-first)")
	}
	// run persisted and marked failed
	run, gErr := stores.Runs.Get(ctx, res.Run.RunID)
	if gErr != nil {
		t.Fatalf("run must be queryable after build failure: %v", gErr)
	}
	if run.Status != storage.RunStatusFailed {
		t.Fatalf("run should be failed, got %s", run.Status)
	}
	// user_message_received + run_created + context_build_failed persisted
	evs, _ := stores.Events.Query(ctx, storage.EventQuery{RunID: res.Run.RunID, Limit: 100})
	got := eventTypes(evs)
	hasBuildFailed := false
	for _, e := range got {
		if e == observability.EventContextBuildFailed {
			hasBuildFailed = true
		}
	}
	if !hasBuildFailed {
		t.Fatalf("context_build_failed must be persisted, got %v", got)
	}
}
