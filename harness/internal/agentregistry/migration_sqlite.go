package agentregistry

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
)

//go:embed sql/sqlite/001_agent_registry.sql
var sqliteRegistrySchema string

//go:embed sql/sqlite/003_agent_config_control.sql
var sqliteConfigControlSchema string

// ApplySQLiteBaseline installs the Registry and Prompt tables owned by this
// package. Cross-module ordering remains the responsibility of the App layer.
func ApplySQLiteBaseline(ctx context.Context, db *sql.DB) error {
	if ctx == nil {
		return errors.New("agentregistry: SQLite migration context is required")
	}
	if db == nil {
		return errors.New("agentregistry: SQLite migration database is required")
	}
	for index, statement := range sqliteMigrationStatements(sqliteRegistrySchema) {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("agentregistry: apply SQLite baseline statement %d: %w", index+1, err)
		}
	}
	return nil
}

// ApplySQLiteConfigControl installs the tenant-scoped authoring, immutable
// version, release pointer, and release audit tables.
func ApplySQLiteConfigControl(ctx context.Context, db *sql.DB) error {
	if ctx == nil {
		return errors.New("agentregistry: SQLite config-control migration context is required")
	}
	if db == nil {
		return errors.New("agentregistry: SQLite config-control migration database is required")
	}
	for index, statement := range sqliteMigrationStatements(sqliteConfigControlSchema) {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("agentregistry: apply SQLite config-control statement %d: %w", index+1, err)
		}
	}
	return nil
}

// ValidateSQLiteSchema verifies the columns consumed by the Registry and
// Prompt SQL stores without changing schema state.
func ValidateSQLiteSchema(ctx context.Context, db *sql.DB) error {
	if ctx == nil {
		return errors.New("agentregistry: SQLite validation context is required")
	}
	if db == nil {
		return errors.New("agentregistry: SQLite validation database is required")
	}
	queries := []struct {
		table   string
		columns string
	}{
		{defaultSQLTable, sqlRecordColumns + ", agent_version_digest, type_version_digest"},
		{defaultSQLSnapshotTable, sqlSnapshotColumns + ", snapshot_ref_digest, agent_mode_digest, agent_hash_digest"},
		{defaultSQLAuditTable, "event_id, event_type, tenant_id, agent_id, agent_version, trace_id, span_id, parent_span_id, request_id, run_id, selection_hash, binding_hash, config_hash, actor, reason, diff_summary, occurred_at_ms, event_id_digest"},
		{defaultSQLPromptTable, sqlPromptColumns + ", prompt_identity_digest"},
	}
	for _, required := range queries {
		rows, err := db.QueryContext(ctx, "SELECT "+required.columns+" FROM "+required.table+" LIMIT 0")
		if err != nil {
			return fmt.Errorf("agentregistry: SQLite schema not ready for table %s: %w", required.table, err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("agentregistry: close SQLite readiness query for table %s: %w", required.table, err)
		}
	}
	return nil
}

func sqliteMigrationStatements(schema string) []string {
	var executable strings.Builder
	for _, line := range strings.Split(schema, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		executable.WriteString(line)
		executable.WriteByte('\n')
	}
	var statements []string
	for _, statement := range strings.Split(executable.String(), ";") {
		if statement = strings.TrimSpace(statement); statement != "" {
			statements = append(statements, statement)
		}
	}
	return statements
}
