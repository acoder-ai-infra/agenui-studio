package metastore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

const mysqlTestSelectColumns = "artifact_id, artifact_ref, tenant_id, user_id, session_id, run_id, step_id, owner_module, owner_id, artifact_type, mime_type, name, size_bytes, artifact_hash, visibility, storage_backend, storage_key, preview_payload, retention_policy, expires_at_sec, expires_at_nano, created_by, created_at_sec, created_at_nano, status, derived_from_payload, schema_version, deleted_at_sec, deleted_at_nano, delete_reason, purge_status, purged_at_sec, purged_at_nano, metadata_payload"

var mysqlTestSelectedColumnNames = []string{
	"artifact_id",
	"artifact_ref",
	"tenant_id",
	"user_id",
	"session_id",
	"run_id",
	"step_id",
	"owner_module",
	"owner_id",
	"artifact_type",
	"mime_type",
	"name",
	"size_bytes",
	"artifact_hash",
	"visibility",
	"storage_backend",
	"storage_key",
	"preview_payload",
	"retention_policy",
	"expires_at_sec",
	"expires_at_nano",
	"created_by",
	"created_at_sec",
	"created_at_nano",
	"status",
	"derived_from_payload",
	"schema_version",
	"deleted_at_sec",
	"deleted_at_nano",
	"delete_reason",
	"purge_status",
	"purged_at_sec",
	"purged_at_nano",
	"metadata_payload",
}

type mysqlGetterTestCase struct {
	name      string
	predicate string
	raw       string
	call      func(*MySQLMetadataStore, context.Context, string) (*artifact.ArtifactMeta, error)
}

var errMySQLNoCallDriverInvoked = errors.New("MySQL no-call test driver invoked")

type mysqlNoCallCounts struct {
	connect atomic.Int64
	open    atomic.Int64
	prepare atomic.Int64
	close   atomic.Int64
	begin   atomic.Int64
	beginTx atomic.Int64
	exec    atomic.Int64
	query   atomic.Int64
	ping    atomic.Int64
}

func (c *mysqlNoCallCounts) total() int64 {
	return c.connect.Load() + c.open.Load() + c.prepare.Load() + c.close.Load() +
		c.begin.Load() + c.beginTx.Load() + c.exec.Load() + c.query.Load() + c.ping.Load()
}

func (c *mysqlNoCallCounts) reset() {
	c.connect.Store(0)
	c.open.Store(0)
	c.prepare.Store(0)
	c.close.Store(0)
	c.begin.Store(0)
	c.beginTx.Store(0)
	c.exec.Store(0)
	c.query.Store(0)
	c.ping.Store(0)
}

type mysqlNoCallConnector struct {
	counts *mysqlNoCallCounts
}

func (c *mysqlNoCallConnector) Connect(context.Context) (driver.Conn, error) {
	c.counts.connect.Add(1)
	return &mysqlNoCallConn{counts: c.counts}, nil
}

func (c *mysqlNoCallConnector) Driver() driver.Driver {
	return &mysqlNoCallDriver{counts: c.counts}
}

type mysqlNoCallDriver struct {
	counts *mysqlNoCallCounts
}

func (d *mysqlNoCallDriver) Open(string) (driver.Conn, error) {
	d.counts.open.Add(1)
	return &mysqlNoCallConn{counts: d.counts}, nil
}

type mysqlNoCallConn struct {
	counts *mysqlNoCallCounts
}

func (c *mysqlNoCallConn) Prepare(string) (driver.Stmt, error) {
	c.counts.prepare.Add(1)
	return nil, errMySQLNoCallDriverInvoked
}

func (c *mysqlNoCallConn) Close() error {
	c.counts.close.Add(1)
	return nil
}

func (c *mysqlNoCallConn) Begin() (driver.Tx, error) {
	c.counts.begin.Add(1)
	return nil, errMySQLNoCallDriverInvoked
}

func (c *mysqlNoCallConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.counts.beginTx.Add(1)
	return nil, errMySQLNoCallDriverInvoked
}

func (c *mysqlNoCallConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	c.counts.exec.Add(1)
	return nil, errMySQLNoCallDriverInvoked
}

func (c *mysqlNoCallConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	c.counts.query.Add(1)
	return nil, errMySQLNoCallDriverInvoked
}

func (c *mysqlNoCallConn) Ping(context.Context) error {
	c.counts.ping.Add(1)
	return errMySQLNoCallDriverInvoked
}

