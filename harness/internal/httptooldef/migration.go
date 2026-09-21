package httptooldef

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
)

//go:embed sql/sqlite/001_http_tools.sql
var sqliteManagedSchema string

//go:embed sql/mysql/001_http_tools.sql
var mysqlManagedSchema string

// ApplySQLiteSchema creates the http_tools table on SQLite.
func ApplySQLiteSchema(ctx context.Context, db *sql.DB) error {
	return applyManagedDDL(ctx, db, sqliteManagedSchema)
}

// ApplyMySQLSchema creates the http_tools table on MySQL.
func ApplyMySQLSchema(ctx context.Context, db *sql.DB) error {
	return applyManagedDDL(ctx, db, mysqlManagedSchema)
}

// ValidateSchema verifies the http_tools table exists with the expected columns.
func ValidateSchema(ctx context.Context, db *sql.DB) error {
	if ctx == nil || db == nil {
		return errors.New("httptooldef schema validation context and database are required")
	}
	rows, err := db.QueryContext(ctx, `SELECT tenant_id, tool_name, definition_json, revision, created_at_ms, updated_at_ms, updated_by, identity_digest FROM http_tools LIMIT 0`)
	if err != nil {
		return fmt.Errorf("httptooldef schema not ready: %w", err)
	}
	return rows.Close()
}

func applyManagedDDL(ctx context.Context, db *sql.DB, schema string) error {
	if ctx == nil || db == nil {
		return errors.New("httptooldef migration context and database are required")
	}
	for _, statement := range strings.Split(schema, ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply httptooldef schema: %w", err)
		}
	}
	return nil
}
