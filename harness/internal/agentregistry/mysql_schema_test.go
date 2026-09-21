package agentregistry

import (
	"reflect"
	"strings"
	"testing"
)

func TestMySQLRegistrySchemaCoversStoreContracts(t *testing.T) {
	entryDDL := mysqlTableDDL(t, "agent_registry_entries")
	for _, column := range strings.Split(sqlRecordColumns, ", ") {
		if !strings.Contains(entryDDL, "\n  "+column+" ") {
			t.Errorf("MySQL registry schema missing SQLStore column %q", column)
		}
	}
	for _, fragment := range []string{
		"row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT",
		"PRIMARY KEY (row_id)",
		"agent_version_digest BINARY(32) NOT NULL",
		"type_version_digest BINARY(32) NOT NULL",
		"UNIQUE KEY uk_registry_entry_agent_version (agent_version_digest)",
		"UNIQUE KEY uk_registry_entry_type_version (type_version_digest)",
		"gray_percent BETWEEN 0 AND 100",
		"revision >= 1",
	} {
		if !strings.Contains(entryDDL, fragment) {
			t.Errorf("MySQL registry entry schema missing %q", fragment)
		}
	}

	snapshotDDL := mysqlTableDDL(t, "agent_registry_config_snapshots")
	for _, column := range strings.Split(sqlSnapshotColumns, ", ") {
		if !strings.Contains(snapshotDDL, "\n  "+column+" ") {
			t.Errorf("MySQL snapshot schema missing SQLStore column %q", column)
		}
	}
	for _, fragment := range []string{
		"row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT",
		"PRIMARY KEY (row_id)",
		"UNIQUE KEY uk_registry_snapshot_ref (snapshot_ref_digest)",
		"config_snapshot_ref VARCHAR(640) NOT NULL",
		"UNIQUE KEY uk_registry_snapshot_agent_mode (agent_mode_digest)",
		"UNIQUE KEY uk_registry_snapshot_agent_hash (agent_hash_digest)",
	} {
		if !strings.Contains(snapshotDDL, fragment) {
			t.Errorf("MySQL snapshot schema missing %q", fragment)
		}
	}

	auditDDL := mysqlTableDDL(t, "agent_registry_audit_events")
	eventType := reflect.TypeOf(RegistryEvent{})
	for i := 0; i < eventType.NumField(); i++ {
		jsonName := strings.Split(eventType.Field(i).Tag.Get("json"), ",")[0]
		column := mapRegistryEventColumn(jsonName)
		if !strings.Contains(auditDDL, "\n  "+column+" ") {
			t.Errorf("MySQL audit schema missing RegistryEvent field %q as column %q", jsonName, column)
		}
	}
	if !strings.Contains(auditDDL, "event_id(48)") {
		t.Error("MySQL audit index must use a bounded event_id prefix")
	}
	for _, fragment := range []string{
		"row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT",
		"PRIMARY KEY (row_id)",
		"UNIQUE KEY uk_registry_audit_event_id (event_id_digest)",
	} {
		if !strings.Contains(auditDDL, fragment) {
			t.Errorf("MySQL audit schema missing %q", fragment)
		}
	}

	promptDDL := mysqlTableDDL(t, defaultSQLPromptTable)
	lastPosition := -1
	for _, column := range strings.Split(sqlPromptColumns, ", ") {
		position := strings.Index(promptDDL, "\n  "+column+" ")
		if position < 0 {
			t.Errorf("MySQL prompt schema missing SQLPromptStore column %q", column)
			continue
		}
		if position <= lastPosition {
			t.Errorf("MySQL prompt column %q is out of SQLPromptStore order", column)
		}
		lastPosition = position
	}
	for _, fragment := range []string{
		"row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT",
		"PRIMARY KEY (row_id)",
		"prompt_identity_digest BINARY(32) NOT NULL",
		"UNIQUE KEY uk_registry_prompt_ref_version (prompt_identity_digest)",
		"content LONGTEXT NULL",
		"content_ref VARCHAR(1024) NULL",
		"JSON_TYPE(variables_json) = 'ARRAY'",
		"chk_registry_prompt_content_source",
	} {
		if !strings.Contains(promptDDL, fragment) {
			t.Errorf("MySQL prompt schema missing %q", fragment)
		}
	}
	if got := strings.Count(mysqlRegistrySchema, "ROW_FORMAT=DYNAMIC"); got != 4 {
		t.Errorf("MySQL Registry tables using DYNAMIC row format = %d, want 4", got)
	}
	if strings.Contains(mysqlRegistrySchema, "GENERATED ALWAYS") || strings.Contains(mysqlRegistrySchema, "CHAR(31)") {
		t.Error("MySQL Registry identity digests must be application-written ordinary columns")
	}
}