func newMySQLNoCallDB(t *testing.T) (*sql.DB, *mysqlNoCallCounts) {
	t.Helper()
	counts := &mysqlNoCallCounts{}
	db := sql.OpenDB(&mysqlNoCallConnector{counts: counts})
	t.Cleanup(func() { _ = db.Close() })
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("prewarm MySQL no-call DB connection: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("return prewarmed MySQL no-call DB connection: %v", err)
	}
	counts.reset()
	return db, counts
}

func requireNoMySQLDriverCalls(t *testing.T, counts *mysqlNoCallCounts) {
	t.Helper()
	if total := counts.total(); total != 0 {
		t.Fatalf("database driver calls = %d (connect=%d open=%d prepare=%d close=%d begin=%d beginTx=%d exec=%d query=%d ping=%d), want zero",
			total,
			counts.connect.Load(),
			counts.open.Load(),
			counts.prepare.Load(),
			counts.close.Load(),
			counts.begin.Load(),
			counts.beginTx.Load(),
			counts.exec.Load(),
			counts.query.Load(),
			counts.ping.Load(),
		)
	}
}

func TestNewMySQLMetadataStoreRejectsNilDB(t *testing.T) {
	store, err := NewMySQLMetadataStore(nil)
	if store != nil {
		t.Fatalf("NewMySQLMetadataStore(nil) store = %#v, want nil", store)
	}
	if !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
		t.Fatalf("NewMySQLMetadataStore(nil) error = %v, want %s", err, artifact.ErrInvalidArgument)
	}
}

func TestNewMySQLMetadataStoreDoesNotInitializeSchema(t *testing.T) {
	db, counts := newMySQLNoCallDB(t)

	store, err := NewMySQLMetadataStore(db)
	if err != nil {
		t.Fatalf("NewMySQLMetadataStore() error = %v", err)
	}
	if store == nil {
		t.Fatal("NewMySQLMetadataStore() store is nil")
	}
	requireNoMySQLDriverCalls(t, counts)
}

func TestMySQLMetadataStoreGetHonorsPreCanceledContext(t *testing.T) {
	db, counts := newMySQLNoCallDB(t)
	var digestCalls atomic.Int64
	store, err := newMySQLMetadataStore(db, func(raw []byte) [32]byte {
		digestCalls.Add(1)
		return sha256.Sum256(raw)
	})
	if err != nil {
		t.Fatalf("newMySQLMetadataStore() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for _, test := range mysqlGetterTestCases("already-canceled") {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.call(store, ctx, test.raw)
			if got != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("getter result = (%#v, %v), want (nil, context.Canceled)", got, err)
			}
		})
	}
	got, err := store.GetByIdempotencyKey(ctx, "")
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("GetByIdempotencyKey(pre-canceled empty) = (%#v, %v), want (nil, context.Canceled)", got, err)
	}
	if got := digestCalls.Load(); got != 0 {
		t.Fatalf("pre-canceled getter digest calls = %d, want zero before database/sql", got)
	}
	requireNoMySQLDriverCalls(t, counts)
}

func TestMySQLMetadataStoreGetBindsSHA256AndExactRawBytes(t *testing.T) {
	for _, test := range mysqlGetterTestCases("value\x00with-trailing-space \xff") {
		t.Run(test.name, func(t *testing.T) {
			store, mock := newMySQLGetterTestStore(t)
			digest := sha256.Sum256([]byte(test.raw))
			mock.ExpectQuery(mysqlTestSelectQuery(test.predicate)).
				WithArgs(digest[:], []byte(test.raw)).
				WillReturnRows(sqlmock.NewRows(mysqlTestSelectedColumnNames)).
				RowsWillBeClosed()

			got, err := test.call(store, context.Background(), test.raw)
			if got != nil || !artifact.IsErrorCode(err, artifact.ErrNotFound) {
				t.Fatalf("getter result = (%#v, %v), want (nil, %s)", got, err, artifact.ErrNotFound)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("getter SQL/hash/raw mismatch: %v", err)
			}
		})
	}
}

func TestMySQLMetadataStoreGetUsesInjectedDigest(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	wantDigest := [32]byte{0xde, 0xad, 0xbe, 0xef}
	store, err := newMySQLMetadataStore(db, func([]byte) [32]byte { return wantDigest })
	if err != nil {
		t.Fatalf("newMySQLMetadataStore() error = %v", err)
	}
	raw := "collision-candidate\x00\xff "
	mock.ExpectQuery(mysqlTestSelectQuery("artifact_ref_hash = ? AND artifact_ref = ?")).
		WithArgs(wantDigest[:], []byte(raw)).
		WillReturnRows(sqlmock.NewRows(mysqlTestSelectedColumnNames)).
		RowsWillBeClosed()

	got, err := store.GetByRef(context.Background(), raw)
	if got != nil || !artifact.IsErrorCode(err, artifact.ErrNotFound) {
		t.Fatalf("GetByRef() = (%#v, %v), want (nil, %s)", got, err, artifact.ErrNotFound)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("injected digest was not used exactly: %v", err)
	}
}

