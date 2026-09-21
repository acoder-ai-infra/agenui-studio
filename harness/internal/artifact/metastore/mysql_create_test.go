package metastore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

const mysqlTestCreateColumns = "artifact_id, artifact_id_hash, artifact_ref, artifact_ref_hash, idempotency_key, idempotency_hash, tenant_id, tenant_id_hash, user_id, session_id, session_id_hash, run_id, run_id_hash, step_id, owner_module, owner_module_hash, owner_id, owner_id_hash, artifact_type, artifact_type_hash, mime_type, name, size_bytes, artifact_hash, visibility, visibility_hash, storage_backend, storage_key, preview_payload, retention_policy, expires_at_sec, expires_at_nano, created_by, created_at_sec, created_at_nano, status, derived_from_payload, schema_version, deleted_at_sec, deleted_at_nano, delete_reason, purge_status, purged_at_sec, purged_at_nano, metadata_payload"

const mysqlTestKeyLockUpsert = "INSERT INTO artifact_metadata_key_lock (key_kind, key_hash) VALUES (?, ?) ON DUPLICATE KEY UPDATE key_hash = VALUES(key_hash)"
const mysqlTestKeyLockSelect = "SELECT key_hash FROM artifact_metadata_key_lock WHERE key_kind = ? AND key_hash = ? FOR UPDATE"

var mysqlTestCreateInsert = "INSERT INTO artifact_metadata (" + mysqlTestCreateColumns + ") VALUES (" + strings.TrimSuffix(strings.Repeat("?, ", 45), ", ") + ")"

func TestMySQLMetadataStoreCreateHonorsPreCanceledContext(t *testing.T) {
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

	got, err := store.Create(ctx, portableMySQLMetaFixture(), "idempotency")
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Create(pre-canceled) = (%#v, %v), want (nil, context.Canceled)", got, err)
	}
	if calls := digestCalls.Load(); calls != 0 {
		t.Fatalf("Create(pre-canceled) digest calls = %d, want zero", calls)
	}
	requireNoMySQLDriverCalls(t, counts)
}

func TestMySQLMetadataStoreCreateIdempotencyFastHitPrecedesEncoding(t *testing.T) {
	store, mock := newMySQLCreateTestStore(t, sha256.Sum256)
	var hookCalls atomic.Int64
	store.afterCreateExactMiss = func(uint8) { hookCalls.Add(1) }
	winner := mysqlGetterMetaFixture()
	candidate := portableMySQLMetaFixture()
	candidate.ArtifactID = "unsupported-candidate"
	candidate.Preview.Fields = map[string]any{"unsupported": make(chan int)}
	key := "fast-hit\x00\xff "
	mysqlExpectExactRows(t, mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key, winner, 1)

	got, err := store.Create(context.Background(), candidate, key)
	if err != nil {
		t.Fatalf("Create(fast idempotency hit) error = %v", err)
	}
	if !reflect.DeepEqual(got, &winner) {
		t.Fatalf("Create(fast idempotency hit) = %#v, want %#v", got, &winner)
	}
	if calls := hookCalls.Load(); calls != 0 {
		t.Fatalf("afterCreateExactMiss calls on fast hit = %d, want zero", calls)
	}
	mysqlRequireCreateExpectations(t, mock)
}

func TestMySQLMetadataStoreCreateFastHitDirectlyCountsZeroTransactions(t *testing.T) {
	winner := mysqlGetterMetaFixture()
	connector := &mysqlCreateFailureConnector{winnerRow: mysqlTestRowValues(t, winner), externalWinner: true}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewMySQLMetadataStore(db)
	if err != nil {
		t.Fatalf("NewMySQLMetadataStore() error = %v", err)
	}
	candidate := portableMySQLMetaFixture()
	candidate.Preview.Fields = map[string]any{"unsupported": make(chan int)}

	got, err := store.Create(context.Background(), candidate, "direct-fast-hit")
	if err != nil || !reflect.DeepEqual(got, &winner) {
		t.Fatalf("Create(direct fast hit) = (%#v, %v), want winner %#v", got, err, &winner)
	}
	snapshot := connector.snapshot()
	if snapshot.begins != 0 {
		t.Fatalf("Create(direct fast hit) BeginTx calls = %d, want zero", snapshot.begins)
	}
	if snapshot.externalQueries != 1 {
		t.Fatalf("Create(direct fast hit) exact queries = %d, want one fast read", snapshot.externalQueries)
	}
}

func TestMySQLMetadataStoreCreateRejectsUnpersistableCandidateBeforeTransaction(t *testing.T) {
	for _, key := range []string{"", "codec-miss"} {
		name := "empty_idempotency"
		if key != "" {
			name = "nonempty_idempotency"
		}
		t.Run(name, func(t *testing.T) {
			store, mock := newMySQLCreateTestStore(t, sha256.Sum256)
			candidate := portableMySQLMetaFixture()
			candidate.Preview.Fields = map[string]any{"unsupported": make(chan int)}
			if key != "" {
				mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
			}

			got, err := store.Create(context.Background(), candidate, key)
			if got != nil || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
				t.Fatalf("Create(unpersistable candidate) = (%#v, %v), want invalid_argument", got, err)
			}
			mysqlRequireCreateExpectations(t, mock)
		})
	}
}

func TestMySQLMetadataStoreCreateUsesFixedLockOrderAndInsertBindings(t *testing.T) {
	for _, test := range []struct {
		name string
		key  string
	}{
		{name: "empty_idempotency_skips_fast_read_and_lock", key: ""},
		{name: "nonempty_idempotency_locks_first", key: "idem\x00\xff "},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, mock := newMySQLCreateTestStore(t, sha256.Sum256)
			original := portableMySQLMetaFixture()
			original.PurgeStatus = artifact.PurgeStatusPurged
			original.PurgedAt = time.Unix(1_720_000_000, 765_432_109).UTC()
			want := mysqlMustPrepareImmediate(t, original)
			if test.key != "" {
				mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", test.key)
			}
			mock.ExpectBegin()
			if test.key != "" {
				mysqlExpectKeyLock(mock, sha256.Sum256, 1, test.key)
				mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", test.key)
			}
			mysqlExpectKeyLock(mock, sha256.Sum256, 2, original.ArtifactRef)
			mysqlExpectExactMissing(mock, sha256.Sum256, "artifact_ref_hash = ? AND artifact_ref = ?", original.ArtifactRef)
			mysqlExpectKeyLock(mock, sha256.Sum256, 3, original.ArtifactID)
			mysqlExpectExactMissing(mock, sha256.Sum256, "artifact_id_hash = ? AND artifact_id = ?", original.ArtifactID)
			args := mysqlTestCreateArgs(t, original, test.key, sha256.Sum256)
			mock.ExpectExec(mysqlTestCreateInsert).WithArgs(args...).WillReturnResult(sqlmock.NewResult(1, 1))
			mock.ExpectCommit()

			got, err := store.Create(context.Background(), original, test.key)
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Create() immediate result = %#v, want %#v", got, want)
			}
			original.Preview.Text = "mutated input"
			original.Preview.Fields["nested"].([]any)[1].([]string)[0] = "mutated input"
			original.DerivedFrom[0].ArtifactRef = "mutated input"
			original.Metadata["key-\xff"] = "mutated input"
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Create() result aliases candidate containers: got %#v, want %#v", got, want)
			}
			got.Preview.Text = "mutated result"
			if original.Preview.Text == got.Preview.Text {
				t.Fatal("Create() candidate aliases returned Preview")
			}
			mysqlRequireCreateExpectations(t, mock)
		})
	}
}

