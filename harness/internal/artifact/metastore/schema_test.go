package metastore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

const approvedArtifactMetadataKeyLockDDL = `CREATE TABLE IF NOT EXISTS artifact_metadata_key_lock (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT 'id',
    key_kind TINYINT NOT NULL COMMENT '1 idempotency key ；2 artifact ref ；3 artifact ID',
    key_hash BINARY(32) NOT NULL COMMENT '原始值的 SHA-256',
    PRIMARY KEY (id),
    UNIQUE KEY uk_kind_hash (key_kind, key_hash)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COMMENT='lock';`

const approvedArtifactMetadataDDL = `CREATE TABLE IF NOT EXISTS artifact_metadata (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键id',

    artifact_id LONGBLOB NOT NULL COMMENT 'artifact_id',
    artifact_id_hash BINARY(32) NOT NULL COMMENT 'artifact_id_hash',
    artifact_ref LONGBLOB NOT NULL COMMENT 'artifact_ref',
    artifact_ref_hash BINARY(32) NOT NULL COMMENT 'artifact_ref_hash',
    idempotency_key LONGBLOB NULL COMMENT 'idempotency_key',
    idempotency_hash BINARY(32) NULL COMMENT 'idempotency_hash',

    tenant_id LONGBLOB NOT NULL COMMENT 'tenant_id',
    tenant_id_hash BINARY(32) NOT NULL COMMENT 'tenant_id_hash',
    user_id LONGBLOB NOT NULL COMMENT 'user_id',
    session_id LONGBLOB NOT NULL COMMENT 'session_id',
    session_id_hash BINARY(32) NOT NULL COMMENT 'session_id_hash',
    run_id LONGBLOB NOT NULL COMMENT 'run_id',
    run_id_hash BINARY(32) NOT NULL COMMENT 'run_id_hash',
    step_id LONGBLOB NOT NULL COMMENT 'step_id',

    owner_module LONGBLOB NOT NULL COMMENT 'owner_module',
    owner_module_hash BINARY(32) NOT NULL COMMENT 'owner_module_hash',
    owner_id LONGBLOB NOT NULL COMMENT 'owner_id',
    owner_id_hash BINARY(32) NOT NULL COMMENT 'owner_id_hash',
    artifact_type LONGBLOB NOT NULL COMMENT 'artifact_type',
    artifact_type_hash BINARY(32) NOT NULL COMMENT 'artifact_type_hash',
    mime_type LONGBLOB NOT NULL COMMENT 'mime_type',
    name LONGBLOB NOT NULL COMMENT 'name',
    size_bytes BIGINT NOT NULL COMMENT 'size_bytes',
    artifact_hash LONGBLOB NOT NULL COMMENT 'artifact_hash',
    visibility_hash BINARY(32) NOT NULL COMMENT 'visibility_hash',
    visibility LONGBLOB NOT NULL COMMENT 'visibility',
    storage_backend LONGBLOB NOT NULL COMMENT 'storage_backend',
    storage_key LONGBLOB NOT NULL COMMENT 'storage_key',

    preview_payload LONGBLOB NOT NULL COMMENT 'preview_payload',
    retention_policy LONGBLOB NOT NULL COMMENT 'retention_policy',
    expires_at_sec BIGINT NULL COMMENT 'expires_at_sec',
    expires_at_nano INT UNSIGNED NULL COMMENT 'expires_at_nano',
    created_by LONGBLOB NOT NULL COMMENT 'created_by',
    created_at_sec BIGINT NOT NULL COMMENT 'created_at_sec',
    created_at_nano INT UNSIGNED NOT NULL COMMENT 'created_at_nano',
    status LONGBLOB NOT NULL COMMENT 'status',
    derived_from_payload LONGBLOB NULL COMMENT 'derived_from_payload',
    schema_version LONGBLOB NOT NULL COMMENT 'schema_version',
    deleted_at_sec BIGINT NULL COMMENT 'deleted_at_sec',
    deleted_at_nano INT UNSIGNED NULL COMMENT 'deleted_at_nano',
    delete_reason LONGBLOB NOT NULL COMMENT 'delete_reason',
    purge_status VARBINARY(16) NOT NULL DEFAULT '' COMMENT 'purge_status',
    purged_at_sec BIGINT NULL COMMENT 'purged_at_sec',
    purged_at_nano INT UNSIGNED NULL COMMENT 'purged_at_nano',
    metadata_payload LONGBLOB NULL COMMENT 'metadata_payload',

    PRIMARY KEY (id),
    KEY idx_artifact_id_hash (artifact_id_hash),
    KEY idx_artifact_ref_hash (artifact_ref_hash),
    KEY idx_idempotency_hash (idempotency_hash),
    KEY idx_tenant_created (tenant_id_hash, created_at_sec, created_at_nano),
    KEY idx_tenant_session_created (tenant_id_hash, session_id_hash, created_at_sec, created_at_nano),
    KEY idx_tenant_run_created (tenant_id_hash, run_id_hash, created_at_sec, created_at_nano),
    KEY idx_tenant_owner_created (tenant_id_hash, owner_module_hash, owner_id_hash, created_at_sec, created_at_nano),
    KEY idx_tenant_type_created (tenant_id_hash, artifact_type_hash, created_at_sec, created_at_nano),
    KEY idx_tenant_visibility_created (tenant_id_hash, visibility_hash, created_at_sec, created_at_nano),
    KEY idx_tenant_expiry (tenant_id_hash, expires_at_sec, expires_at_nano)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COMMENT='metadata';`

