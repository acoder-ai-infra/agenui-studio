package metastore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

// These expected statements are deliberately test-owned literals. They must
// not be assembled from production SQL constants.
const mysqlTestDeleteSelect = "SELECT id, artifact_id, artifact_ref, tenant_id, user_id, session_id, run_id, step_id, owner_module, owner_id, artifact_type, mime_type, name, size_bytes, artifact_hash, visibility, storage_backend, storage_key, preview_payload, retention_policy, expires_at_sec, expires_at_nano, created_by, created_at_sec, created_at_nano, status, derived_from_payload, schema_version, deleted_at_sec, deleted_at_nano, delete_reason, purge_status, purged_at_sec, purged_at_nano, metadata_payload FROM artifact_metadata WHERE artifact_ref_hash = ? AND artifact_ref = ? LIMIT 2 FOR UPDATE"
const mysqlTestDeleteUpdate = "UPDATE artifact_metadata SET status = ?, deleted_at_sec = ?, deleted_at_nano = ?, delete_reason = ?, purge_status = ?, purged_at_sec = NULL, purged_at_nano = NULL WHERE id = ?"

var mysqlTestDeleteColumnNames = append([]string{"id"}, mysqlTestSelectedColumnNames...)

type mysqlDeleteRollbackExpectation struct {
	name  string
	cause error
}

func mysqlDeleteRollbackExpectations() []mysqlDeleteRollbackExpectation {
	return []mysqlDeleteRollbackExpectation{
		{name: "rollback_nil"},
		{name: "rollback_tx_done", cause: sql.ErrTxDone},
		{name: "rollback_failure", cause: errors.New("MarkDeleted rollback failed")},
	}
}

func mysqlRequireDeleteRollbackCause(t *testing.T, err error, expectation mysqlDeleteRollbackExpectation) {
	t.Helper()
	if errors.Is(expectation.cause, sql.ErrTxDone) {
		if errors.Is(err, sql.ErrTxDone) {
			t.Fatalf("MarkDeleted error = %v, must not expose tolerated sql.ErrTxDone", err)
		}
		return
	}
	if expectation.cause != nil && !errors.Is(err, expectation.cause) {
		t.Fatalf("MarkDeleted error = %v, want joined rollback cause %v", err, expectation.cause)
	}
}

func TestMySQLMetadataStoreMarkDeletedHonorsPreCanceledContext(t *testing.T) {
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

	got, err := store.MarkDeleted(ctx, "must-not-digest", artifact.DeleteReasonUser, time.Now())
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("MarkDeleted(pre-canceled) = (%#v, %v), want nil context.Canceled", got, err)
	}
	if calls := digestCalls.Load(); calls != 0 {
		t.Fatalf("MarkDeleted(pre-canceled) digest calls = %d, want zero", calls)
	}
	requireNoMySQLDriverCalls(t, counts)
}

func TestMySQLMetadataStoreMarkDeletedHooksDefaultNil(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewMySQLMetadataStore(db)
	if err != nil {
		t.Fatalf("NewMySQLMetadataStore() error = %v", err)
	}
	if store.beforeMarkDeletedLockQuery != nil || store.afterMarkDeletedExactRead != nil {
		t.Fatalf("MarkDeleted hooks = (%p, %p), want nil defaults", store.beforeMarkDeletedLockQuery, store.afterMarkDeletedExactRead)
	}
}

func TestFinalizeMySQLMarkDeletedUsesExactlyOneTransactionEndpoint(t *testing.T) {
	primary := errors.New("primary MarkDeleted failure")
	rollbackCause := errors.New("MarkDeleted rollback failure")
	commitCause := errors.New("MarkDeleted commit failure")
	tests := []struct {
		name         string
		primary      error
		rollbackErr  error
		commitErr    error
		wantPrimary  bool
		wantRollback bool
		wantCommit   bool
	}{
		{name: "primary_rollback_nil", primary: primary, wantPrimary: true, wantRollback: true},
		{name: "primary_rollback_tx_done", primary: primary, rollbackErr: sql.ErrTxDone, wantPrimary: true, wantRollback: true},
		{name: "primary_rollback_failure_joined", primary: primary, rollbackErr: rollbackCause, wantPrimary: true, wantRollback: true},
		{name: "success_commit_nil", wantCommit: true},
		{name: "success_commit_failure_no_rollback", commitErr: commitCause, wantCommit: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			finalizer := &mysqlDeleteFakeFinalizer{commitErr: test.commitErr, rollbackErr: test.rollbackErr}
			err := finalizeMySQLMarkDeleted(finalizer, test.primary)
			if test.wantPrimary && !errors.Is(err, primary) {
				t.Fatalf("finalize error = %v, want primary cause", err)
			}
			if test.rollbackErr != nil && !errors.Is(test.rollbackErr, sql.ErrTxDone) && !errors.Is(err, test.rollbackErr) {
				t.Fatalf("finalize error = %v, want rollback cause", err)
			}
			if errors.Is(test.rollbackErr, sql.ErrTxDone) && errors.Is(err, sql.ErrTxDone) {
				t.Fatalf("finalize error = %v, must not expose tolerated sql.ErrTxDone", err)
			}
			if test.commitErr != nil && !errors.Is(err, test.commitErr) {
				t.Fatalf("finalize error = %v, want commit cause", err)
			}
			wantCommits := int64(0)
			if test.wantCommit {
				wantCommits = 1
			}
			wantRollbacks := int64(0)
			if test.wantRollback {
				wantRollbacks = 1
			}
			if finalizer.commits.Load() != wantCommits || finalizer.rollbacks.Load() != wantRollbacks {
				t.Fatalf("finalizer calls = commit %d rollback %d, want %d/%d", finalizer.commits.Load(), finalizer.rollbacks.Load(), wantCommits, wantRollbacks)
			}
			if test.primary == nil && test.commitErr == nil && err != nil {
				t.Fatalf("successful finalize error = %v", err)
			}
		})
	}
}