func TestMySQLMetadataStoreCreateLockTimeIdempotencyWinnerFinalizesTransaction(t *testing.T) {
	for _, test := range []struct {
		name        string
		rollbackErr error
		wantWinner  bool
	}{
		{name: "rollback_nil", wantWinner: true},
		{name: "rollback_tx_done", rollbackErr: sql.ErrTxDone, wantWinner: true},
		{name: "rollback_failure", rollbackErr: errors.New("lock-time rollback failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, mock := newMySQLCreateTestStore(t, sha256.Sum256)
			candidate := portableMySQLMetaFixture()
			winner := mysqlGetterMetaFixture()
			key := "lock-time-idempotency"
			mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
			mock.ExpectBegin()
			mysqlExpectKeyLock(mock, sha256.Sum256, 1, key)
			mysqlExpectExactRows(t, mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key, winner, 1)
			rollback := mock.ExpectRollback()
			if test.rollbackErr != nil {
				rollback.WillReturnError(test.rollbackErr)
			}

			got, err := store.Create(context.Background(), candidate, key)
			if test.wantWinner {
				if err != nil || !reflect.DeepEqual(got, &winner) {
					t.Fatalf("Create(lock-time winner) = (%#v, %v), want winner %#v", got, err, &winner)
				}
			} else if got != nil || !errors.Is(err, test.rollbackErr) {
				t.Fatalf("Create(lock-time rollback failure) = (%#v, %v), want rollback cause", got, err)
			}
			mysqlRequireCreateExpectations(t, mock)
		})
	}
}

func TestMySQLMetadataStoreCreateExactRefOrIDConflictRollsBackWithoutRecovery(t *testing.T) {
	for _, kind := range []string{"ref", "id"} {
		t.Run(kind, func(t *testing.T) {
			store, mock := newMySQLCreateTestStore(t, sha256.Sum256)
			candidate := portableMySQLMetaFixture()
			existing := mysqlGetterMetaFixture()
			key := "conflicting-candidate-key"
			mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
			mock.ExpectBegin()
			mysqlExpectKeyLock(mock, sha256.Sum256, 1, key)
			mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
			mysqlExpectKeyLock(mock, sha256.Sum256, 2, candidate.ArtifactRef)
			if kind == "ref" {
				existing.ArtifactRef = candidate.ArtifactRef
				mysqlExpectExactRows(t, mock, sha256.Sum256, "artifact_ref_hash = ? AND artifact_ref = ?", candidate.ArtifactRef, existing, 1)
			} else {
				mysqlExpectExactMissing(mock, sha256.Sum256, "artifact_ref_hash = ? AND artifact_ref = ?", candidate.ArtifactRef)
				mysqlExpectKeyLock(mock, sha256.Sum256, 3, candidate.ArtifactID)
				existing.ArtifactID = candidate.ArtifactID
				mysqlExpectExactRows(t, mock, sha256.Sum256, "artifact_id_hash = ? AND artifact_id = ?", candidate.ArtifactID, existing, 1)
			}
			mock.ExpectRollback()

			got, err := store.Create(context.Background(), candidate, key)
			if got != nil || !artifact.IsErrorCode(err, artifact.ErrConflict) {
				t.Fatalf("Create(exact %s conflict) = (%#v, %v), want conflict", kind, got, err)
			}
			mysqlRequireCreateExpectations(t, mock)
		})
	}
}

func TestMySQLMetadataStoreCreateDomainConflictJoinsRollbackFailureWithoutRecovery(t *testing.T) {
	store, mock := newMySQLCreateTestStore(t, sha256.Sum256)
	candidate := portableMySQLMetaFixture()
	existing := mysqlGetterMetaFixture()
	existing.ArtifactRef = candidate.ArtifactRef
	key := "domain-conflict-rollback-failure"
	rollbackCause := errors.New("domain conflict rollback failed")
	mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
	mock.ExpectBegin()
	mysqlExpectKeyLock(mock, sha256.Sum256, 1, key)
	mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
	mysqlExpectKeyLock(mock, sha256.Sum256, 2, candidate.ArtifactRef)
	mysqlExpectExactRows(t, mock, sha256.Sum256, "artifact_ref_hash = ? AND artifact_ref = ?", candidate.ArtifactRef, existing, 1)
	mock.ExpectRollback().WillReturnError(rollbackCause)

	got, err := store.Create(context.Background(), candidate, key)
	if got != nil || !artifact.IsErrorCode(err, artifact.ErrConflict) || !errors.Is(err, rollbackCause) {
		t.Fatalf("Create(conflict rollback failure) = (%#v, %v), want joined conflict and rollback cause", got, err)
	}
	mysqlRequireCreateExpectations(t, mock)
}

func TestMySQLMetadataStoreCreateDomainConflictRollbackFailureDirectlyCountsNoRecovery(t *testing.T) {
	candidate := portableMySQLMetaFixture()
	existing := mysqlGetterMetaFixture()
	existing.ArtifactRef = candidate.ArtifactRef
	rollbackCause := errors.New("direct domain rollback failed")
	connector := &mysqlCreateFailureConnector{
		winnerRow:           mysqlTestRowValues(t, existing),
		transactionWinnerAt: 3,
		rollbackCause:       rollbackCause,
	}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewMySQLMetadataStore(db)
	if err != nil {
		t.Fatalf("NewMySQLMetadataStore() error = %v", err)
	}

	got, err := store.Create(context.Background(), candidate, "direct-domain-conflict")
	if got != nil || !artifact.IsErrorCode(err, artifact.ErrConflict) || !errors.Is(err, rollbackCause) {
		t.Fatalf("Create(direct conflict rollback failure) = (%#v, %v), want joined conflict and rollback cause", got, err)
	}
	snapshot := connector.snapshot()
	if snapshot.externalQueries != 1 {
		t.Fatalf("Create(direct conflict rollback failure) external queries = %d, want only the fast read", snapshot.externalQueries)
	}
	if snapshot.rollbacks != 1 {
		t.Fatalf("Create(direct conflict rollback failure) rollbacks = %d, want 1", snapshot.rollbacks)
	}
}

