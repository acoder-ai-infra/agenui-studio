package metastore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

// These are deliberately test-owned facts. Expected List SQL must not be
// derived from production constants or the production clause assembler.
const mysqlTestListSelect = "SELECT artifact_id, artifact_ref, tenant_id, user_id, session_id, run_id, step_id, owner_module, owner_id, artifact_type, mime_type, name, size_bytes, artifact_hash, visibility, storage_backend, storage_key, preview_payload, retention_policy, expires_at_sec, expires_at_nano, created_by, created_at_sec, created_at_nano, status, derived_from_payload, schema_version, deleted_at_sec, deleted_at_nano, delete_reason, purge_status, purged_at_sec, purged_at_nano, metadata_payload FROM artifact_metadata"
const mysqlTestListOrder = " ORDER BY created_at_sec ASC, created_at_nano ASC, artifact_id ASC"

func TestMySQLMetadataStoreListHonorsPreCanceledContext(t *testing.T) {
	db, counts := newMySQLNoCallDB(t)
	var digestCalls atomic.Int64
	store, err := newMySQLMetadataStore(db, func(raw []byte) [sha256.Size]byte {
		digestCalls.Add(1)
		return sha256.Sum256(raw)
	})
	if err != nil {
		t.Fatalf("newMySQLMetadataStore() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := store.List(ctx, artifact.ListQuery{
		TenantID:          "must-not-be-digested",
		SessionID:         "must-not-be-digested",
		ExpiredAtOrBefore: time.Now(),
	})
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("List(pre-canceled) = (%#v, %v), want (nil, context.Canceled)", got, err)
	}
	if calls := digestCalls.Load(); calls != 0 {
		t.Fatalf("List(pre-canceled) digest calls = %d, want zero", calls)
	}
	requireNoMySQLDriverCalls(t, counts)
}