func TestMySQLMarkDeletedTxFinalizerInterfaceIsNarrow(t *testing.T) {
	typeOfFinalizer := reflect.TypeOf((*mysqlMarkDeletedTxFinalizer)(nil)).Elem()
	if typeOfFinalizer.NumMethod() != 2 || typeOfFinalizer.Method(0).Name != "Commit" || typeOfFinalizer.Method(1).Name != "Rollback" {
		t.Fatalf("mysqlMarkDeletedTxFinalizer methods = %v, want only Commit/Rollback", typeOfFinalizer)
	}
}

func TestMySQLMetadataStoreMarkDeletedFirstUpdateBindingsAndImmediateResult(t *testing.T) {
	location := time.FixedZone("delete-non-UTC", 5*60*60+47)
	tests := []struct {
		name   string
		status artifact.ArtifactStatus
		at     time.Time
		reason artifact.DeleteReason
	}{
		{name: "ready_zero_at_empty_reason", status: artifact.ArtifactStatusReady},
		{name: "expired_non_UTC_binary_reason", status: artifact.ArtifactStatusExpired, at: time.Date(2026, time.July, 14, 3, 4, 5, 987_654_321, location), reason: artifact.DeleteReason("reason\x00\xff ")},
		{name: "uppercase_deleted_is_not_tombstone", status: artifact.ArtifactStatus("Deleted"), at: time.Now(), reason: artifact.DeleteReasonTTL},
		{name: "deleted_with_suffix_is_not_tombstone", status: artifact.ArtifactStatus("deleted\x00"), at: time.Now(), reason: artifact.DeleteReasonCleanup},
		{name: "arbitrary_alias_updates", status: artifact.ArtifactStatus("status-alias"), at: time.Now(), reason: artifact.DeleteReason("reason-alias")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
			base := mysqlDeleteMetaFixture(test.name)
			base.Status = test.status
			recordID := uint64(42)
			refDigest := sha256.Sum256([]byte(base.ArtifactRef))
			mock.ExpectBegin()
			mock.ExpectQuery(mysqlTestDeleteSelect).
				WithArgs(refDigest[:], []byte(base.ArtifactRef)).
				WillReturnRows(mysqlTestDeleteRows(t, recordID, base)).
				RowsWillBeClosed()
			seconds, nanos := mysqlTestDeleteTimeArguments(test.at)
			mock.ExpectExec(mysqlTestDeleteUpdate).
				WithArgs([]byte(artifact.ArtifactStatusDeleted), seconds, nanos, []byte(test.reason), []byte(artifact.PurgeStatusPending), int64(recordID)).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()

			got, err := store.MarkDeleted(context.Background(), base.ArtifactRef, test.reason, test.at)
			if err != nil {
				t.Fatalf("MarkDeleted(first update) error = %v", err)
			}
			want := base
			want.Status = artifact.ArtifactStatusDeleted
			want.DeletedAt = test.at
			want.DeleteReason = test.reason
			want.PurgeStatus = artifact.PurgeStatusPending
			want.PurgedAt = time.Time{}
			if !reflect.DeepEqual(got, &want) {
				t.Fatalf("MarkDeleted(first update) = %#v, want complete immediate %#v", got, &want)
			}
			base.Preview.Text = "mutated fixture"
			base.Preview.Fields["nested"].([]any)[1].([]string)[0] = "mutated fixture"
			base.DerivedFrom[0].ArtifactRef = "mutated fixture"
			base.Metadata["key-\xff"] = "mutated fixture"
			if got.Preview.Text == base.Preview.Text || got.DerivedFrom[0].ArtifactRef == base.DerivedFrom[0].ArtifactRef || got.Metadata["key-\xff"] == base.Metadata["key-\xff"] {
				t.Fatalf("MarkDeleted(first update) aliases fixture containers: %#v", got)
			}
			mysqlRequireDeleteExpectations(t, mock)
		})
	}
}