func TestMySQLMetadataStoreCreateDuplicateExactRowsAreCorruptNotConflict(t *testing.T) {
	for _, kind := range []string{"ref", "id"} {
		t.Run(kind, func(t *testing.T) {
			store, mock := newMySQLCreateTestStore(t, sha256.Sum256)
			candidate := portableMySQLMetaFixture()
			key := "corrupt-candidate-key"
			mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
			mock.ExpectBegin()
			mysqlExpectKeyLock(mock, sha256.Sum256, 1, key)
			mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
			mysqlExpectKeyLock(mock, sha256.Sum256, 2, candidate.ArtifactRef)
			if kind == "ref" {
				mysqlExpectExactRows(t, mock, sha256.Sum256, "artifact_ref_hash = ? AND artifact_ref = ?", candidate.ArtifactRef, candidate, 2)
			} else {
				mysqlExpectExactMissing(mock, sha256.Sum256, "artifact_ref_hash = ? AND artifact_ref = ?", candidate.ArtifactRef)
				mysqlExpectKeyLock(mock, sha256.Sum256, 3, candidate.ArtifactID)
				mysqlExpectExactRows(t, mock, sha256.Sum256, "artifact_id_hash = ? AND artifact_id = ?", candidate.ArtifactID, candidate, 2)
			}
			mock.ExpectRollback()

			got, err := store.Create(context.Background(), candidate, key)
			if got != nil || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) || artifact.IsErrorCode(err, artifact.ErrConflict) {
				t.Fatalf("Create(duplicate exact %s rows) = (%#v, %v), want only invalid_argument", kind, got, err)
			}
			mysqlRequireCreateExpectations(t, mock)
		})
	}
}

func TestMySQLMetadataStoreCreateFastIdempotencyFailuresDoNotRecover(t *testing.T) {
	t.Run("query_error", func(t *testing.T) {
		store, mock := newMySQLCreateTestStore(t, sha256.Sum256)
		cause := errors.New("fast query failed")
		key := "fast-query-failure"
		digest := mysqlTestDigest(sha256.Sum256, key)
		mock.ExpectQuery(mysqlTestSelectQuery("idempotency_hash = ? AND idempotency_key = ?")).
			WithArgs(digest, []byte(key)).WillReturnError(cause)

		got, err := store.Create(context.Background(), portableMySQLMetaFixture(), key)
		if got != nil || !errors.Is(err, cause) {
			t.Fatalf("Create(fast query failure) = (%#v, %v), want query cause", got, err)
		}
		mysqlRequireCreateExpectations(t, mock)
	})

	t.Run("corrupt_duplicate_rows", func(t *testing.T) {
		store, mock := newMySQLCreateTestStore(t, sha256.Sum256)
		winner := mysqlGetterMetaFixture()
		key := "fast-corrupt"
		mysqlExpectExactRows(t, mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key, winner, 2)

		got, err := store.Create(context.Background(), portableMySQLMetaFixture(), key)
		if got != nil || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
			t.Fatalf("Create(fast duplicate rows) = (%#v, %v), want invalid_argument", got, err)
		}
		mysqlRequireCreateExpectations(t, mock)
	})

	t.Run("corrupt_payload", func(t *testing.T) {
		store, mock := newMySQLCreateTestStore(t, sha256.Sum256)
		winner := mysqlGetterMetaFixture()
		key := "fast-corrupt-payload"
		values := mysqlTestRowValues(t, winner)
		values[17] = []byte(`{"version":1,"text":"broken"}`)
		hash := mysqlTestDigest(sha256.Sum256, key)
		mock.ExpectQuery(mysqlTestSelectQuery("idempotency_hash = ? AND idempotency_key = ?")).
			WithArgs(hash, []byte(key)).
			WillReturnRows(sqlmock.NewRows(mysqlTestSelectedColumnNames).AddRow(values...)).
			RowsWillBeClosed()

		got, err := store.Create(context.Background(), portableMySQLMetaFixture(), key)
		if got != nil || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
			t.Fatalf("Create(fast corrupt payload) = (%#v, %v), want invalid_argument", got, err)
		}
		mysqlRequireCreateExpectations(t, mock)
	})
}

func TestMySQLMetadataStoreCreateInjectedDigestStillComparesExactRaw(t *testing.T) {
	constantDigest := func([]byte) [32]byte { return [32]byte{0xde, 0xad, 0xbe, 0xef} }
	store, mock := newMySQLCreateTestStore(t, constantDigest)
	metas := []artifact.ArtifactMeta{
		mysqlCreateIntegrationMeta("unit-collision-first\x00"),
		mysqlCreateIntegrationMeta("unit-collision-second\xff "),
	}
	keys := []string{"unit-collision-key-first", "unit-collision-key-second"}
	for index, meta := range metas {
		key := keys[index]
		mysqlExpectExactMissing(mock, constantDigest, "idempotency_hash = ? AND idempotency_key = ?", key)
		mock.ExpectBegin()
		mysqlExpectAllLocksAndMisses(mock, constantDigest, meta, key)
		args := mysqlTestCreateArgs(t, meta, key, constantDigest)
		mock.ExpectExec(mysqlTestCreateInsert).WithArgs(args...).WillReturnResult(sqlmock.NewResult(int64(index+1), 1))
		mock.ExpectCommit()
	}

	for index, meta := range metas {
		got, err := store.Create(context.Background(), meta, keys[index])
		if err != nil || got == nil || got.ArtifactRef != meta.ArtifactRef {
			t.Fatalf("Create(collision raw %d) = (%#v, %v), want exact raw success", index, got, err)
		}
	}
	mysqlRequireCreateExpectations(t, mock)
}

