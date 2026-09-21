package skill

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
)

//go:embed sql/sqlite/001_skill_registry.sql
var sqliteSchema string

//go:embed sql/mysql/001_skill_registry.sql
var mysqlSchema string

//go:embed sql/sqlite/002_skill_retirements.sql
var sqliteLifecycleSchema string

//go:embed sql/mysql/002_skill_retirements.sql
var mysqlLifecycleSchema string

//go:embed sql/sqlite/003_skill_package_files.sql
var sqlitePackageFileSchema string

//go:embed sql/mysql/003_skill_package_files.sql
var mysqlPackageFileSchema string

func ApplySQLiteSchema(ctx context.Context, db *sql.DB) error {
	return applySkillDDL(ctx, db, sqliteSchema)
}
func ApplyMySQLSchema(ctx context.Context, db *sql.DB) error {
	return applySkillDDL(ctx, db, mysqlSchema)
}

func ApplySQLiteLifecycleSchema(ctx context.Context, db *sql.DB) error {
	return applySkillDDL(ctx, db, sqliteLifecycleSchema)
}

func ApplyMySQLLifecycleSchema(ctx context.Context, db *sql.DB) error {
	return applySkillDDL(ctx, db, mysqlLifecycleSchema)
}

func ApplySQLitePackageFileSchema(ctx context.Context, db *sql.DB) error {
	return applySkillDDL(ctx, db, sqlitePackageFileSchema)
}

func ApplyMySQLPackageFileSchema(ctx context.Context, db *sql.DB) error {
	return applySkillDDL(ctx, db, mysqlPackageFileSchema)
}

func ValidateSchema(ctx context.Context, db *sql.DB) error {
	if ctx == nil || db == nil {
		return errors.New("skill schema validation context and database are required")
	}
	rows, err := db.QueryContext(ctx, `SELECT tenant_id, skill_id, version, definition_json, content_hash, content_size, content, published_at_ms, identity_digest FROM skill_versions LIMIT 0`)
	if err != nil {
		return fmt.Errorf("skill schema not ready: %w", err)
	}
	return rows.Close()
}

func ValidateLifecycleSchema(ctx context.Context, db *sql.DB) error {
	if ctx == nil || db == nil {
		return errors.New("skill lifecycle schema validation context and database are required")
	}
	rows, err := db.QueryContext(ctx, `SELECT tenant_id, skill_id, version, retired_at_ms, identity_digest FROM skill_retirements LIMIT 0`)
	if err != nil {
		return fmt.Errorf("skill lifecycle schema not ready: %w", err)
	}
	return rows.Close()
}

func ValidatePackageFileSchema(ctx context.Context, db *sql.DB) error {
	if ctx == nil || db == nil {
		return errors.New("skill package file schema validation context and database are required")
	}
	rows, err := db.QueryContext(ctx, `SELECT tenant_id, skill_id, version, file_path, mime_type, content_hash, content_size, previewable, artifact_ref, identity_digest FROM skill_package_files LIMIT 0`)
	if err != nil {
		return fmt.Errorf("skill package file schema not ready: %w", err)
	}
	return rows.Close()
}

func applySkillDDL(ctx context.Context, db *sql.DB, schema string) error {
	if ctx == nil || db == nil {
		return errors.New("skill migration context and database are required")
	}
	for _, statement := range strings.Split(schema, ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply skill schema: %w", err)
		}
	}
	return nil
}
