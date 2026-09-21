package agentregistry

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
)

//go:embed sql/mysql/001_agent_registry.sql
var mysqlRegistrySchema string

//go:embed sql/mysql/003_agent_config_control.sql
var mysqlConfigControlSchema string

type mysqlMigrationExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type mysqlSchemaQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// ApplyMySQLBaseline installs the versioned Registry schema. Re-execution is
// controlled by the application migration ledger. Statements are idempotent
// so rolling-start instances and a retry after partial DDL can converge.
func ApplyMySQLBaseline(ctx context.Context, db *sql.DB) error {
	if ctx == nil {
		return errors.New("agentregistry: MySQL migration context is required")
	}
	if db == nil {
		return errors.New("agentregistry: MySQL migration database is required")
	}
	return applyMySQLBaseline(ctx, db)
}

// ApplyMySQLConfigControl installs the MySQL form of the tenant-scoped
// configuration control plane. The application migration ledger owns ordering.
func ApplyMySQLConfigControl(ctx context.Context, db *sql.DB) error {
	if ctx == nil {
		return errors.New("agentregistry: MySQL config-control migration context is required")
	}
	if db == nil {
		return errors.New("agentregistry: MySQL config-control migration database is required")
	}
	for index, statement := range mysqlMigrationStatements(mysqlConfigControlSchema) {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("agentregistry: apply MySQL config-control statement %d: %w", index+1, err)
		}
	}
	return nil
}

// ValidateMySQLSchemaV2 校验 001 之后引入的摘要列契约。
// 它只做结构门禁：前向 DDL 由发布平台执行，应用启动时不猜测性改表。
func ValidateMySQLSchemaV2(ctx context.Context, db *sql.DB) error {
	if ctx == nil {
		return errors.New("agentregistry: MySQL migration context is required")
	}
	if db == nil {
		return errors.New("agentregistry: MySQL migration database is required")
	}
	return validateMySQLSchemaV2(ctx, db)
}

func applyMySQLBaseline(ctx context.Context, execer mysqlMigrationExecer) error {
	for index, statement := range mysqlMigrationStatements(mysqlRegistrySchema) {
		if _, err := execer.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("agentregistry: apply MySQL baseline statement %d: %w", index+1, err)
		}
	}
	return nil
}

type mysqlSchemaColumn struct {
	dataType             string
	columnType           string
	nullable             string
	extra                string
	generationExpression string
}

type mysqlSchemaIndexColumn struct {
	column       string
	nonUnique    bool
	sequence     int64
	prefixLength sql.NullInt64
}

type mysqlRegistryTableSchema struct {
	table         string
	digestColumns []string
}

var mysqlRegistrySchemaV2 = [...]mysqlRegistryTableSchema{
	{table: "agent_registry_entries", digestColumns: []string{"agent_version_digest", "type_version_digest"}},
	{table: "agent_registry_config_snapshots", digestColumns: []string{"snapshot_ref_digest", "agent_mode_digest", "agent_hash_digest"}},
	{table: "agent_registry_audit_events", digestColumns: []string{"event_id_digest"}},
	{table: "agent_registry_prompt_versions", digestColumns: []string{"prompt_identity_digest"}},
}

const mysqlSchemaV2ColumnsQuery = `
SELECT TABLE_NAME, COLUMN_NAME, DATA_TYPE, COLUMN_TYPE, IS_NULLABLE, EXTRA, GENERATION_EXPRESSION
FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA = DATABASE()
  AND TABLE_NAME IN (
    'agent_registry_entries',
    'agent_registry_config_snapshots',
    'agent_registry_audit_events',
    'agent_registry_prompt_versions'
  )
  AND COLUMN_NAME IN (
    'row_id',
    'agent_version_digest',
    'type_version_digest',
    'snapshot_ref_digest',
    'agent_mode_digest',
    'agent_hash_digest',
    'event_id_digest',
    'prompt_identity_digest'
  )
ORDER BY TABLE_NAME, COLUMN_NAME`

const mysqlSchemaV2IndexesQuery = `
SELECT TABLE_NAME, INDEX_NAME, NON_UNIQUE, SEQ_IN_INDEX, COLUMN_NAME, SUB_PART
FROM information_schema.STATISTICS
WHERE TABLE_SCHEMA = DATABASE()
  AND TABLE_NAME IN (
    'agent_registry_entries',
    'agent_registry_config_snapshots',
    'agent_registry_audit_events',
    'agent_registry_prompt_versions'
  )
ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX`

func validateMySQLSchemaV2(ctx context.Context, execer mysqlSchemaQuerier) error {
	columns, err := readMySQLSchemaColumns(ctx, execer)
	if err != nil {
		return err
	}
	if err := validateMySQLSchemaColumns(columns); err != nil {
		return err
	}
	indexes, err := readMySQLSchemaIndexes(ctx, execer)
	if err != nil {
		return err
	}
	return validateMySQLSchemaIndexes(indexes)
}