func TestMySQLMetadataStoreGetByIdempotencyKeyEmptySkipsSQL(t *testing.T) {
	db, counts := newMySQLNoCallDB(t)
	var digestCalls atomic.Int64
	store, err := newMySQLMetadataStore(db, func(raw []byte) [32]byte {
		digestCalls.Add(1)
		return sha256.Sum256(raw)
	})
	if err != nil {
		t.Fatalf("newMySQLMetadataStore() error = %v", err)
	}

	got, err := store.GetByIdempotencyKey(context.Background(), "")
	if got != nil || !artifact.IsErrorCode(err, artifact.ErrNotFound) {
		t.Fatalf("GetByIdempotencyKey(empty) = (%#v, %v), want (nil, %s)", got, err, artifact.ErrNotFound)
	}
	if got := digestCalls.Load(); got != 0 {
		t.Fatalf("empty idempotency digest calls = %d, want zero", got)
	}
	requireNoMySQLDriverCalls(t, counts)
}

func TestMySQLMetadataStoreGetPreservesQueryCause(t *testing.T) {
	queryCause := errors.New("query transport failed")
	for _, test := range mysqlGetterTestCases("query-cause") {
		t.Run(test.name, func(t *testing.T) {
			store, mock := newMySQLGetterTestStore(t)
			digest := sha256.Sum256([]byte(test.raw))
			mock.ExpectQuery(mysqlTestSelectQuery(test.predicate)).
				WithArgs(digest[:], []byte(test.raw)).
				WillReturnError(queryCause)

			got, err := test.call(store, context.Background(), test.raw)
			if got != nil || !errors.Is(err, queryCause) {
				t.Fatalf("getter result = (%#v, %v), want query cause", got, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("getter query expectation: %v", err)
			}
		})
	}
}

func TestMySQLMetadataStoreGetPreservesRowsCause(t *testing.T) {
	store, mock := newMySQLGetterTestStore(t)
	fixture := mysqlGetterMetaFixture()
	rowCause := errors.New("rows stream failed")
	digest := sha256.Sum256([]byte(fixture.ArtifactRef))
	rows := mysqlTestRowsForMeta(t, fixture, 1).RowError(0, rowCause)
	mock.ExpectQuery(mysqlTestSelectQuery("artifact_ref_hash = ? AND artifact_ref = ?")).
		WithArgs(digest[:], []byte(fixture.ArtifactRef)).
		WillReturnRows(rows).
		RowsWillBeClosed()

	got, err := store.GetByRef(context.Background(), fixture.ArtifactRef)
	if got != nil || !errors.Is(err, rowCause) {
		t.Fatalf("GetByRef() = (%#v, %v), want rows cause", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("rows cause query expectation: %v", err)
	}
}

func TestMySQLMetadataStoreGetZeroRowsIsNotFound(t *testing.T) {
	for _, test := range mysqlGetterTestCases("missing") {
		t.Run(test.name, func(t *testing.T) {
			store, mock := newMySQLGetterTestStore(t)
			digest := sha256.Sum256([]byte(test.raw))
			mock.ExpectQuery(mysqlTestSelectQuery(test.predicate)).
				WithArgs(digest[:], []byte(test.raw)).
				WillReturnRows(sqlmock.NewRows(mysqlTestSelectedColumnNames)).
				RowsWillBeClosed()

			got, err := test.call(store, context.Background(), test.raw)
			if got != nil || !artifact.IsErrorCode(err, artifact.ErrNotFound) {
				t.Fatalf("getter result = (%#v, %v), want (nil, %s)", got, err, artifact.ErrNotFound)
			}
		})
	}
}

func TestMySQLMetadataStoreGetOneExactRow(t *testing.T) {
	fixture := mysqlGetterMetaFixture()
	for _, test := range mysqlGetterTestCasesForMeta(fixture, "idem-one-row") {
		t.Run(test.name, func(t *testing.T) {
			store, mock := newMySQLGetterTestStore(t)
			digest := sha256.Sum256([]byte(test.raw))
			mock.ExpectQuery(mysqlTestSelectQuery(test.predicate)).
				WithArgs(digest[:], []byte(test.raw)).
				WillReturnRows(mysqlTestRowsForMeta(t, fixture, 1)).
				RowsWillBeClosed()

			got, err := test.call(store, context.Background(), test.raw)
			if err != nil {
				t.Fatalf("getter error = %v", err)
			}
			if !reflect.DeepEqual(got, &fixture) {
				t.Fatalf("getter meta = %#v, want %#v", got, &fixture)
			}
		})
	}
}

func TestMySQLMetadataStoreGetTwoExactRowsAreInvalid(t *testing.T) {
	fixture := mysqlGetterMetaFixture()
	for _, test := range mysqlGetterTestCasesForMeta(fixture, "idem-two-rows") {
		t.Run(test.name, func(t *testing.T) {
			store, mock := newMySQLGetterTestStore(t)
			digest := sha256.Sum256([]byte(test.raw))
			mock.ExpectQuery(mysqlTestSelectQuery(test.predicate)).
				WithArgs(digest[:], []byte(test.raw)).
				WillReturnRows(mysqlTestRowsForMeta(t, fixture, 2)).
				RowsWillBeClosed()

			got, err := test.call(store, context.Background(), test.raw)
			if got != nil || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
				t.Fatalf("getter result = (%#v, %v), want (nil, %s)", got, err, artifact.ErrInvalidArgument)
			}
		})
	}
}

func TestMySQLMetadataStoreGetCorruptPayloadIsInvalid(t *testing.T) {
	store, mock := newMySQLGetterTestStore(t)
	fixture := mysqlGetterMetaFixture()
	values := mysqlTestRowValues(t, fixture)
	values[17] = []byte(`{"version":1,"text":"broken"}`)
	digest := sha256.Sum256([]byte(fixture.ArtifactRef))
	mock.ExpectQuery(mysqlTestSelectQuery("artifact_ref_hash = ? AND artifact_ref = ?")).
		WithArgs(digest[:], []byte(fixture.ArtifactRef)).
		WillReturnRows(sqlmock.NewRows(mysqlTestSelectedColumnNames).AddRow(values...)).
		RowsWillBeClosed()

	got, err := store.GetByRef(context.Background(), fixture.ArtifactRef)
	if got != nil || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
		t.Fatalf("GetByRef(corrupt payload) = (%#v, %v), want (nil, %s)", got, err, artifact.ErrInvalidArgument)
	}
}

