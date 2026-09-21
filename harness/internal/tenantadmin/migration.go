package tenantadmin

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
)

//go:embed sql/sqlite/001_tenants.sql
var sqliteTenantSchema string

//go:embed sql/mysql/001_tenants.sql
var mysqlTenantSchema string

// ApplySQLiteSchema creates the tenants table on SQLite.
func ApplySQLiteSchema(ctx context.Context, db *sql.DB) error {
	return applyTenantDDL(ctx, db, sqliteTenantSchema)
}

// ApplyMySQLSchema creates the tenants table on MySQL.
func ApplyMySQLSchema(ctx context.Context, db *sql.DB) error {
	return applyTenantDDL(ctx, db, mysqlTenantSchema)
}

// ValidateSchema verifies the tenants table exists with the expected columns.
func ValidateSchema(ctx context.Context, db *sql.DB) error {
	if ctx == nil || db == nil {
		return errors.New("tenantadmin schema validation context and database are required")
	}
	rows, err := db.QueryContext(ctx, `SELECT tenant_id, name, owner, plan, status, secret_key, revision, created_at_ms, updated_at_ms, updated_by FROM tenants LIMIT 0`)
	if err != nil {
		return fmt.Errorf("tenantadmin schema not ready: %w", err)
	}
	return rows.Close()
}

// ValidateDefaultTenant verifies the canonical default tenant has been
// materialized in the tenant directory. Runtime readiness uses this after the
// tenant/002 seed migration.
func ValidateDefaultTenant(ctx context.Context, db *sql.DB) error {
	if _, err := NewSQLManagedRegistry(db).Get(ctx, DefaultTenantID); err != nil {
		return fmt.Errorf("tenantadmin default tenant not ready: %w", err)
	}
	return nil
}

func applyTenantDDL(ctx context.Context, db *sql.DB, schema string) error {
	if ctx == nil || db == nil {
		return errors.New("tenantadmin migration context and database are required")
	}
	for _, statement := range strings.Split(schema, ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply tenantadmin schema: %w", err)
		}
	}
	return nil
}
