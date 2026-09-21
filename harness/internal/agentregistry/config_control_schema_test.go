package agentregistry

import (
	"strings"
	"testing"
)

func TestAgentConfigControlSchemasStaySynchronized(t *testing.T) {
	tables := map[string][]string{
		"agent_config_drafts": {
			"tenant_id", "agent_id", "config_json", "content_hash", "revision",
			"created_at_ms", "updated_at_ms", "created_by", "updated_by", "identity_digest",
		},
		"agent_config_versions": {
			"tenant_id", "agent_id", "version", "config_json", "content_hash", "source_draft_revision",
			"created_at_ms", "created_by", "prepared_json", "identity_digest",
		},
		"agent_config_releases": {
			"tenant_id", "environment", "agent_id", "version", "content_hash", "revision",
			"created_at_ms", "updated_at_ms", "updated_by", "identity_digest",
		},
		"agent_config_release_events": {
			"event_id", "tenant_id", "environment", "agent_id", "from_version", "to_version",
			"content_hash", "release_revision", "actor", "reason", "occurred_at_ms", "event_id_digest",
		},
	}
	for table, columns := range tables {
		sqliteDDL := configControlTableDDL(t, sqliteConfigControlSchema, table)
		mysqlDDL := configControlTableDDL(t, mysqlConfigControlSchema, table)
		for _, column := range columns {
			if !strings.Contains(sqliteDDL, "\n  "+column+" ") {
				t.Errorf("SQLite %s missing column %s", table, column)
			}
			if !strings.Contains(mysqlDDL, "\n  "+column+" ") {
				t.Errorf("MySQL %s missing column %s", table, column)
			}
		}
	}
	if got := strings.Count(strings.ToLower(sqliteConfigControlSchema), "create table if not exists"); got != len(tables) {
		t.Fatalf("SQLite control-plane tables = %d, want %d", got, len(tables))
	}
	if got := strings.Count(strings.ToLower(mysqlConfigControlSchema), "create table if not exists"); got != len(tables) {
		t.Fatalf("MySQL control-plane tables = %d, want %d", got, len(tables))
	}
}

func TestMySQLAgentConfigControlUsesDigestUniquenessAndBoundedIndexes(t *testing.T) {
	for _, fragment := range []string{
		"identity_digest BINARY(32) NOT NULL",
		"event_id_digest BINARY(32) NOT NULL",
		"UNIQUE KEY uk_agent_config_draft_identity (identity_digest)",
		"UNIQUE KEY uk_agent_config_version_identity (identity_digest)",
		"UNIQUE KEY uk_agent_config_release_identity (identity_digest)",
		"UNIQUE KEY uk_agent_config_release_event_id (event_id_digest)",
		"tenant_id(64)",
		"agent_id(64)",
	} {
		if !strings.Contains(mysqlConfigControlSchema, fragment) {
			t.Errorf("MySQL config-control schema missing %q", fragment)
		}
	}
	if strings.Contains(mysqlConfigControlSchema, "GENERATED ALWAYS") {
		t.Fatal("MySQL config-control identity digests must be application-written")
	}
	const legacyIndexLimit = 767
	indexes := map[string]int{
		"draft tenant time":          64*4 + 8 + 64*4 + 8,
		"version tenant agent time":  64*4 + 64*4 + 8 + 48*4 + 8,
		"release tenant environment": 64*4 + 32*4 + 8 + 64*4 + 8,
		"release event scope time":   48*4 + 32*4 + 48*4 + 8 + 48*4 + 8,
		"digest unique":              32 + 8,
	}
	for name, width := range indexes {
		if width > legacyIndexLimit {
			t.Errorf("MySQL config-control %s index worst-case width = %d, limit = %d", name, width, legacyIndexLimit)
		}
	}
}

func TestMySQLAgentConfigControlMigrationParsesAllOwnedTables(t *testing.T) {
	statements := mysqlMigrationStatements(mysqlConfigControlSchema)
	tables := []string{
		"agent_config_drafts", "agent_config_versions", "agent_config_releases", "agent_config_release_events",
	}
	if len(statements) != len(tables) {
		t.Fatalf("MySQL config-control statements = %d, want %d", len(statements), len(tables))
	}
	for index, table := range tables {
		if !strings.HasPrefix(statements[index], "CREATE TABLE IF NOT EXISTS "+table+" (") {
			t.Errorf("statement %d does not create %s", index+1, table)
		}
	}
}

func configControlTableDDL(t *testing.T, schema, table string) string {
	t.Helper()
	declaration := "CREATE TABLE IF NOT EXISTS " + table + " ("
	start := strings.Index(schema, declaration)
	if start < 0 {
		t.Fatalf("schema missing table %s", table)
	}
	end := strings.Index(schema[start:], "\n);")
	if end < 0 {
		end = strings.Index(schema[start:], "\n) ENGINE=")
	}
	if end < 0 {
		t.Fatalf("schema table %s has no closing declaration", table)
	}
	return schema[start : start+end]
}
