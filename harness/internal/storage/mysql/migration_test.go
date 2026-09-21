package mysql

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestMySQLSchemaStatementsKeepFirstTableAfterHeaderComments(t *testing.T) {
	statements := mysqlSchemaStatements(Schema)
	if len(statements) == 0 {
		t.Fatal("mysqlSchemaStatements(Schema) returned no executable statements")
	}
	if !strings.HasPrefix(statements[0], "CREATE TABLE IF NOT EXISTS sessions") {
		t.Fatalf("first executable statement = %q, want sessions table", statements[0])
	}
	for index, statement := range statements {
		if strings.HasPrefix(strings.TrimSpace(statement), "--") {
			t.Fatalf("statement %d still begins with a SQL comment: %q", index, statement)
		}
	}
}

func TestMySQLSchemaStatementsRemoveFullLineCommentsWithoutDroppingSQL(t *testing.T) {
	input := "-- header\nCREATE TABLE first (id BIGINT);\n  -- between\nCREATE TABLE second (id BIGINT);"
	want := []string{"CREATE TABLE first (id BIGINT)", "CREATE TABLE second (id BIGINT)"}
	got := mysqlSchemaStatements(input)
	if len(got) != len(want) {
		t.Fatalf("mysqlSchemaStatements() = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("statement %d = %q, want %q", index, got[index], want[index])
		}
	}
}

func TestEnsureColumnAcceptsConcurrentMigrationWinner(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	query := "SELECT COUNT\\(\\*\\) FROM information_schema.columns"
	mock.ExpectQuery(query).WithArgs("runs", "runtime_binding").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("ALTER TABLE runs ADD COLUMN runtime_binding JSON NULL").
		WillReturnError(errors.New("duplicate column"))
	mock.ExpectQuery(query).WithArgs("runs", "runtime_binding").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	if err := New(db).ensureColumn(context.Background(), "runs", "runtime_binding", "JSON NULL"); err != nil {
		t.Fatalf("ensureColumn() rejected concurrent winner: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckReadyRejectsNumericStorageIDsFromPhysicalDDL(t *testing.T) {
	columns := map[string]columnInfo{
		"run_id":     {dataType: "bigint", columnType: "bigint unsigned"},
		"session_id": {dataType: "varchar", columnType: "varchar(64)"},
		"tenant_id":  {dataType: "varchar", columnType: "varchar(64)"},
		"status":     {dataType: "varchar", columnType: "varchar(24)"},
	}
	schema := defaultPhysicalSchema()
	err := schema.observeColumnTypes("runs", columns)
	if err == nil {
		t.Fatal("observeColumnTypes() accepted numeric run_id")
	}
	if !strings.Contains(err.Error(), "expected string type") {
		t.Fatalf("observeColumnTypes() error = %v, want expected string type", err)
	}
}

func TestCheckReadyAcceptsStringStorageIDsAndJSONPayloadColumns(t *testing.T) {
	columns := map[string]columnInfo{
		"event_id":        {dataType: "varchar", columnType: "varchar(64)"},
		"run_id":          {dataType: "varchar", columnType: "varchar(64)"},
		"tenant_id":       {dataType: "varchar", columnType: "varchar(64)"},
		"session_id":      {dataType: "varchar", columnType: "varchar(64)"},
		"payload":         {dataType: "json", columnType: "json"},
		"payload_preview": {dataType: "json", columnType: "json"},
		"usage":           {dataType: "json", columnType: "json"},
		"error":           {dataType: "json", columnType: "json"},
	}
	schema := defaultPhysicalSchema()
	if err := schema.observeColumnTypes("agent_events", columns); err != nil {
		t.Fatalf("observeColumnTypes() error = %v", err)
	}
}