func TestMySQLMetadataStoreCreateExactMissHookOrderAndBoundaries(t *testing.T) {
	t.Run("success_reports_idempotency_ref_id", func(t *testing.T) {
		store, mock := newMySQLCreateTestStore(t, sha256.Sum256)
		meta := portableMySQLMetaFixture()
		key := "hook-success"
		var kinds []uint8
		store.afterCreateExactMiss = func(kind uint8) { kinds = append(kinds, kind) }
		mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
		mock.ExpectBegin()
		mysqlExpectAllLocksAndMisses(mock, sha256.Sum256, meta, key)
		args := mysqlTestCreateArgs(t, meta, key, sha256.Sum256)
		mock.ExpectExec(mysqlTestCreateInsert).WithArgs(args...).WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()

		got, err := store.Create(context.Background(), meta, key)
		if err != nil || got == nil {
			t.Fatalf("Create(hook success) = (%#v, %v)", got, err)
		}
		if !reflect.DeepEqual(kinds, []uint8{1, 2, 3}) {
			t.Fatalf("afterCreateExactMiss kinds = %#v, want [1 2 3]", kinds)
		}
		mysqlRequireCreateExpectations(t, mock)
	})

	t.Run("lock_time_hit_does_not_report_miss", func(t *testing.T) {
		store, mock := newMySQLCreateTestStore(t, sha256.Sum256)
		winner := mysqlGetterMetaFixture()
		key := "hook-lock-hit"
		var kinds []uint8
		store.afterCreateExactMiss = func(kind uint8) { kinds = append(kinds, kind) }
		mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
		mock.ExpectBegin()
		mysqlExpectKeyLock(mock, sha256.Sum256, 1, key)
		mysqlExpectExactRows(t, mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key, winner, 1)
		mock.ExpectRollback()

		got, err := store.Create(context.Background(), portableMySQLMetaFixture(), key)
		if err != nil || got == nil {
			t.Fatalf("Create(hook lock hit) = (%#v, %v)", got, err)
		}
		if len(kinds) != 0 {
			t.Fatalf("afterCreateExactMiss kinds on exact hit = %#v, want none", kinds)
		}
		mysqlRequireCreateExpectations(t, mock)
	})

	for _, ending := range []string{"query_error", "corrupt_rows"} {
		t.Run("ref_"+ending+"_does_not_report_ref_miss", func(t *testing.T) {
			store, mock := newMySQLCreateTestStore(t, sha256.Sum256)
			meta := portableMySQLMetaFixture()
			key := "hook-ref-" + ending
			var kinds []uint8
			store.afterCreateExactMiss = func(kind uint8) { kinds = append(kinds, kind) }
			mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
			mock.ExpectBegin()
			mysqlExpectKeyLock(mock, sha256.Sum256, 1, key)
			mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
			mysqlExpectKeyLock(mock, sha256.Sum256, 2, meta.ArtifactRef)
			if ending == "query_error" {
				cause := errors.New("hook ref query failed")
				hash := mysqlTestDigest(sha256.Sum256, meta.ArtifactRef)
				mock.ExpectQuery(mysqlTestSelectQuery("artifact_ref_hash = ? AND artifact_ref = ?")).
					WithArgs(hash, []byte(meta.ArtifactRef)).WillReturnError(cause)
			} else {
				mysqlExpectExactRows(t, mock, sha256.Sum256, "artifact_ref_hash = ? AND artifact_ref = ?", meta.ArtifactRef, meta, 2)
			}
			mock.ExpectRollback()
			if ending == "query_error" {
				mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
			}

			_, _ = store.Create(context.Background(), meta, key)
			if !reflect.DeepEqual(kinds, []uint8{1}) {
				t.Fatalf("afterCreateExactMiss kinds on ref %s = %#v, want only prior idempotency miss", ending, kinds)
			}
			mysqlRequireCreateExpectations(t, mock)
		})
	}
}

func TestMySQLMetadataStoreCreateInfrastructureFailureRecoveryClassification(t *testing.T) {
	stages := []struct {
		name         string
		arrangeError func(*testing.T, sqlmock.Sqlmock, artifact.ArtifactMeta, string, error)
		commitStage  bool
	}{
		{
			name: "begin",
			arrangeError: func(_ *testing.T, mock sqlmock.Sqlmock, _ artifact.ArtifactMeta, _ string, cause error) {
				mock.ExpectBegin().WillReturnError(cause)
			},
		},
		{
			name: "lock_upsert",
			arrangeError: func(_ *testing.T, mock sqlmock.Sqlmock, _ artifact.ArtifactMeta, key string, cause error) {
				mock.ExpectBegin()
				digest := mysqlTestDigest(sha256.Sum256, key)
				mock.ExpectExec(mysqlTestKeyLockUpsert).WithArgs(int64(1), digest).WillReturnError(cause)
				mock.ExpectRollback()
			},
		},
		{
			name: "lock_select",
			arrangeError: func(_ *testing.T, mock sqlmock.Sqlmock, _ artifact.ArtifactMeta, key string, cause error) {
				mock.ExpectBegin()
				digest := mysqlTestDigest(sha256.Sum256, key)
				mock.ExpectExec(mysqlTestKeyLockUpsert).WithArgs(int64(1), digest).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery(mysqlTestKeyLockSelect).WithArgs(int64(1), digest).WillReturnError(cause)
				mock.ExpectRollback()
			},
		},
		{
			name: "transaction_exact_query",
			arrangeError: func(_ *testing.T, mock sqlmock.Sqlmock, _ artifact.ArtifactMeta, key string, cause error) {
				mock.ExpectBegin()
				mysqlExpectKeyLock(mock, sha256.Sum256, 1, key)
				digest := mysqlTestDigest(sha256.Sum256, key)
				mock.ExpectQuery(mysqlTestSelectQuery("idempotency_hash = ? AND idempotency_key = ?")).
					WithArgs(digest, []byte(key)).WillReturnError(cause)
				mock.ExpectRollback()
			},
		},
		{
			name: "metadata_insert",
			arrangeError: func(t *testing.T, mock sqlmock.Sqlmock, meta artifact.ArtifactMeta, key string, cause error) {
				mock.ExpectBegin()
				mysqlExpectAllLocksAndMisses(mock, sha256.Sum256, meta, key)
				args := mysqlTestCreateArgs(t, meta, key, sha256.Sum256)
				mock.ExpectExec(mysqlTestCreateInsert).WithArgs(args...).WillReturnError(cause)
				mock.ExpectRollback()
			},
		},
		{
			name:        "commit",
			commitStage: true,
			arrangeError: func(t *testing.T, mock sqlmock.Sqlmock, meta artifact.ArtifactMeta, key string, cause error) {
				mock.ExpectBegin()
				mysqlExpectAllLocksAndMisses(mock, sha256.Sum256, meta, key)
				args := mysqlTestCreateArgs(t, meta, key, sha256.Sum256)
				mock.ExpectExec(mysqlTestCreateInsert).WithArgs(args...).WillReturnResult(sqlmock.NewResult(1, 1))
				mock.ExpectCommit().WillReturnError(cause)
			},
		},
	}

	for _, stage := range stages {
		stage := stage
		t.Run(stage.name+"_winner_recovers", func(t *testing.T) {
			store, mock := newMySQLCreateTestStore(t, sha256.Sum256)
			meta := portableMySQLMetaFixture()
			winner := mysqlGetterMetaFixture()
			key := "recover-" + stage.name
			cause := errors.New(stage.name + " infrastructure failed")
			mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
			stage.arrangeError(t, mock, meta, key, cause)
			mysqlExpectExactRows(t, mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key, winner, 1)

			got, err := store.Create(context.Background(), meta, key)
			if err != nil || !reflect.DeepEqual(got, &winner) {
				t.Fatalf("Create(%s recovery winner) = (%#v, %v), want winner %#v", stage.name, got, err, &winner)
			}
			mysqlRequireCreateExpectations(t, mock)
		})

		t.Run(stage.name+"_miss_preserves_primary", func(t *testing.T) {
			store, mock := newMySQLCreateTestStore(t, sha256.Sum256)
			meta := portableMySQLMetaFixture()
			key := "recover-miss-" + stage.name
			cause := errors.New(stage.name + " primary infrastructure failed")
			mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
			stage.arrangeError(t, mock, meta, key, cause)
			mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)

			got, err := store.Create(context.Background(), meta, key)
			if got != nil || !errors.Is(err, cause) {
				t.Fatalf("Create(%s recovery miss) = (%#v, %v), want primary cause", stage.name, got, err)
			}
			mysqlRequireCreateExpectations(t, mock)
		})
	}
}

