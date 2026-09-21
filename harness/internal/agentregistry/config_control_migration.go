package agentregistry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var configControlRequiredColumns = [...]struct {
	table   string
	columns string
}{
	{configDraftTable, "tenant_id, agent_id, config_json, content_hash, revision, created_at_ms, updated_at_ms, created_by, updated_by, identity_digest"},
	{configVersionTable, "tenant_id, agent_id, version, config_json, content_hash, source_draft_revision, created_at_ms, created_by, prepared_json, identity_digest"},
	{configReleaseTable, "tenant_id, environment, agent_id, version, content_hash, revision, created_at_ms, updated_at_ms, updated_by, identity_digest"},
	{configReleaseEventTable, "event_id, tenant_id, environment, agent_id, from_version, to_version, content_hash, release_revision, actor, reason, occurred_at_ms, event_id_digest"},
}

// ValidateConfigControlSchema is metadata-only and works for SQLite, MySQL and
// MySQL because it asks each backend to compile the exact Store projection.
func ValidateConfigControlSchema(ctx context.Context, db *sql.DB) error {
	if ctx == nil {
		return errors.New("agentregistry: config-control validation context is required")
	}
	if db == nil {
		return errors.New("agentregistry: config-control validation database is required")
	}
	for _, required := range configControlRequiredColumns {
		rows, err := db.QueryContext(ctx, "SELECT "+required.columns+" FROM "+required.table+" LIMIT 0")
		if err != nil {
			return fmt.Errorf("agentregistry: config-control schema not ready for table %s: %w", required.table, err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("agentregistry: close config-control readiness query for table %s: %w", required.table, err)
		}
	}
	return nil
}

func ApplySQLitePreparedProjection(ctx context.Context, db *sql.DB) error {
	return applyPreparedProjection(ctx, db, "BLOB")
}

func ApplyMySQLPreparedProjection(ctx context.Context, db *sql.DB) error {
	return applyPreparedProjection(ctx, db, "LONGTEXT NULL")
}

func applyPreparedProjection(ctx context.Context, db *sql.DB, definition string) error {
	if ctx == nil || db == nil {
		return errors.New("agentregistry: prepared projection migration context and database are required")
	}
	rows, err := db.QueryContext(ctx, "SELECT prepared_json FROM "+configVersionTable+" LIMIT 0")
	if err == nil {
		return rows.Close()
	}
	if _, alterErr := db.ExecContext(ctx, "ALTER TABLE "+configVersionTable+" ADD COLUMN prepared_json "+definition); alterErr != nil {
		return fmt.Errorf("agentregistry: add prepared runtime projection: %w", alterErr)
	}
	rows, err = db.QueryContext(ctx, "SELECT prepared_json FROM "+configVersionTable+" LIMIT 0")
	if err != nil {
		return fmt.Errorf("agentregistry: validate prepared runtime projection: %w", err)
	}
	return rows.Close()
}
