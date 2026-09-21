package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	frameworkmysql "github.com/AGenUI/agenui-studio/harness/framework/mysql"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	"github.com/AGenUI/agenui-studio/harness/internal/tenantadmin"
	"github.com/DATA-DOG/go-sqlmock"
)

func TestMigrateStorageSQLiteCreatesAllTablesAndIsIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nested", "harness.db")
	raw, err := json.Marshal(SQLiteConfig{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	config := StorageConfig{Backend: "sqlite", Config: raw}

	for attempt := 0; attempt < 2; attempt++ {
		result, err := migrateStorage(context.Background(), config, false, storageSchemaUp, defaultBuildDependencies())
		if err != nil {
			t.Fatalf("migrate attempt %d: %v", attempt+1, err)
		}
		if result.Backend != "sqlite" || !result.Ready || result.Action != storageSchemaUp {
			t.Fatalf("result = %#v", result)
		}
	}

	status, err := migrateStorage(context.Background(), config, false, storageSchemaStatus, defaultBuildDependencies())
	if err != nil {
		t.Fatalf("status after migrate: %v", err)
	}
	if !status.Ready || status.Action != storageSchemaStatus {
		t.Fatalf("status = %#v", status)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, table := range []string{
		"harness_schema_migrations", "runs", "artifact_metadata",
		"agent_registry_entries", "agent_registry_config_snapshots", "agent_registry_audit_events", "agent_registry_prompt_versions",
		"agent_config_drafts", "agent_config_versions", "agent_config_releases", "agent_config_release_events",
		"skill_versions", "skill_retirements", "mcp_servers", "mcp_oauth_grants", "mcp_debug_records", "mcp_oauth_pending",
		"model_providers", "tool_configs", "http_tools", "tenants",
	} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if table == "artifact_metadata" {
			if count != 0 {
				t.Fatalf("SQLite file artifact backend must not create SQL table %s", table)
			}
			continue
		}
		if count != 1 {
			t.Fatalf("unified SQLite migration did not create table %s", table)
		}
	}
	if err := tenantadmin.ValidateDefaultTenant(context.Background(), db); err != nil {
		t.Fatalf("unified SQLite migration did not seed default tenant: %v", err)
	}
}

func TestStorageStatusRequiresManagedSQLiteSchemas(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "harness.db")
	raw, err := json.Marshal(SQLiteConfig{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	config := StorageConfig{Backend: "sqlite", Config: raw}
	if _, err := migrateStorage(context.Background(), config, false, storageSchemaUp, defaultBuildDependencies()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE http_tools`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = migrateStorage(context.Background(), config, false, storageSchemaStatus, defaultBuildDependencies())
	if err == nil || !strings.Contains(err.Error(), "http_tools") {
		t.Fatalf("status error = %v, want missing http_tools schema", err)
	}
}

func TestStorageStatusRequiresManagedSQLiteMigrationMarkers(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "harness.db")
	raw, err := json.Marshal(SQLiteConfig{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	config := StorageConfig{Backend: "sqlite", Config: raw}
	if _, err := migrateStorage(context.Background(), config, false, storageSchemaUp, defaultBuildDependencies()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM harness_schema_migrations WHERE migration_id = ?`, "tool_config/001"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = migrateStorage(context.Background(), config, false, storageSchemaStatus, defaultBuildDependencies())
	if err == nil || !strings.Contains(err.Error(), `required step "tool_config/001" is not applied`) {
		t.Fatalf("status error = %v, want missing tool_config/001 marker", err)
	}
}

func TestStorageStatusDoesNotCreateSQLiteSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "empty.db")
	raw, err := json.Marshal(SQLiteConfig{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	config := StorageConfig{Backend: "sqlite", Config: raw}

	_, err = migrateStorage(context.Background(), config, false, storageSchemaStatus, defaultBuildDependencies())
	if err == nil || !strings.Contains(err.Error(), "schema not ready") {
		t.Fatalf("status error = %v, want schema-not-ready", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("status must not execute DDL; found %d tables", count)
	}
}

func TestMigrateStorageMySQLUsesUnifiedBackendAndClosesPool(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	backend := &recordingMySQLBackend{stores: storage.Stores{}}
	mock.ExpectClose()
	var initialized frameworkmysql.Config
	deps := defaultBuildDependencies()
	deps.initMySQL = func(config frameworkmysql.Config) error {
		initialized = config
		return nil
	}
	deps.getMySQL = func(name string) *sql.DB {
		if name != "harness" {
			t.Fatalf("get mysql name = %q", name)
		}
		return db
	}
	deps.newMySQLBackend = func(got *sql.DB, selected string, _ bool) mysqlStorageBackend {
		if got != db {
			t.Fatal("backend received a different pool")
		}
		if selected != "mysql" {
			t.Fatalf("selected backend = %q, want mysql", selected)
		}
		return backend
	}
	raw, err := json.Marshal(MySQLConfig{
		Name: "harness", DBName: "harness", Host: "127.0.0.1", Port: 3306, User: "app",
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := migrateStorage(context.Background(), StorageConfig{Backend: "mysql", Config: raw}, false, storageSchemaUp, deps)
	if err != nil {
		t.Fatalf("migrate mysql: %v", err)
	}
	if initialized.Name != "harness" || initialized.DBName != "harness" {
		t.Fatalf("initialized config = %#v", initialized)
	}
	if backend.migrateCalls != 1 || backend.checkReadyCalls != 1 {
		t.Fatalf("migrate calls=%d readiness calls=%d", backend.migrateCalls, backend.checkReadyCalls)
	}
	if result.Backend != "mysql" || !result.Ready {
		t.Fatalf("result = %#v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pool was not closed: %v", err)
	}
}

func TestMigrateStorageClosesMySQLPoolWhenMigrationFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	migrateFailure := errors.New("ddl failed")
	mock.ExpectClose()
	backend := &recordingMySQLBackend{migrateErr: migrateFailure}
	deps := defaultBuildDependencies()
	deps.initMySQL = func(frameworkmysql.Config) error { return nil }
	deps.getMySQL = func(string) *sql.DB { return db }
	deps.newMySQLBackend = func(*sql.DB, string, bool) mysqlStorageBackend { return backend }
	raw, err := json.Marshal(MySQLConfig{
		Name: "harness", DBName: "harness", Host: "127.0.0.1", Port: 3306, User: "app",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = migrateStorage(context.Background(), StorageConfig{Backend: "mysql", Config: raw}, false, storageSchemaUp, deps)
	if !errors.Is(err, migrateFailure) {
		t.Fatalf("error = %v, want %v", err, migrateFailure)
	}
	if backend.checkReadyCalls != 0 {
		t.Fatalf("readiness calls = %d, want 0 after failed migration", backend.checkReadyCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pool was not closed: %v", err)
	}
}

func TestMigrateStorageMemoryIsExplicitNoopOnlyLocally(t *testing.T) {
	result, err := migrateStorage(context.Background(), StorageConfig{Backend: "memory"}, false, storageSchemaUp, defaultBuildDependencies())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ready || !result.Noop {
		t.Fatalf("result = %#v", result)
	}
	if _, err := migrateStorage(context.Background(), StorageConfig{Backend: "memory"}, true, storageSchemaUp, defaultBuildDependencies()); err == nil {
		t.Fatal("production memory migration must fail closed")
	}
}
