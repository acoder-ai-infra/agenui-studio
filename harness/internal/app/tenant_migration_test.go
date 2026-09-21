package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/tenantadmin"

	_ "modernc.org/sqlite"
)

// TestMigrateStorageSQLiteCreatesTenants proves the real sqlite migration chain
// (including tenant/001 schema and tenant/002 default seed) succeeds on a clean
// db and materializes the tenants table. A fresh db avoids any pre-existing
// local schema drift, matching a real deployment's first migrate.
func TestMigrateStorageSQLiteCreatesTenants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness.db")
	cfg := StorageConfig{Backend: "sqlite", Config: json.RawMessage(`{"path":"` + path + `"}`)}

	result, err := MigrateStorage(context.Background(), cfg, false)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !result.Ready {
		t.Fatalf("migrate not ready: %#v", result)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := tenantadmin.ValidateSchema(context.Background(), db); err != nil {
		t.Fatalf("tenants table not created by migration: %v", err)
	}
	if err := tenantadmin.ValidateDefaultTenant(context.Background(), db); err != nil {
		t.Fatalf("default tenant not seeded by migration: %v", err)
	}
	var publicName string
	if err := db.QueryRow(`SELECT name FROM tenants WHERE tenant_id=?`, tenantadmin.DefaultTenantID).Scan(&publicName); err != nil {
		t.Fatalf("read default tenant: %v", err)
	}
	if publicName != tenantadmin.DefaultTenantName {
		t.Fatalf("default tenant name = %q, want %q", publicName, tenantadmin.DefaultTenantName)
	}
}