func TestMySQLMetadataStoreGetNullRequiredColumnIsInvalid(t *testing.T) {
	store, mock := newMySQLGetterTestStore(t)
	fixture := mysqlGetterMetaFixture()
	values := mysqlTestRowValues(t, fixture)
	values[0] = nil
	digest := sha256.Sum256([]byte(fixture.ArtifactRef))
	mock.ExpectQuery(mysqlTestSelectQuery("artifact_ref_hash = ? AND artifact_ref = ?")).
		WithArgs(digest[:], []byte(fixture.ArtifactRef)).
		WillReturnRows(sqlmock.NewRows(mysqlTestSelectedColumnNames).AddRow(values...)).
		RowsWillBeClosed()

	got, err := store.GetByRef(context.Background(), fixture.ArtifactRef)
	if got != nil || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
		t.Fatalf("GetByRef(NULL required column) = (%#v, %v), want (nil, %s)", got, err, artifact.ErrInvalidArgument)
	}
}

func TestMySQLMetadataStoreGetReturnsIndependentContainers(t *testing.T) {
	store, mock := newMySQLGetterTestStore(t)
	fixture := mysqlGetterMetaFixture()
	digest := sha256.Sum256([]byte(fixture.ArtifactRef))
	query := mysqlTestSelectQuery("artifact_ref_hash = ? AND artifact_ref = ?")
	for i := 0; i < 2; i++ {
		mock.ExpectQuery(query).
			WithArgs(digest[:], []byte(fixture.ArtifactRef)).
			WillReturnRows(mysqlTestRowsForMeta(t, fixture, 1)).
			RowsWillBeClosed()
	}

	first, err := store.GetByRef(context.Background(), fixture.ArtifactRef)
	if err != nil {
		t.Fatalf("first GetByRef() error = %v", err)
	}
	second, err := store.GetByRef(context.Background(), fixture.ArtifactRef)
	if err != nil {
		t.Fatalf("second GetByRef() error = %v", err)
	}

	first.Preview.Text = "mutated"
	first.Preview.Fields["nested"].([]any)[1].([]string)[0] = "mutated"
	first.DerivedFrom[0].ArtifactRef = "mutated"
	first.Metadata["key-\xff"] = "mutated"
	if !reflect.DeepEqual(second, &fixture) {
		t.Fatalf("second result changed after mutating first: got %#v, want %#v", second, &fixture)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("independent getter query expectations: %v", err)
	}
}