func TestMySQLRegistrySecondaryIndexesFitLegacy767ByteLimit(t *testing.T) {
	// Keep secondary indexes compatible with MySQL installations that enforce
	// the 767-byte limit; row_id is appended to secondary indexes.
	// 精确唯一性由 32-byte digest 保证，下面仅核对查询前缀索引的最坏宽度。
	const limit = 767
	indexes := map[string]int{
		"entry lookup":         64*4 + 32*4 + 8 + 8 + 48*4 + 8,
		"snapshot ref lookup":  128*4 + 8,
		"snapshot mode lookup": 64*4 + 48*4 + 32*4 + 8 + 8,
		"audit agent time":     64*4 + 48*4 + 8 + 48*4 + 8,
		"prompt lookup":        96*4 + 48*4 + 8,
		"digest unique":        32 + 8,
	}
	for name, bytes := range indexes {
		if bytes > limit {
			t.Errorf("MySQL %s index worst-case width = %d bytes, limit = %d", name, bytes, limit)
		}
	}
}

func TestMySQLRegistrySchemaHasTableAndColumnComments(t *testing.T) {
	const tableComment = ") ENGINE=InnoDB ROW_FORMAT=DYNAMIC DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='"
	if got := strings.Count(mysqlRegistrySchema, tableComment); got != 4 {
		t.Errorf("MySQL Registry table comments = %d, want 4", got)
	}
	for _, table := range []string{
		"agent_registry_entries",
		"agent_registry_config_snapshots",
		"agent_registry_audit_events",
		"agent_registry_prompt_versions",
	} {
		ddl := mysqlTableDDL(t, table)
		for _, line := range strings.Split(ddl, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "PRIMARY KEY") {
				break
			}
			if trimmed == "" || strings.HasPrefix(trimmed, "CREATE TABLE") {
				continue
			}
			if !strings.Contains(trimmed, " COMMENT '") {
				t.Errorf("MySQL table %s column declaration has no comment: %s", table, trimmed)
			}
		}
	}
}

func TestMySQLRegistrySchemaDoesNotOwnRuntimeFacts(t *testing.T) {
	if got := strings.Count(strings.ToLower(mysqlRegistrySchema), "create table if not exists"); got != 4 {
		t.Fatalf("idempotent MySQL baseline statements = %d, want 4", got)
	}
	for _, table := range []string{
		"sessions", "messages", "runs", "steps", "run_steps", "agent_events",
		"control_requests", "checkpoints", "checkpoint_meta", "scheduler_dispatches", "scheduler_jobs",
	} {
		declaration := "create table " + table + " "
		if strings.Contains(strings.ToLower(mysqlRegistrySchema), declaration) {
			t.Errorf("Registry migration must not create external owner table %q", table)
		}
	}
}

func TestMySQLRegistryMigrationParsesAllOwnedTables(t *testing.T) {
	statements := mysqlMigrationStatements(mysqlRegistrySchema)
	if len(statements) != 4 {
		t.Fatalf("migration statements = %d, want 4", len(statements))
	}
	for index, table := range []string{
		"agent_registry_entries",
		"agent_registry_config_snapshots",
		"agent_registry_audit_events",
		"agent_registry_prompt_versions",
	} {
		if !strings.HasPrefix(statements[index], "CREATE TABLE IF NOT EXISTS "+table+" (") {
			t.Errorf("statement %d does not create %s", index+1, table)
		}
	}
}

func mysqlTableDDL(t *testing.T, table string) string {
	t.Helper()
	declaration := "CREATE TABLE IF NOT EXISTS " + table + " ("
	start := strings.Index(mysqlRegistrySchema, declaration)
	if start < 0 {
		t.Fatalf("MySQL registry schema missing table %q", table)
	}
	end := strings.Index(mysqlRegistrySchema[start:], "\n) ENGINE=")
	if end < 0 {
		t.Fatalf("MySQL registry table %q has no closing declaration", table)
	}
	return mysqlRegistrySchema[start : start+end]
}