func TestMySQLMetadataStoreMarkDeletedExactDeletedReturnsFirstTombstoneWithoutUpdate(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	var hookCalls atomic.Int64
	store.afterMarkDeletedExactRead = func() { hookCalls.Add(1) }
	first := mysqlDeleteMetaFixture("existing-first")
	first.Status = artifact.ArtifactStatusDeleted
	first.DeletedAt = time.Unix(1_720_001_000, 123).UTC()
	first.DeleteReason = artifact.DeleteReason("first-reason\x00\xff")
	refDigest := sha256.Sum256([]byte(first.ArtifactRef))
	mock.ExpectBegin()
	mock.ExpectQuery(mysqlTestDeleteSelect).
		WithArgs(refDigest[:], []byte(first.ArtifactRef)).
		WillReturnRows(mysqlTestDeleteRows(t, 77, first)).
		RowsWillBeClosed()
	mock.ExpectCommit()

	got, err := store.MarkDeleted(context.Background(), first.ArtifactRef, artifact.DeleteReasonUser, time.Now())
	if err != nil || !reflect.DeepEqual(got, &first) {
		t.Fatalf("MarkDeleted(existing tombstone) = (%#v, %v), want first %#v", got, err, &first)
	}
	if hookCalls.Load() != 1 {
		t.Fatalf("after-read hook calls for existing tombstone = %d, want one", hookCalls.Load())
	}
	mysqlRequireDeleteExpectations(t, mock)
}

func TestMySQLMetadataStoreMarkDeletedReadFailuresRollbackAndReturnNil(t *testing.T) {
	queryCause := errors.New("delete lock query failed")
	rowsCause := errors.New("delete rows failed")
	base := mysqlDeleteMetaFixture("read-failure")
	refDigest := sha256.Sum256([]byte(base.ArtifactRef))
	tests := []struct {
		name       string
		rows       func(*testing.T) *sqlmock.Rows
		queryCause error
		wantCause  error
		wantCode   artifact.ErrorCode
		wantSyntax bool
	}{
		{name: "query", queryCause: queryCause, wantCause: queryCause},
		{name: "missing", rows: func(t *testing.T) *sqlmock.Rows { return mysqlTestDeleteRows(t, 0) }, wantCode: artifact.ErrNotFound},
		{name: "duplicate", rows: func(t *testing.T) *sqlmock.Rows { return mysqlTestDeleteRows(t, 1, base, base) }, wantCode: artifact.ErrInvalidArgument},
		{name: "record_id_scan", rows: func(t *testing.T) *sqlmock.Rows {
			values := append([]driver.Value{int64(-1)}, mysqlTestRowValues(t, base)...)
			return sqlmock.NewRows(mysqlTestDeleteColumnNames).AddRow(values...)
		}, wantCode: artifact.ErrInvalidArgument},
		{name: "second_row_decode_after_valid_prefix", rows: func(t *testing.T) *sqlmock.Rows {
			values := mysqlTestRowValues(t, base)
			values[17] = []byte{0xff}
			rows := mysqlTestDeleteRows(t, 1, base)
			combined := append([]driver.Value{int64(2)}, values...)
			return rows.AddRow(combined...)
		}, wantCode: artifact.ErrInvalidArgument, wantSyntax: true},
		{name: "second_row_error_after_valid_prefix", rows: func(t *testing.T) *sqlmock.Rows {
			second := base
			second.ArtifactID += "-second"
			second.ArtifactRef += "-second"
			rows := mysqlTestDeleteRows(t, 1, base, second)
			return rows.RowError(1, rowsCause)
		}, wantCause: rowsCause},
		{name: "context", rows: func(t *testing.T) *sqlmock.Rows {
			rows := mysqlTestDeleteRows(t, 1, base)
			return rows.RowError(0, context.Canceled)
		}, wantCause: context.Canceled},
	}
	for _, test := range tests {
		for _, rollback := range mysqlDeleteRollbackExpectations() {
			t.Run(test.name+"/"+rollback.name, func(t *testing.T) {
				store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
				var beforeHookCalls atomic.Int64
				var hookCalls atomic.Int64
				store.beforeMarkDeletedLockQuery = func() { beforeHookCalls.Add(1) }
				store.afterMarkDeletedExactRead = func() { hookCalls.Add(1) }
				mock.ExpectBegin()
				expectation := mock.ExpectQuery(mysqlTestDeleteSelect).WithArgs(refDigest[:], []byte(base.ArtifactRef))
				if test.queryCause != nil {
					expectation.WillReturnError(test.queryCause)
				} else {
					expectation.WillReturnRows(test.rows(t)).RowsWillBeClosed()
				}
				rollbackExpectation := mock.ExpectRollback()
				if rollback.cause != nil {
					rollbackExpectation.WillReturnError(rollback.cause)
				}

				got, err := store.MarkDeleted(context.Background(), base.ArtifactRef, artifact.DeleteReasonUser, time.Now())
				if got != nil {
					t.Fatalf("MarkDeleted(%s) result = %#v, want nil", test.name, got)
				}
				if test.wantCause != nil && !errors.Is(err, test.wantCause) {
					t.Fatalf("MarkDeleted(%s) error = %v, want cause %v", test.name, err, test.wantCause)
				}
				if test.wantCode != "" && !artifact.IsErrorCode(err, test.wantCode) {
					t.Fatalf("MarkDeleted(%s) error = %v, want %s", test.name, err, test.wantCode)
				}
				if test.wantSyntax {
					var syntaxError *json.SyntaxError
					if !errors.As(err, &syntaxError) {
						t.Fatalf("MarkDeleted(%s) error = %v, want second-row *json.SyntaxError cause", test.name, err)
					}
				}
				mysqlRequireDeleteRollbackCause(t, err, rollback)
				if hookCalls.Load() != 0 {
					t.Fatalf("after-read hook calls on %s = %d, want zero", test.name, hookCalls.Load())
				}
				if beforeHookCalls.Load() != 1 {
					t.Fatalf("before-query hook calls on %s = %d, want one", test.name, beforeHookCalls.Load())
				}
				mysqlRequireDeleteExpectations(t, mock)
			})
		}
	}
}