func TestMySQLMetadataStoreCreateRefAndIDStageFailuresRollbackBeforeRecovery(t *testing.T) {
	stages := []struct {
		name    string
		arrange func(*testing.T, sqlmock.Sqlmock, artifact.ArtifactMeta, string, error)
	}{
		{
			name: "ref_lock_upsert",
			arrange: func(_ *testing.T, mock sqlmock.Sqlmock, meta artifact.ArtifactMeta, key string, cause error) {
				mysqlExpectKeyLock(mock, sha256.Sum256, 1, key)
				mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
				hash := mysqlTestDigest(sha256.Sum256, meta.ArtifactRef)
				mock.ExpectExec(mysqlTestKeyLockUpsert).WithArgs(int64(2), hash).WillReturnError(cause)
			},
		},
		{
			name: "ref_lock_select",
			arrange: func(_ *testing.T, mock sqlmock.Sqlmock, meta artifact.ArtifactMeta, key string, cause error) {
				mysqlExpectKeyLock(mock, sha256.Sum256, 1, key)
				mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
				hash := mysqlTestDigest(sha256.Sum256, meta.ArtifactRef)
				mock.ExpectExec(mysqlTestKeyLockUpsert).WithArgs(int64(2), hash).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery(mysqlTestKeyLockSelect).WithArgs(int64(2), hash).WillReturnError(cause)
			},
		},
		{
			name: "ref_exact_query",
			arrange: func(_ *testing.T, mock sqlmock.Sqlmock, meta artifact.ArtifactMeta, key string, cause error) {
				mysqlExpectKeyLock(mock, sha256.Sum256, 1, key)
				mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
				mysqlExpectKeyLock(mock, sha256.Sum256, 2, meta.ArtifactRef)
				hash := mysqlTestDigest(sha256.Sum256, meta.ArtifactRef)
				mock.ExpectQuery(mysqlTestSelectQuery("artifact_ref_hash = ? AND artifact_ref = ?")).
					WithArgs(hash, []byte(meta.ArtifactRef)).WillReturnError(cause)
			},
		},
		{
			name: "ref_exact_rows",
			arrange: func(t *testing.T, mock sqlmock.Sqlmock, meta artifact.ArtifactMeta, key string, cause error) {
				mysqlExpectKeyLock(mock, sha256.Sum256, 1, key)
				mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
				mysqlExpectKeyLock(mock, sha256.Sum256, 2, meta.ArtifactRef)
				hash := mysqlTestDigest(sha256.Sum256, meta.ArtifactRef)
				rows := mysqlTestRowsForMeta(t, meta, 1).RowError(0, cause)
				mock.ExpectQuery(mysqlTestSelectQuery("artifact_ref_hash = ? AND artifact_ref = ?")).
					WithArgs(hash, []byte(meta.ArtifactRef)).WillReturnRows(rows).RowsWillBeClosed()
			},
		},
		{
			name: "id_lock_upsert",
			arrange: func(_ *testing.T, mock sqlmock.Sqlmock, meta artifact.ArtifactMeta, key string, cause error) {
				mysqlExpectCreateThroughRefMiss(mock, sha256.Sum256, meta, key)
				hash := mysqlTestDigest(sha256.Sum256, meta.ArtifactID)
				mock.ExpectExec(mysqlTestKeyLockUpsert).WithArgs(int64(3), hash).WillReturnError(cause)
			},
		},
		{
			name: "id_lock_select",
			arrange: func(_ *testing.T, mock sqlmock.Sqlmock, meta artifact.ArtifactMeta, key string, cause error) {
				mysqlExpectCreateThroughRefMiss(mock, sha256.Sum256, meta, key)
				hash := mysqlTestDigest(sha256.Sum256, meta.ArtifactID)
				mock.ExpectExec(mysqlTestKeyLockUpsert).WithArgs(int64(3), hash).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery(mysqlTestKeyLockSelect).WithArgs(int64(3), hash).WillReturnError(cause)
			},
		},
		{
			name: "id_exact_query",
			arrange: func(_ *testing.T, mock sqlmock.Sqlmock, meta artifact.ArtifactMeta, key string, cause error) {
				mysqlExpectCreateThroughRefMiss(mock, sha256.Sum256, meta, key)
				mysqlExpectKeyLock(mock, sha256.Sum256, 3, meta.ArtifactID)
				hash := mysqlTestDigest(sha256.Sum256, meta.ArtifactID)
				mock.ExpectQuery(mysqlTestSelectQuery("artifact_id_hash = ? AND artifact_id = ?")).
					WithArgs(hash, []byte(meta.ArtifactID)).WillReturnError(cause)
			},
		},
		{
			name: "id_exact_rows",
			arrange: func(t *testing.T, mock sqlmock.Sqlmock, meta artifact.ArtifactMeta, key string, cause error) {
				mysqlExpectCreateThroughRefMiss(mock, sha256.Sum256, meta, key)
				mysqlExpectKeyLock(mock, sha256.Sum256, 3, meta.ArtifactID)
				hash := mysqlTestDigest(sha256.Sum256, meta.ArtifactID)
				rows := mysqlTestRowsForMeta(t, meta, 1).RowError(0, cause)
				mock.ExpectQuery(mysqlTestSelectQuery("artifact_id_hash = ? AND artifact_id = ?")).
					WithArgs(hash, []byte(meta.ArtifactID)).WillReturnRows(rows).RowsWillBeClosed()
			},
		},
	}

	for _, stage := range stages {
		stage := stage
		t.Run(stage.name, func(t *testing.T) {
			store, mock := newMySQLCreateTestStore(t, sha256.Sum256)
			meta := portableMySQLMetaFixture()
			key := "stage-failure-" + stage.name
			cause := errors.New(stage.name + " failed")
			mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
			mock.ExpectBegin()
			stage.arrange(t, mock, meta, key, cause)
			mock.ExpectRollback()
			mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)

			got, err := store.Create(context.Background(), meta, key)
			if got != nil || !errors.Is(err, cause) {
				t.Fatalf("Create(%s) = (%#v, %v), want stage cause", stage.name, got, err)
			}
			mysqlRequireCreateExpectations(t, mock)
		})
	}
}

