package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	metamem "github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	objmem "github.com/AGenUI/agenui-studio/harness/internal/artifact/objectstore"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol/server"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// withIdentity seeds the trusted server-side identity (tenant + user) that the
// auth middleware would inject in production.
func withIdentity(req *http.Request, tenant, user string) *http.Request {
	tc := observability.MustTraceContext(req.Context())
	tc.TenantID, tc.UserID = tenant, user
	return req.WithContext(observability.WithTraceContext(req.Context(), tc))
}

func mustCreateOwnedSessionRun(t *testing.T, stores storage.Stores, sid, rid, tenant, user string) {
	t.Helper()
	ctx := context.Background()
	if err := stores.Sessions.Create(ctx, &storage.Session{ID: sid, TenantID: tenant, UserID: user, Status: storage.SessionStatusActive}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if rid != "" {
		if err := stores.Runs.Create(ctx, &storage.Run{RunID: rid, SessionID: sid, TenantID: tenant, Status: storage.RunStatusCreated}); err != nil {
			t.Fatalf("create run: %v", err)
		}
	}
}

// P0-1: a same-tenant user must not be able to open a turn/run on another user's
// session via a known session_id — neither through /runs nor /ai/chat.
func TestCrossUserSessionInjectionDenied(t *testing.T) {
	deps, stores := newDeps()
	mustCreateOwnedSessionRun(t, stores, "s1", "", "acme", "u1")
	router := server.NewRouter(deps)

	// u2 (same tenant) → POST /sessions/s1/runs must be forbidden.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/s1/runs", strings.NewReader(`{"agent_id":"a"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, withIdentity(req, "acme", "u2"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-user create run: want 403 got %d body=%s", rec.Code, rec.Body.String())
	}

	// u2 → POST /ai/chat with u1's sessionId must be forbidden.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/ai/chat", strings.NewReader(`{"sessionId":"s1","prompt":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, withIdentity(req, "acme", "u2"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-user ai/chat: want 403 got %d body=%s", rec.Code, rec.Body.String())
	}

	// No run was injected into u1's session by the rejected attempts.
	runs, err := stores.Runs.ListBySession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("no run should have been created in another user's session, got %d", len(runs))
	}

	// The owner u1 can create a run.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/sessions/s1/runs", strings.NewReader(`{"agent_id":"a"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, withIdentity(req, "acme", "u1"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("owner create run: want 201 got %d body=%s", rec.Code, rec.Body.String())
	}
}

// P0-2: a same-tenant user must not be able to read another user's artifact.
func TestCrossUserArtifactReadDenied(t *testing.T) {
	deps, stores := newDeps()
	art := artifact.NewStore(artifact.StoreConfig{ObjectStore: objmem.NewMemory(), MetadataStore: metamem.NewMemory()})
	deps.Artifacts = art
	mustCreateOwnedSessionRun(t, stores, "s1", "r1", "acme", "u1")

	// Runtime writes a user_visible artifact owned by (acme, s1, r1).
	putCtx := artifact.ContextWithActor(context.Background(), artifact.Actor{TenantID: "acme", SessionID: "s1", RunID: "r1", Role: artifact.ActorRuntime})
	meta, err := art.Put(putCtx, artifact.PutArtifactRequest{
		TenantID: "acme", UserID: "u1", SessionID: "s1", RunID: "r1",
		OwnerModule: artifact.OwnerModuleModelGateway, OwnerID: "r1",
		ArtifactType: artifact.ArtifactTypeFinalResult, MimeType: "text/plain",
		Visibility: artifact.VisibilityUserVisible, Content: strings.NewReader("secret output"),
	})
	if err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	router := server.NewRouter(deps)

	get := func(user string) int {
		// The ref contains "://" and slashes; pass it via the ?ref= query the
		// handler supports to avoid ServeMux path cleaning in the test.
		req := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/x?ref="+url.QueryEscape(meta.ArtifactRef), nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, withIdentity(req, "acme", user))
		return rec.Code
	}

	if code := get("u2"); code != http.StatusForbidden {
		t.Fatalf("cross-user artifact read: want 403 got %d", code)
	}
	if code := get("u1"); code != http.StatusOK {
		t.Fatalf("owner artifact read: want 200 got %d", code)
	}
}