func TestMySQLMetadataStoreMarkDeletedUpdateFailuresRollback(t *testing.T) {
	base := mysqlDeleteMetaFixture("update-failure")
	refDigest := sha256.Sum256([]byte(base.ArtifactRef))
	at := time.Unix(1_720_002_000, 999).UTC()
	execCause := errors.New("delete update failed")
	rowsAffectedCause := errors.New("delete RowsAffected failed")
	tests := []struct {
		name      string
		result    driver.Result
		execCause error
		wantCause error
		wantCode  artifact.ErrorCode
	}{
		{name: "exec", execCause: execCause, wantCause: execCause},
		{name: "rows_affected", result: sqlmock.NewErrorResult(rowsAffectedCause), wantCause: rowsAffectedCause},
		{name: "zero_rows", result: sqlmock.NewResult(0, 0), wantCode: artifact.ErrInvalidArgument},
		{name: "two_rows", result: sqlmock.NewResult(0, 2), wantCode: artifact.ErrInvalidArgument},
	}
	for _, test := range tests {
		for _, rollback := range mysqlDeleteRollbackExpectations() {
			t.Run(test.name+"/"+rollback.name, func(t *testing.T) {
				store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
				mock.ExpectBegin()
				mock.ExpectQuery(mysqlTestDeleteSelect).
					WithArgs(refDigest[:], []byte(base.ArtifactRef)).
					WillReturnRows(mysqlTestDeleteRows(t, 91, base)).
					RowsWillBeClosed()
				exec := mock.ExpectExec(mysqlTestDeleteUpdate).
					WithArgs([]byte(artifact.ArtifactStatusDeleted), at.Unix(), int64(at.Nanosecond()), []byte(artifact.DeleteReasonUser), []byte(artifact.PurgeStatusPending), int64(91))
				if test.execCause != nil {
					exec.WillReturnError(test.execCause)
				} else {
					exec.WillReturnResult(test.result)
				}
				rollbackExpectation := mock.ExpectRollback()
				if rollback.cause != nil {
					rollbackExpectation.WillReturnError(rollback.cause)
				}

				got, err := store.MarkDeleted(context.Background(), base.ArtifactRef, artifact.DeleteReasonUser, at)
				if got != nil {
					t.Fatalf("MarkDeleted(%s) result = %#v, want nil", test.name, got)
				}
				if test.wantCause != nil && !errors.Is(err, test.wantCause) {
					t.Fatalf("MarkDeleted(%s) error = %v, want cause", test.name, err)
				}
				if test.wantCode != "" && !artifact.IsErrorCode(err, test.wantCode) {
					t.Fatalf("MarkDeleted(%s) error = %v, want %s", test.name, err, test.wantCode)
				}
				mysqlRequireDeleteRollbackCause(t, err, rollback)
				mysqlRequireDeleteExpectations(t, mock)
			})
		}
	}
}

