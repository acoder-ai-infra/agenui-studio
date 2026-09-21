// Package migration coordinates versioned schema migrations without taking
// ownership of module-specific DDL.
package migration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const createLedgerSQL = `CREATE TABLE IF NOT EXISTS harness_schema_migrations (
  row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增物理主键',
  migration_id VARCHAR(128) NOT NULL COMMENT '只前进的模块迁移版本标识',
  applied_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '迁移成功记录时间',
  PRIMARY KEY (row_id),
  UNIQUE KEY uk_harness_schema_migration_id (migration_id)
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC DEFAULT CHARSET=utf8mb4 COMMENT='Harness 模块 Schema 迁移版本表'`

const createSQLiteLedgerSQL = `CREATE TABLE IF NOT EXISTS harness_schema_migrations (
  migration_id TEXT NOT NULL PRIMARY KEY,
  applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
)`

// Step identifies one module-owned, forward-only migration.
type Step struct {
	ID    string
	Apply func(context.Context) error
}

// Apply executes unapplied steps in order and records a step only after its
// owner reports success. Owner steps must be idempotent; the version-table
// unique key lets concurrent rolling-start instances converge without a lock.
func Apply(ctx context.Context, db *sql.DB, steps ...Step) error {
	return apply(ctx, db, createLedgerSQL, steps...)
}

// ApplySQLite uses the same ordered owner-step contract with SQLite-compatible
// ledger DDL. It exists so local/CI storage is not an unversioned special case.
func ApplySQLite(ctx context.Context, db *sql.DB, steps ...Step) error {
	return apply(ctx, db, createSQLiteLedgerSQL, steps...)
}

func apply(ctx context.Context, db *sql.DB, createLedger string, steps ...Step) error {
	if ctx == nil {
		return errors.New("migration: context is required")
	}
	if db == nil {
		return errors.New("migration: database is required")
	}
	seen := make(map[string]struct{}, len(steps))
	for index := range steps {
		steps[index].ID = strings.TrimSpace(steps[index].ID)
		if steps[index].ID == "" || steps[index].Apply == nil {
			return errors.New("migration: step id and apply function are required")
		}
		if len(steps[index].ID) > 128 {
			return fmt.Errorf("migration: step id %q exceeds 128 bytes", steps[index].ID)
		}
		if _, duplicate := seen[steps[index].ID]; duplicate {
			return fmt.Errorf("migration: duplicate step %q", steps[index].ID)
		}
		seen[steps[index].ID] = struct{}{}
	}
	if createLedger != "" {
		if _, err := db.ExecContext(ctx, createLedger); err != nil {
			return fmt.Errorf("migration: initialize ledger: %w", err)
		}
	}

	for _, step := range steps {
		applied, err := isApplied(ctx, db, step.ID)
		if err != nil {
			return fmt.Errorf("migration: inspect step %q: %w", step.ID, err)
		}
		if applied {
			continue
		}
		if err := step.Apply(ctx); err != nil {
			return fmt.Errorf("migration: apply step %q: %w", step.ID, err)
		}
		if _, err := db.ExecContext(ctx,
			"INSERT INTO harness_schema_migrations (migration_id) VALUES (?)", step.ID,
		); err != nil {
			// Concurrent rolling-start instances may both finish the same
			// idempotent owner migration. The unique marker is the CAS point.
			if recorded, inspectErr := isApplied(ctx, db, step.ID); inspectErr == nil && recorded {
				continue
			}
			return fmt.Errorf("migration: record step %q: %w", step.ID, err)
		}
	}
	return nil
}

// RequireApplied verifies that every required owner version is recorded. It is
// metadata-only and is safe for Runtime readiness and migration status checks.
func RequireApplied(ctx context.Context, db *sql.DB, ids ...string) error {
	if ctx == nil {
		return errors.New("migration: context is required")
	}
	if db == nil {
		return errors.New("migration: database is required")
	}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			return errors.New("migration: required step id is empty")
		}
		applied, err := isApplied(ctx, db, id)
		if err != nil {
			return fmt.Errorf("migration: inspect required step %q: %w", id, err)
		}
		if !applied {
			return fmt.Errorf("migration: required step %q is not applied", id)
		}
	}
	return nil
}

func isApplied(ctx context.Context, db *sql.DB, id string) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM harness_schema_migrations WHERE migration_id = ?", id,
	).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}