const approvedArtifactPurgeJobDDL = `CREATE TABLE IF NOT EXISTS artifact_purge_job (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT 'id',
    metadata_id BIGINT UNSIGNED NOT NULL COMMENT 'metadata id',
    status VARBINARY(16) NOT NULL COMMENT '状态',
    attempts BIGINT UNSIGNED NOT NULL COMMENT 'attempts',
    last_error LONGBLOB NOT NULL COMMENT 'errorinfo',
    created_at_sec BIGINT NOT NULL COMMENT 'create',
    created_at_nano INT UNSIGNED NOT NULL COMMENT 'create',
    updated_at_sec BIGINT NOT NULL COMMENT 'udpate',
    updated_at_nano INT UNSIGNED NOT NULL COMMENT 'update',
    lease_owner LONGBLOB NOT NULL COMMENT 'owner',
    lease_until_sec BIGINT NULL COMMENT 'lease',
    lease_until_nano INT UNSIGNED NULL COMMENT 'lease',
    lease_version BIGINT NOT NULL COMMENT 'lease',
    PRIMARY KEY (id),
    UNIQUE KEY uk_meta_id (metadata_id),
    KEY idx_artifact_purge_ready (
        status, lease_until_sec, lease_until_nano,
        created_at_sec, created_at_nano
    )
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COMMENT='异步删除';`

const testMySQLSchemaStatementDelimiter = "\n-- harness:artifact-mysql-schema-statement --\n"

func TestInitializeMySQLSchemaRejectsNilDB(t *testing.T) {
	err := InitializeMySQLSchema(nil, nil)
	if err == nil {
		t.Fatal("InitializeMySQLSchema() error = nil, want database configuration error")
	}
	lower := strings.ToLower(err.Error())
	if !strings.Contains(lower, "database") || !strings.Contains(lower, "required") {
		t.Fatalf("InitializeMySQLSchema() error = %q, want required database configuration error", err)
	}
}