func newMySQLGetterTestStore(t *testing.T) (*MySQLMetadataStore, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewMySQLMetadataStore(db)
	if err != nil {
		t.Fatalf("NewMySQLMetadataStore() error = %v", err)
	}
	return store, mock
}

func mysqlGetterTestCases(raw string) []mysqlGetterTestCase {
	return []mysqlGetterTestCase{
		{
			name:      "ByRef",
			predicate: "artifact_ref_hash = ? AND artifact_ref = ?",
			raw:       raw,
			call:      (*MySQLMetadataStore).GetByRef,
		},
		{
			name:      "ByID",
			predicate: "artifact_id_hash = ? AND artifact_id = ?",
			raw:       raw,
			call:      (*MySQLMetadataStore).GetByID,
		},
		{
			name:      "ByIdempotencyKey",
			predicate: "idempotency_hash = ? AND idempotency_key = ?",
			raw:       raw,
			call:      (*MySQLMetadataStore).GetByIdempotencyKey,
		},
	}
}

func mysqlGetterTestCasesForMeta(meta artifact.ArtifactMeta, idempotencyKey string) []mysqlGetterTestCase {
	tests := mysqlGetterTestCases("")
	tests[0].raw = meta.ArtifactRef
	tests[1].raw = meta.ArtifactID
	tests[2].raw = idempotencyKey
	return tests
}

func mysqlTestSelectQuery(predicate string) string {
	return "SELECT " + mysqlTestSelectColumns + " FROM artifact_metadata WHERE " + predicate + " LIMIT 2"
}

func mysqlGetterMetaFixture() artifact.ArtifactMeta {
	meta := portableMySQLMetaFixture()
	meta.CreatedAt = time.Unix(1_700_000_000, 1).UTC()
	meta.ExpiresAt = time.Unix(1_700_003_600, 999_999_999).UTC()
	meta.DeletedAt = time.Unix(1_700_007_200, 17).UTC()
	meta.PurgeStatus = artifact.PurgeStatusPurged
	meta.PurgedAt = time.Unix(1_700_007_300, 23).UTC()
	return meta
}

func mysqlTestRowsForMeta(t *testing.T, meta artifact.ArtifactMeta, count int) *sqlmock.Rows {
	t.Helper()
	rows := sqlmock.NewRows(mysqlTestSelectedColumnNames)
	for i := 0; i < count; i++ {
		rows.AddRow(mysqlTestRowValues(t, meta)...)
	}
	return rows
}

func mysqlTestRowValues(t *testing.T, meta artifact.ArtifactMeta) []driver.Value {
	t.Helper()
	row, _, err := prepareMySQLArtifactRow(meta)
	if err != nil {
		t.Fatalf("prepareMySQLArtifactRow() error = %v", err)
	}
	return []driver.Value{
		row.artifactID,
		row.artifactRef,
		row.tenantID,
		row.userID,
		row.sessionID,
		row.runID,
		row.stepID,
		row.ownerModule,
		row.ownerID,
		row.artifactType,
		row.mimeType,
		row.name,
		row.sizeBytes,
		row.artifactHash,
		row.visibility,
		row.storageBackend,
		row.storageKey,
		row.previewPayload,
		row.retentionPolicy,
		mysqlTestNullableInt64(row.expiresAt.Seconds),
		mysqlTestNullableInt64(row.expiresAt.Nanoseconds),
		row.createdBy,
		mysqlTestNullableInt64(row.createdAt.Seconds),
		mysqlTestNullableInt64(row.createdAt.Nanoseconds),
		row.status,
		row.derivedFromPayload,
		row.schemaVersion,
		mysqlTestNullableInt64(row.deletedAt.Seconds),
		mysqlTestNullableInt64(row.deletedAt.Nanoseconds),
		row.deleteReason,
		[]byte(meta.PurgeStatus),
		mysqlTestNullableInt64(encodeNullableMySQLTime(meta.PurgedAt).Seconds),
		mysqlTestNullableInt64(encodeNullableMySQLTime(meta.PurgedAt).Nanoseconds),
		row.metadataPayload,
	}
}

func mysqlTestNullableInt64(value sql.NullInt64) driver.Value {
	if !value.Valid {
		return nil
	}
	return value.Int64
}