func TestMySQLMetadataStoreMarkDeletedCommitErrorsPreserveCause(t *testing.T) {
	commitCause := errors.New("delete commit unknown")
	for _, existing := range []bool{false, true} {
		name := "first_update"
		if existing {
			name = "existing_tombstone"
		}
		t.Run(name, func(t *testing.T) {
			store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
			base := mysqlDeleteMetaFixture("commit-" + name)
			if existing {
				base.Status = artifact.ArtifactStatusDeleted
				base.DeletedAt = time.Unix(1_720_003_000, 1).UTC()
				base.DeleteReason = artifact.DeleteReasonTTL
			}
			refDigest := sha256.Sum256([]byte(base.ArtifactRef))
			at := time.Unix(1_720_003_100, 2).UTC()
			mock.ExpectBegin()
			mock.ExpectQuery(mysqlTestDeleteSelect).
				WithArgs(refDigest[:], []byte(base.ArtifactRef)).
				WillReturnRows(mysqlTestDeleteRows(t, 101, base)).
				RowsWillBeClosed()
			if !existing {
				mock.ExpectExec(mysqlTestDeleteUpdate).
					WithArgs([]byte(artifact.ArtifactStatusDeleted), at.Unix(), int64(at.Nanosecond()), []byte(artifact.DeleteReasonUser), []byte(artifact.PurgeStatusPending), int64(101)).
					WillReturnResult(sqlmock.NewResult(0, 1))
			}
			mock.ExpectCommit().WillReturnError(commitCause)

			got, err := store.MarkDeleted(context.Background(), base.ArtifactRef, artifact.DeleteReasonUser, at)
			if got != nil || !errors.Is(err, commitCause) {
				t.Fatalf("MarkDeleted(%s commit error) = (%#v, %v), want commit cause", name, got, err)
			}
			mysqlRequireDeleteExpectations(t, mock)
		})
	}
}

func TestMySQLMetadataStoreMarkDeletedRequestsReadCommittedWithoutFallback(t *testing.T) {
	beginCause := errors.New("MarkDeleted isolation observation stop")
	connector := &mysqlCreateIsolationConnector{beginCause: beginCause}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewMySQLMetadataStore(db)
	if err != nil {
		t.Fatalf("NewMySQLMetadataStore() error = %v", err)
	}
	var beforeHookCalls atomic.Int64
	store.beforeMarkDeletedLockQuery = func() { beforeHookCalls.Add(1) }

	got, err := store.MarkDeleted(context.Background(), "isolation-ref", artifact.DeleteReasonUser, time.Now())
	if got != nil || !errors.Is(err, beginCause) {
		t.Fatalf("MarkDeleted(isolation observer) = (%#v, %v), want Begin cause", got, err)
	}
	options, fallbackBegins := connector.snapshot()
	if len(options) != 1 || options[0].Isolation != driver.IsolationLevel(sql.LevelReadCommitted) || options[0].ReadOnly {
		t.Fatalf("MarkDeleted BeginTx options = %#v, want one read-write LevelReadCommitted", options)
	}
	if fallbackBegins != 0 {
		t.Fatalf("MarkDeleted fallback Begin calls = %d, want zero", fallbackBegins)
	}
	if beforeHookCalls.Load() != 0 {
		t.Fatalf("before-query hook calls after Begin failure = %d, want zero", beforeHookCalls.Load())
	}
}