func TestMySQLMetadataStoreListUsesFixedClausesAndBindings(t *testing.T) {
	location := time.FixedZone("UTC+05:45", 5*60*60+45*60)
	expiry := time.Date(2026, time.July, 14, 12, 13, 14, 987_654_321, location)
	unsafeRaw := "value' OR 1=1 --\x00 \xff"
	all := artifact.ListQuery{
		TenantID:          "tenant\x00\xff ",
		SessionID:         "session\x00\xff ",
		RunID:             "run\x00\xff ",
		OwnerModule:       artifact.OwnerModule("owner-module-alias\x00\xff "),
		OwnerID:           "owner-id\x00\xff ",
		Type:              artifact.ArtifactType("type-alias\x00\xff "),
		Visibility:        artifact.Visibility("visibility-alias\x00\xff "),
		ExpiredAtOrBefore: expiry,
	}
	allIncludingDeleted := all
	allIncludingDeleted.IncludeDeleted = true
	tests := []struct {
		name      string
		query     artifact.ListQuery
		fragments []string
		args      []driver.Value
	}{
		{
			name:      "default_only_excludes_exact_deleted",
			fragments: []string{"status <> ?"},
			args:      []driver.Value{[]byte(artifact.ArtifactStatusDeleted)},
		},
		{
			name:  "include_deleted_omits_status_fragment",
			query: artifact.ListQuery{IncludeDeleted: true},
		},
		mysqlListFilterCase("tenant", artifact.ListQuery{TenantID: unsafeRaw}, "tenant_id_hash = ? AND tenant_id = ?", unsafeRaw),
		mysqlListFilterCase("session", artifact.ListQuery{SessionID: unsafeRaw}, "session_id_hash = ? AND session_id = ?", unsafeRaw),
		mysqlListFilterCase("run", artifact.ListQuery{RunID: unsafeRaw}, "run_id_hash = ? AND run_id = ?", unsafeRaw),
		mysqlListFilterCase("owner_module", artifact.ListQuery{OwnerModule: artifact.OwnerModule(unsafeRaw)}, "owner_module_hash = ? AND owner_module = ?", unsafeRaw),
		mysqlListFilterCase("owner_id", artifact.ListQuery{OwnerID: unsafeRaw}, "owner_id_hash = ? AND owner_id = ?", unsafeRaw),
		mysqlListFilterCase("artifact_type", artifact.ListQuery{Type: artifact.ArtifactType(unsafeRaw)}, "artifact_type_hash = ? AND artifact_type = ?", unsafeRaw),
		mysqlListFilterCase("visibility", artifact.ListQuery{Visibility: artifact.Visibility(unsafeRaw)}, "visibility_hash = ? AND visibility = ?", unsafeRaw),
		{
			name:      "expiry_is_inclusive_second_second_nanosecond",
			query:     artifact.ListQuery{ExpiredAtOrBefore: expiry},
			fragments: []string{"status <> ?", "expires_at_sec IS NOT NULL AND (expires_at_sec < ? OR (expires_at_sec = ? AND expires_at_nano <= ?))"},
			args: []driver.Value{
				[]byte(artifact.ArtifactStatusDeleted), expiry.Unix(), expiry.Unix(), int64(expiry.Nanosecond()),
			},
		},
		{
			name:  "all_filters_follow_fixed_order",
			query: all,
			fragments: []string{
				"status <> ?",
				"tenant_id_hash = ? AND tenant_id = ?",
				"session_id_hash = ? AND session_id = ?",
				"run_id_hash = ? AND run_id = ?",
				"owner_module_hash = ? AND owner_module = ?",
				"owner_id_hash = ? AND owner_id = ?",
				"artifact_type_hash = ? AND artifact_type = ?",
				"visibility_hash = ? AND visibility = ?",
				"expires_at_sec IS NOT NULL AND (expires_at_sec < ? OR (expires_at_sec = ? AND expires_at_nano <= ?))",
			},
			args: mysqlTestListCombinedArguments(all, true),
		},
		{
			name:  "include_deleted_with_all_filters_omits_only_status",
			query: allIncludingDeleted,
			fragments: []string{
				"tenant_id_hash = ? AND tenant_id = ?",
				"session_id_hash = ? AND session_id = ?",
				"run_id_hash = ? AND run_id = ?",
				"owner_module_hash = ? AND owner_module = ?",
				"owner_id_hash = ? AND owner_id = ?",
				"artifact_type_hash = ? AND artifact_type = ?",
				"visibility_hash = ? AND visibility = ?",
				"expires_at_sec IS NOT NULL AND (expires_at_sec < ? OR (expires_at_sec = ? AND expires_at_nano <= ?))",
			},
			args: mysqlTestListCombinedArguments(allIncludingDeleted, false),
		},
		{
			name:  "all_zero_aliases_and_zero_expiry_are_omitted",
			query: artifact.ListQuery{IncludeDeleted: true},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, mock := newMySQLListTestStore(t, sha256.Sum256)
			expectedSQL := mysqlTestListQuery(test.fragments...)
			expectation := mock.ExpectQuery(expectedSQL)
			if len(test.args) != 0 {
				expectation.WithArgs(test.args...)
			} else {
				expectation.WithoutArgs()
			}
			expectation.WillReturnRows(sqlmock.NewRows(mysqlTestSelectedColumnNames)).RowsWillBeClosed()

			got, err := store.List(context.Background(), test.query)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if got == nil || len(got) != 0 {
				t.Fatalf("List(empty) = %#v, want non-nil empty slice", got)
			}
			mysqlRequireListExpectations(t, mock)
			if strings.Contains(expectedSQL, unsafeRaw) {
				t.Fatalf("List SQL interpolated caller input: %q", expectedSQL)
			}
		})
	}
}

func TestMySQLMetadataStoreListPassesCallerContextToQuery(t *testing.T) {
	contextKey := &struct{ name string }{name: "mysql-list-context"}
	contextValue := &struct{ name string }{name: "caller-value"}
	queryCause := errors.New("context-aware list query failed")
	missingContextCause := errors.New("List did not pass caller context to database/sql")
	connector := &mysqlListContextConnector{
		key:                 contextKey,
		value:               contextValue,
		queryCause:          queryCause,
		missingContextCause: missingContextCause,
	}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewMySQLMetadataStore(db)
	if err != nil {
		t.Fatalf("NewMySQLMetadataStore() error = %v", err)
	}
	ctx := context.WithValue(context.Background(), contextKey, contextValue)

	got, err := store.List(ctx, artifact.ListQuery{})
	if got != nil || !errors.Is(err, queryCause) || errors.Is(err, missingContextCause) {
		t.Fatalf("List(active caller context) = (%#v, %v), want nil preserving query cause only", got, err)
	}
	if calls := connector.queryCalls.Load(); calls != 1 {
		t.Fatalf("List(active caller context) QueryContext calls = %d, want one", calls)
	}
}