func readMySQLSchemaColumns(ctx context.Context, execer mysqlSchemaQuerier) (map[string]mysqlSchemaColumn, error) {
	rows, err := execer.QueryContext(ctx, mysqlSchemaV2ColumnsQuery)
	if err != nil {
		return nil, fmt.Errorf("agentregistry: inspect MySQL Registry columns: %w", err)
	}
	defer rows.Close()

	columns := make(map[string]mysqlSchemaColumn)
	for rows.Next() {
		var table, name, dataType, columnType, nullable string
		var extra, generationExpression sql.NullString
		if err := rows.Scan(&table, &name, &dataType, &columnType, &nullable, &extra, &generationExpression); err != nil {
			return nil, fmt.Errorf("agentregistry: scan MySQL Registry column: %w", err)
		}
		columns[table+"."+name] = mysqlSchemaColumn{
			dataType:             dataType,
			columnType:           columnType,
			nullable:             nullable,
			extra:                extra.String,
			generationExpression: generationExpression.String,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agentregistry: read MySQL Registry columns: %w", err)
	}
	return columns, nil
}

func validateMySQLSchemaColumns(columns map[string]mysqlSchemaColumn) error {
	for _, schema := range mysqlRegistrySchemaV2 {
		rowIDKey := schema.table + ".row_id"
		rowID, ok := columns[rowIDKey]
		if !ok {
			return incompatibleMySQLSchema("missing required column " + rowIDKey)
		}
		if !validMySQLRowID(rowID) {
			return incompatibleMySQLSchema(fmt.Sprintf(
				"column %s must be BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, got data_type=%q column_type=%q nullable=%q extra=%q",
				rowIDKey, rowID.dataType, rowID.columnType, rowID.nullable, rowID.extra,
			))
		}
		for _, name := range schema.digestColumns {
			key := schema.table + "." + name
			column, ok := columns[key]
			if !ok {
				return incompatibleMySQLSchema("missing required column " + key)
			}
			if !validMySQLDigestColumn(column) {
				return incompatibleMySQLSchema(fmt.Sprintf(
					"column %s must be an ordinary BINARY(32) NOT NULL column (not GENERATED), got data_type=%q column_type=%q nullable=%q extra=%q generation_expression=%q",
					key, column.dataType, column.columnType, column.nullable, column.extra, column.generationExpression,
				))
			}
		}
	}
	return nil
}

func validMySQLRowID(column mysqlSchemaColumn) bool {
	return strings.EqualFold(strings.TrimSpace(column.dataType), "bigint") &&
		strings.Contains(strings.ToLower(column.columnType), "unsigned") &&
		strings.EqualFold(strings.TrimSpace(column.nullable), "NO") &&
		strings.Contains(strings.ToLower(column.extra), "auto_increment") &&
		!mysqlColumnIsGenerated(column)
}

func validMySQLDigestColumn(column mysqlSchemaColumn) bool {
	return strings.EqualFold(strings.TrimSpace(column.dataType), "binary") &&
		strings.EqualFold(strings.ReplaceAll(column.columnType, " ", ""), "binary(32)") &&
		strings.EqualFold(strings.TrimSpace(column.nullable), "NO") &&
		!mysqlColumnIsGenerated(column)
}

func mysqlColumnIsGenerated(column mysqlSchemaColumn) bool {
	return strings.Contains(strings.ToUpper(column.extra), "GENERATED") ||
		strings.TrimSpace(column.generationExpression) != ""
}

func readMySQLSchemaIndexes(ctx context.Context, execer mysqlSchemaQuerier) (map[string][]mysqlSchemaIndexColumn, error) {
	rows, err := execer.QueryContext(ctx, mysqlSchemaV2IndexesQuery)
	if err != nil {
		return nil, fmt.Errorf("agentregistry: inspect MySQL Registry indexes: %w", err)
	}
	defer rows.Close()

	indexes := make(map[string][]mysqlSchemaIndexColumn)
	for rows.Next() {
		var table, name string
		var nonUnique, sequence int64
		var column sql.NullString
		var prefixLength sql.NullInt64
		if err := rows.Scan(&table, &name, &nonUnique, &sequence, &column, &prefixLength); err != nil {
			return nil, fmt.Errorf("agentregistry: scan MySQL Registry index: %w", err)
		}
		indexes[table+"."+name] = append(indexes[table+"."+name], mysqlSchemaIndexColumn{
			column:       column.String,
			nonUnique:    nonUnique != 0,
			sequence:     sequence,
			prefixLength: prefixLength,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agentregistry: read MySQL Registry indexes: %w", err)
	}
	return indexes, nil
}

func validateMySQLSchemaIndexes(indexes map[string][]mysqlSchemaIndexColumn) error {
	for _, schema := range mysqlRegistrySchemaV2 {
		if !hasMySQLSingleColumnUniqueIndex(indexes, schema.table, "row_id", true) {
			return incompatibleMySQLSchema("missing PRIMARY KEY on " + schema.table + ".row_id")
		}
		for _, column := range schema.digestColumns {
			if !hasMySQLSingleColumnUniqueIndex(indexes, schema.table, column, false) {
				return incompatibleMySQLSchema("missing unique single-column index on " + schema.table + "." + column)
			}
		}
	}
	return nil
}

func hasMySQLSingleColumnUniqueIndex(indexes map[string][]mysqlSchemaIndexColumn, table, column string, primaryOnly bool) bool {
	for key, columns := range indexes {
		prefix := table + "."
		if !strings.HasPrefix(key, prefix) || (primaryOnly && key != prefix+"PRIMARY") {
			continue
		}
		if len(columns) == 1 && !columns[0].nonUnique && columns[0].sequence == 1 &&
			columns[0].column == column && !columns[0].prefixLength.Valid {
			return true
		}
	}
	return false
}

func incompatibleMySQLSchema(reason string) error {
	return fmt.Errorf(
		"agentregistry: incompatible MySQL Registry schema: %s; apply the current schema migration before starting",
		reason,
	)
}

func mysqlMigrationStatements(schema string) []string {
	lines := strings.Split(schema, "\n")
	filtered := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		filtered = append(filtered, line)
	}
	parts := strings.Split(strings.Join(filtered, "\n"), ";")
	statements := make([]string, 0, len(parts))
	for _, part := range parts {
		if statement := strings.TrimSpace(part); statement != "" {
			statements = append(statements, statement)
		}
	}
	return statements
}