func TestDebugArtifactReadUsesDebugVisibilityRole(t *testing.T) {
	deps, stores := newDeps()
	art := artifact.NewStore(artifact.StoreConfig{ObjectStore: objmem.NewMemory(), MetadataStore: metamem.NewMemory()})
	deps.Artifacts = art
	mustCreateOwnedSessionRun(t, stores, "s1", "r1", "acme", "u1")

	putCtx := artifact.ContextWithActor(context.Background(), artifact.Actor{TenantID: "acme", SessionID: "s1", RunID: "r1", Role: artifact.ActorRuntime})
	meta, err := art.Put(putCtx, artifact.PutArtifactRequest{
		TenantID: "acme", UserID: "u1", SessionID: "s1", RunID: "r1",
		OwnerModule: artifact.OwnerModuleModelGateway, OwnerID: "r1",
		ArtifactType: artifact.ArtifactTypeFinalResult, MimeType: "text/plain",
		Visibility: artifact.VisibilityInternal, Content: strings.NewReader("internal output"),
	})
	if err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	router := server.NewRouter(deps)
	request := func(debug bool) *httptest.ResponseRecorder {
		req := withIdentity(httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/x?ref="+url.QueryEscape(meta.ArtifactRef), nil), "acme", "u1")
		tc := observability.MustTraceContext(req.Context())
		tc.DebugEnabled = debug
		req = req.WithContext(observability.WithTraceContext(req.Context(), tc))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	if rec := request(false); rec.Code != http.StatusForbidden {
		t.Fatalf("ordinary user must not read internal artifact: want 403 got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request(true); rec.Code != http.StatusOK || rec.Body.String() != "internal output" {
		t.Fatalf("debug actor must read internal artifact: status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestArtifactReadFailsClosedWhenOwnerFactsMissing(t *testing.T) {
	deps, _ := newDeps()
	art := artifact.NewStore(artifact.StoreConfig{ObjectStore: objmem.NewMemory(), MetadataStore: metamem.NewMemory()})
	deps.Artifacts = art
	putCtx := artifact.ContextWithActor(context.Background(), artifact.Actor{TenantID: "acme", SessionID: "missing-session", RunID: "missing-run", Role: artifact.ActorRuntime})
	meta, err := art.Put(putCtx, artifact.PutArtifactRequest{
		TenantID: "acme", UserID: "u1", SessionID: "missing-session", RunID: "missing-run",
		OwnerModule: artifact.OwnerModuleModelGateway, OwnerID: "missing-run",
		ArtifactType: artifact.ArtifactTypeFinalResult, MimeType: "text/plain",
		Visibility: artifact.VisibilityUserVisible, Content: strings.NewReader("secret output"),
	})
	if err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/x?ref="+url.QueryEscape(meta.ArtifactRef), nil)
	rec := httptest.NewRecorder()
	server.NewRouter(deps).ServeHTTP(rec, withIdentity(req, "acme", "u1"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing owner facts must fail closed: want 403 got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestOwnerlessSessionDeniedToOrdinaryUser(t *testing.T) {
	deps, stores := newDeps()
	if err := stores.Sessions.Create(context.Background(), &storage.Session{ID: "legacy", TenantID: "acme", Status: storage.SessionStatusActive}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/legacy/messages", nil)
	rec := httptest.NewRecorder()
	server.NewRouter(deps).ServeHTTP(rec, withIdentity(req, "acme", "u1"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ownerless session must fail closed: want 403 got %d body=%s", rec.Code, rec.Body.String())
	}
}
