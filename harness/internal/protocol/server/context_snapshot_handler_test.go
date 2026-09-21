package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	ctxpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol/server"
)

type contextSnapshotResolverFunc func(context.Context, string) (ctxpkg.Snapshot, []ctxpkg.Message, error)

func (f contextSnapshotResolverFunc) Resolve(ctx context.Context, ref string) (ctxpkg.Snapshot, []ctxpkg.Message, error) {
	return f(ctx, ref)
}

func TestContextSnapshotEndpointRequiresDebugPermission(t *testing.T) {
	deps, stores := newDeps()
	seedTenantRun(t, stores, "snapshot-session", "snapshot-run", "acme")
	if err := stores.Runs.BindContextSnapshot(context.Background(), "snapshot-run", "artifact://context/snapshot"); err != nil {
		t.Fatal(err)
	}
	resolverCalls := 0
	deps.ContextSnapshots = contextSnapshotResolverFunc(func(context.Context, string) (ctxpkg.Snapshot, []ctxpkg.Message, error) {
		resolverCalls++
		return ctxpkg.Snapshot{ID: "snapshot-1", SessionID: "snapshot-session", RunID: "snapshot-run"}, nil, nil
	})
	router := server.NewRouter(deps)

	request := withTrace(httptest.NewRequest(http.MethodGet, "/api/v1/runs/snapshot-run/context-snapshot", nil), "acme", false)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("context snapshot without debug permission: status=%d body=%s", response.Code, response.Body.String())
	}
	if resolverCalls != 0 {
		t.Fatalf("unauthorized request resolved snapshot %d times", resolverCalls)
	}

	request = withTrace(httptest.NewRequest(http.MethodGet, "/api/v1/runs/snapshot-run/context-snapshot", nil), "other-tenant", true)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant context snapshot: status=%d body=%s", response.Code, response.Body.String())
	}
	if resolverCalls != 0 {
		t.Fatalf("cross-tenant request resolved snapshot %d times", resolverCalls)
	}

	request = withTrace(httptest.NewRequest(http.MethodGet, "/api/v1/runs/snapshot-run/context-snapshot", nil), "acme", true)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("context snapshot with debug permission: status=%d body=%s", response.Code, response.Body.String())
	}
	if resolverCalls != 1 {
		t.Fatalf("authorized request resolved snapshot %d times, want 1", resolverCalls)
	}
}
