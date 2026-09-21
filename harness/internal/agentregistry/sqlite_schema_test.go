package agentregistry

import (
	"reflect"
	"strings"
	"testing"
)

func TestSQLiteRegistrySchemaCoversSQLStoreColumns(t *testing.T) {
	tableDDL := sqliteTableDDL(t, "agent_registry_entries")
	lastPosition := -1
	for _, column := range strings.Split(sqlRecordColumns, ", ") {
		position := strings.Index(tableDDL, "\n  "+column+" ")
		if position < 0 {
			t.Errorf("SQLite registry schema missing SQLStore column %q", column)
			continue
		}
		if position <= lastPosition {
			t.Errorf("SQLite registry column %q is out of SQLStore order", column)
		}
		lastPosition = position
	}

	for _, fragment := range []string{
		"PRIMARY KEY (agent_id, version)",
		"UNIQUE (agent_type, version)",
		"status IN ('enabled', 'disabled')",
		"gray_percent BETWEEN 0 AND 100",
		"revision >= 1",
		"agent_registry_entries_agent_lookup_idx",
		"agent_registry_entries_type_lookup_idx",
	} {
		if !strings.Contains(sqliteRegistrySchema, fragment) {
			t.Errorf("SQLite registry schema missing %q", fragment)
		}
	}
}

func TestSQLiteRegistryAuditSchemaCoversRegistryEvent(t *testing.T) {
	tableDDL := sqliteTableDDL(t, "agent_registry_audit_events")
	eventType := reflect.TypeOf(RegistryEvent{})
	for i := 0; i < eventType.NumField(); i++ {
		jsonName := strings.Split(eventType.Field(i).Tag.Get("json"), ",")[0]
		column := mapRegistryEventColumn(jsonName)
		if !strings.Contains(tableDDL, "\n  "+column+" ") {
			t.Errorf("SQLite audit schema missing RegistryEvent field %q as column %q", jsonName, column)
		}
	}

	for _, index := range []string{
		"agent_registry_audit_events_agent_time_idx",
		"agent_registry_audit_events_tenant_time_idx",
		"agent_registry_audit_events_trace_time_idx",
		"agent_registry_audit_events_run_time_idx",
		"agent_registry_audit_events_type_time_idx",
	} {
		if !strings.Contains(sqliteRegistrySchema, index) {
			t.Errorf("SQLite audit schema missing index %q", index)
		}
	}
}

func TestSQLiteRegistryConfigSnapshotSchemaIsContentAddressed(t *testing.T) {
	tableDDL := sqliteTableDDL(t, "agent_registry_config_snapshots")
	for _, fragment := range []string{
		"config_snapshot_ref TEXT NOT NULL PRIMARY KEY",
		"agent_id TEXT NOT NULL",
		"version TEXT NOT NULL",
		"execution_mode TEXT NOT NULL",
		"config_hash TEXT NOT NULL",
		"effective_json TEXT NOT NULL",
		"created_at_ms INTEGER NOT NULL",
		"UNIQUE (agent_id, version, execution_mode)",
		"UNIQUE (agent_id, version, config_hash)",
	} {
		if !strings.Contains(tableDDL, fragment) {
			t.Errorf("SQLite config snapshot schema missing %q", fragment)
		}
	}
	if !strings.Contains(sqliteRegistrySchema, "agent_registry_config_snapshots_agent_time_idx") {
		t.Error("SQLite config snapshot schema missing agent/version lookup index")
	}
}

func TestSQLitePromptSchemaCoversImmutableStoreContract(t *testing.T) {
	tableDDL := sqliteTableDDL(t, defaultSQLPromptTable)
	lastPosition := -1
	for _, column := range strings.Split(sqlPromptColumns, ", ") {
		position := strings.Index(tableDDL, "\n  "+column+" ")
		if position < 0 {
			t.Errorf("SQLite prompt schema missing SQLPromptStore column %q", column)
			continue
		}
		if position <= lastPosition {
			t.Errorf("SQLite prompt column %q is out of SQLPromptStore order", column)
		}
		lastPosition = position
	}
	for _, fragment := range []string{
		"PRIMARY KEY (prompt_ref, version)",
		"json_type(variables_json) = 'array'",
		"content IS NOT NULL",
		"content_ref IS NOT NULL",
		"created_at_ms >= 0",
	} {
		if !strings.Contains(tableDDL, fragment) {
			t.Errorf("SQLite prompt schema missing %q", fragment)
		}
	}
}

func TestSQLiteRegistrySchemaDoesNotOwnRuntimeFacts(t *testing.T) {
	// 这些表由 Session/Run Storage、Control 或 Scheduler owner 管理。
	for _, table := range []string{
		"sessions",
		"messages",
		"runs",
		"steps",
		"run_steps",
		"agent_events",
		"control_requests",
		"checkpoints",
		"checkpoint_meta",
		"scheduler_dispatches",
		"scheduler_jobs",
	} {
		declaration := "create table if not exists " + table
		if strings.Contains(strings.ToLower(sqliteRegistrySchema), declaration) {
			t.Errorf("Registry migration must not create external owner table %q", table)
		}
	}
}

func sqliteTableDDL(t *testing.T, table string) string {
	t.Helper()
	declaration := "CREATE TABLE IF NOT EXISTS " + table + " ("
	start := strings.Index(sqliteRegistrySchema, declaration)
	if start < 0 {
		t.Fatalf("SQLite registry schema missing table %q", table)
	}
	end := strings.Index(sqliteRegistrySchema[start:], "\n);")
	if end < 0 {
		t.Fatalf("SQLite registry table %q has no closing declaration", table)
	}
	return sqliteRegistrySchema[start : start+end]
}

func mapRegistryEventColumn(jsonName string) string {
	switch jsonName {
	case "type":
		return "event_type"
	case "tenant":
		return "tenant_id"
	case "version":
		return "agent_version"
	case "timestamp":
		return "occurred_at_ms"
	default:
		return jsonName
	}
}
