package metastore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

const mysqlTestPurgeJobColumns = "status, attempts, last_error, created_at_sec, created_at_nano, updated_at_sec, updated_at_nano, lease_owner, lease_until_sec, lease_until_nano, lease_version"

const mysqlTestPurgeJobLockSelect = "SELECT " + mysqlTestPurgeJobColumns + " FROM artifact_purge_job WHERE metadata_id = ? FOR UPDATE"

const mysqlTestPurgeJobInsert = "INSERT INTO artifact_purge_job (metadata_id, status, attempts, last_error, created_at_sec, created_at_nano, updated_at_sec, updated_at_nano, lease_owner, lease_until_sec, lease_until_nano, lease_version) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"

const mysqlTestPurgeMetadataPendingUpdate = "UPDATE artifact_metadata SET status = ?, deleted_at_sec = ?, deleted_at_nano = ?, delete_reason = ?, purge_status = ?, purged_at_sec = NULL, purged_at_nano = NULL WHERE id = ?"

const mysqlTestPurgeClaimJobUpdate = "UPDATE artifact_purge_job SET status = ?, updated_at_sec = ?, updated_at_nano = ?, lease_owner = ?, lease_until_sec = ?, lease_until_nano = ?, lease_version = ? WHERE metadata_id = ?"

const mysqlTestPurgeMetadataStatusUpdate = "UPDATE artifact_metadata SET purge_status = ?, purged_at_sec = NULL, purged_at_nano = NULL WHERE id = ?"

const mysqlTestPurgeSuccessJobUpdate = "UPDATE artifact_purge_job SET status = ?, last_error = ?, updated_at_sec = ?, updated_at_nano = ?, lease_owner = ?, lease_until_sec = NULL, lease_until_nano = NULL WHERE metadata_id = ? AND status = ? AND lease_owner = ? AND lease_version = ?"

const mysqlTestPurgeSuccessMetadataUpdate = "UPDATE artifact_metadata SET purge_status = ?, purged_at_sec = ?, purged_at_nano = ? WHERE id = ?"

const mysqlTestPurgeFailureJobUpdate = "UPDATE artifact_purge_job SET status = ?, attempts = ?, last_error = ?, updated_at_sec = ?, updated_at_nano = ?, lease_owner = ?, lease_until_sec = NULL, lease_until_nano = NULL WHERE metadata_id = ? AND status = ? AND lease_owner = ? AND lease_version = ?"

var _ artifact.PurgeStore = (*MySQLMetadataStore)(nil)
var _ artifact.ProductionDependency = (*MySQLMetadataStore)(nil)

func TestMySQLMetadataStoreRequestPurgeDeclaresProductionReadiness(t *testing.T) {
	var dependency artifact.ProductionDependency = (*MySQLMetadataStore)(nil)
	if !dependency.ProductionReady() {
		t.Fatal("MySQLMetadataStore.ProductionReady() = false, want true")
	}
}

