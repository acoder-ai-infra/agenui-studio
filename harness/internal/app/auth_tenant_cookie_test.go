package app

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/tenantadmin"

	_ "modernc.org/sqlite"
)

func TestTenantCookieAuthenticator(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "tenants.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := tenantadmin.ApplySQLiteSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	registry := tenantadmin.NewSQLManagedRegistry(db)
	if _, err := registry.EnsureDefaultTenant(context.Background(), "migration"); err != nil {
		t.Fatal(err)
	}
	created, err := registry.Create(context.Background(), "owner", tenantadmin.TenantDefinition{ID: "tenant-x", Name: "X", Status: tenantadmin.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	paused, err := registry.Create(context.Background(), "owner", tenantadmin.TenantDefinition{ID: "tenant-paused", Name: "Paused", Status: tenantadmin.StatusPaused})
	if err != nil {
		t.Fatal(err)
	}

	auth := &TenantCookieAuthenticator{Base: InsecureHeaderAuthenticator{}, Resolver: registry}

	// Valid cookie → resolves to the tenant.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/agent-configs", nil)
	req.AddCookie(&http.Cookie{Name: TenantSessionCookie, Value: created.Definition.SecretKey})
	principal, err := auth.Authenticate(req)
	if err != nil {
		t.Fatal(err)
	}
	if principal.TenantID != "tenant-x" || !hasScope(principal, "agent.config.admin") {
		t.Fatalf("cookie principal = %#v", principal)
	}

	// No cookie → falls through to base auth, then validates default public.
	base, err := auth.Authenticate(httptest.NewRequest(http.MethodGet, "/x", nil))
	if err != nil {
		t.Fatal(err)
	}
	if base.TenantID != tenantadmin.DefaultTenantID {
		t.Fatalf("fallthrough tenant = %q", base.TenantID)
	}

	// Unknown cookie must not bypass tenant selection by falling through.
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.AddCookie(&http.Cookie{Name: TenantSessionCookie, Value: "ZZZZZZ"})
	if got, err := auth.Authenticate(req); err == nil {
		t.Fatalf("unknown cookie principal=%#v err=<nil>", got)
	}

	// Paused cookie is also rejected.
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.AddCookie(&http.Cookie{Name: TenantSessionCookie, Value: paused.Definition.SecretKey})
	if got, err := auth.Authenticate(req); err == nil {
		t.Fatalf("paused cookie principal=%#v err=<nil>", got)
	}

	// Header/JWT fallback tenants are allowed only when present and active.
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("x-tenant-id", "missing-tenant")
	if got, err := auth.Authenticate(req); err == nil {
		t.Fatalf("missing header tenant principal=%#v err=<nil>", got)
	}
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("x-tenant-id", "tenant-paused")
	if got, err := auth.Authenticate(req); err == nil {
		t.Fatalf("paused header tenant principal=%#v err=<nil>", got)
	}
}

func hasScope(p Principal, scope string) bool {
	for _, s := range p.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}
