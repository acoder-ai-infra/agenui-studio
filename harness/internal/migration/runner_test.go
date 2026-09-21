package migration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestLedgerSchemaFitsPortableMySQLDDLContract(t *testing.T) {
	for _, fragment := range []string{
		"row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '",
		"migration_id VARCHAR(128) NOT NULL COMMENT '",
		"applied_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '",
		"PRIMARY KEY (row_id)",
		"UNIQUE KEY uk_harness_schema_migration_id (migration_id)",
		"ROW_FORMAT=DYNAMIC",
		"COMMENT='Harness 模块 Schema 迁移版本表'",
	} {
		if !strings.Contains(createLedgerSQL, fragment) {
			t.Errorf("migration ledger DDL missing %q", fragment)
		}
	}
	if strings.Contains(strings.ToUpper(createLedgerSQL), "ON UPDATE") {
		t.Fatal("migration applied_at must remain immutable")
	}
	// utf8mb4 uses at most 4 bytes per character; the bounded key remains
	// compatible with MySQL installations that enforce the 767-byte limit.
	if 128*4 > 767 {
		t.Fatal("migration ledger unique key exceeds legacy 767-byte limit")
	}
}

func TestApplyRunsFreshStepsInOrderAndRecordsSuccess(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(createLedgerSQL).WillReturnResult(sqlmock.NewResult(0, 0))
	order := make([]string, 0, 2)
	for _, id := range []string{"ledger/001", "artifact/001"} {
		mock.ExpectQuery("SELECT COUNT(*) FROM harness_schema_migrations WHERE migration_id = ?").
			WithArgs(id).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
		mock.ExpectExec("INSERT INTO harness_schema_migrations (migration_id) VALUES (?)").
			WithArgs(id).WillReturnResult(sqlmock.NewResult(0, 1))
	}
	err = Apply(context.Background(), db,
		Step{ID: "ledger/001", Apply: func(context.Context) error { order = append(order, "ledger"); return nil }},
		Step{ID: "artifact/001", Apply: func(context.Context) error { order = append(order, "artifact"); return nil }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(order); got != 2 || order[0] != "ledger" || order[1] != "artifact" {
		t.Fatalf("migration order = %v", order)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestApplySkipsRecordedStep(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(createLedgerSQL).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT COUNT(*) FROM harness_schema_migrations WHERE migration_id = ?").
		WithArgs("ledger/001").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	called := false
	if err := Apply(context.Background(), db, Step{ID: "ledger/001", Apply: func(context.Context) error { called = true; return nil }}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("recorded migration was applied again")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestApplyDoesNotRecordFailedStepOrContinue(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(createLedgerSQL).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT COUNT(*) FROM harness_schema_migrations WHERE migration_id = ?").
		WithArgs("ledger/001").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	cause := errors.New("ddl failed")
	err = Apply(context.Background(), db,
		Step{ID: "ledger/001", Apply: func(context.Context) error { return cause }},
		Step{ID: "artifact/001", Apply: func(context.Context) error { t.Fatal("later step ran"); return nil }},
	)
	if !errors.Is(err, cause) {
		t.Fatalf("Apply() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestApplyAcceptsConcurrentMarkerWinner(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(createLedgerSQL).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT COUNT(*) FROM harness_schema_migrations WHERE migration_id = ?").
		WithArgs("ledger/001").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("INSERT INTO harness_schema_migrations (migration_id) VALUES (?)").
		WithArgs("ledger/001").WillReturnError(errors.New("duplicate key"))
	mock.ExpectQuery("SELECT COUNT(*) FROM harness_schema_migrations WHERE migration_id = ?").
		WithArgs("ledger/001").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	if err := Apply(context.Background(), db, Step{ID: "ledger/001", Apply: func(context.Context) error { return nil }}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRequireAppliedFailsOnFirstMissingVersion(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery("SELECT COUNT(*) FROM harness_schema_migrations WHERE migration_id = ?").
		WithArgs("ledger/001").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("SELECT COUNT(*) FROM harness_schema_migrations WHERE migration_id = ?").
		WithArgs("artifact/001").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	err = RequireApplied(context.Background(), db, "ledger/001", "artifact/001")
	if err == nil || !strings.Contains(err.Error(), "artifact/001") {
		t.Fatalf("RequireApplied() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