func TestMySQLMetadataStoreRequestPurgeCreatesJobAndTombstoneAtomically(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	meta := mysqlPurgeMetaFixture("request-new")
	at := time.Unix(1_721_100_000, 456).UTC()
	const recordID = uint64(401)
	digest := sha256.Sum256([]byte(meta.ArtifactRef))

	mock.ExpectBegin()
	mock.ExpectQuery(mysqlMetadataMarkDeletedSelect).
		WithArgs(digest[:], []byte(meta.ArtifactRef)).
		WillReturnRows(mysqlTestDeleteRows(t, recordID, meta))
	mock.ExpectQuery(mysqlTestPurgeJobLockSelect).
		WithArgs(int64(recordID)).
		WillReturnRows(mysqlTestPurgeJobRows())
	mock.ExpectExec(mysqlTestPurgeJobInsert).
		WithArgs(int64(recordID), []byte(artifact.PurgeStatusPending), int64(0), []byte{}, at.Unix(), int64(at.Nanosecond()), at.Unix(), int64(at.Nanosecond()), []byte{}, nil, nil, int64(0)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(mysqlTestPurgeMetadataPendingUpdate).
		WithArgs([]byte(artifact.ArtifactStatusDeleted), at.Unix(), int64(at.Nanosecond()), []byte(artifact.DeleteReasonUser), []byte(artifact.PurgeStatusPending), int64(recordID)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	gotMeta, gotJob, err := store.RequestPurge(context.Background(), meta.ArtifactRef, artifact.DeleteReasonUser, at)
	if err != nil {
		t.Fatalf("RequestPurge() error = %v", err)
	}
	wantMeta := meta
	wantMeta.Status = artifact.ArtifactStatusDeleted
	wantMeta.DeletedAt = at
	wantMeta.DeleteReason = artifact.DeleteReasonUser
	wantMeta.PurgeStatus = artifact.PurgeStatusPending
	wantJob := artifact.PurgeJob{
		ArtifactRef: meta.ArtifactRef, TenantID: meta.TenantID, SessionID: meta.SessionID,
		RunID: meta.RunID, StorageKey: meta.StorageKey, Status: artifact.PurgeStatusPending,
		CreatedAt: at, UpdatedAt: at,
	}
	if !reflect.DeepEqual(gotMeta, &wantMeta) || !reflect.DeepEqual(gotJob, &wantJob) {
		t.Fatalf("RequestPurge() = (%#v, %#v), want (%#v, %#v)", gotMeta, gotJob, &wantMeta, &wantJob)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreRequestPurgePreservesExistingValidJobAndFirstTombstone(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	firstAt := time.Unix(1_721_100_000, 456).UTC()
	requestAt := firstAt.Add(time.Hour)
	meta := mysqlPurgeMetaFixture("request-existing")
	meta.Status = artifact.ArtifactStatusDeleted
	meta.DeletedAt = firstAt
	meta.DeleteReason = artifact.DeleteReasonTTL
	meta.PurgeStatus = artifact.PurgeStatusRetrying
	const recordID = uint64(402)
	row := mysqlPurgeJobRow{
		status: []byte(artifact.PurgeStatusRetrying), attempts: 3, lastError: []byte("object store timeout"),
		createdAt: encodeRequiredMySQLTime(firstAt), updatedAt: encodeRequiredMySQLTime(firstAt.Add(time.Minute)),
		leaseOwner: []byte{}, leaseVersion: 4,
	}
	digest := sha256.Sum256([]byte(meta.ArtifactRef))

	mock.ExpectBegin()
	mock.ExpectQuery(mysqlMetadataMarkDeletedSelect).
		WithArgs(digest[:], []byte(meta.ArtifactRef)).
		WillReturnRows(mysqlTestDeleteRows(t, recordID, meta))
	mock.ExpectQuery(mysqlTestPurgeJobLockSelect).
		WithArgs(int64(recordID)).
		WillReturnRows(mysqlTestPurgeJobRows(row))
	mock.ExpectCommit()

	gotMeta, gotJob, err := store.RequestPurge(context.Background(), meta.ArtifactRef, artifact.DeleteReasonUser, requestAt)
	if err != nil {
		t.Fatalf("RequestPurge(existing) error = %v", err)
	}
	wantJob, decodeErr := decodeMySQLPurgeJob(&meta, row)
	if decodeErr != nil {
		t.Fatalf("decode expected existing job: %v", decodeErr)
	}
	if !reflect.DeepEqual(gotMeta, &meta) || !reflect.DeepEqual(gotJob, wantJob) {
		t.Fatalf("RequestPurge(existing) = (%#v, %#v), want unchanged (%#v, %#v)", gotMeta, gotJob, &meta, wantJob)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreRequestPurgeRepairsDeletedTombstoneWithoutJob(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	requestAt := time.Unix(1_721_100_000, 456).UTC()
	meta := mysqlPurgeMetaFixture("request-repair-zero")
	meta.Status = artifact.ArtifactStatusDeleted
	meta.DeleteReason = artifact.DeleteReasonCleanup
	meta.PurgeStatus = artifact.PurgeStatusPending
	const recordID = uint64(403)
	digest := sha256.Sum256([]byte(meta.ArtifactRef))

	mock.ExpectBegin()
	mock.ExpectQuery(mysqlMetadataMarkDeletedSelect).
		WithArgs(digest[:], []byte(meta.ArtifactRef)).
		WillReturnRows(mysqlTestDeleteRows(t, recordID, meta))
	mock.ExpectQuery(mysqlTestPurgeJobLockSelect).
		WithArgs(int64(recordID)).
		WillReturnRows(mysqlTestPurgeJobRows())
	mock.ExpectExec(mysqlTestPurgeJobInsert).
		WithArgs(int64(recordID), []byte(artifact.PurgeStatusPending), int64(0), []byte{}, requestAt.Unix(), int64(requestAt.Nanosecond()), requestAt.Unix(), int64(requestAt.Nanosecond()), []byte{}, nil, nil, int64(0)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	gotMeta, gotJob, err := store.RequestPurge(context.Background(), meta.ArtifactRef, artifact.DeleteReasonUser, requestAt)
	if err != nil {
		t.Fatalf("RequestPurge(repair zero deletion clock) error = %v", err)
	}
	if !reflect.DeepEqual(gotMeta, &meta) || gotJob == nil || gotJob.CreatedAt != requestAt || gotJob.Status != artifact.PurgeStatusPending {
		t.Fatalf("RequestPurge(repair zero deletion clock) = (%#v, %#v), want unchanged tombstone and pending repair job", gotMeta, gotJob)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreRequestPurgeRepairsLegacyTombstoneStatus(t *testing.T) {
	for _, legacyStatus := range []artifact.PurgeStatus{"", artifact.PurgeStatusNone} {
		t.Run(string(legacyStatus), func(t *testing.T) {
			store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
			deletedAt := time.Unix(1_721_050_000, 321).UTC()
			requestAt := deletedAt.Add(time.Hour)
			meta := mysqlPurgeMetaFixture("request-legacy-" + string(legacyStatus))
			meta.Status = artifact.ArtifactStatusDeleted
			meta.DeletedAt = deletedAt
			meta.DeleteReason = artifact.DeleteReasonTTL
			meta.PurgeStatus = legacyStatus
			const recordID = uint64(405)
			digest := sha256.Sum256([]byte(meta.ArtifactRef))

			mock.ExpectBegin()
			mock.ExpectQuery(mysqlMetadataMarkDeletedSelect).
				WithArgs(digest[:], []byte(meta.ArtifactRef)).
				WillReturnRows(mysqlTestDeleteRows(t, recordID, meta))
			mock.ExpectQuery(mysqlTestPurgeJobLockSelect).
				WithArgs(int64(recordID)).
				WillReturnRows(mysqlTestPurgeJobRows())
			mock.ExpectExec(mysqlTestPurgeJobInsert).
				WithArgs(int64(recordID), []byte(artifact.PurgeStatusPending), int64(0), []byte{}, requestAt.Unix(), int64(requestAt.Nanosecond()), requestAt.Unix(), int64(requestAt.Nanosecond()), []byte{}, nil, nil, int64(0)).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(mysqlTestPurgeMetadataPendingUpdate).
				WithArgs([]byte(artifact.ArtifactStatusDeleted), deletedAt.Unix(), int64(deletedAt.Nanosecond()), []byte(artifact.DeleteReasonTTL), []byte(artifact.PurgeStatusPending), int64(recordID)).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()

			gotMeta, gotJob, err := store.RequestPurge(context.Background(), meta.ArtifactRef, artifact.DeleteReasonUser, requestAt)
			if err != nil {
				t.Fatalf("RequestPurge(legacy %q) error = %v", legacyStatus, err)
			}
			wantMeta := meta
			wantMeta.PurgeStatus = artifact.PurgeStatusPending
			if !reflect.DeepEqual(gotMeta, &wantMeta) || gotJob == nil || gotJob.Status != artifact.PurgeStatusPending {
				t.Fatalf("RequestPurge(legacy %q) = (%#v, %#v), want repaired metadata %#v", legacyStatus, gotMeta, gotJob, &wantMeta)
			}
			mysqlRequirePurgeExpectations(t, mock)
		})
	}
}

func TestMySQLMetadataStoreRequestPurgeRejectsInconsistentMetadataAndJob(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	at := time.Unix(1_721_100_000, 456).UTC()
	meta := mysqlPurgeMetaFixture("request-invalid")
	meta.Status = artifact.ArtifactStatusDeleted
	meta.DeletedAt = at
	meta.DeleteReason = artifact.DeleteReasonUser
	meta.PurgeStatus = artifact.PurgeStatusPending
	const recordID = uint64(404)
	row := mysqlPurgeJobRow{
		status: []byte(artifact.PurgeStatusRetrying), attempts: 1, lastError: []byte("failed"),
		createdAt: encodeRequiredMySQLTime(at), updatedAt: encodeRequiredMySQLTime(at), leaseOwner: []byte{},
	}
	digest := sha256.Sum256([]byte(meta.ArtifactRef))

	mock.ExpectBegin()
	mock.ExpectQuery(mysqlMetadataMarkDeletedSelect).
		WithArgs(digest[:], []byte(meta.ArtifactRef)).
		WillReturnRows(mysqlTestDeleteRows(t, recordID, meta))
	mock.ExpectQuery(mysqlTestPurgeJobLockSelect).
		WithArgs(int64(recordID)).
		WillReturnRows(mysqlTestPurgeJobRows(row))
	mock.ExpectRollback()

	gotMeta, gotJob, err := store.RequestPurge(context.Background(), meta.ArtifactRef, artifact.DeleteReasonUser, at.Add(time.Hour))
	if gotMeta != nil || gotJob != nil || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
		t.Fatalf("RequestPurge(inconsistent) = (%#v, %#v, %v), want typed invalid persisted state", gotMeta, gotJob, err)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreRequestPurgeRejectsInvalidLiveMetadataPurgeState(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	at := time.Unix(1_721_100_000, 456).UTC()
	meta := mysqlPurgeMetaFixture("request-invalid-live")
	meta.PurgeStatus = artifact.PurgeStatusRetrying
	const recordID = uint64(406)
	digest := sha256.Sum256([]byte(meta.ArtifactRef))

	mock.ExpectBegin()
	mock.ExpectQuery(mysqlMetadataMarkDeletedSelect).
		WithArgs(digest[:], []byte(meta.ArtifactRef)).
		WillReturnRows(mysqlTestDeleteRows(t, recordID, meta))
	mock.ExpectQuery(mysqlTestPurgeJobLockSelect).
		WithArgs(int64(recordID)).
		WillReturnRows(mysqlTestPurgeJobRows())
	mock.ExpectRollback()

	gotMeta, gotJob, err := store.RequestPurge(context.Background(), meta.ArtifactRef, artifact.DeleteReasonUser, at)
	if gotMeta != nil || gotJob != nil || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
		t.Fatalf("RequestPurge(invalid live purge state) = (%#v, %#v, %v), want typed invalid persisted state", gotMeta, gotJob, err)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreRequestPurgePreservesPurgeJobQueryFailureClassification(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	meta := mysqlPurgeMetaFixture("request-query-error")
	const recordID = uint64(407)
	digest := sha256.Sum256([]byte(meta.ArtifactRef))
	cause := errors.New("purge job query transport failed")

	mock.ExpectBegin()
	mock.ExpectQuery(mysqlMetadataMarkDeletedSelect).
		WithArgs(digest[:], []byte(meta.ArtifactRef)).
		WillReturnRows(mysqlTestDeleteRows(t, recordID, meta))
	mock.ExpectQuery(mysqlTestPurgeJobLockSelect).
		WithArgs(int64(recordID)).
		WillReturnError(cause)
	mock.ExpectRollback()

	gotMeta, gotJob, err := store.RequestPurge(context.Background(), meta.ArtifactRef, artifact.DeleteReasonUser, time.Now())
	if gotMeta != nil || gotJob != nil || !errors.Is(err, cause) || artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
		t.Fatalf("RequestPurge(query failure) = (%#v, %#v, %v), want untyped infrastructure error preserving cause", gotMeta, gotJob, err)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreRequestPurgeRejectsInvalidInputsBeforeSQL(t *testing.T) {
	validAt := time.Unix(1_721_100_000, 456).UTC()
	for _, test := range []struct {
		name   string
		reason artifact.DeleteReason
		at     time.Time
	}{
		{name: "empty reason", at: validAt},
		{name: "unknown reason", reason: artifact.DeleteReason("unknown"), at: validAt},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
			gotMeta, gotJob, err := store.RequestPurge(context.Background(), "purge-ref-invalid-request", test.reason, test.at)
			if gotMeta != nil || gotJob != nil || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
				t.Fatalf("RequestPurge(invalid input) = (%#v, %#v, %v), want typed invalid without SQL", gotMeta, gotJob, err)
			}
			mysqlRequirePurgeExpectations(t, mock)
		})
	}
}

func TestMySQLMetadataStoreGetPurgeJobRejectsInvalidPersistedState(t *testing.T) {
	createdAt := time.Unix(1_721_000_000, 123).UTC()
	purgedAt := createdAt.Add(time.Minute)
	validMeta := mysqlPurgeMetaFixture("codec")
	validMeta.Status = artifact.ArtifactStatusDeleted
	validMeta.DeletedAt = createdAt
	validMeta.DeleteReason = artifact.DeleteReasonUser
	validMeta.PurgeStatus = artifact.PurgeStatusPending
	validRow := mysqlPurgeJobRow{
		status:       []byte(artifact.PurgeStatusPending),
		attempts:     0,
		lastError:    []byte{},
		createdAt:    encodeRequiredMySQLTime(createdAt),
		updatedAt:    encodeRequiredMySQLTime(createdAt),
		leaseOwner:   []byte{},
		leaseVersion: 0,
	}

	tests := []struct {
		name   string
		mutate func(*artifact.ArtifactMeta, *mysqlPurgeJobRow)
	}{
		{name: "mismatched lease time", mutate: func(_ *artifact.ArtifactMeta, row *mysqlPurgeJobRow) {
			row.leaseUntil.Seconds = sql.NullInt64{Int64: createdAt.Unix(), Valid: true}
		}},
		{name: "noncanonical status", mutate: func(_ *artifact.ArtifactMeta, row *mysqlPurgeJobRow) {
			row.status = []byte("PENDING")
		}},
		{name: "none status", mutate: func(_ *artifact.ArtifactMeta, row *mysqlPurgeJobRow) {
			row.status = []byte(artifact.PurgeStatusNone)
		}},
		{name: "attempts overflow int", mutate: func(_ *artifact.ArtifactMeta, row *mysqlPurgeJobRow) {
			row.attempts = uint64(maxIntForMySQLPurgeTest()) + 1
		}},
		{name: "negative lease version", mutate: func(_ *artifact.ArtifactMeta, row *mysqlPurgeJobRow) {
			row.leaseVersion = -1
		}},
		{name: "pending with attempts", mutate: func(_ *artifact.ArtifactMeta, row *mysqlPurgeJobRow) {
			row.attempts = 1
		}},
		{name: "pending with error", mutate: func(_ *artifact.ArtifactMeta, row *mysqlPurgeJobRow) {
			row.lastError = []byte("impossible")
		}},
		{name: "pending with lease version", mutate: func(_ *artifact.ArtifactMeta, row *mysqlPurgeJobRow) {
			row.leaseVersion = 1
		}},
		{name: "leased without owner", mutate: func(meta *artifact.ArtifactMeta, row *mysqlPurgeJobRow) {
			meta.PurgeStatus = artifact.PurgeStatusLeased
			row.status = []byte(artifact.PurgeStatusLeased)
			row.leaseUntil = encodeRequiredMySQLTime(createdAt.Add(time.Minute))
		}},
		{name: "leased without deadline", mutate: func(meta *artifact.ArtifactMeta, row *mysqlPurgeJobRow) {
			meta.PurgeStatus = artifact.PurgeStatusLeased
			row.status = []byte(artifact.PurgeStatusLeased)
			row.leaseOwner = []byte("worker")
		}},
		{name: "leased without version", mutate: func(meta *artifact.ArtifactMeta, row *mysqlPurgeJobRow) {
			meta.PurgeStatus = artifact.PurgeStatusLeased
			row.status = []byte(artifact.PurgeStatusLeased)
			row.leaseOwner = []byte("worker")
			row.leaseUntil = encodeRequiredMySQLTime(createdAt.Add(time.Minute))
		}},
		{name: "retrying without attempts", mutate: func(meta *artifact.ArtifactMeta, row *mysqlPurgeJobRow) {
			meta.PurgeStatus = artifact.PurgeStatusRetrying
			row.status = []byte(artifact.PurgeStatusRetrying)
			row.leaseVersion = 1
		}},
		{name: "retrying without version", mutate: func(meta *artifact.ArtifactMeta, row *mysqlPurgeJobRow) {
			meta.PurgeStatus = artifact.PurgeStatusRetrying
			row.status = []byte(artifact.PurgeStatusRetrying)
			row.attempts = 1
		}},
		{name: "pending with lease", mutate: func(_ *artifact.ArtifactMeta, row *mysqlPurgeJobRow) {
			row.leaseOwner = []byte("worker")
			row.leaseUntil = encodeRequiredMySQLTime(createdAt.Add(time.Minute))
		}},
		{name: "purged without completion", mutate: func(meta *artifact.ArtifactMeta, row *mysqlPurgeJobRow) {
			meta.PurgeStatus = artifact.PurgeStatusPurged
			row.status = []byte(artifact.PurgeStatusPurged)
		}},
		{name: "purged without version", mutate: func(meta *artifact.ArtifactMeta, row *mysqlPurgeJobRow) {
			meta.PurgeStatus = artifact.PurgeStatusPurged
			meta.PurgedAt = purgedAt
			row.status = []byte(artifact.PurgeStatusPurged)
		}},
		{name: "non-purged with completion", mutate: func(meta *artifact.ArtifactMeta, _ *mysqlPurgeJobRow) {
			meta.PurgedAt = purgedAt
		}},
		{name: "metadata and job status mismatch", mutate: func(meta *artifact.ArtifactMeta, _ *mysqlPurgeJobRow) {
			meta.PurgeStatus = artifact.PurgeStatusRetrying
		}},
		{name: "metadata is not deleted", mutate: func(meta *artifact.ArtifactMeta, _ *mysqlPurgeJobRow) {
			meta.Status = artifact.ArtifactStatusReady
			meta.DeletedAt = time.Time{}
			meta.DeleteReason = ""
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			meta := validMeta
			row := validRow
			test.mutate(&meta, &row)
			got, err := decodeMySQLPurgeJob(&meta, row)
			if got != nil || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
				t.Fatalf("decodeMySQLPurgeJob() = (%#v, %v), want typed invalid persisted state", got, err)
			}
		})
	}
}

func TestMySQLMetadataStoreGetPurgeJobDecodesCanonicalStates(t *testing.T) {
	createdAt := time.Unix(1_721_000_000, 123).UTC()
	updatedAt := createdAt.Add(time.Second)
	purgedAt := updatedAt.Add(time.Second)
	tests := []struct {
		status     artifact.PurgeStatus
		attempts   uint64
		lastError  string
		version    int64
		owner      string
		leaseUntil time.Time
		purgedAt   time.Time
	}{
		{status: artifact.PurgeStatusPending},
		{status: artifact.PurgeStatusRetrying, attempts: 2, lastError: "last error", version: 7},
		{status: artifact.PurgeStatusLeased, version: 1, owner: "worker", leaseUntil: updatedAt.Add(time.Minute)},
		{status: artifact.PurgeStatusPurged, attempts: 2, version: 7, purgedAt: purgedAt},
	}
	for _, test := range tests {
		t.Run(string(test.status), func(t *testing.T) {
			meta := mysqlPurgeMetaFixture(string(test.status))
			meta.Status = artifact.ArtifactStatusDeleted
			meta.DeletedAt = createdAt
			meta.DeleteReason = artifact.DeleteReasonUser
			meta.PurgeStatus = test.status
			meta.PurgedAt = test.purgedAt
			row := mysqlPurgeJobRow{
				status:       []byte(test.status),
				attempts:     test.attempts,
				lastError:    []byte(test.lastError),
				createdAt:    encodeRequiredMySQLTime(createdAt),
				updatedAt:    encodeRequiredMySQLTime(updatedAt),
				leaseOwner:   []byte(test.owner),
				leaseUntil:   encodeNullableMySQLTime(test.leaseUntil),
				leaseVersion: test.version,
			}
			got, err := decodeMySQLPurgeJob(&meta, row)
			if err != nil {
				t.Fatalf("decodeMySQLPurgeJob() error = %v", err)
			}
			want := &artifact.PurgeJob{
				ArtifactRef: meta.ArtifactRef, TenantID: meta.TenantID, SessionID: meta.SessionID,
				RunID: meta.RunID, StorageKey: meta.StorageKey, Status: test.status,
				Attempts: int(test.attempts), LastError: test.lastError, CreatedAt: createdAt, UpdatedAt: updatedAt,
				LeaseOwner: test.owner, LeaseUntil: test.leaseUntil, LeaseVersion: test.version,
			}
			if *got != *want {
				t.Fatalf("decodeMySQLPurgeJob() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestMySQLMetadataStoreGetPurgeJobAllowsZeroDeletionClock(t *testing.T) {
	jobTime := time.Unix(1_721_000_000, 123).UTC()
	meta := mysqlPurgeMetaFixture("zero-delete-clock")
	meta.Status = artifact.ArtifactStatusDeleted
	meta.DeleteReason = artifact.DeleteReasonUser
	meta.PurgeStatus = artifact.PurgeStatusPending
	row := mysqlPurgeJobRow{
		status:       []byte(artifact.PurgeStatusPending),
		attempts:     0,
		lastError:    []byte{},
		createdAt:    encodeRequiredMySQLTime(jobTime),
		updatedAt:    encodeRequiredMySQLTime(jobTime),
		leaseOwner:   []byte{},
		leaseVersion: 0,
	}
	if got, err := decodeMySQLPurgeJob(&meta, row); err != nil || got == nil {
		t.Fatalf("decodeMySQLPurgeJob(zero deletion clock) = (%#v, %v), want valid job", got, err)
	}
}

func TestMySQLMetadataStoreGetPurgeJobReadsJoinedAuthoritativeIdentity(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	createdAt := time.Unix(1_721_200_000, 111).UTC()
	meta := mysqlPurgeMetaFixture("get")
	meta.Status = artifact.ArtifactStatusDeleted
	meta.DeletedAt = createdAt
	meta.DeleteReason = artifact.DeleteReasonUser
	meta.PurgeStatus = artifact.PurgeStatusRetrying
	row := mysqlPurgeJobRow{
		status: []byte(artifact.PurgeStatusRetrying), attempts: 2, lastError: []byte("retry"),
		createdAt: encodeRequiredMySQLTime(createdAt), updatedAt: encodeRequiredMySQLTime(createdAt.Add(time.Second)),
		leaseOwner: []byte{}, leaseVersion: 3,
	}
	const recordID = uint64(501)
	digest := sha256.Sum256([]byte(meta.ArtifactRef))
	mock.ExpectQuery(mysqlPurgeGetSelect).
		WithArgs(digest[:], []byte(meta.ArtifactRef)).
		WillReturnRows(mysqlTestPurgeJoinedRows(t, recordID, meta, &row))

	got, err := store.GetPurgeJob(context.Background(), meta.ArtifactRef)
	if err != nil {
		t.Fatalf("GetPurgeJob() error = %v", err)
	}
	want, err := decodeMySQLPurgeJob(&meta, row)
	if err != nil {
		t.Fatalf("decode expected job: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GetPurgeJob() = %#v, want %#v", got, want)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreGetPurgeJobDistinguishesMissingMetadataAndMissingJob(t *testing.T) {
	for _, test := range []struct {
		name        string
		metadataRow bool
		active      bool
		wantCode    artifact.ErrorCode
	}{
		{name: "metadata missing", wantCode: artifact.ErrNotFound},
		{name: "job missing", metadataRow: true, wantCode: artifact.ErrInvalidArgument},
		{name: "active metadata has no job", metadataRow: true, active: true, wantCode: artifact.ErrNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
			meta := mysqlPurgeMetaFixture("get-missing")
			meta.Status = artifact.ArtifactStatusDeleted
			meta.DeleteReason = artifact.DeleteReasonUser
			meta.PurgeStatus = artifact.PurgeStatusPending
			if test.active {
				meta.Status = artifact.ArtifactStatusReady
				meta.DeleteReason = ""
				meta.PurgeStatus = ""
			}
			digest := sha256.Sum256([]byte(meta.ArtifactRef))
			rows := mysqlTestPurgeJoinedRows(t, 502, meta, nil)
			if !test.metadataRow {
				rows = mysqlTestPurgeJoinedEmptyRows()
			}
			mock.ExpectQuery(mysqlPurgeGetSelect).
				WithArgs(digest[:], []byte(meta.ArtifactRef)).
				WillReturnRows(rows)

			got, err := store.GetPurgeJob(context.Background(), meta.ArtifactRef)
			if got != nil || !artifact.IsErrorCode(err, test.wantCode) {
				t.Fatalf("GetPurgeJob() = (%#v, %v), want %s", got, err, test.wantCode)
			}
			mysqlRequirePurgeExpectations(t, mock)
		})
	}
}

func TestMySQLMetadataStoreGetPurgeJobPreservesQueryFailureClassification(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	ref := "purge-ref-get-query-error"
	digest := sha256.Sum256([]byte(ref))
	cause := errors.New("joined purge query transport failed")
	mock.ExpectQuery(mysqlPurgeGetSelect).
		WithArgs(digest[:], []byte(ref)).
		WillReturnError(cause)

	got, err := store.GetPurgeJob(context.Background(), ref)
	if got != nil || !errors.Is(err, cause) || artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
		t.Fatalf("GetPurgeJob(query failure) = (%#v, %v), want untyped infrastructure error preserving cause", got, err)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreGetPurgeJobRejectsInvalidJoinedQueryRows(t *testing.T) {
	createdAt := time.Unix(1_721_200_000, 111).UTC()
	for _, test := range []struct {
		name   string
		status artifact.PurgeStatus
		mutate func(*artifact.ArtifactMeta, *mysqlPurgeJobRow, []driver.Value)
	}{
		{name: "attempts overflow", status: artifact.PurgeStatusRetrying, mutate: func(_ *artifact.ArtifactMeta, row *mysqlPurgeJobRow, values []driver.Value) {
			row.attempts = 1
			row.leaseVersion = 1
			values[37] = "9223372036854775808"
		}},
		{name: "lease version overflow", status: artifact.PurgeStatusLeased, mutate: func(_ *artifact.ArtifactMeta, row *mysqlPurgeJobRow, values []driver.Value) {
			row.leaseOwner = []byte("worker")
			row.leaseUntil = encodeRequiredMySQLTime(createdAt.Add(time.Minute))
			row.leaseVersion = 1
			values[46] = "9223372036854775808"
		}},
		{name: "mismatched nullable lease", status: artifact.PurgeStatusLeased, mutate: func(_ *artifact.ArtifactMeta, row *mysqlPurgeJobRow, _ []driver.Value) {
			row.leaseOwner = []byte("worker")
			row.leaseUntil.Seconds = sql.NullInt64{Int64: createdAt.Unix(), Valid: true}
			row.leaseVersion = 1
		}},
		{name: "noncanonical status", status: artifact.PurgeStatusPending, mutate: func(meta *artifact.ArtifactMeta, row *mysqlPurgeJobRow, _ []driver.Value) {
			meta.PurgeStatus = artifact.PurgeStatus("PENDING")
			row.status = []byte("PENDING")
		}},
		{name: "purged without completion", status: artifact.PurgeStatusPurged, mutate: func(_ *artifact.ArtifactMeta, row *mysqlPurgeJobRow, _ []driver.Value) {
			row.leaseVersion = 1
		}},
		{name: "pending with completion", status: artifact.PurgeStatusPending, mutate: func(meta *artifact.ArtifactMeta, _ *mysqlPurgeJobRow, _ []driver.Value) {
			meta.PurgedAt = createdAt.Add(time.Minute)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
			meta, row := mysqlPurgeStateFixture("get-invalid-"+test.name, test.status, createdAt)
			const recordID = uint64(503)
			values := mysqlTestPurgeJoinedValues(t, recordID, meta, row)
			test.mutate(&meta, &row, values)
			values = mysqlTestPurgeJoinedValues(t, recordID, meta, row)
			if test.name == "attempts overflow" {
				values[37] = "9223372036854775808"
			}
			if test.name == "lease version overflow" {
				values[46] = "9223372036854775808"
			}
			rows := mysqlTestPurgeJoinedEmptyRows().AddRow(values...)
			digest := sha256.Sum256([]byte(meta.ArtifactRef))
			mock.ExpectQuery(mysqlPurgeGetSelect).
				WithArgs(digest[:], []byte(meta.ArtifactRef)).
				WillReturnRows(rows)

			got, err := store.GetPurgeJob(context.Background(), meta.ArtifactRef)
			if got != nil || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
				t.Fatalf("GetPurgeJob(invalid joined row) = (%#v, %v), want typed invalid persisted state", got, err)
			}
			mysqlRequirePurgeExpectations(t, mock)
		})
	}
}

func TestMySQLMetadataStoreListPendingPurgesAppliesInclusiveBoundaryScopesStableOrderAndLimit(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	readyAt := time.Unix(1_721_300_000, 555).UTC()
	createdAt := readyAt.Add(-time.Hour)
	tenantID := "purge-tenant-list"
	sessionID := "purge-session-list"
	runID := "purge-run-list"
	tenantHash := sha256.Sum256([]byte(tenantID))
	sessionHash := sha256.Sum256([]byte(sessionID))
	runHash := sha256.Sum256([]byte(runID))

	metas := make([]artifact.ArtifactMeta, 3)
	rows := make([]mysqlPurgeJobRow, 3)
	statuses := []artifact.PurgeStatus{
		artifact.PurgeStatusPending,
		artifact.PurgeStatusRetrying,
		artifact.PurgeStatusLeased,
	}
	for index, suffix := range []string{"a", "b", "c"} {
		meta := mysqlPurgeMetaFixture("list-" + suffix)
		meta.ArtifactRef = "purge-ref-list-" + suffix
		meta.TenantID = tenantID
		meta.SessionID = sessionID
		meta.RunID = runID
		meta.Status = artifact.ArtifactStatusDeleted
		meta.DeleteReason = artifact.DeleteReasonTTL
		meta.PurgeStatus = statuses[index]
		metas[index] = meta
		row := mysqlPurgeJobRow{
			status: []byte(statuses[index]), attempts: uint64(index), lastError: []byte{},
			createdAt: encodeRequiredMySQLTime(createdAt), updatedAt: encodeRequiredMySQLTime(createdAt),
			leaseOwner: []byte{}, leaseVersion: int64(index),
		}
		if statuses[index] == artifact.PurgeStatusLeased {
			row.leaseOwner = []byte("boundary-worker")
			row.leaseUntil = encodeRequiredMySQLTime(readyAt)
		}
		rows[index] = row
	}

	wantSQL := mysqlTestPurgeListQuery(true, true, true)
	mock.ExpectQuery(wantSQL).
		WithArgs(
			[]byte(artifact.PurgeStatusPending), []byte(artifact.PurgeStatusRetrying), []byte(artifact.PurgeStatusLeased),
			readyAt.Unix(), readyAt.Unix(), int64(readyAt.Nanosecond()),
			tenantHash[:], []byte(tenantID), sessionHash[:], []byte(sessionID), runHash[:], []byte(runID), int64(100),
		).
		WillReturnRows(mysqlTestPurgeJoinedRowsMany(t, []uint64{601, 602, 603}, metas, rows))

	got, err := store.ListPendingPurges(context.Background(), artifact.PurgeQuery{
		TenantID: tenantID, SessionID: sessionID, RunID: runID, ReadyAt: readyAt, Limit: 1000,
	})
	if err != nil {
		t.Fatalf("ListPendingPurges() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListPendingPurges() length = %d, want 3", len(got))
	}
	for index := range got {
		if got[index].ArtifactRef != metas[index].ArtifactRef || got[index].Status != statuses[index] {
			t.Fatalf("ListPendingPurges()[%d] = %#v, want ref %q status %q", index, got[index], metas[index].ArtifactRef, statuses[index])
		}
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreListPendingPurgesDefaultsLimitToOneHundred(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	readyAt := time.Unix(1_721_300_000, 555).UTC()
	mock.ExpectQuery(mysqlTestPurgeListQuery(false, false, false)).
		WithArgs(
			[]byte(artifact.PurgeStatusPending), []byte(artifact.PurgeStatusRetrying), []byte(artifact.PurgeStatusLeased),
			readyAt.Unix(), readyAt.Unix(), int64(readyAt.Nanosecond()), int64(100),
		).
		WillReturnRows(mysqlTestPurgeJoinedEmptyRows())
	got, err := store.ListPendingPurges(context.Background(), artifact.PurgeQuery{ReadyAt: readyAt})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("ListPendingPurges(default limit) = (%#v, %v), want non-nil empty list", got, err)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreListPendingPurgesPreservesQueryFailureClassification(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	readyAt := time.Unix(1_721_300_000, 555).UTC()
	cause := errors.New("list purge query transport failed")
	mock.ExpectQuery(mysqlTestPurgeListQuery(false, false, false)).
		WithArgs(
			[]byte(artifact.PurgeStatusPending), []byte(artifact.PurgeStatusRetrying), []byte(artifact.PurgeStatusLeased),
			readyAt.Unix(), readyAt.Unix(), int64(readyAt.Nanosecond()), int64(10),
		).
		WillReturnError(cause)
	got, err := store.ListPendingPurges(context.Background(), artifact.PurgeQuery{ReadyAt: readyAt, Limit: 10})
	if got != nil || !errors.Is(err, cause) || artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
		t.Fatalf("ListPendingPurges(query failure) = (%#v, %v), want untyped infrastructure error preserving cause", got, err)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreClaimPurgeClaimsPendingAndUpdatesBothRows(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	now := time.Unix(1_721_400_000, 222).UTC()
	leaseUntil := now.Add(time.Minute)
	meta, row := mysqlPurgeStateFixture("claim-pending", artifact.PurgeStatusPending, now.Add(-time.Hour))
	const recordID = uint64(701)
	mysqlExpectLockedPurgeState(t, mock, recordID, meta, &row)
	mock.ExpectExec(mysqlTestPurgeClaimJobUpdate).
		WithArgs([]byte(artifact.PurgeStatusLeased), now.Unix(), int64(now.Nanosecond()), []byte("worker-a"), leaseUntil.Unix(), int64(leaseUntil.Nanosecond()), int64(1), int64(recordID)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(mysqlTestPurgeMetadataStatusUpdate).
		WithArgs([]byte(artifact.PurgeStatusLeased), int64(recordID)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	got, claimed, err := store.ClaimPurge(context.Background(), meta.ArtifactRef, "worker-a", now, leaseUntil)
	if err != nil || !claimed || got == nil || got.Status != artifact.PurgeStatusLeased || got.LeaseOwner != "worker-a" || got.LeaseVersion != 1 || got.LeaseUntil != leaseUntil {
		t.Fatalf("ClaimPurge(pending) = (%#v, %t, %v), want worker-a leased version 1", got, claimed, err)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreClaimPurgeClaimsRetryingJob(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	now := time.Unix(1_721_400_000, 222).UTC()
	leaseUntil := now.Add(time.Minute)
	meta, row := mysqlPurgeStateFixture("claim-retrying", artifact.PurgeStatusRetrying, now.Add(-time.Hour))
	row.attempts = 2
	row.lastError = []byte("previous failure")
	row.leaseVersion = 3
	const recordID = uint64(707)
	mysqlExpectLockedPurgeState(t, mock, recordID, meta, &row)
	mock.ExpectExec(mysqlTestPurgeClaimJobUpdate).
		WithArgs([]byte(artifact.PurgeStatusLeased), now.Unix(), int64(now.Nanosecond()), []byte("worker-r"), leaseUntil.Unix(), int64(leaseUntil.Nanosecond()), int64(4), int64(recordID)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(mysqlTestPurgeMetadataStatusUpdate).
		WithArgs([]byte(artifact.PurgeStatusLeased), int64(recordID)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	got, claimed, err := store.ClaimPurge(context.Background(), meta.ArtifactRef, "worker-r", now, leaseUntil)
	if err != nil || !claimed || got == nil || got.Status != artifact.PurgeStatusLeased || got.Attempts != 2 || got.LastError != "previous failure" || got.LeaseVersion != 4 {
		t.Fatalf("ClaimPurge(retrying) = (%#v, %t, %v), want leased retry state", got, claimed, err)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreClaimPurgeSameOwnerActiveReclaimIncrementsVersionWithoutMetadataNoop(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	now := time.Unix(1_721_400_000, 222).UTC()
	leaseUntil := now.Add(2 * time.Minute)
	meta, row := mysqlPurgeStateFixture("claim-same-owner", artifact.PurgeStatusLeased, now.Add(-time.Hour))
	row.leaseOwner = []byte("worker-a")
	row.leaseUntil = encodeRequiredMySQLTime(now.Add(time.Minute))
	row.leaseVersion = 4
	const recordID = uint64(702)
	mysqlExpectLockedPurgeState(t, mock, recordID, meta, &row)
	mock.ExpectExec(mysqlTestPurgeClaimJobUpdate).
		WithArgs([]byte(artifact.PurgeStatusLeased), now.Unix(), int64(now.Nanosecond()), []byte("worker-a"), leaseUntil.Unix(), int64(leaseUntil.Nanosecond()), int64(5), int64(recordID)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	got, claimed, err := store.ClaimPurge(context.Background(), meta.ArtifactRef, "worker-a", now, leaseUntil)
	if err != nil || !claimed || got == nil || got.LeaseVersion != 5 || got.LeaseUntil != leaseUntil {
		t.Fatalf("ClaimPurge(same-owner active) = (%#v, %t, %v), want re-leased version 5", got, claimed, err)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreClaimPurgeRejectsOtherOwnerActiveLease(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	now := time.Unix(1_721_400_000, 222).UTC()
	meta, row := mysqlPurgeStateFixture("claim-other-active", artifact.PurgeStatusLeased, now.Add(-time.Hour))
	row.leaseOwner = []byte("worker-a")
	row.leaseUntil = encodeRequiredMySQLTime(now.Add(time.Minute))
	row.leaseVersion = 4
	const recordID = uint64(703)
	mysqlExpectLockedPurgeState(t, mock, recordID, meta, &row)
	mock.ExpectCommit()

	got, claimed, err := store.ClaimPurge(context.Background(), meta.ArtifactRef, "worker-b", now, now.Add(2*time.Minute))
	if err != nil || claimed || got == nil || got.LeaseOwner != "worker-a" || got.LeaseVersion != 4 {
		t.Fatalf("ClaimPurge(other active) = (%#v, %t, %v), want unchanged unclaimed lease", got, claimed, err)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreClaimPurgeTakesOverLeaseAtInclusiveExpiryBoundary(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	now := time.Unix(1_721_400_000, 222).UTC()
	leaseUntil := now.Add(time.Minute)
	meta, row := mysqlPurgeStateFixture("claim-boundary", artifact.PurgeStatusLeased, now.Add(-time.Hour))
	row.leaseOwner = []byte("worker-a")
	row.leaseUntil = encodeRequiredMySQLTime(now)
	row.leaseVersion = 8
	const recordID = uint64(704)
	mysqlExpectLockedPurgeState(t, mock, recordID, meta, &row)
	mock.ExpectExec(mysqlTestPurgeClaimJobUpdate).
		WithArgs([]byte(artifact.PurgeStatusLeased), now.Unix(), int64(now.Nanosecond()), []byte("worker-b"), leaseUntil.Unix(), int64(leaseUntil.Nanosecond()), int64(9), int64(recordID)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	got, claimed, err := store.ClaimPurge(context.Background(), meta.ArtifactRef, "worker-b", now, leaseUntil)
	if err != nil || !claimed || got == nil || got.LeaseOwner != "worker-b" || got.LeaseVersion != 9 {
		t.Fatalf("ClaimPurge(expiry boundary) = (%#v, %t, %v), want worker-b leased version 9", got, claimed, err)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreClaimPurgeKeepsPurgedTerminalStateIdempotent(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	now := time.Unix(1_721_400_000, 222).UTC()
	meta, row := mysqlPurgeStateFixture("claim-purged", artifact.PurgeStatusPurged, now.Add(-time.Hour))
	meta.PurgedAt = now.Add(-time.Minute)
	row.lastError = []byte{}
	row.leaseVersion = 2
	const recordID = uint64(705)
	mysqlExpectLockedPurgeState(t, mock, recordID, meta, &row)
	mock.ExpectCommit()

	got, claimed, err := store.ClaimPurge(context.Background(), meta.ArtifactRef, "worker", now, now.Add(time.Minute))
	if err != nil || claimed || got == nil || got.Status != artifact.PurgeStatusPurged {
		t.Fatalf("ClaimPurge(purged) = (%#v, %t, %v), want terminal no-op", got, claimed, err)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreClaimPurgeRejectsLeaseVersionOverflowBeforeWrite(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	now := time.Unix(1_721_400_000, 222).UTC()
	meta, row := mysqlPurgeLeasedStateFixture("claim-overflow", now.Add(-time.Hour), "worker", math.MaxInt64, now.Add(time.Minute))
	row.leaseVersion = math.MaxInt64
	const recordID = uint64(706)
	mysqlExpectLockedPurgeState(t, mock, recordID, meta, &row)
	mock.ExpectRollback()

	got, claimed, err := store.ClaimPurge(context.Background(), meta.ArtifactRef, "worker", now, now.Add(time.Minute))
	if got != nil || claimed || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
		t.Fatalf("ClaimPurge(lease version overflow) = (%#v, %t, %v), want typed invalid before write", got, claimed, err)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreClaimPurgeRejectsInvalidLeaseInputsBeforeSQL(t *testing.T) {
	now := time.Unix(1_721_400_000, 222).UTC()
	for _, test := range []struct {
		name       string
		workerID   string
		leaseUntil time.Time
	}{
		{name: "empty worker", leaseUntil: now.Add(time.Minute)},
		{name: "equal deadline", workerID: "worker", leaseUntil: now},
		{name: "past deadline", workerID: "worker", leaseUntil: now.Add(-time.Nanosecond)},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
			got, claimed, err := store.ClaimPurge(context.Background(), "purge-ref-invalid-input", test.workerID, now, test.leaseUntil)
			if got != nil || claimed || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
				t.Fatalf("ClaimPurge(invalid input) = (%#v, %t, %v), want typed invalid without SQL", got, claimed, err)
			}
			mysqlRequirePurgeExpectations(t, mock)
		})
	}
}

func TestMySQLMetadataStoreMarkPurgeSucceededFencesLeaseAndUpdatesBothRows(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	createdAt := time.Unix(1_721_500_000, 333).UTC()
	at := createdAt.Add(time.Hour)
	meta, row := mysqlPurgeLeasedStateFixture("success", createdAt, "worker-a", 7, at.Add(time.Minute))
	row.lastError = []byte("old retry error")
	const recordID = uint64(801)
	mysqlExpectLockedPurgeState(t, mock, recordID, meta, &row)
	mock.ExpectExec(mysqlTestPurgeSuccessJobUpdate).
		WithArgs([]byte(artifact.PurgeStatusPurged), []byte{}, at.Unix(), int64(at.Nanosecond()), []byte{}, int64(recordID), []byte(artifact.PurgeStatusLeased), []byte("worker-a"), int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(mysqlTestPurgeSuccessMetadataUpdate).
		WithArgs([]byte(artifact.PurgeStatusPurged), at.Unix(), int64(at.Nanosecond()), int64(recordID)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	got, err := store.MarkPurgeSucceeded(context.Background(), meta.ArtifactRef, "worker-a", 7, at)
	want := meta
	want.PurgeStatus = artifact.PurgeStatusPurged
	want.PurgedAt = at
	if err != nil || !reflect.DeepEqual(got, &want) {
		t.Fatalf("MarkPurgeSucceeded() = (%#v, %v), want %#v", got, err, &want)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreMarkPurgeSucceededRejectsStaleOwnerOrVersion(t *testing.T) {
	for _, test := range []struct {
		name         string
		workerID     string
		leaseVersion int64
	}{
		{name: "owner", workerID: "worker-b", leaseVersion: 7},
		{name: "version", workerID: "worker-a", leaseVersion: 6},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
			createdAt := time.Unix(1_721_500_000, 333).UTC()
			meta, row := mysqlPurgeLeasedStateFixture("success-stale-"+test.name, createdAt, "worker-a", 7, createdAt.Add(time.Hour))
			const recordID = uint64(802)
			mysqlExpectLockedPurgeState(t, mock, recordID, meta, &row)
			mock.ExpectRollback()

			got, err := store.MarkPurgeSucceeded(context.Background(), meta.ArtifactRef, test.workerID, test.leaseVersion, createdAt.Add(time.Minute))
			if got != nil || !artifact.IsErrorCode(err, artifact.ErrConflict) {
				t.Fatalf("MarkPurgeSucceeded(stale %s) = (%#v, %v), want conflict", test.name, got, err)
			}
			mysqlRequirePurgeExpectations(t, mock)
		})
	}
}

func TestMySQLMetadataStoreMarkPurgeSucceededKeepsPurgedTerminalStateIdempotent(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	createdAt := time.Unix(1_721_500_000, 333).UTC()
	meta, row := mysqlPurgeStateFixture("success-purged", artifact.PurgeStatusPurged, createdAt)
	meta.PurgedAt = createdAt.Add(time.Hour)
	row.leaseVersion = 9
	const recordID = uint64(803)
	mysqlExpectLockedPurgeState(t, mock, recordID, meta, &row)
	mock.ExpectCommit()

	got, err := store.MarkPurgeSucceeded(context.Background(), meta.ArtifactRef, "stale-worker", 1, createdAt.Add(2*time.Hour))
	if err != nil || !reflect.DeepEqual(got, &meta) {
		t.Fatalf("MarkPurgeSucceeded(purged) = (%#v, %v), want terminal metadata %#v", got, err, &meta)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreMarkPurgeSucceededRejectsZeroCompletionTimeBeforeSQL(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	got, err := store.MarkPurgeSucceeded(context.Background(), "purge-ref-zero-success", "worker", 1, time.Time{})
	if got != nil || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
		t.Fatalf("MarkPurgeSucceeded(zero completion) = (%#v, %v), want typed invalid without SQL", got, err)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreMarkPurgeSucceededRejectsInvalidFenceInputsBeforeSQL(t *testing.T) {
	at := time.Unix(1_721_500_000, 333).UTC()
	for _, test := range []struct {
		name         string
		workerID     string
		leaseVersion int64
	}{
		{name: "empty worker", leaseVersion: 1},
		{name: "zero version", workerID: "worker"},
		{name: "negative version", workerID: "worker", leaseVersion: -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
			got, err := store.MarkPurgeSucceeded(context.Background(), "purge-ref-invalid-success", test.workerID, test.leaseVersion, at)
			if got != nil || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
				t.Fatalf("MarkPurgeSucceeded(invalid fence) = (%#v, %v), want typed invalid without SQL", got, err)
			}
			mysqlRequirePurgeExpectations(t, mock)
		})
	}
}

func TestMySQLMetadataStoreMarkPurgeSucceededMapsLostSQLFenceToConflict(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	createdAt := time.Unix(1_721_500_000, 333).UTC()
	at := createdAt.Add(time.Hour)
	meta, row := mysqlPurgeLeasedStateFixture("success-lost-fence", createdAt, "worker-a", 7, at.Add(time.Minute))
	const recordID = uint64(808)
	mysqlExpectLockedPurgeState(t, mock, recordID, meta, &row)
	mock.ExpectExec(mysqlTestPurgeSuccessJobUpdate).
		WithArgs([]byte(artifact.PurgeStatusPurged), []byte{}, at.Unix(), int64(at.Nanosecond()), []byte{}, int64(recordID), []byte(artifact.PurgeStatusLeased), []byte("worker-a"), int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	got, err := store.MarkPurgeSucceeded(context.Background(), meta.ArtifactRef, "worker-a", 7, at)
	if got != nil || !artifact.IsErrorCode(err, artifact.ErrConflict) {
		t.Fatalf("MarkPurgeSucceeded(lost SQL fence) = (%#v, %v), want conflict", got, err)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreMarkPurgeFailedFencesLeaseIncrementsAttemptsAndClearsLease(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	createdAt := time.Unix(1_721_500_000, 333).UTC()
	at := createdAt.Add(time.Hour)
	meta, row := mysqlPurgeLeasedStateFixture("failure", createdAt, "worker-a", 7, at.Add(time.Minute))
	row.attempts = 2
	const recordID = uint64(804)
	mysqlExpectLockedPurgeState(t, mock, recordID, meta, &row)
	mock.ExpectExec(mysqlTestPurgeFailureJobUpdate).
		WithArgs([]byte(artifact.PurgeStatusRetrying), int64(3), []byte("object_delete_failed"), at.Unix(), int64(at.Nanosecond()), []byte{}, int64(recordID), []byte(artifact.PurgeStatusLeased), []byte("worker-a"), int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(mysqlTestPurgeMetadataStatusUpdate).
		WithArgs([]byte(artifact.PurgeStatusRetrying), int64(recordID)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	got, err := store.MarkPurgeFailed(context.Background(), meta.ArtifactRef, "worker-a", 7, "object_delete_failed", at)
	if err != nil || got == nil || got.Status != artifact.PurgeStatusRetrying || got.Attempts != 3 || got.LastError != "object_delete_failed" || got.LeaseOwner != "" || !got.LeaseUntil.IsZero() {
		t.Fatalf("MarkPurgeFailed() = (%#v, %v), want retrying attempt 3 with cleared lease", got, err)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreMarkPurgeFailedRejectsStaleOwnerOrVersion(t *testing.T) {
	for _, test := range []struct {
		name         string
		workerID     string
		leaseVersion int64
	}{
		{name: "owner", workerID: "worker-b", leaseVersion: 7},
		{name: "version", workerID: "worker-a", leaseVersion: 6},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
			createdAt := time.Unix(1_721_500_000, 333).UTC()
			meta, row := mysqlPurgeLeasedStateFixture("failure-stale-"+test.name, createdAt, "worker-a", 7, createdAt.Add(time.Hour))
			const recordID = uint64(805)
			mysqlExpectLockedPurgeState(t, mock, recordID, meta, &row)
			mock.ExpectRollback()

			got, err := store.MarkPurgeFailed(context.Background(), meta.ArtifactRef, test.workerID, test.leaseVersion, "failed", createdAt.Add(time.Minute))
			if got != nil || !artifact.IsErrorCode(err, artifact.ErrConflict) {
				t.Fatalf("MarkPurgeFailed(stale %s) = (%#v, %v), want conflict", test.name, got, err)
			}
			mysqlRequirePurgeExpectations(t, mock)
		})
	}
}

func TestMySQLMetadataStoreMarkPurgeFailedKeepsPurgedTerminalStateIdempotent(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	createdAt := time.Unix(1_721_500_000, 333).UTC()
	meta, row := mysqlPurgeStateFixture("failure-purged", artifact.PurgeStatusPurged, createdAt)
	meta.PurgedAt = createdAt.Add(time.Hour)
	row.leaseVersion = 9
	const recordID = uint64(806)
	mysqlExpectLockedPurgeState(t, mock, recordID, meta, &row)
	mock.ExpectCommit()

	got, err := store.MarkPurgeFailed(context.Background(), meta.ArtifactRef, "stale-worker", 1, "late failure", createdAt.Add(2*time.Hour))
	want, decodeErr := decodeMySQLPurgeJob(&meta, row)
	if decodeErr != nil {
		t.Fatalf("decode expected terminal job: %v", decodeErr)
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("MarkPurgeFailed(purged) = (%#v, %v), want terminal job %#v", got, err, want)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreMarkPurgeFailedRejectsAttemptOverflowBeforeWrite(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	createdAt := time.Unix(1_721_500_000, 333).UTC()
	meta, row := mysqlPurgeLeasedStateFixture("failure-overflow", createdAt, "worker-a", 7, createdAt.Add(time.Hour))
	row.attempts = uint64(maxIntForMySQLPurgeTest())
	const recordID = uint64(807)
	mysqlExpectLockedPurgeState(t, mock, recordID, meta, &row)
	mock.ExpectRollback()

	got, err := store.MarkPurgeFailed(context.Background(), meta.ArtifactRef, "worker-a", 7, "failed", createdAt.Add(time.Minute))
	if got != nil || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
		t.Fatalf("MarkPurgeFailed(attempt overflow) = (%#v, %v), want typed invalid before write", got, err)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func TestMySQLMetadataStoreMarkPurgeFailedRejectsInvalidInputsBeforeSQL(t *testing.T) {
	at := time.Unix(1_721_500_000, 333).UTC()
	for _, test := range []struct {
		name         string
		workerID     string
		leaseVersion int64
		at           time.Time
	}{
		{name: "empty worker", leaseVersion: 1, at: at},
		{name: "zero version", workerID: "worker", at: at},
		{name: "negative version", workerID: "worker", leaseVersion: -1, at: at},
		{name: "zero time", workerID: "worker", leaseVersion: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
			got, err := store.MarkPurgeFailed(context.Background(), "purge-ref-invalid-failure", test.workerID, test.leaseVersion, "failed", test.at)
			if got != nil || !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
				t.Fatalf("MarkPurgeFailed(invalid input) = (%#v, %v), want typed invalid without SQL", got, err)
			}
			mysqlRequirePurgeExpectations(t, mock)
		})
	}
}

func TestMySQLMetadataStoreMarkPurgeFailedMapsLostSQLFenceToConflict(t *testing.T) {
	store, mock := newMySQLDeleteTestStore(t, sha256.Sum256)
	createdAt := time.Unix(1_721_500_000, 333).UTC()
	at := createdAt.Add(time.Hour)
	meta, row := mysqlPurgeLeasedStateFixture("failure-lost-fence", createdAt, "worker-a", 7, at.Add(time.Minute))
	const recordID = uint64(809)
	mysqlExpectLockedPurgeState(t, mock, recordID, meta, &row)
	mock.ExpectExec(mysqlTestPurgeFailureJobUpdate).
		WithArgs([]byte(artifact.PurgeStatusRetrying), int64(1), []byte("failed"), at.Unix(), int64(at.Nanosecond()), []byte{}, int64(recordID), []byte(artifact.PurgeStatusLeased), []byte("worker-a"), int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	got, err := store.MarkPurgeFailed(context.Background(), meta.ArtifactRef, "worker-a", 7, "failed", at)
	if got != nil || !artifact.IsErrorCode(err, artifact.ErrConflict) {
		t.Fatalf("MarkPurgeFailed(lost SQL fence) = (%#v, %v), want conflict", got, err)
	}
	mysqlRequirePurgeExpectations(t, mock)
}

func maxIntForMySQLPurgeTest() int {
	return int(^uint(0) >> 1)
}

func mysqlPurgeMetaFixture(suffix string) artifact.ArtifactMeta {
	meta := mysqlGetterMetaFixture()
	meta.ArtifactID = "purge-id-" + suffix
	meta.ArtifactRef = "purge-ref-" + suffix
	meta.TenantID = "purge-tenant-" + suffix
	meta.SessionID = "purge-session-" + suffix
	meta.RunID = "purge-run-" + suffix
	meta.StorageKey = "purge-key-" + suffix
	meta.Status = artifact.ArtifactStatusReady
	meta.DeletedAt = time.Time{}
	meta.DeleteReason = ""
	meta.PurgeStatus = ""
	meta.PurgedAt = time.Time{}
	if math.MaxInt == math.MaxInt32 {
		panic("MySQL purge tests require a 64-bit Go target")
	}
	return meta
}

func mysqlPurgeStateFixture(suffix string, status artifact.PurgeStatus, createdAt time.Time) (artifact.ArtifactMeta, mysqlPurgeJobRow) {
	meta := mysqlPurgeMetaFixture(suffix)
	meta.Status = artifact.ArtifactStatusDeleted
	meta.DeletedAt = createdAt
	meta.DeleteReason = artifact.DeleteReasonUser
	meta.PurgeStatus = status
	row := mysqlPurgeJobRow{
		status:       []byte(status),
		attempts:     0,
		lastError:    []byte{},
		createdAt:    encodeRequiredMySQLTime(createdAt),
		updatedAt:    encodeRequiredMySQLTime(createdAt),
		leaseOwner:   []byte{},
		leaseVersion: 0,
	}
	return meta, row
}

func mysqlPurgeLeasedStateFixture(suffix string, createdAt time.Time, owner string, version int64, leaseUntil time.Time) (artifact.ArtifactMeta, mysqlPurgeJobRow) {
	meta, row := mysqlPurgeStateFixture(suffix, artifact.PurgeStatusLeased, createdAt)
	row.leaseOwner = []byte(owner)
	row.leaseUntil = encodeRequiredMySQLTime(leaseUntil)
	row.leaseVersion = version
	return meta, row
}

func mysqlExpectLockedPurgeState(t *testing.T, mock sqlmock.Sqlmock, recordID uint64, meta artifact.ArtifactMeta, job *mysqlPurgeJobRow) {
	t.Helper()
	digest := sha256.Sum256([]byte(meta.ArtifactRef))
	mock.ExpectBegin()
	mock.ExpectQuery(mysqlMetadataMarkDeletedSelect).
		WithArgs(digest[:], []byte(meta.ArtifactRef)).
		WillReturnRows(mysqlTestDeleteRows(t, recordID, meta))
	mock.ExpectQuery(mysqlTestPurgeJobLockSelect).
		WithArgs(int64(recordID)).
		WillReturnRows(func() *sqlmock.Rows {
			if job == nil {
				return mysqlTestPurgeJobRows()
			}
			return mysqlTestPurgeJobRows(*job)
		}())
}

func mysqlTestPurgeJobRows(rows ...mysqlPurgeJobRow) *sqlmock.Rows {
	result := sqlmock.NewRows([]string{
		"status", "attempts", "last_error", "created_at_sec", "created_at_nano",
		"updated_at_sec", "updated_at_nano", "lease_owner", "lease_until_sec",
		"lease_until_nano", "lease_version",
	})
	for _, row := range rows {
		result.AddRow(mysqlTestPurgeJobValues(row)...)
	}
	return result
}

func mysqlTestPurgeJobValues(row mysqlPurgeJobRow) []driver.Value {
	return []driver.Value{
		row.status,
		int64(row.attempts),
		row.lastError,
		mysqlTestNullableInt64(row.createdAt.Seconds),
		mysqlTestNullableInt64(row.createdAt.Nanoseconds),
		mysqlTestNullableInt64(row.updatedAt.Seconds),
		mysqlTestNullableInt64(row.updatedAt.Nanoseconds),
		row.leaseOwner,
		mysqlTestNullableInt64(row.leaseUntil.Seconds),
		mysqlTestNullableInt64(row.leaseUntil.Nanoseconds),
		row.leaseVersion,
	}
}

func mysqlTestPurgeJoinedEmptyRows() *sqlmock.Rows {
	columns := append([]string{"id"}, mysqlTestSelectedColumnNames...)
	columns = append(columns,
		"job_metadata_id", "job_status", "job_attempts", "job_last_error",
		"job_created_at_sec", "job_created_at_nano", "job_updated_at_sec", "job_updated_at_nano",
		"job_lease_owner", "job_lease_until_sec", "job_lease_until_nano", "job_lease_version",
	)
	return sqlmock.NewRows(columns)
}

func mysqlTestPurgeJoinedRows(t *testing.T, recordID uint64, meta artifact.ArtifactMeta, job *mysqlPurgeJobRow) *sqlmock.Rows {
	t.Helper()
	rows := mysqlTestPurgeJoinedEmptyRows()
	values := append([]driver.Value{int64(recordID)}, mysqlTestRowValues(t, meta)...)
	if job == nil {
		values = append(values, make([]driver.Value, 12)...)
	} else {
		values = append(values, int64(recordID))
		values = append(values, mysqlTestPurgeJobValues(*job)...)
	}
	rows.AddRow(values...)
	return rows
}

func mysqlTestPurgeJoinedRowsMany(t *testing.T, recordIDs []uint64, metas []artifact.ArtifactMeta, jobs []mysqlPurgeJobRow) *sqlmock.Rows {
	t.Helper()
	if len(recordIDs) != len(metas) || len(metas) != len(jobs) {
		t.Fatal("invalid joined purge rows fixture lengths")
	}
	rows := mysqlTestPurgeJoinedEmptyRows()
	for index := range metas {
		values := append([]driver.Value{int64(recordIDs[index])}, mysqlTestRowValues(t, metas[index])...)
		values = append(values, int64(recordIDs[index]))
		values = append(values, mysqlTestPurgeJobValues(jobs[index])...)
		rows.AddRow(values...)
	}
	return rows
}

func mysqlTestPurgeJoinedValues(t *testing.T, recordID uint64, meta artifact.ArtifactMeta, job mysqlPurgeJobRow) []driver.Value {
	t.Helper()
	values := append([]driver.Value{int64(recordID)}, mysqlTestRowValues(t, meta)...)
	values = append(values, int64(recordID))
	return append(values, mysqlTestPurgeJobValues(job)...)
}

func mysqlTestPurgeListQuery(tenant, session, run bool) string {
	query := "SELECT " + mysqlPurgeJoinedSelectColumns + " FROM artifact_metadata AS metadata JOIN artifact_purge_job AS job ON job.metadata_id = metadata.id WHERE (job.status IN (?, ?) OR (job.status = ? AND (job.lease_until_sec IS NULL OR job.lease_until_nano IS NULL OR job.lease_until_sec < ? OR (job.lease_until_sec = ? AND job.lease_until_nano <= ?))))"
	if tenant {
		query += " AND metadata.tenant_id_hash = ? AND metadata.tenant_id = ?"
	}
	if session {
		query += " AND metadata.session_id_hash = ? AND metadata.session_id = ?"
	}
	if run {
		query += " AND metadata.run_id_hash = ? AND metadata.run_id = ?"
	}
	return query + " ORDER BY job.created_at_sec ASC, job.created_at_nano ASC, metadata.artifact_ref ASC LIMIT ?"
}

func mysqlRequirePurgeExpectations(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("MySQL purge SQL protocol mismatch: %v", err)
	}
}