func TestMySQLMetadataStoreCreateRecoveryQueryFailurePreservesPrimary(t *testing.T) {
	store, mock := newMySQLCreateTestStore(t, sha256.Sum256)
	meta := portableMySQLMetaFixture()
	key := "recovery-query-error"
	primary := errors.New("begin isolation rejected")
	recoveryCause := errors.New("recovery query failed")
	mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
	mock.ExpectBegin().WillReturnError(primary)
	digest := mysqlTestDigest(sha256.Sum256, key)
	mock.ExpectQuery(mysqlTestSelectQuery("idempotency_hash = ? AND idempotency_key = ?")).
		WithArgs(digest, []byte(key)).WillReturnError(recoveryCause)

	got, err := store.Create(context.Background(), meta, key)
	if got != nil || !errors.Is(err, primary) {
		t.Fatalf("Create(recovery query failure) = (%#v, %v), want original cause", got, err)
	}
	mysqlRequireCreateExpectations(t, mock)
}

func TestMySQLMetadataStoreCreateRollbackFailureJoinsPrimaryAndStopsRecovery(t *testing.T) {
	store, mock := newMySQLCreateTestStore(t, sha256.Sum256)
	meta := portableMySQLMetaFixture()
	key := "rollback-failure"
	primary := errors.New("metadata insert failed")
	rollbackCause := errors.New("rollback failed")
	mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
	mock.ExpectBegin()
	mysqlExpectAllLocksAndMisses(mock, sha256.Sum256, meta, key)
	args := mysqlTestCreateArgs(t, meta, key, sha256.Sum256)
	mock.ExpectExec(mysqlTestCreateInsert).WithArgs(args...).WillReturnError(primary)
	mock.ExpectRollback().WillReturnError(rollbackCause)

	got, err := store.Create(context.Background(), meta, key)
	if got != nil || !errors.Is(err, primary) || !errors.Is(err, rollbackCause) {
		t.Fatalf("Create(rollback failure) = (%#v, %v), want joined primary and rollback causes", got, err)
	}
	mysqlRequireCreateExpectations(t, mock)
}

func TestMySQLMetadataStoreCreateRollbackTxDoneStillAllowsRecovery(t *testing.T) {
	store, mock := newMySQLCreateTestStore(t, sha256.Sum256)
	meta := portableMySQLMetaFixture()
	winner := mysqlGetterMetaFixture()
	key := "rollback-tx-done"
	primary := errors.New("metadata insert failed")
	mysqlExpectExactMissing(mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key)
	mock.ExpectBegin()
	mysqlExpectAllLocksAndMisses(mock, sha256.Sum256, meta, key)
	args := mysqlTestCreateArgs(t, meta, key, sha256.Sum256)
	mock.ExpectExec(mysqlTestCreateInsert).WithArgs(args...).WillReturnError(primary)
	mock.ExpectRollback().WillReturnError(sql.ErrTxDone)
	mysqlExpectExactRows(t, mock, sha256.Sum256, "idempotency_hash = ? AND idempotency_key = ?", key, winner, 1)

	got, err := store.Create(context.Background(), meta, key)
	if err != nil || !reflect.DeepEqual(got, &winner) {
		t.Fatalf("Create(sql.ErrTxDone rollback) = (%#v, %v), want recovery winner", got, err)
	}
	mysqlRequireCreateExpectations(t, mock)
}

func TestMySQLMetadataStoreCreateEmptyIdempotencyCommitErrorDoesNotGuessByRefOrID(t *testing.T) {
	store, mock := newMySQLCreateTestStore(t, sha256.Sum256)
	meta := portableMySQLMetaFixture()
	commitCause := errors.New("ambiguous commit")
	mock.ExpectBegin()
	mysqlExpectAllLocksAndMisses(mock, sha256.Sum256, meta, "")
	args := mysqlTestCreateArgs(t, meta, "", sha256.Sum256)
	mock.ExpectExec(mysqlTestCreateInsert).WithArgs(args...).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit().WillReturnError(commitCause)

	got, err := store.Create(context.Background(), meta, "")
	if got != nil || !errors.Is(err, commitCause) {
		t.Fatalf("Create(empty idempotency ambiguous commit) = (%#v, %v), want original commit cause", got, err)
	}
	mysqlRequireCreateExpectations(t, mock)
}

func TestMySQLMetadataStoreCreateEmptyIdempotencyFailuresNeverRunRecoveryQueries(t *testing.T) {
	stages := []string{
		"begin",
		"ref_upsert",
		"ref_lock_select",
		"ref_exact_query",
		"id_upsert",
		"id_lock_select",
		"id_exact_query",
		"insert",
		"commit",
	}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			cause := errors.New("empty idempotency " + stage + " failed")
			connector := &mysqlCreateFailureConnector{stage: stage, cause: cause}
			db := sql.OpenDB(connector)
			t.Cleanup(func() { _ = db.Close() })
			store, err := NewMySQLMetadataStore(db)
			if err != nil {
				t.Fatalf("NewMySQLMetadataStore() error = %v", err)
			}

			got, err := store.Create(context.Background(), portableMySQLMetaFixture(), "")
			if got != nil || !errors.Is(err, cause) {
				t.Fatalf("Create(empty idempotency %s failure) = (%#v, %v), want stage cause", stage, got, err)
			}
			snapshot := connector.snapshot()
			if snapshot.externalQueries != 0 {
				t.Fatalf("Create(empty idempotency %s) external recovery queries = %d, want zero", stage, snapshot.externalQueries)
			}
			wantRollbacks := int64(1)
			if stage == "begin" || stage == "commit" {
				wantRollbacks = 0
			}
			if snapshot.rollbacks != wantRollbacks {
				t.Fatalf("Create(empty idempotency %s) rollbacks = %d, want %d", stage, snapshot.rollbacks, wantRollbacks)
			}
			if stage == "commit" && snapshot.commits != 1 {
				t.Fatalf("Create(empty idempotency commit) commit calls = %d, want 1", snapshot.commits)
			}
		})
	}
}