func TestMySQLMetadataStoreMarkDeletedProtocolContextRowsCloseAndUint64RecordID(t *testing.T) {
	base := mysqlDeleteMetaFixture("protocol")
	recordID := uint64(1<<63 + 17)
	connector := newMySQLDeleteProtocolConnector(t, recordID, base)
	contextKey := &struct{ name string }{name: "MarkDeleted caller context"}
	contextValue := &struct{ name string }{name: "active value"}
	connector.contextKey = contextKey
	connector.contextValue = contextValue
	db := sql.OpenDB(connector)
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewMySQLMetadataStore(db)
	if err != nil {
		t.Fatalf("NewMySQLMetadataStore() error = %v", err)
	}
	store.beforeMarkDeletedLockQuery = func() { connector.addEvent("beforeQueryHook") }
	store.afterMarkDeletedExactRead = func() {
		if !connector.rowsClosed.Load() {
			connector.hookBeforeClose.Store(true)
		}
		connector.addEvent("afterReadHook")
	}
	at := time.Now()
	reason := artifact.DeleteReason("protocol-reason\x00\xff ")
	ctx := context.WithValue(context.Background(), contextKey, contextValue)

	got, err := store.MarkDeleted(ctx, base.ArtifactRef, reason, at)
	if err != nil {
		t.Fatalf("MarkDeleted(protocol) error = %v", err)
	}
	want := base
	want.Status = artifact.ArtifactStatusDeleted
	want.DeletedAt = at
	want.DeleteReason = reason
	want.PurgeStatus = artifact.PurgeStatusPending
	want.PurgedAt = time.Time{}
	if !reflect.DeepEqual(got, &want) {
		t.Fatalf("MarkDeleted(protocol) = %#v, want %#v", got, &want)
	}
	if connector.hookBeforeClose.Load() {
		t.Fatal("after-read hook ran before rows Close")
	}
	if connector.endpointBeforeClose.Load() {
		t.Fatal("transaction endpoint ran before rows Close")
	}
	if gotOptions := connector.optionsSnapshot(); len(gotOptions) != 1 || gotOptions[0].Isolation != driver.IsolationLevel(sql.LevelReadCommitted) || gotOptions[0].ReadOnly {
		t.Fatalf("MarkDeleted protocol BeginTx options = %#v", gotOptions)
	}
	if connector.fallbackBegins.Load() != 0 {
		t.Fatalf("MarkDeleted protocol fallback Begins = %d, want zero", connector.fallbackBegins.Load())
	}
	refDigest := sha256.Sum256([]byte(base.ArtifactRef))
	query, queryArgs, execQuery, execArgs := connector.sqlSnapshot()
	if query != mysqlTestDeleteSelect || !reflect.DeepEqual(queryArgs, []any{append([]byte(nil), refDigest[:]...), []byte(base.ArtifactRef)}) {
		t.Fatalf("MarkDeleted lock query/args = (%q, %#v), want test-owned exact SQL/hash/raw", query, queryArgs)
	}
	wantExecArgs := []any{[]byte(artifact.ArtifactStatusDeleted), at.Unix(), int64(at.Nanosecond()), []byte(reason), []byte(artifact.PurgeStatusPending), recordID}
	if execQuery != mysqlTestDeleteUpdate || !reflect.DeepEqual(execArgs, wantExecArgs) {
		t.Fatalf("MarkDeleted update/args = (%q, %#v), want (%q, %#v)", execQuery, execArgs, mysqlTestDeleteUpdate, wantExecArgs)
	}
	if gotEvents := connector.eventsSnapshot(); !reflect.DeepEqual(gotEvents, []string{"Begin", "beforeQueryHook", "Query", "RowsClose", "afterReadHook", "Exec", "Commit"}) {
		t.Fatalf("MarkDeleted protocol events = %#v", gotEvents)
	}
	if connector.queryCalls.Load() != 1 || connector.commitCalls.Load() != 1 || connector.rollbackCalls.Load() != 0 {
		t.Fatalf("MarkDeleted protocol calls query/commit/rollback = %d/%d/%d, want 1/1/0", connector.queryCalls.Load(), connector.commitCalls.Load(), connector.rollbackCalls.Load())
	}
}

func TestMySQLMetadataStoreMarkDeletedCloseFailuresJoinReadFailureAndSkipHook(t *testing.T) {
	closeCause := errors.New("MarkDeleted rows close failed")
	for _, corrupt := range []bool{false, true} {
		name := "close_only"
		if corrupt {
			name = "decode_and_close_joined"
		}
		for _, rollback := range mysqlDeleteRollbackExpectations() {
			t.Run(name+"/"+rollback.name, func(t *testing.T) {
				base := mysqlDeleteMetaFixture("close-" + name)
				connector := newMySQLDeleteProtocolConnector(t, 202, base)
				connector.closeCause = closeCause
				connector.rollbackCause = rollback.cause
				if corrupt {
					connector.row[1+17] = []byte(`{"version":1,"text":"corrupt"}`)
				}
				db := sql.OpenDB(connector)
				t.Cleanup(func() { _ = db.Close() })
				store, err := NewMySQLMetadataStore(db)
				if err != nil {
					t.Fatalf("NewMySQLMetadataStore() error = %v", err)
				}
				var hookCalls atomic.Int64
				store.afterMarkDeletedExactRead = func() { hookCalls.Add(1) }

				got, err := store.MarkDeleted(context.Background(), base.ArtifactRef, artifact.DeleteReasonUser, time.Now())
				if got != nil || !errors.Is(err, closeCause) {
					t.Fatalf("MarkDeleted(%s) = (%#v, %v), want close cause", name, got, err)
				}
				if corrupt && !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
					t.Fatalf("MarkDeleted(%s) error = %v, want joined invalid_argument", name, err)
				}
				mysqlRequireDeleteRollbackCause(t, err, rollback)
				if hookCalls.Load() != 0 {
					t.Fatalf("after-read hook calls on %s = %d, want zero", name, hookCalls.Load())
				}
				if connector.execCalls.Load() != 0 || connector.commitCalls.Load() != 0 || connector.rollbackCalls.Load() != 1 {
					t.Fatalf("MarkDeleted(%s) exec/commit/rollback = %d/%d/%d, want 0/0/1", name, connector.execCalls.Load(), connector.commitCalls.Load(), connector.rollbackCalls.Load())
				}
				if connector.endpointBeforeClose.Load() {
					t.Fatal("Rollback ran before rows Close")
				}
			})
		}
	}
}

