package mcp

import (
	"strings"
	"testing"
)

func TestMCPManagedSQLiteAndMySQLSchemasStaySynchronized(t *testing.T) {
	for _, column := range []string{"tenant_id", "server_id", "definition_json", "revision", "created_at_ms", "updated_at_ms", "updated_by", "identity_digest"} {
		if !strings.Contains(sqliteManagedSchema, "\n  "+column+" ") {
			t.Errorf("SQLite MCP schema missing %s", column)
		}
		if !strings.Contains(mysqlManagedSchema, "\n  "+column+" ") {
			t.Errorf("MySQL MCP schema missing %s", column)
		}
	}
	for _, fragment := range []string{"identity_digest BINARY(32) NOT NULL", "UNIQUE KEY uk_mcp_server_identity", "tenant_id(64)", "server_id(64)"} {
		if !strings.Contains(mysqlManagedSchema, fragment) {
			t.Errorf("MySQL MCP schema missing %q", fragment)
		}
	}
	for _, fragment := range []string{
		"row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT",
		"tenant_id VARCHAR(255) NOT NULL COMMENT",
		"definition_json JSON NOT NULL COMMENT",
		"revision BIGINT NOT NULL COMMENT",
		"identity_digest BINARY(32) NOT NULL COMMENT",
		"COMMENT='租户级 MCP Server 定义与版本事实表'",
	} {
		if !strings.Contains(mysqlManagedSchema, fragment) {
			t.Errorf("MySQL MCP schema missing comment %q", fragment)
		}
	}
}

func TestMCPOAuthSQLiteAndMySQLSchemasStaySynchronized(t *testing.T) {
	for _, column := range []string{"tenant_id", "user_id", "server_id", "provider", "scope_hash", "scopes_json", "resource", "access_token_ciphertext", "refresh_token_ciphertext", "status", "revision", "updated_at_ms", "identity_digest"} {
		if !strings.Contains(sqliteOAuthSchema, "\n  "+column+" ") {
			t.Errorf("SQLite MCP OAuth schema missing %s", column)
		}
		if !strings.Contains(mysqlOAuthSchema, "\n  "+column+" ") {
			t.Errorf("MySQL MCP OAuth schema missing %s", column)
		}
	}
	for _, fragment := range []string{"access_token_ciphertext TEXT NOT NULL", "UNIQUE KEY uk_mcp_oauth_grant_identity", "tenant_id(64)", "user_id(64)"} {
		if !strings.Contains(mysqlOAuthSchema, fragment) {
			t.Errorf("MySQL MCP OAuth schema missing %q", fragment)
		}
	}
}

func TestMCPOAuthRefreshMigrationContainsClientMetadata(t *testing.T) {
	for _, column := range []string{"issuer", "token_endpoint", "client_id", "client_secret_ciphertext", "token_auth_method"} {
		if !strings.Contains(sqliteOAuthRefreshSchema, "ADD COLUMN "+column+" ") {
			t.Errorf("SQLite MCP OAuth refresh migration missing %s", column)
		}
		if !strings.Contains(mysqlOAuthRefreshSchema, "ADD COLUMN "+column+" ") {
			t.Errorf("MySQL MCP OAuth refresh migration missing %s", column)
		}
	}
}

func TestMCPOAuthAuthorizationSQLiteAndMySQLSchemasStaySynchronized(t *testing.T) {
	for _, column := range []string{"state_digest", "tenant_id", "user_id", "server_id", "provider", "scope_hash", "scopes_json", "resource", "issuer", "token_endpoint", "redirect_uri", "client_id", "client_secret_ciphertext", "token_auth_method", "code_verifier_ciphertext", "expires_at_ms", "consumed_at_ms", "created_at_ms"} {
		if !strings.Contains(sqliteOAuthAuthorizationSchema, "\n  "+column+" ") {
			t.Errorf("SQLite MCP OAuth authorization schema missing %s", column)
		}
		if !strings.Contains(mysqlOAuthAuthorizationSchema, "\n  "+column+" ") {
			t.Errorf("MySQL MCP OAuth authorization schema missing %s", column)
		}
	}
}