func TestMySQLMetadataStoreListUsesInjectedDigestAndExactRaw(t *testing.T) {
	constant := [sha256.Size]byte{0xde, 0xad, 0xbe, 0xef}
	store, mock := newMySQLListTestStore(t, func([]byte) [sha256.Size]byte { return constant })
	raw := "collision-candidate\x00\xff "
	first := mysqlListMetaFixture("collision-first")
	second := mysqlListMetaFixture("collision-second")
	rows := sqlmock.NewRows(mysqlTestSelectedColumnNames).
		AddRow(mysqlTestRowValues(t, first)...).
		AddRow(mysqlTestRowValues(t, second)...)
	mock.ExpectQuery(mysqlTestListQuery("status <> ?", "tenant_id_hash = ? AND tenant_id = ?")).
		WithArgs([]byte(artifact.ArtifactStatusDeleted), constant[:], []byte(raw)).
		WillReturnRows(rows).
		RowsWillBeClosed()

	got, err := store.List(context.Background(), artifact.ListQuery{TenantID: raw})
	if err != nil || len(got) != 2 {
		t.Fatalf("List(constant digest, different raw identities) = (%#v, %v), want two legal rows", got, err)
	}
	mysqlRequireListExpectations(t, mock)
}

func TestMySQLMetadataStoreListReturnsIndependentContainersAndAllowsAliases(t *testing.T) {
	store, mock := newMySQLListTestStore(t, sha256.Sum256)
	first := mysqlListMetaFixture("isolation-first")
	second := mysqlListMetaFixture("isolation-second")
	first.OwnerModule = artifact.OwnerModule("owner-module-unknown")
	first.ArtifactType = artifact.ArtifactType("type-unknown")
	first.Visibility = artifact.Visibility("visibility-unknown")
	first.Status = artifact.ArtifactStatus("status-unknown")
	first.Preview.Fields = map[string]any{"nested": []any{[]string{"original"}}}
	first.DerivedFrom = []artifact.ArtifactLineage{{ArtifactRef: "source", Relation: artifact.LineageRelation("relation-unknown")}}
	first.Metadata = map[string]string{"key": "original"}
	second.Preview = first.Preview
	second.DerivedFrom = first.DerivedFrom
	second.Metadata = first.Metadata

	for i := 0; i < 2; i++ {
		rows := sqlmock.NewRows(mysqlTestSelectedColumnNames).
			AddRow(mysqlTestRowValues(t, first)...).
			AddRow(mysqlTestRowValues(t, second)...)
		mock.ExpectQuery(mysqlTestListQuery()).WithoutArgs().WillReturnRows(rows).RowsWillBeClosed()
	}

	got, err := store.List(context.Background(), artifact.ListQuery{IncludeDeleted: true})
	if err != nil || len(got) != 2 {
		t.Fatalf("first List() = (%#v, %v), want two rows", got, err)
	}
	mysqlRequireMetaSlicesEqual(t, got, []artifact.ArtifactMeta{first, second})
	if got[0].OwnerModule != first.OwnerModule || got[0].ArtifactType != first.ArtifactType ||
		got[0].Visibility != first.Visibility || got[0].Status != first.Status {
		t.Fatalf("List() imposed enum validation or changed aliases: %#v", got[0])
	}
	got[0].Preview.Fields["nested"].([]any)[0].([]string)[0] = "mutated"
	got[0].DerivedFrom[0].ArtifactRef = "mutated"
	got[0].Metadata["key"] = "mutated"
	if got[1].Preview.Fields["nested"].([]any)[0].([]string)[0] != "original" ||
		got[1].DerivedFrom[0].ArtifactRef != "source" || got[1].Metadata["key"] != "original" {
		t.Fatalf("List() rows share decoded containers: %#v", got)
	}

	repeated, err := store.List(context.Background(), artifact.ListQuery{IncludeDeleted: true})
	if err != nil || len(repeated) != 2 {
		t.Fatalf("repeated List() = (%#v, %v), want two rows", repeated, err)
	}
	mysqlRequireMetaSlicesEqual(t, repeated, []artifact.ArtifactMeta{first, second})
	if repeated[0].Preview.Fields["nested"].([]any)[0].([]string)[0] != "original" ||
		repeated[0].DerivedFrom[0].ArtifactRef != "source" || repeated[0].Metadata["key"] != "original" {
		t.Fatalf("repeated List() aliases prior result: %#v", repeated[0])
	}
	mysqlRequireListExpectations(t, mock)
}