func TestMySQLMetadataStoreMarkDeletedCommitErrorHasNoRollbackOrRecoveryQuery(t *testing.T) {
	commitCause := errors.New("MarkDeleted unknown commit result")
	for _, existing := range []bool{false, true} {
		name := "first_update"
		if existing {
			name = "existing_tombstone"
		}
		t.Run(name, func(t *testing.T) {
			base := mysqlDeleteMetaFixture("counter-" + name)
			if existing {
				base.Status = artifact.ArtifactStatusDeleted
				base.DeletedAt = time.Unix(1_720_004_000, 7).UTC()
				base.DeleteReason = artifact.DeleteReasonTTL
			}
			connector := newMySQLDeleteProtocolConnector(t, 303, base)
			connector.commitCause = commitCause
			db := sql.OpenDB(connector)
			t.Cleanup(func() { _ = db.Close() })
			store, err := NewMySQLMetadataStore(db)
			if err != nil {
				t.Fatalf("NewMySQLMetadataStore() error = %v", err)
			}

			got, err := store.MarkDeleted(context.Background(), base.ArtifactRef, artifact.DeleteReasonUser, time.Now())
			if got != nil || !errors.Is(err, commitCause) {
				t.Fatalf("MarkDeleted(%s commit counter) = (%#v, %v), want commit cause", name, got, err)
			}
			if connector.queryCalls.Load() != 1 {
				t.Fatalf("MarkDeleted(%s commit error) total queries = %d, want one lock query and zero external recovery", name, connector.queryCalls.Load())
			}
			if connector.commitCalls.Load() != 1 || connector.rollbackCalls.Load() != 0 {
				t.Fatalf("MarkDeleted(%s commit error) commit/rollback = %d/%d, want 1/0", name, connector.commitCalls.Load(), connector.rollbackCalls.Load())
			}
		})
	}
}

type mysqlDeleteFakeFinalizer struct {
	commitErr   error
	rollbackErr error
	commits     atomic.Int64
	rollbacks   atomic.Int64
}

type mysqlDeleteProtocolConnector struct {
	row           []driver.Value
	contextKey    any
	contextValue  any
	closeCause    error
	commitCause   error
	rollbackCause error

	mu                  sync.Mutex
	events              []string
	options             []driver.TxOptions
	query               string
	queryArgs           []any
	execQuery           string
	execArgs            []any
	rowsClosed          atomic.Bool
	hookBeforeClose     atomic.Bool
	endpointBeforeClose atomic.Bool
	fallbackBegins      atomic.Int64
	queryCalls          atomic.Int64
	execCalls           atomic.Int64
	commitCalls         atomic.Int64
	rollbackCalls       atomic.Int64
}

func newMySQLDeleteProtocolConnector(t *testing.T, recordID uint64, meta artifact.ArtifactMeta) *mysqlDeleteProtocolConnector {
	t.Helper()
	row := append([]driver.Value{[]byte(strconv.FormatUint(recordID, 10))}, mysqlTestRowValues(t, meta)...)
	return &mysqlDeleteProtocolConnector{row: row}
}

func (c *mysqlDeleteProtocolConnector) Connect(context.Context) (driver.Conn, error) {
	return &mysqlDeleteProtocolConn{connector: c}, nil
}

func (c *mysqlDeleteProtocolConnector) Driver() driver.Driver {
	return mysqlDeleteProtocolDriver{connector: c}
}

func (c *mysqlDeleteProtocolConnector) addEvent(event string) {
	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()
}

func (c *mysqlDeleteProtocolConnector) eventsSnapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

func (c *mysqlDeleteProtocolConnector) optionsSnapshot() []driver.TxOptions {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]driver.TxOptions(nil), c.options...)
}

func (c *mysqlDeleteProtocolConnector) sqlSnapshot() (string, []any, string, []any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.query, append([]any(nil), c.queryArgs...), c.execQuery, append([]any(nil), c.execArgs...)
}

type mysqlDeleteProtocolDriver struct {
	connector *mysqlDeleteProtocolConnector
}

func (d mysqlDeleteProtocolDriver) Open(string) (driver.Conn, error) {
	return &mysqlDeleteProtocolConn{connector: d.connector}, nil
}

type mysqlDeleteProtocolConn struct {
	connector *mysqlDeleteProtocolConnector
}

func (c *mysqlDeleteProtocolConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected MarkDeleted Prepare")
}

func (c *mysqlDeleteProtocolConn) Close() error { return nil }

func (c *mysqlDeleteProtocolConn) Begin() (driver.Tx, error) {
	c.connector.fallbackBegins.Add(1)
	return nil, errors.New("unexpected MarkDeleted default Begin")
}

func (c *mysqlDeleteProtocolConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	if c.connector.contextKey != nil && ctx.Value(c.connector.contextKey) != c.connector.contextValue {
		return nil, errors.New("MarkDeleted did not pass caller context to BeginTx")
	}
	c.connector.mu.Lock()
	c.connector.options = append(c.connector.options, options)
	c.connector.mu.Unlock()
	c.connector.addEvent("Begin")
	return &mysqlDeleteProtocolTx{connector: c.connector}, nil
}