func TestMySQLMetadataStoreCreateRequestsReadCommittedIsolation(t *testing.T) {
	beginCause := errors.New("isolation observation stop")
	connector := &mysqlCreateIsolationConnector{beginCause: beginCause}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewMySQLMetadataStore(db)
	if err != nil {
		t.Fatalf("NewMySQLMetadataStore() error = %v", err)
	}

	got, err := store.Create(context.Background(), portableMySQLMetaFixture(), "")
	if got != nil || !errors.Is(err, beginCause) {
		t.Fatalf("Create(isolation observer) = (%#v, %v), want Begin cause", got, err)
	}
	options, fallbackBegins := connector.snapshot()
	if len(options) != 1 {
		t.Fatalf("BeginTx options count = %d, want 1", len(options))
	}
	if options[0].Isolation != driver.IsolationLevel(sql.LevelReadCommitted) || options[0].ReadOnly {
		t.Fatalf("BeginTx options = %#v, want LevelReadCommitted and read-write", options[0])
	}
	if fallbackBegins != 0 {
		t.Fatalf("fallback Begin calls = %d, want zero", fallbackBegins)
	}
}

func newMySQLCreateTestStore(t *testing.T, digest mysqlDigestFunc) (*MySQLMetadataStore, sqlmock.Sqlmock) {
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

func mysqlExpectAllLocksAndMisses(mock sqlmock.Sqlmock, digest mysqlDigestFunc, meta artifact.ArtifactMeta, key string) {
	if key != "" {
		mysqlExpectKeyLock(mock, digest, 1, key)
		mysqlExpectExactMissing(mock, digest, "idempotency_hash = ? AND idempotency_key = ?", key)
	}
	mysqlExpectKeyLock(mock, digest, 2, meta.ArtifactRef)
	mysqlExpectExactMissing(mock, digest, "artifact_ref_hash = ? AND artifact_ref = ?", meta.ArtifactRef)
	mysqlExpectKeyLock(mock, digest, 3, meta.ArtifactID)
	mysqlExpectExactMissing(mock, digest, "artifact_id_hash = ? AND artifact_id = ?", meta.ArtifactID)
}

func mysqlExpectCreateThroughRefMiss(mock sqlmock.Sqlmock, digest mysqlDigestFunc, meta artifact.ArtifactMeta, key string) {
	mysqlExpectKeyLock(mock, digest, 1, key)
	mysqlExpectExactMissing(mock, digest, "idempotency_hash = ? AND idempotency_key = ?", key)
	mysqlExpectKeyLock(mock, digest, 2, meta.ArtifactRef)
	mysqlExpectExactMissing(mock, digest, "artifact_ref_hash = ? AND artifact_ref = ?", meta.ArtifactRef)
}

func mysqlExpectKeyLock(mock sqlmock.Sqlmock, digest mysqlDigestFunc, kind int64, raw string) {
	hash := mysqlTestDigest(digest, raw)
	mock.ExpectExec(mysqlTestKeyLockUpsert).
		WithArgs(kind, hash).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(mysqlTestKeyLockSelect).
		WithArgs(kind, hash).
		WillReturnRows(sqlmock.NewRows([]string{"key_hash"}).AddRow(hash)).
		RowsWillBeClosed()
}

func mysqlExpectExactMissing(mock sqlmock.Sqlmock, digest mysqlDigestFunc, predicate, raw string) {
	hash := mysqlTestDigest(digest, raw)
	mock.ExpectQuery(mysqlTestSelectQuery(predicate)).
		WithArgs(hash, []byte(raw)).
		WillReturnRows(sqlmock.NewRows(mysqlTestSelectedColumnNames)).
		RowsWillBeClosed()
}

func mysqlExpectExactRows(
	t *testing.T,
	mock sqlmock.Sqlmock,
	digest mysqlDigestFunc,
	predicate string,
	raw string,
	meta artifact.ArtifactMeta,
	count int,
) {
	t.Helper()
	hash := mysqlTestDigest(digest, raw)
	mock.ExpectQuery(mysqlTestSelectQuery(predicate)).
		WithArgs(hash, []byte(raw)).
		WillReturnRows(mysqlTestRowsForMeta(t, meta, count)).
		RowsWillBeClosed()
}

func mysqlTestCreateArgs(t *testing.T, meta artifact.ArtifactMeta, key string, digest mysqlDigestFunc) []driver.Value {
	t.Helper()
	row, _, err := prepareMySQLArtifactRow(meta)
	if err != nil {
		t.Fatalf("prepareMySQLArtifactRow() create fixture error = %v", err)
	}
	var idempotencyRaw driver.Value
	var idempotencyHash driver.Value
	if key != "" {
		idempotencyRaw = []byte(key)
		idempotencyHash = mysqlTestDigest(digest, key)
	}
	return []driver.Value{
		row.artifactID,
		mysqlTestDigestBytes(digest, row.artifactID),
		row.artifactRef,
		mysqlTestDigestBytes(digest, row.artifactRef),
		idempotencyRaw,
		idempotencyHash,
		row.tenantID,
		mysqlTestDigestBytes(digest, row.tenantID),
		row.userID,
		row.sessionID,
		mysqlTestDigestBytes(digest, row.sessionID),
		row.runID,
		mysqlTestDigestBytes(digest, row.runID),
		row.stepID,
		row.ownerModule,
		mysqlTestDigestBytes(digest, row.ownerModule),
		row.ownerID,
		mysqlTestDigestBytes(digest, row.ownerID),
		row.artifactType,
		mysqlTestDigestBytes(digest, row.artifactType),
		row.mimeType,
		row.name,
		row.sizeBytes,
		row.artifactHash,
		row.visibility,
		mysqlTestDigestBytes(digest, row.visibility),
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

func mysqlMustPrepareImmediate(t *testing.T, meta artifact.ArtifactMeta) *artifact.ArtifactMeta {
	t.Helper()
	_, immediate, err := prepareMySQLArtifactRow(meta)
	if err != nil {
		t.Fatalf("prepareMySQLArtifactRow() error = %v", err)
	}
	return immediate
}

func mysqlTestDigest(digest mysqlDigestFunc, raw string) []byte {
	return mysqlTestDigestBytes(digest, []byte(raw))
}

func mysqlTestDigestBytes(digest mysqlDigestFunc, raw []byte) []byte {
	hash := digest(raw)
	return append([]byte(nil), hash[:]...)
}

func mysqlRequireCreateExpectations(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("Create SQL protocol mismatch: %v", err)
	}
}

type mysqlCreateIsolationConnector struct {
	mu             sync.Mutex
	options        []driver.TxOptions
	fallbackBegins int
	beginCause     error
}

func (c *mysqlCreateIsolationConnector) Connect(context.Context) (driver.Conn, error) {
	return &mysqlCreateIsolationConn{connector: c}, nil
}

func (c *mysqlCreateIsolationConnector) Driver() driver.Driver {
	return mysqlCreateIsolationDriver{connector: c}
}

func (c *mysqlCreateIsolationConnector) snapshot() ([]driver.TxOptions, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]driver.TxOptions(nil), c.options...), c.fallbackBegins
}

type mysqlCreateIsolationDriver struct {
	connector *mysqlCreateIsolationConnector
}

func (d mysqlCreateIsolationDriver) Open(string) (driver.Conn, error) {
	return &mysqlCreateIsolationConn{connector: d.connector}, nil
}

