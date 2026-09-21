package mcp

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
)

//go:embed sql/sqlite/001_mcp_registry.sql
var sqliteManagedSchema string

//go:embed sql/sqlite/002_mcp_oauth.sql
var sqliteOAuthSchema string

//go:embed sql/sqlite/003_mcp_debug_history.sql
var sqliteDebugHistorySchema string

//go:embed sql/sqlite/004_mcp_oauth_authorization.sql
var sqliteOAuthAuthorizationSchema string

//go:embed sql/sqlite/005_mcp_oauth_refresh.sql
var sqliteOAuthRefreshSchema string

//go:embed sql/mysql/001_mcp_registry.sql
var mysqlManagedSchema string

//go:embed sql/mysql/002_mcp_oauth.sql
var mysqlOAuthSchema string

//go:embed sql/mysql/003_mcp_debug_history.sql
var mysqlDebugHistorySchema string

//go:embed sql/mysql/004_mcp_oauth_authorization.sql
var mysqlOAuthAuthorizationSchema string

//go:embed sql/mysql/005_mcp_oauth_refresh.sql
var mysqlOAuthRefreshSchema string

func ApplySQLiteSchema(ctx context.Context, db *sql.DB) error {
	return applyManagedDDL(ctx, db, sqliteManagedSchema)
}
func ApplyMySQLSchema(ctx context.Context, db *sql.DB) error {
	return applyManagedDDL(ctx, db, mysqlManagedSchema)
}
func ApplySQLiteOAuthSchema(ctx context.Context, db *sql.DB) error {
	return applyManagedDDL(ctx, db, sqliteOAuthSchema)
}
func ApplyMySQLOAuthSchema(ctx context.Context, db *sql.DB) error {
	return applyManagedDDL(ctx, db, mysqlOAuthSchema)
}
func ApplySQLiteDebugHistorySchema(ctx context.Context, db *sql.DB) error {
	return applyManagedDDL(ctx, db, sqliteDebugHistorySchema)
}
func ApplyMySQLDebugHistorySchema(ctx context.Context, db *sql.DB) error {
	return applyManagedDDL(ctx, db, mysqlDebugHistorySchema)
}
func ApplySQLiteOAuthAuthorizationSchema(ctx context.Context, db *sql.DB) error {
	return applyManagedDDL(ctx, db, sqliteOAuthAuthorizationSchema)
}
func ApplyMySQLOAuthAuthorizationSchema(ctx context.Context, db *sql.DB) error {
	return applyManagedDDL(ctx, db, mysqlOAuthAuthorizationSchema)
}
func ApplySQLiteOAuthRefreshSchema(ctx context.Context, db *sql.DB) error {
	return applyManagedDDL(ctx, db, sqliteOAuthRefreshSchema)
}
func ApplyMySQLOAuthRefreshSchema(ctx context.Context, db *sql.DB) error {
	return applyManagedDDL(ctx, db, mysqlOAuthRefreshSchema)
}

func ValidateSchema(ctx context.Context, db *sql.DB) error {
	if err := ValidateRegistrySchema(ctx, db); err != nil {
		return err
	}
	if err := ValidateOAuthSchema(ctx, db); err != nil {
		return err
	}
	if err := ValidateDebugHistorySchema(ctx, db); err != nil {
		return err
	}
	return ValidateOAuthAuthorizationSchema(ctx, db)
}

func ValidateRegistrySchema(ctx context.Context, db *sql.DB) error {
	if ctx == nil || db == nil {
		return errors.New("mcp schema validation context and database are required")
	}
	rows, err := db.QueryContext(ctx, `SELECT tenant_id, server_id, definition_json, revision, created_at_ms, updated_at_ms, updated_by, identity_digest FROM mcp_servers LIMIT 0`)
	if err != nil {
		return fmt.Errorf("mcp schema not ready: %w", err)
	}
	return rows.Close()
}

func ValidateOAuthSchema(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT tenant_id, user_id, server_id, provider, scope_hash, scopes_json, resource, issuer, token_endpoint, client_id, client_secret_ciphertext, token_auth_method, access_token_ciphertext, refresh_token_ciphertext, access_expires_at_ms, refresh_expires_at_ms, status, revision, updated_at_ms, identity_digest FROM mcp_oauth_grants LIMIT 0`)
	if err != nil {
		return fmt.Errorf("mcp oauth schema not ready: %w", err)
	}
	return rows.Close()
}

func ValidateDebugHistorySchema(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT tenant_id, record_id, server_id, operator_id, operation, tool_name, snapshot_id, request_json, response_json, error_code, error_message, latency_ms, created_at_ms, identity_digest FROM mcp_debug_records LIMIT 0`)
	if err != nil {
		return fmt.Errorf("mcp debug history schema not ready: %w", err)
	}
	return rows.Close()
}

// ValidateOAuthBaseSchema is used by the migration that creates the original
// grant table, before the refresh metadata columns are added by mcp/005.
func ValidateOAuthBaseSchema(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT tenant_id, user_id, server_id, provider, scope_hash, scopes_json, resource, access_token_ciphertext, refresh_token_ciphertext, access_expires_at_ms, refresh_expires_at_ms, status, revision, updated_at_ms, identity_digest FROM mcp_oauth_grants LIMIT 0`)
	if err != nil {
		return fmt.Errorf("mcp oauth base schema not ready: %w", err)
	}
	return rows.Close()
}

func ValidateOAuthAuthorizationSchema(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT state_digest, tenant_id, user_id, server_id, provider, scope_hash, scopes_json, resource, issuer, token_endpoint, redirect_uri, client_id, client_secret_ciphertext, token_auth_method, code_verifier_ciphertext, expires_at_ms, consumed_at_ms, created_at_ms FROM mcp_oauth_pending LIMIT 0`)
	if err != nil {
		return fmt.Errorf("mcp oauth authorization schema not ready: %w", err)
	}
	return rows.Close()
}

func applyManagedDDL(ctx context.Context, db *sql.DB, schema string) error {
	if ctx == nil || db == nil {
		return errors.New("mcp migration context and database are required")
	}
	for _, statement := range strings.Split(schema, ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply mcp schema: %w", err)
		}
	}
	return nil
}