func TestMySQLMetadataStoreListDuplicateRawIdentityIsInvalid(t *testing.T) {
	for _, duplicate := range []string{"artifact_id", "artifact_ref"} {
		t.Run(duplicate, func(t *testing.T) {
			store, mock := newMySQLListTestStore(t, sha256.Sum256)
			first := mysqlListMetaFixture("duplicate-first")
			second := mysqlListMetaFixture("duplicate-second")
			if duplicate == "artifact_id" {
				second.ArtifactID = first.ArtifactID
			} else {
				second.ArtifactRef = first.ArtifactRef
			}
			rows := sqlmock.NewRows(mysqlTestSelectedColumnNames).
				AddRow(mysqlTestRowValues(t, first)...).
				AddRow(mysqlTestRowValues(t, second)...)
			mock.ExpectQuery(mysqlTestListQuery()).WithoutArgs().WillReturnRows(rows).RowsWillBeClosed()

			got, err := store.List(context.Background(), artifact.ListQuery{IncludeDeleted: true})
			if got != nil || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
				t.Fatalf("List(duplicate %s) = (%#v, %v), want nil invalid_argument", duplicate, got, err)
			}
			mysqlRequireListExpectations(t, mock)
		})
	}
}

func TestMySQLMetadataStoreListReturnsNoPartialResultsOnFailure(t *testing.T) {
	queryCause := errors.New("list query failed")
	rowsCause := errors.New("list rows failed")
	first := mysqlListMetaFixture("partial-first")
	second := mysqlListMetaFixture("partial-second")
	tests := []struct {
		name        string
		setup       func(*testing.T, sqlmock.Sqlmock)
		wantCause   error
		wantInvalid bool
	}{
		{
			name: "query_cause",
			setup: func(_ *testing.T, mock sqlmock.Sqlmock) {
				mock.ExpectQuery(mysqlTestListQuery()).WithoutArgs().WillReturnError(queryCause)
			},
			wantCause: queryCause,
		},
		{
			name: "second_row_stream_cause",
			setup: func(t *testing.T, mock sqlmock.Sqlmock) {
				rows := sqlmock.NewRows(mysqlTestSelectedColumnNames).
					AddRow(mysqlTestRowValues(t, first)...).
					AddRow(mysqlTestRowValues(t, second)...).
					RowError(1, rowsCause)
				mock.ExpectQuery(mysqlTestListQuery()).WithoutArgs().WillReturnRows(rows).RowsWillBeClosed()
			},
			wantCause: rowsCause,
		},
		{
			name: "context_cause_from_rows",
			setup: func(t *testing.T, mock sqlmock.Sqlmock) {
				rows := sqlmock.NewRows(mysqlTestSelectedColumnNames).
					AddRow(mysqlTestRowValues(t, first)...).
					AddRow(mysqlTestRowValues(t, second)...).
					RowError(1, context.Canceled)
				mock.ExpectQuery(mysqlTestListQuery()).WithoutArgs().WillReturnRows(rows).RowsWillBeClosed()
			},
			wantCause: context.Canceled,
		},
		{
			name: "second_row_scan_failure",
			setup: func(t *testing.T, mock sqlmock.Sqlmock) {
				values := mysqlTestRowValues(t, second)
				values[12] = "not-an-int64"
				rows := sqlmock.NewRows(mysqlTestSelectedColumnNames).
					AddRow(mysqlTestRowValues(t, first)...).
					AddRow(values...)
				mock.ExpectQuery(mysqlTestListQuery()).WithoutArgs().WillReturnRows(rows).RowsWillBeClosed()
			},
			wantInvalid: true,
		},
		{
			name: "second_row_decode_failure",
			setup: func(t *testing.T, mock sqlmock.Sqlmock) {
				values := mysqlTestRowValues(t, second)
				values[17] = []byte(`{"version":1,"text":"corrupt"}`)
				rows := sqlmock.NewRows(mysqlTestSelectedColumnNames).
					AddRow(mysqlTestRowValues(t, first)...).
					AddRow(values...)
				mock.ExpectQuery(mysqlTestListQuery()).WithoutArgs().WillReturnRows(rows).RowsWillBeClosed()
			},
			wantInvalid: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, mock := newMySQLListTestStore(t, sha256.Sum256)
			test.setup(t, mock)
			got, err := store.List(context.Background(), artifact.ListQuery{IncludeDeleted: true})
			if got != nil {
				t.Fatalf("List(failure) result = %#v, want nil (no partial result)", got)
			}
			if test.wantInvalid {
				if !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
					t.Fatalf("List(failure) error = %v, want invalid_argument", err)
				}
			} else if !errors.Is(err, test.wantCause) {
				t.Fatalf("List(failure) error = %v, want cause %v", err, test.wantCause)
			}
			mysqlRequireListExpectations(t, mock)
		})
	}
}

