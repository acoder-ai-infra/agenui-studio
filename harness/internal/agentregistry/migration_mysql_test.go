package agentregistry

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestValidateMySQLSchemaV2AcceptsCompatibleSchema(t *testing.T) {
	db, mock := newMySQLMigrationMock(t)
	expectMySQLSchemaV2(mock, mysqlV2ColumnRows(), mysqlV2IndexRows())

	if err := ValidateMySQLSchemaV2(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateMySQLSchemaV2PreservesInspectionFailure(t *testing.T) {
	db, mock := newMySQLMigrationMock(t)
	queryFailure := errors.New("table unavailable")
	mock.ExpectQuery("FROM information_schema\\.COLUMNS").
		WillReturnError(queryFailure)

	err := ValidateMySQLSchemaV2(context.Background(), db)
	if !errors.Is(err, queryFailure) || !strings.Contains(err.Error(), "MySQL Registry") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateMySQLSchemaV2RejectsMissingRowID(t *testing.T) {
	db, mock := newMySQLMigrationMock(t)
	columns := mysqlV2ColumnRows()
	filtered := columns[:0]
	for _, column := range columns {
		if column.table == "agent_registry_prompt_versions" && column.column == "row_id" {
			continue
		}
		filtered = append(filtered, column)
	}
	expectMySQLSchemaV2(mock, filtered, nil)

	err := ValidateMySQLSchemaV2(context.Background(), db)
	if err == nil || !strings.Contains(err.Error(), "agent_registry_prompt_versions.row_id") {
		t.Fatalf("error = %v, want missing row_id", err)
	}
	if !strings.Contains(err.Error(), "current schema migration") {
		t.Errorf("error = %v, want migration guidance", err)
	}
}

func TestValidateMySQLSchemaV2RejectsIncompatibleDigestColumns(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*mysqlV2Column)
		wantError string
	}{
		{
			name: "generated",
			mutate: func(column *mysqlV2Column) {
				column.extra = "STORED GENERATED"
				column.generationExpression = "unhex(sha2(agent_id, 256))"
			},
			wantError: "ordinary BINARY(32) NOT NULL",
		},
		{
			name: "wrong type",
			mutate: func(column *mysqlV2Column) {
				column.dataType = "varbinary"
				column.columnType = "varbinary(32)"
			},
			wantError: "ordinary BINARY(32) NOT NULL",
		},
		{
			name: "nullable",
			mutate: func(column *mysqlV2Column) {
				column.nullable = "YES"
			},
			wantError: "ordinary BINARY(32) NOT NULL",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, mock := newMySQLMigrationMock(t)
			columns := mysqlV2ColumnRows()
			for index := range columns {
				if columns[index].column == "agent_version_digest" {
					test.mutate(&columns[index])
					break
				}
			}
			expectMySQLSchemaV2(mock, columns, nil)

			err := ValidateMySQLSchemaV2(context.Background(), db)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestValidateMySQLSchemaV2RejectsMissingUniqueDigestIndex(t *testing.T) {
	db, mock := newMySQLMigrationMock(t)
	indexes := mysqlV2IndexRows()
	filtered := indexes[:0]
	for _, index := range indexes {
		if index.table == "agent_registry_entries" && index.column == "agent_version_digest" {
			continue
		}
		filtered = append(filtered, index)
	}
	expectMySQLSchemaV2(mock, mysqlV2ColumnRows(), filtered)

	err := ValidateMySQLSchemaV2(context.Background(), db)
	if err == nil || !strings.Contains(err.Error(), "unique single-column index on agent_registry_entries.agent_version_digest") {
		t.Fatalf("error = %v, want missing digest unique index", err)
	}
}

func TestValidateMySQLSchemaV2RejectsPrefixedUniqueDigestIndex(t *testing.T) {
	db, mock := newMySQLMigrationMock(t)
	indexes := mysqlV2IndexRows()
	for index := range indexes {
		if indexes[index].table == "agent_registry_entries" && indexes[index].column == "agent_version_digest" {
			indexes[index].prefixLength = sql.NullInt64{Int64: 16, Valid: true}
			break
		}
	}
	expectMySQLSchemaV2(mock, mysqlV2ColumnRows(), indexes)

	err := ValidateMySQLSchemaV2(context.Background(), db)
	if err == nil || !strings.Contains(err.Error(), "unique single-column index on agent_registry_entries.agent_version_digest") {
		t.Fatalf("error = %v, want prefixed digest unique index rejection", err)
	}
}

func TestValidateMySQLSchemaV2RejectsPrefixedPrimaryKey(t *testing.T) {
	db, mock := newMySQLMigrationMock(t)
	indexes := mysqlV2IndexRows()
	for index := range indexes {
		if indexes[index].table == "agent_registry_entries" && indexes[index].name == "PRIMARY" {
			indexes[index].prefixLength = sql.NullInt64{Int64: 4, Valid: true}
			break
		}
	}
	expectMySQLSchemaV2(mock, mysqlV2ColumnRows(), indexes)

	err := ValidateMySQLSchemaV2(context.Background(), db)
	if err == nil || !strings.Contains(err.Error(), "PRIMARY KEY on agent_registry_entries.row_id") {
		t.Fatalf("error = %v, want prefixed primary-key rejection", err)
	}
}

type mysqlV2Column struct {
	table                string
	column               string
	dataType             string
	columnType           string
	nullable             string
	extra                string
	generationExpression string
}

type mysqlV2Index struct {
	table        string
	name         string
	nonUnique    int
	sequence     int
	column       string
	prefixLength sql.NullInt64
}

func newMySQLMigrationMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

func expectMySQLSchemaV2(mock sqlmock.Sqlmock, columns []mysqlV2Column, indexes []mysqlV2Index) {
	columnRows := sqlmock.NewRows([]string{
		"TABLE_NAME", "COLUMN_NAME", "DATA_TYPE", "COLUMN_TYPE", "IS_NULLABLE", "EXTRA", "GENERATION_EXPRESSION",
	})
	for _, column := range columns {
		columnRows.AddRow(
			column.table,
			column.column,
			column.dataType,
			column.columnType,
			column.nullable,
			column.extra,
			column.generationExpression,
		)
	}
	mock.ExpectQuery("FROM information_schema\\.COLUMNS").WillReturnRows(columnRows)
	if indexes == nil {
		return
	}
	indexRows := sqlmock.NewRows([]string{
		"TABLE_NAME", "INDEX_NAME", "NON_UNIQUE", "SEQ_IN_INDEX", "COLUMN_NAME", "SUB_PART",
	})
	for _, index := range indexes {
		var prefixLength any
		if index.prefixLength.Valid {
			prefixLength = index.prefixLength.Int64
		}
		indexRows.AddRow(index.table, index.name, index.nonUnique, index.sequence, index.column, prefixLength)
	}
	mock.ExpectQuery("FROM information_schema\\.STATISTICS").WillReturnRows(indexRows)
}

func mysqlV2ColumnRows() []mysqlV2Column {
	columns := make([]mysqlV2Column, 0, 11)
	for _, table := range mysqlV2Tables() {
		columns = append(columns, mysqlV2Column{
			table: table, column: "row_id", dataType: "bigint", columnType: "bigint unsigned", nullable: "NO", extra: "auto_increment",
		})
	}
	for table, names := range map[string][]string{
		"agent_registry_entries":          {"agent_version_digest", "type_version_digest"},
		"agent_registry_config_snapshots": {"snapshot_ref_digest", "agent_mode_digest", "agent_hash_digest"},
		"agent_registry_audit_events":     {"event_id_digest"},
		"agent_registry_prompt_versions":  {"prompt_identity_digest"},
	} {
		for _, name := range names {
			columns = append(columns, mysqlV2Column{
				table: table, column: name, dataType: "binary", columnType: "binary(32)", nullable: "NO",
			})
		}
	}
	return columns
}

func mysqlV2IndexRows() []mysqlV2Index {
	indexes := make([]mysqlV2Index, 0, 11)
	for _, table := range mysqlV2Tables() {
		indexes = append(indexes, mysqlV2Index{table: table, name: "PRIMARY", sequence: 1, column: "row_id"})
	}
	for table, columns := range map[string][]string{
		"agent_registry_entries":          {"agent_version_digest", "type_version_digest"},
		"agent_registry_config_snapshots": {"snapshot_ref_digest", "agent_mode_digest", "agent_hash_digest"},
		"agent_registry_audit_events":     {"event_id_digest"},
		"agent_registry_prompt_versions":  {"prompt_identity_digest"},
	} {
		for _, column := range columns {
			indexes = append(indexes, mysqlV2Index{
				table: table, name: table + "_" + column, sequence: 1, column: column,
			})
		}
	}
	return indexes
}

func mysqlV2Tables() []string {
	return []string{
		"agent_registry_entries",
		"agent_registry_config_snapshots",
		"agent_registry_audit_events",
		"agent_registry_prompt_versions",
	}
}