func TestInitializeMySQLSchemaExecutesKeyLockThenMetadataThenPurgeJob(t *testing.T) {
	schemaSQL, err := os.ReadFile("../artifact_mysql.sql")
	if err != nil {
		t.Fatalf("os.ReadFile(../artifact_mysql.sql) error = %v", err)
	}
	if got := artifact.MySQLSchemaSQL(); got != string(schemaSQL) {
		t.Fatal("embedded artifact MySQL schema differs from internal/artifact/artifact_mysql.sql")
	}
	wantSchemaSQL := approvedArtifactMetadataKeyLockDDL + testMySQLSchemaStatementDelimiter + approvedArtifactMetadataDDL + testMySQLSchemaStatementDelimiter + approvedArtifactPurgeJobDDL + "\n"
	if string(schemaSQL) != wantSchemaSQL {
		t.Fatalf("artifact_mysql.sql does not match the approved final DDL line for line:\n--- got ---\n%s\n--- want ---\n%s", schemaSQL, wantSchemaSQL)
	}

	db, mock := newMySQLSchemaMockDB(t)
	mock.MatchExpectationsInOrder(true)
	mock.ExpectExec(approvedArtifactMetadataKeyLockDDL).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(approvedArtifactMetadataDDL).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(approvedArtifactPurgeJobDDL).WillReturnResult(sqlmock.NewResult(0, 0))

	if err := InitializeMySQLSchema(context.Background(), db); err != nil {
		t.Fatalf("InitializeMySQLSchema() error = %v, want nil", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("InitializeMySQLSchema() DDL order/content: %v", err)
	}
}

func TestInitializeMySQLSchemaPreservesThirdStatementCause(t *testing.T) {
	thirdStatementErr := errors.New("purge job DDL failed")
	db, mock := newMySQLSchemaMockDB(t)
	mock.MatchExpectationsInOrder(true)
	mock.ExpectExec(approvedArtifactMetadataKeyLockDDL).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(approvedArtifactMetadataDDL).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(approvedArtifactPurgeJobDDL).WillReturnError(thirdStatementErr)

	err := InitializeMySQLSchema(context.Background(), db)
	if !errors.Is(err, thirdStatementErr) {
		t.Fatalf("InitializeMySQLSchema() error = %v, want third statement cause %v", err, thirdStatementErr)
	}
	if !strings.Contains(err.Error(), "statement 3") {
		t.Fatalf("InitializeMySQLSchema() error = %q, want third-statement context", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("InitializeMySQLSchema() DDL order/content: %v", err)
	}
}

func TestInitializeMySQLSchemaPreservesSecondStatementCause(t *testing.T) {
	secondStatementErr := errors.New("metadata DDL failed")
	db, mock := newMySQLSchemaMockDB(t)
	mock.MatchExpectationsInOrder(true)
	mock.ExpectExec(approvedArtifactMetadataKeyLockDDL).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(approvedArtifactMetadataDDL).WillReturnError(secondStatementErr)

	err := InitializeMySQLSchema(context.Background(), db)
	if !errors.Is(err, secondStatementErr) {
		t.Fatalf("InitializeMySQLSchema() error = %v, want second statement cause %v", err, secondStatementErr)
	}
	if err == secondStatementErr {
		t.Fatal("InitializeMySQLSchema() returned the bare cause, want operation context")
	}
	if !strings.Contains(err.Error(), "statement 2") {
		t.Fatalf("InitializeMySQLSchema() error = %q, want second-statement context", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("InitializeMySQLSchema() DDL order/content: %v", err)
	}
}

func TestInitializeMySQLSchemaHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	execer := &countingMySQLSchemaExecer{}
	err := initializeMySQLSchema(ctx, execer)
	if execer.execCalls != 0 {
		t.Fatalf("initializeMySQLSchema() ExecContext calls = %d, want 0 for a pre-canceled context", execer.execCalls)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("initializeMySQLSchema() error = %v, want context.Canceled in error chain", err)
	}
}

func TestArtifactMySQLDDLHasSinglePhysicalSource(t *testing.T) {
	var sqlPaths []string
	if err := filepath.WalkDir("..", func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(path), ".sql") {
			sqlPaths = append(sqlPaths, filepath.Clean(path))
		}
		return nil
	}); err != nil {
		t.Fatalf("walk internal/artifact SQL sources: %v", err)
	}
	want := filepath.Clean("../artifact_mysql.sql")
	if len(sqlPaths) != 1 || sqlPaths[0] != want {
		t.Fatalf("internal/artifact SQL sources = %v, want only %q", sqlPaths, want)
	}
}

type countingMySQLSchemaExecer struct {
	execCalls int
}

func (e *countingMySQLSchemaExecer) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	e.execCalls++
	return sqlmock.NewResult(0, 0), nil
}

func newMySQLSchemaMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}