func (c *mysqlDeleteProtocolConn) CheckNamedValue(*driver.NamedValue) error { return nil }

func (c *mysqlDeleteProtocolConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.connector.queryCalls.Add(1)
	c.connector.addEvent("Query")
	if c.connector.contextKey != nil && ctx.Value(c.connector.contextKey) != c.connector.contextValue {
		return nil, errors.New("MarkDeleted did not pass caller context")
	}
	c.connector.mu.Lock()
	c.connector.query = query
	c.connector.queryArgs = mysqlDeleteNamedValues(args)
	c.connector.mu.Unlock()
	c.connector.rowsClosed.Store(false)
	return &mysqlDeleteProtocolRows{connector: c.connector, row: append([]driver.Value(nil), c.connector.row...)}, nil
}

func (c *mysqlDeleteProtocolConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.connector.contextKey != nil && ctx.Value(c.connector.contextKey) != c.connector.contextValue {
		return nil, errors.New("MarkDeleted did not pass caller context to ExecContext")
	}
	c.connector.execCalls.Add(1)
	c.connector.addEvent("Exec")
	c.connector.mu.Lock()
	c.connector.execQuery = query
	c.connector.execArgs = mysqlDeleteNamedValues(args)
	c.connector.mu.Unlock()
	return driver.RowsAffected(1), nil
}

type mysqlDeleteProtocolTx struct {
	connector *mysqlDeleteProtocolConnector
}

func (tx *mysqlDeleteProtocolTx) Commit() error {
	if !tx.connector.rowsClosed.Load() {
		tx.connector.endpointBeforeClose.Store(true)
	}
	tx.connector.commitCalls.Add(1)
	tx.connector.addEvent("Commit")
	return tx.connector.commitCause
}

func (tx *mysqlDeleteProtocolTx) Rollback() error {
	if !tx.connector.rowsClosed.Load() {
		tx.connector.endpointBeforeClose.Store(true)
	}
	tx.connector.rollbackCalls.Add(1)
	tx.connector.addEvent("Rollback")
	return tx.connector.rollbackCause
}

type mysqlDeleteProtocolRows struct {
	connector *mysqlDeleteProtocolConnector
	row       []driver.Value
	delivered bool
}

func (r *mysqlDeleteProtocolRows) Columns() []string {
	return append([]string(nil), mysqlTestDeleteColumnNames...)
}

func (r *mysqlDeleteProtocolRows) Close() error {
	r.connector.rowsClosed.Store(true)
	r.connector.addEvent("RowsClose")
	return r.connector.closeCause
}

func (r *mysqlDeleteProtocolRows) Next(destination []driver.Value) error {
	if r.delivered {
		return io.EOF
	}
	r.delivered = true
	copy(destination, r.row)
	return nil
}

func mysqlDeleteNamedValues(values []driver.NamedValue) []any {
	result := make([]any, len(values))
	for index := range values {
		switch value := values[index].Value.(type) {
		case []byte:
			result[index] = append([]byte(nil), value...)
		default:
			result[index] = value
		}
	}
	return result
}

func (f *mysqlDeleteFakeFinalizer) Commit() error {
	f.commits.Add(1)
	return f.commitErr
}

func (f *mysqlDeleteFakeFinalizer) Rollback() error {
	f.rollbacks.Add(1)
	return f.rollbackErr
}

func newMySQLDeleteTestStore(t *testing.T, digest mysqlDigestFunc) (*MySQLMetadataStore, sqlmock.Sqlmock) {
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

func mysqlDeleteMetaFixture(suffix string) artifact.ArtifactMeta {
	meta := mysqlGetterMetaFixture()
	meta.ArtifactID = "delete-id-" + suffix
	meta.ArtifactRef = "delete-ref-" + suffix
	meta.StorageKey = "delete-key-" + suffix
	meta.Status = artifact.ArtifactStatusReady
	meta.DeletedAt = time.Time{}
	meta.DeleteReason = ""
	return meta
}

func mysqlTestDeleteRows(t *testing.T, recordID uint64, metas ...artifact.ArtifactMeta) *sqlmock.Rows {
	t.Helper()
	rows := sqlmock.NewRows(mysqlTestDeleteColumnNames)
	for _, meta := range metas {
		values := append([]driver.Value{int64(recordID)}, mysqlTestRowValues(t, meta)...)
		rows.AddRow(values...)
	}
	return rows
}

func mysqlTestDeleteTimeArguments(at time.Time) (driver.Value, driver.Value) {
	if at.IsZero() {
		return nil, nil
	}
	return at.Unix(), int64(at.Nanosecond())
}

func mysqlRequireDeleteExpectations(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("MarkDeleted SQL protocol mismatch: %v", err)
	}
}