func mysqlListFilterCase(name string, query artifact.ListQuery, fragment string, raw string) struct {
	name      string
	query     artifact.ListQuery
	fragments []string
	args      []driver.Value
} {
	digest := sha256.Sum256([]byte(raw))
	return struct {
		name      string
		query     artifact.ListQuery
		fragments []string
		args      []driver.Value
	}{
		name:      name,
		query:     query,
		fragments: []string{"status <> ?", fragment},
		args:      []driver.Value{[]byte(artifact.ArtifactStatusDeleted), digest[:], []byte(raw)},
	}
}

func mysqlTestListCombinedArguments(query artifact.ListQuery, includeStatus bool) []driver.Value {
	args := make([]driver.Value, 0, 18)
	if includeStatus {
		args = append(args, []byte(artifact.ArtifactStatusDeleted))
	}
	for _, raw := range []string{
		query.TenantID,
		query.SessionID,
		query.RunID,
		string(query.OwnerModule),
		query.OwnerID,
		string(query.Type),
		string(query.Visibility),
	} {
		digest := sha256.Sum256([]byte(raw))
		args = append(args, digest[:], []byte(raw))
	}
	args = append(args, query.ExpiredAtOrBefore.Unix(), query.ExpiredAtOrBefore.Unix(), int64(query.ExpiredAtOrBefore.Nanosecond()))
	return args
}

type mysqlListContextConnector struct {
	key                 any
	value               any
	queryCause          error
	missingContextCause error
	queryCalls          atomic.Int64
}

func (c *mysqlListContextConnector) Connect(context.Context) (driver.Conn, error) {
	return &mysqlListContextConn{connector: c}, nil
}

func (c *mysqlListContextConnector) Driver() driver.Driver {
	return mysqlListContextDriver{connector: c}
}

type mysqlListContextDriver struct {
	connector *mysqlListContextConnector
}

func (d mysqlListContextDriver) Open(string) (driver.Conn, error) {
	return &mysqlListContextConn{connector: d.connector}, nil
}

type mysqlListContextConn struct {
	connector *mysqlListContextConnector
}

func (c *mysqlListContextConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected List Prepare")
}

func (c *mysqlListContextConn) Close() error { return nil }

func (c *mysqlListContextConn) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected List Begin")
}

func (c *mysqlListContextConn) QueryContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	c.connector.queryCalls.Add(1)
	if ctx.Value(c.connector.key) != c.connector.value {
		return nil, c.connector.missingContextCause
	}
	return nil, c.connector.queryCause
}

func mysqlTestListQuery(fragments ...string) string {
	query := mysqlTestListSelect
	if len(fragments) != 0 {
		query += " WHERE " + strings.Join(fragments, " AND ")
	}
	return query + mysqlTestListOrder
}

func newMySQLListTestStore(t *testing.T, digest mysqlDigestFunc) (*MySQLMetadataStore, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mock.MatchExpectationsInOrder(true)
	store, err := newMySQLMetadataStore(db, digest)
	if err != nil {
		t.Fatalf("newMySQLMetadataStore() error = %v", err)
	}
	return store, mock
}

func mysqlRequireListExpectations(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("List SQL expectation: %v", err)
	}
}

func mysqlListMetaFixture(suffix string) artifact.ArtifactMeta {
	meta := mysqlGetterMetaFixture()
	meta.ArtifactID = "list-id-" + suffix
	meta.ArtifactRef = "list-ref-" + suffix
	meta.StorageKey = "list-key-" + suffix
	meta.CreatedAt = time.Unix(1_720_000_000, int64(len(suffix))).UTC()
	return meta
}

func mysqlRequireMetaSlicesEqual(t *testing.T, got, want []artifact.ArtifactMeta) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata slice = %#v, want %#v", got, want)
	}
}
