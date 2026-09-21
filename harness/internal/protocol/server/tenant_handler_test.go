package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/authcontext"
	"github.com/AGenUI/agenui-studio/harness/internal/tenantadmin"

	_ "modernc.org/sqlite"
)

// tenantTestServer builds a router backed by a real tenant registry over a temp
// sqlite db. Owner endpoints require an admin principal, so requests are wrapped
// to inject one (mirroring what app.authTraceMiddleware does in production).
func tenantTestServer(t *testing.T) http.Handler {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "tenants.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := tenantadmin.ApplySQLiteSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	mux := NewRouter(Deps{Tenants: tenantadmin.NewSQLManagedRegistry(db)})
	principal := authcontext.Principal{UserID: "owner", TenantID: "platform", Scopes: []string{"agent.config.admin"}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(authcontext.WithPrincipal(r.Context(), principal)))
	})
}

func doJSON(t *testing.T, srv http.Handler, method, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestTenantOwnerCRUDAndSession(t *testing.T) {
	srv := tenantTestServer(t)

	// Create.
	rec := doJSON(t, srv, http.MethodPost, "/api/v1/owner/tenants", map[string]any{
		"definition": map[string]any{"id": "tenant-demo", "name": "演示租户", "owner": "平台", "plan": "Sandbox", "status": "active"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		Definition tenantadmin.TenantDefinition `json:"definition"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if len(created.Definition.SecretKey) != tenantadmin.SecretKeyLength {
		t.Fatalf("generated key = %q", created.Definition.SecretKey)
	}

	// List sees it.
	rec = doJSON(t, srv, http.MethodGet, "/api/v1/owner/tenants", nil, nil)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("tenant-demo")) {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Login with the key sets the session cookie.
	rec = doJSON(t, srv, http.MethodPost, "/api/v1/tenant-session", map[string]any{"secret_key": created.Definition.SecretKey}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", rec.Code, rec.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "harness_tenant" {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value != created.Definition.SecretKey {
		t.Fatalf("session cookie = %#v", cookie)
	}

	// GET session with the cookie resolves the tenant; without it → 401.
	rec = doJSON(t, srv, http.MethodGet, "/api/v1/tenant-session", nil, cookie)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("tenant-demo")) {
		t.Fatalf("get session status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec = doJSON(t, srv, http.MethodGet, "/api/v1/tenant-session", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon session status=%d", rec.Code)
	}

	// Wrong key is rejected.
	if rec = doJSON(t, srv, http.MethodPost, "/api/v1/tenant-session", map[string]any{"secret_key": "ZZZZZZ"}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad key status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Update (name) with CAS revision 1.
	rec = doJSON(t, srv, http.MethodPut, "/api/v1/owner/tenants/tenant-demo", map[string]any{
		"definition":        map[string]any{"id": "tenant-demo", "name": "改名后", "plan": "Growth", "status": "active"},
		"expected_revision": 1,
	}, nil)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("改名后")) {
		t.Fatalf("update status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Delete.
	if rec = doJSON(t, srv, http.MethodDelete, "/api/v1/owner/tenants/tenant-demo", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec = doJSON(t, srv, http.MethodGet, "/api/v1/tenant-session", nil, cookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("session after delete status=%d", rec.Code)
	}
}

func TestTenantOwnerForbiddenWithoutScope(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "tenants.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := tenantadmin.ApplySQLiteSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	mux := NewRouter(Deps{Tenants: tenantadmin.NewSQLManagedRegistry(db)})
	// No principal in context → owner endpoints must be forbidden.
	rec := doJSON(t, mux, http.MethodGet, "/api/v1/owner/tenants", nil, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without scope, got %d", rec.Code)
	}
}
