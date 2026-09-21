package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/control"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol/server"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// withTrace seeds a trusted identity (tenant + debug flag) into the request,
// standing in for what authTraceMiddleware would inject in production.
func withTrace(req *http.Request, tenant string, debug bool) *http.Request {
	tc := observability.MustTraceContext(req.Context())
	tc.TenantID = tenant
	tc.DebugEnabled = debug
	return req.WithContext(observability.WithTraceContext(req.Context(), tc))
}

func seedTenantRun(t *testing.T, stores storage.Stores, sid, rid, tenant string) {
	t.Helper()
	ctx := context.Background()
	if err := stores.Sessions.Create(ctx, &storage.Session{ID: sid, TenantID: tenant, Status: storage.SessionStatusActive}); err != nil && !storage.IsErrorCode(err, storage.ErrConflict) {
		t.Fatalf("seed session: %v", err)
	}
	if err := stores.Runs.Create(ctx, &storage.Run{RunID: rid, SessionID: sid, TenantID: tenant, Status: storage.RunStatusCreated}); err != nil && !storage.IsErrorCode(err, storage.ErrConflict) {
		t.Fatalf("seed run: %v", err)
	}
}

func TestDebugEndpoints_RequireDebug(t *testing.T) {
	deps, stores := newDeps()
	seedTenantRun(t, stores, "s1", "r1", "acme")
	router := server.NewRouter(deps)

	for _, path := range []string{
		"/api/v1/debug/info",
		"/api/v1/debug/events?run_id=r1",
		"/api/v1/debug/steps?run_id=r1",
		"/api/v1/debug/control-requests?run_id=r1",
		"/api/v1/debug/checkpoints?run_id=r1",
		"/api/v1/debug/logs",
	} {
		rec := httptest.NewRecorder()
		// no debug flag on the trace context
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: want 403 got %d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestDebugControlRequests_TenantScoped(t *testing.T) {
	deps, stores := newDeps()
	seedTenantRun(t, stores, "s1", "r1", "acme")
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "acme", TraceID: "trace_control_debug"})
	if err := stores.Controls.Create(ctx, &storage.ControlRequest{
		RequestID: "ctrl_1", RunID: "r1", TenantID: "acme", Type: "ask_user", Status: string(control.StatusPending),
		PromptPreview: "Need approval", SchemaVersion: storage.ControlRequestSchemaVersion,
	}); err != nil {
		t.Fatal(err)
	}
	router := server.NewRouter(deps)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, withTrace(httptest.NewRequest(http.MethodGet, "/api/v1/debug/control-requests?run_id=r1", nil), "acme", true))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"request_id":"ctrl_1"`) || !strings.Contains(rec.Body.String(), `"prompt_preview":"Need approval"`) {
		t.Fatalf("control list status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, withTrace(httptest.NewRequest(http.MethodGet, "/api/v1/debug/control-requests?run_id=r1", nil), "other", true))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant control list want 403 got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDebugInfo_ServesSnapshot(t *testing.T) {
	deps, _ := newDeps()
	deps.System = json.RawMessage(`{"auth_mode":"insecure","tenants":[]}`)
	router := server.NewRouter(deps)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, withTrace(httptest.NewRequest(http.MethodGet, "/api/v1/debug/info", nil), "acme", true))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"auth_mode":"insecure"`) {
		t.Fatalf("snapshot not served: %s", rec.Body.String())
	}
}

func TestDebugEvents_TenantScopedAndRaw(t *testing.T) {
	deps, stores := newDeps()
	seedTenantRun(t, stores, "s1", "r1", "acme")
	if _, err := stores.Events.Append(context.Background(), observability.AgentEvent{
		RunID: "r1", EventType: observability.EventAgentTextDelta,
		Visibility: observability.VisibilityDebug, Payload: json.RawMessage(`{"marker":"raw-payload"}`),
	}); err != nil {
		t.Fatal(err)
	}
	router := server.NewRouter(deps)

	// same tenant + debug → 200, raw payload present (debug visibility included)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, withTrace(httptest.NewRequest(http.MethodGet, "/api/v1/debug/events?run_id=r1", nil), "acme", true))
	if rec.Code != http.StatusOK {
		t.Fatalf("same-tenant status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "raw-payload") {
		t.Fatalf("raw payload missing: %s", rec.Body.String())
	}

	// cross tenant → 403
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, withTrace(httptest.NewRequest(http.MethodGet, "/api/v1/debug/events?run_id=r1", nil), "other", true))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant want 403 got %d", rec.Code)
	}
}

func TestDebugSteps_ReturnsTimeline(t *testing.T) {
	deps, stores := newDeps()
	seedTenantRun(t, stores, "s1", "r1", "acme")
	if err := stores.Steps.Upsert(context.Background(), &storage.Step{StepID: "st1", RunID: "r1", StepType: "model", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	router := server.NewRouter(deps)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, withTrace(httptest.NewRequest(http.MethodGet, "/api/v1/debug/steps?run_id=r1", nil), "acme", true))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"step_type":"model"`) {
		t.Fatalf("steps status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDebugLogs_UnavailableThenQueried(t *testing.T) {
	deps, _ := newDeps()
	router := server.NewRouter(deps)

	// nil LogQuery → 501
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, withTrace(httptest.NewRequest(http.MethodGet, "/api/v1/debug/logs", nil), "acme", true))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("want 501 got %d", rec.Code)
	}

	// with a RingLogger holding an entry for tenant acme
	ring := observability.NewRingLogger(observability.NoopLogger{}, 10)
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "acme", RunID: "r1"})
	ring.Info(ctx, "hello from run")
	deps.LogQuery = ring
	router = server.NewRouter(deps)

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, withTrace(httptest.NewRequest(http.MethodGet, "/api/v1/debug/logs?run_id=r1", nil), "acme", true))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hello from run") {
		t.Fatalf("logs status=%d body=%s", rec.Code, rec.Body.String())
	}

	// tenant isolation: caller in another tenant sees nothing
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, withTrace(httptest.NewRequest(http.MethodGet, "/api/v1/debug/logs?run_id=r1", nil), "other", true))
	if strings.Contains(rec.Body.String(), "hello from run") {
		t.Fatalf("tenant leak: %s", rec.Body.String())
	}
}