type mysqlCreateIsolationConn struct {
	connector *mysqlCreateIsolationConnector
}

func (c *mysqlCreateIsolationConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected Prepare in Create isolation test")
}

func (c *mysqlCreateIsolationConn) Close() error { return nil }

func (c *mysqlCreateIsolationConn) Begin() (driver.Tx, error) {
	c.connector.mu.Lock()
	defer c.connector.mu.Unlock()
	c.connector.fallbackBegins++
	return nil, c.connector.beginCause
}

func (c *mysqlCreateIsolationConn) BeginTx(_ context.Context, options driver.TxOptions) (driver.Tx, error) {
	c.connector.mu.Lock()
	defer c.connector.mu.Unlock()
	c.connector.options = append(c.connector.options, options)
	return nil, c.connector.beginCause
}

func (c *mysqlCreateIsolationConn) CheckNamedValue(*driver.NamedValue) error { return nil }

func (c *mysqlCreateIsolationConn) String() string {
	return fmt.Sprintf("mysqlCreateIsolationConn(%p)", c.connector)
}

type mysqlCreateFailureSnapshot struct {
	externalQueries int64
	begins          int64
	rollbacks       int64
	commits         int64
}

type mysqlCreateFailureConnector struct {
	stage               string
	cause               error
	winnerRow           []driver.Value
	externalWinner      bool
	transactionWinnerAt int
	rollbackCause       error

	externalQueries atomic.Int64
	begins          atomic.Int64
	rollbacks       atomic.Int64
	commits         atomic.Int64
}

func (c *mysqlCreateFailureConnector) Connect(context.Context) (driver.Conn, error) {
	return &mysqlCreateFailureConn{connector: c}, nil
}

func (c *mysqlCreateFailureConnector) Driver() driver.Driver {
	return mysqlCreateFailureDriver{connector: c}
}

func (c *mysqlCreateFailureConnector) snapshot() mysqlCreateFailureSnapshot {
	return mysqlCreateFailureSnapshot{
		externalQueries: c.externalQueries.Load(),
		begins:          c.begins.Load(),
		rollbacks:       c.rollbacks.Load(),
		commits:         c.commits.Load(),
	}
}

type mysqlCreateFailureDriver struct {
	connector *mysqlCreateFailureConnector
}

func (d mysqlCreateFailureDriver) Open(string) (driver.Conn, error) {
	return &mysqlCreateFailureConn{connector: d.connector}, nil
}

type mysqlCreateFailureConn struct {
	connector       *mysqlCreateFailureConnector
	inTx            bool
	upsertCalls     int
	lockQueries     int
	metadataQueries int
}

func (c *mysqlCreateFailureConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected Prepare in Create failure counter")
}

func (c *mysqlCreateFailureConn) Close() error { return nil }

func (c *mysqlCreateFailureConn) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected default-isolation Begin")
}

func (c *mysqlCreateFailureConn) BeginTx(_ context.Context, options driver.TxOptions) (driver.Tx, error) {
	if options.Isolation != driver.IsolationLevel(sql.LevelReadCommitted) {
		return nil, fmt.Errorf("unexpected isolation %d", options.Isolation)
	}
	if c.connector.stage == "begin" {
		return nil, c.connector.cause
	}
	c.connector.begins.Add(1)
	c.inTx = true
	return &mysqlCreateFailureTx{conn: c}, nil
}

func (c *mysqlCreateFailureConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if query == mysqlTestKeyLockUpsert {
		c.upsertCalls++
		if (c.upsertCalls == 1 && c.connector.stage == "ref_upsert") ||
			(c.upsertCalls == 2 && c.connector.stage == "id_upsert") {
			return nil, c.connector.cause
		}
		return driver.RowsAffected(1), nil
	}
	if query == mysqlTestCreateInsert {
		if c.connector.stage == "insert" {
			return nil, c.connector.cause
		}
		return driver.RowsAffected(1), nil
	}
	return nil, fmt.Errorf("unexpected Create ExecContext query %q", query)
}

func (c *mysqlCreateFailureConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if !c.inTx {
		c.connector.externalQueries.Add(1)
	}
	if query == mysqlTestKeyLockSelect {
		c.lockQueries++
		if (c.lockQueries == 1 && c.connector.stage == "ref_lock_select") ||
			(c.lockQueries == 2 && c.connector.stage == "id_lock_select") {
			return nil, c.connector.cause
		}
		var hash driver.Value = []byte(nil)
		if len(args) > 1 {
			hash = args[1].Value
		}
		return &mysqlCreateDriverRows{columns: []string{"key_hash"}, rows: [][]driver.Value{{hash}}}, nil
	}
	if strings.HasPrefix(query, "SELECT "+mysqlTestSelectColumns+" FROM artifact_metadata WHERE ") {
		c.metadataQueries++
		if (c.metadataQueries == 1 && c.connector.stage == "ref_exact_query") ||
			(c.metadataQueries == 2 && c.connector.stage == "id_exact_query") {
			return nil, c.connector.cause
		}
		rows := &mysqlCreateDriverRows{columns: append([]string(nil), mysqlTestSelectedColumnNames...)}
		if ((!c.inTx && c.connector.externalWinner) || (c.connector.transactionWinnerAt > 0 && c.metadataQueries == c.connector.transactionWinnerAt)) && c.connector.winnerRow != nil {
			rows.rows = [][]driver.Value{append([]driver.Value(nil), c.connector.winnerRow...)}
		}
		return rows, nil
	}
	return nil, fmt.Errorf("unexpected Create QueryContext query %q", query)
}

func (c *mysqlCreateFailureConn) CheckNamedValue(*driver.NamedValue) error { return nil }

type mysqlCreateFailureTx struct {
	conn *mysqlCreateFailureConn
}

func (t *mysqlCreateFailureTx) Commit() error {
	t.conn.inTx = false
	t.conn.connector.commits.Add(1)
	if t.conn.connector.stage == "commit" {
		return t.conn.connector.cause
	}
	return nil
}

func (t *mysqlCreateFailureTx) Rollback() error {
	t.conn.inTx = false
	t.conn.connector.rollbacks.Add(1)
	return t.conn.connector.rollbackCause
}

type mysqlCreateDriverRows struct {
	columns []string
	rows    [][]driver.Value
	next    int
	err     error
}

func (r *mysqlCreateDriverRows) Columns() []string { return r.columns }

func (r *mysqlCreateDriverRows) Close() error { return nil }

func (r *mysqlCreateDriverRows) Next(destination []driver.Value) error {
	if r.err != nil {
		err := r.err
		r.err = nil
		return err
	}
	if r.next >= len(r.rows) {
		return io.EOF
	}
	copy(destination, r.rows[r.next])
	r.next++
	return nil
}
