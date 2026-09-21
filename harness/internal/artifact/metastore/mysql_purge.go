package metastore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

var _ artifact.PurgeStore = (*MySQLMetadataStore)(nil)
var _ artifact.ProductionDependency = (*MySQLMetadataStore)(nil)

type mysqlPurgeJobRow struct {
	status       []byte
	attempts     uint64
	lastError    []byte
	createdAt    mysqlTimeColumns
	updatedAt    mysqlTimeColumns
	leaseOwner   []byte
	leaseUntil   mysqlTimeColumns
	leaseVersion int64
}

const mysqlPurgeJobColumns = "status, attempts, last_error, created_at_sec, created_at_nano, updated_at_sec, updated_at_nano, lease_owner, lease_until_sec, lease_until_nano, lease_version"

const mysqlPurgeJobLockSelect = "SELECT " + mysqlPurgeJobColumns + " FROM artifact_purge_job WHERE metadata_id = ? FOR UPDATE"

const mysqlPurgeJobInsert = "INSERT INTO artifact_purge_job (metadata_id, status, attempts, last_error, created_at_sec, created_at_nano, updated_at_sec, updated_at_nano, lease_owner, lease_until_sec, lease_until_nano, lease_version) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"

const mysqlPurgeClaimJobUpdate = "UPDATE artifact_purge_job SET status = ?, updated_at_sec = ?, updated_at_nano = ?, lease_owner = ?, lease_until_sec = ?, lease_until_nano = ?, lease_version = ? WHERE metadata_id = ?"

const mysqlPurgeMetadataStatusUpdate = "UPDATE artifact_metadata SET purge_status = ?, purged_at_sec = NULL, purged_at_nano = NULL WHERE id = ?"

const mysqlPurgeSuccessJobUpdate = "UPDATE artifact_purge_job SET status = ?, last_error = ?, updated_at_sec = ?, updated_at_nano = ?, lease_owner = ?, lease_until_sec = NULL, lease_until_nano = NULL WHERE metadata_id = ? AND status = ? AND lease_owner = ? AND lease_version = ?"

const mysqlPurgeSuccessMetadataUpdate = "UPDATE artifact_metadata SET purge_status = ?, purged_at_sec = ?, purged_at_nano = ? WHERE id = ?"

const mysqlPurgeFailureJobUpdate = "UPDATE artifact_purge_job SET status = ?, attempts = ?, last_error = ?, updated_at_sec = ?, updated_at_nano = ?, lease_owner = ?, lease_until_sec = NULL, lease_until_nano = NULL WHERE metadata_id = ? AND status = ? AND lease_owner = ? AND lease_version = ?"

var mysqlPurgeJoinedSelectColumns = "metadata.id, metadata." + strings.ReplaceAll(mysqlMetadataSelectColumns, ", ", ", metadata.") + ", job.metadata_id, job." + strings.ReplaceAll(mysqlPurgeJobColumns, ", ", ", job.")

var mysqlPurgeGetSelect = "SELECT " + mysqlPurgeJoinedSelectColumns + " FROM artifact_metadata AS metadata LEFT JOIN artifact_purge_job AS job ON job.metadata_id = metadata.id WHERE metadata.artifact_ref_hash = ? AND metadata.artifact_ref = ? LIMIT 2"

var mysqlPurgeListBaseSelect = "SELECT " + mysqlPurgeJoinedSelectColumns + " FROM artifact_metadata AS metadata JOIN artifact_purge_job AS job ON job.metadata_id = metadata.id WHERE (job.status IN (?, ?) OR (job.status = ? AND (job.lease_until_sec IS NULL OR job.lease_until_nano IS NULL OR job.lease_until_sec < ? OR (job.lease_until_sec = ? AND job.lease_until_nano <= ?))))"

const mysqlPurgeListOrderLimit = " ORDER BY job.created_at_sec ASC, job.created_at_nano ASC, metadata.artifact_ref ASC LIMIT ?"

type mysqlJoinedPurgeJobRow struct {
	metadataRecordID *uint64
	status           []byte
	attempts         *uint64
	lastError        []byte
	createdAt        mysqlTimeColumns
	updatedAt        mysqlTimeColumns
	leaseOwner       []byte
	leaseUntil       mysqlTimeColumns
	leaseVersion     *int64
}

func (*MySQLMetadataStore) ProductionReady() bool { return true }

func decodeMySQLPurgeJob(meta *artifact.ArtifactMeta, row mysqlPurgeJobRow) (*artifact.PurgeJob, error) {
	if meta == nil {
		return nil, mysqlCodecInvalid("MySQL artifact purge metadata is NULL")
	}
	if row.status == nil || row.lastError == nil || row.leaseOwner == nil {
		return nil, mysqlCodecInvalid("required MySQL artifact purge job column is NULL")
	}
	status := artifact.PurgeStatus(row.status)
	switch status {
	case artifact.PurgeStatusPending, artifact.PurgeStatusLeased, artifact.PurgeStatusRetrying, artifact.PurgeStatusPurged:
	default:
		return nil, mysqlCodecInvalid("invalid MySQL artifact purge job status")
	}
	if row.attempts > uint64(^uint(0)>>1) {
		return nil, mysqlCodecInvalid("MySQL artifact purge attempts overflow int")
	}
	if row.leaseVersion < 0 {
		return nil, mysqlCodecInvalid("invalid MySQL artifact purge lease version")
	}
	createdAt, err := decodeRequiredMySQLTime(row.createdAt)
	if err != nil {
		return nil, err
	}
	updatedAt, err := decodeRequiredMySQLTime(row.updatedAt)
	if err != nil {
		return nil, err
	}
	leaseUntil, err := decodeNullableMySQLTime(row.leaseUntil)
	if err != nil {
		return nil, err
	}
	leaseOwner := string(row.leaseOwner)
	if status == artifact.PurgeStatusLeased {
		if leaseOwner == "" || leaseUntil.IsZero() {
			return nil, mysqlCodecInvalid("leased MySQL artifact purge job requires owner and deadline")
		}
	} else if leaseOwner != "" || !leaseUntil.IsZero() {
		return nil, mysqlCodecInvalid("non-leased MySQL artifact purge job cannot retain a lease")
	}
	if meta.Status != artifact.ArtifactStatusDeleted {
		return nil, mysqlCodecInvalid("MySQL artifact purge job metadata is not a tombstone")
	}
	if meta.PurgeStatus != status {
		return nil, mysqlCodecInvalid("MySQL artifact purge job and metadata statuses differ")
	}
	switch status {
	case artifact.PurgeStatusPending:
		if row.attempts != 0 || len(row.lastError) != 0 || row.leaseVersion != 0 {
			return nil, mysqlCodecInvalid("pending MySQL artifact purge job has impossible history")
		}
	case artifact.PurgeStatusLeased:
		if row.leaseVersion <= 0 {
			return nil, mysqlCodecInvalid("leased MySQL artifact purge job requires a positive lease version")
		}
	case artifact.PurgeStatusRetrying:
		if row.attempts == 0 || row.leaseVersion <= 0 {
			return nil, mysqlCodecInvalid("retrying MySQL artifact purge job requires attempts and a lease version")
		}
	case artifact.PurgeStatusPurged:
		if row.leaseVersion <= 0 {
			return nil, mysqlCodecInvalid("purged MySQL artifact purge job requires a lease version")
		}
	}
	if status == artifact.PurgeStatusPurged {
		if meta.PurgedAt.IsZero() {
			return nil, mysqlCodecInvalid("purged MySQL artifact purge job has no completion time")
		}
		if len(row.lastError) != 0 {
			return nil, mysqlCodecInvalid("purged MySQL artifact purge job retains an error")
		}
	} else if !meta.PurgedAt.IsZero() {
		return nil, mysqlCodecInvalid("non-purged MySQL artifact purge job has a completion time")
	}
	return &artifact.PurgeJob{
		ArtifactRef:  meta.ArtifactRef,
		TenantID:     meta.TenantID,
		SessionID:    meta.SessionID,
		RunID:        meta.RunID,
		StorageKey:   meta.StorageKey,
		Status:       status,
		Attempts:     int(row.attempts),
		LastError:    string(row.lastError),
		CreatedAt:    createdAt,
		UpdatedAt:    updatedAt,
		LeaseOwner:   leaseOwner,
		LeaseUntil:   leaseUntil,
		LeaseVersion: row.leaseVersion,
	}, nil
}

func lockMySQLPurgeJob(ctx context.Context, tx *sql.Tx, recordID uint64) (mysqlPurgeJobRow, bool, error) {
	rows, err := tx.QueryContext(ctx, mysqlPurgeJobLockSelect, recordID)
	if err != nil {
		return mysqlPurgeJobRow{}, false, fmt.Errorf("query locked MySQL artifact purge job: %w", err)
	}
	var result mysqlPurgeJobRow
	found := false
	var readErr error
	for rows.Next() {
		if found {
			readErr = mysqlCodecInvalid("multiple locked MySQL artifact purge job rows")
			break
		}
		var row mysqlPurgeJobRow
		if err := rows.Scan(
			&row.status,
			&row.attempts,
			&row.lastError,
			&row.createdAt.Seconds,
			&row.createdAt.Nanoseconds,
			&row.updatedAt.Seconds,
			&row.updatedAt.Nanoseconds,
			&row.leaseOwner,
			&row.leaseUntil.Seconds,
			&row.leaseUntil.Nanoseconds,
			&row.leaseVersion,
		); err != nil {
			readErr = mysqlCodecInvalidCause("invalid locked MySQL artifact purge job row", err)
			break
		}
		result = row
		found = true
	}
	if err := rows.Err(); err != nil {
		readErr = errors.Join(readErr, fmt.Errorf("read locked MySQL artifact purge job rows: %w", err))
	}
	if err := rows.Close(); err != nil {
		readErr = errors.Join(readErr, fmt.Errorf("close locked MySQL artifact purge job rows: %w", err))
	}
	if readErr != nil {
		return mysqlPurgeJobRow{}, false, readErr
	}
	return result, found, nil
}

func scanMySQLJoinedPurgeRow(scanner mysqlRowScanner) (*artifact.ArtifactMeta, mysqlPurgeJobRow, bool, error) {
	var recordID uint64
	var metadataRow mysqlArtifactRow
	var joinedJob mysqlJoinedPurgeJobRow
	destinations := make([]any, 0, 1+len(mysqlArtifactRowScanDestinations(&metadataRow))+12)
	destinations = append(destinations, &recordID)
	destinations = append(destinations, mysqlArtifactRowScanDestinations(&metadataRow)...)
	destinations = append(destinations,
		&joinedJob.metadataRecordID,
		&joinedJob.status,
		&joinedJob.attempts,
		&joinedJob.lastError,
		&joinedJob.createdAt.Seconds,
		&joinedJob.createdAt.Nanoseconds,
		&joinedJob.updatedAt.Seconds,
		&joinedJob.updatedAt.Nanoseconds,
		&joinedJob.leaseOwner,
		&joinedJob.leaseUntil.Seconds,
		&joinedJob.leaseUntil.Nanoseconds,
		&joinedJob.leaseVersion,
	)
	if err := scanner.Scan(destinations...); err != nil {
		return nil, mysqlPurgeJobRow{}, false, mysqlCodecInvalidCause("invalid joined MySQL artifact purge row", err)
	}
	if mysqlArtifactRowHasNullRequiredRawColumn(metadataRow) {
		return nil, mysqlPurgeJobRow{}, false, mysqlCodecInvalid("required joined MySQL artifact metadata column is NULL")
	}
	meta, err := decodeMySQLArtifactRow(metadataRow)
	if err != nil {
		return nil, mysqlPurgeJobRow{}, false, err
	}
	if joinedJob.metadataRecordID == nil {
		return meta, mysqlPurgeJobRow{}, false, nil
	}
	if *joinedJob.metadataRecordID != recordID {
		return nil, mysqlPurgeJobRow{}, false, mysqlCodecInvalid("joined MySQL artifact purge identity mismatch")
	}
	if joinedJob.attempts == nil || joinedJob.leaseVersion == nil {
		return nil, mysqlPurgeJobRow{}, false, mysqlCodecInvalid("required joined MySQL artifact purge numeric column is NULL")
	}
	return meta, mysqlPurgeJobRow{
		status:       joinedJob.status,
		attempts:     *joinedJob.attempts,
		lastError:    joinedJob.lastError,
		createdAt:    joinedJob.createdAt,
		updatedAt:    joinedJob.updatedAt,
		leaseOwner:   joinedJob.leaseOwner,
		leaseUntil:   joinedJob.leaseUntil,
		leaseVersion: *joinedJob.leaseVersion,
	}, true, nil
}

func requireOneMySQLPurgeRow(result sql.Result, operation string) error {
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read MySQL artifact %s RowsAffected: %w", operation, err)
	}
	if rowsAffected != 1 {
		return mysqlCodecInvalid("MySQL artifact " + operation + " did not affect exactly one row")
	}
	return nil
}

func requireMySQLPurgeFence(result sql.Result, ref string) error {
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read MySQL artifact purge fence RowsAffected: %w", err)
	}
	if rowsAffected != 1 {
		return mysqlPurgeLeaseConflict(ref)
	}
	return nil
}

func finalizeMySQLPurge(tx *sql.Tx, primary error) error {
	if primary != nil {
		rollbackErr := tx.Rollback()
		if rollbackErr == nil || errors.Is(rollbackErr, sql.ErrTxDone) {
			return primary
		}
		return errors.Join(primary, fmt.Errorf("rollback MySQL artifact purge transaction: %w", rollbackErr))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit MySQL artifact purge transaction: %w", err)
	}
	return nil
}

func (s *MySQLMetadataStore) RequestPurge(ctx context.Context, ref string, reason artifact.DeleteReason, at time.Time) (*artifact.ArtifactMeta, *artifact.PurgeJob, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	switch reason {
	case artifact.DeleteReasonUser, artifact.DeleteReasonTTL, artifact.DeleteReasonCleanup:
	default:
		return nil, nil, mysqlCodecInvalid("invalid MySQL artifact purge delete reason")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, nil, fmt.Errorf("begin MySQL artifact purge request: %w", err)
	}
	refDigest := s.digest([]byte(ref))
	recordID, meta, err := s.lockAndReadMySQLMarkDeleted(ctx, tx, refDigest, ref)
	if err != nil {
		return nil, nil, finalizeMySQLPurge(tx, err)
	}
	row, exists, err := lockMySQLPurgeJob(ctx, tx, recordID)
	if err != nil {
		return nil, nil, finalizeMySQLPurge(tx, err)
	}
	if exists {
		job, err := decodeMySQLPurgeJob(meta, row)
		if err != nil {
			return nil, nil, finalizeMySQLPurge(tx, err)
		}
		if err := finalizeMySQLPurge(tx, nil); err != nil {
			return nil, nil, err
		}
		return meta, job, nil
	}

	immediate := *meta
	needsMetadataUpdate := immediate.Status != artifact.ArtifactStatusDeleted
	if needsMetadataUpdate {
		if immediate.Status != artifact.ArtifactStatusReady && immediate.Status != artifact.ArtifactStatusExpired {
			return nil, nil, finalizeMySQLPurge(tx, mysqlCodecInvalid("invalid live MySQL artifact metadata status for purge"))
		}
		if (immediate.PurgeStatus != "" && immediate.PurgeStatus != artifact.PurgeStatusNone) || !immediate.PurgedAt.IsZero() {
			return nil, nil, finalizeMySQLPurge(tx, mysqlCodecInvalid("live MySQL artifact metadata has purge state"))
		}
		immediate.Status = artifact.ArtifactStatusDeleted
		immediate.DeletedAt = at
		immediate.DeleteReason = reason
		immediate.PurgeStatus = artifact.PurgeStatusPending
		immediate.PurgedAt = time.Time{}
	} else {
		switch immediate.PurgeStatus {
		case artifact.PurgeStatusPending:
			if !immediate.PurgedAt.IsZero() {
				return nil, nil, finalizeMySQLPurge(tx, mysqlCodecInvalid("pending MySQL artifact metadata has a completion time"))
			}
		case "", artifact.PurgeStatusNone:
			if !immediate.PurgedAt.IsZero() {
				return nil, nil, finalizeMySQLPurge(tx, mysqlCodecInvalid("legacy MySQL artifact tombstone has a completion time"))
			}
			immediate.PurgeStatus = artifact.PurgeStatusPending
			needsMetadataUpdate = true
		default:
			return nil, nil, finalizeMySQLPurge(tx, mysqlCodecInvalid("deleted MySQL artifact metadata without purge job has invalid purge state"))
		}
	}

	jobTime := encodeRequiredMySQLTime(at)
	result, err := tx.ExecContext(
		ctx,
		mysqlPurgeJobInsert,
		recordID,
		[]byte(artifact.PurgeStatusPending),
		uint64(0),
		[]byte{},
		jobTime.Seconds.Int64,
		jobTime.Nanoseconds.Int64,
		jobTime.Seconds.Int64,
		jobTime.Nanoseconds.Int64,
		[]byte{},
		nil,
		nil,
		int64(0),
	)
	if err != nil {
		return nil, nil, finalizeMySQLPurge(tx, fmt.Errorf("insert MySQL artifact purge job: %w", err))
	}
	if err := requireOneMySQLPurgeRow(result, "purge job insert"); err != nil {
		return nil, nil, finalizeMySQLPurge(tx, err)
	}
	if needsMetadataUpdate {
		deletedAt := encodeNullableMySQLTime(immediate.DeletedAt)
		result, err = tx.ExecContext(
			ctx,
			mysqlMetadataMarkDeletedUpdate,
			[]byte(immediate.Status),
			mysqlBindNullInt64(deletedAt.Seconds),
			mysqlBindNullInt64(deletedAt.Nanoseconds),
			[]byte(immediate.DeleteReason),
			[]byte(immediate.PurgeStatus),
			recordID,
		)
		if err != nil {
			return nil, nil, finalizeMySQLPurge(tx, fmt.Errorf("update MySQL artifact purge tombstone: %w", err))
		}
		if err := requireOneMySQLPurgeRow(result, "purge tombstone update"); err != nil {
			return nil, nil, finalizeMySQLPurge(tx, err)
		}
	}
	if err := finalizeMySQLPurge(tx, nil); err != nil {
		return nil, nil, err
	}
	job := &artifact.PurgeJob{
		ArtifactRef: immediate.ArtifactRef,
		TenantID:    immediate.TenantID,
		SessionID:   immediate.SessionID,
		RunID:       immediate.RunID,
		StorageKey:  immediate.StorageKey,
		Status:      artifact.PurgeStatusPending,
		CreatedAt:   at,
		UpdatedAt:   at,
	}
	return &immediate, job, nil
}

func (s *MySQLMetadataStore) GetPurgeJob(ctx context.Context, ref string) (*artifact.PurgeJob, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	digest := s.digest([]byte(ref))
	rows, err := s.db.QueryContext(ctx, mysqlPurgeGetSelect, digest[:], []byte(ref))
	if err != nil {
		return nil, fmt.Errorf("query MySQL artifact purge job: %w", err)
	}
	var meta *artifact.ArtifactMeta
	var jobRow mysqlPurgeJobRow
	jobExists := false
	var readErr error
	for rows.Next() {
		if meta != nil {
			readErr = mysqlCodecInvalid("multiple exact MySQL artifact purge metadata rows")
			break
		}
		meta, jobRow, jobExists, readErr = scanMySQLJoinedPurgeRow(rows)
		if readErr != nil {
			break
		}
	}
	if err := rows.Err(); err != nil {
		readErr = errors.Join(readErr, fmt.Errorf("read MySQL artifact purge job rows: %w", err))
	}
	if err := rows.Close(); err != nil {
		readErr = errors.Join(readErr, fmt.Errorf("close MySQL artifact purge job rows: %w", err))
	}
	if readErr != nil {
		return nil, readErr
	}
	if meta == nil {
		return nil, mysqlMetadataNotFound()
	}
	if !jobExists {
		if meta.Status != artifact.ArtifactStatusDeleted {
			return nil, mysqlPurgeJobNotFound(ref)
		}
		return nil, mysqlCodecInvalid("MySQL artifact tombstone has no purge job")
	}
	return decodeMySQLPurgeJob(meta, jobRow)
}

func mysqlPurgeJobNotFound(ref string) error {
	return &artifact.Error{Code: artifact.ErrNotFound, Message: "artifact purge job not found: " + ref}
}

func (s *MySQLMetadataStore) ListPendingPurges(ctx context.Context, query artifact.PurgeQuery) ([]artifact.PurgeJob, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	readyAt := query.ReadyAt
	if readyAt.IsZero() {
		readyAt = time.Now()
	}
	limit := query.Limit
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	ready := encodeRequiredMySQLTime(readyAt)
	statement := mysqlPurgeListBaseSelect
	args := []any{
		[]byte(artifact.PurgeStatusPending),
		[]byte(artifact.PurgeStatusRetrying),
		[]byte(artifact.PurgeStatusLeased),
		ready.Seconds.Int64,
		ready.Seconds.Int64,
		ready.Nanoseconds.Int64,
	}
	if query.TenantID != "" {
		digest := s.digest([]byte(query.TenantID))
		statement += " AND metadata.tenant_id_hash = ? AND metadata.tenant_id = ?"
		args = append(args, digest[:], []byte(query.TenantID))
	}
	if query.SessionID != "" {
		digest := s.digest([]byte(query.SessionID))
		statement += " AND metadata.session_id_hash = ? AND metadata.session_id = ?"
		args = append(args, digest[:], []byte(query.SessionID))
	}
	if query.RunID != "" {
		digest := s.digest([]byte(query.RunID))
		statement += " AND metadata.run_id_hash = ? AND metadata.run_id = ?"
		args = append(args, digest[:], []byte(query.RunID))
	}
	statement += mysqlPurgeListOrderLimit
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("query pending MySQL artifact purge jobs: %w", err)
	}
	jobs := make([]artifact.PurgeJob, 0, limit)
	var readErr error
	for rows.Next() {
		meta, row, exists, err := scanMySQLJoinedPurgeRow(rows)
		if err != nil {
			readErr = err
			break
		}
		if !exists {
			readErr = mysqlCodecInvalid("pending MySQL artifact purge query returned metadata without a job")
			break
		}
		job, err := decodeMySQLPurgeJob(meta, row)
		if err != nil {
			readErr = err
			break
		}
		jobs = append(jobs, *job)
	}
	if err := rows.Err(); err != nil {
		readErr = errors.Join(readErr, fmt.Errorf("read pending MySQL artifact purge job rows: %w", err))
	}
	if err := rows.Close(); err != nil {
		readErr = errors.Join(readErr, fmt.Errorf("close pending MySQL artifact purge job rows: %w", err))
	}
	if readErr != nil {
		return nil, readErr
	}
	return jobs, nil
}

func (s *MySQLMetadataStore) ClaimPurge(ctx context.Context, ref string, workerID string, now time.Time, leaseUntil time.Time) (*artifact.PurgeJob, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if workerID == "" {
		return nil, false, mysqlCodecInvalid("MySQL artifact purge worker ID is required")
	}
	if !leaseUntil.After(now) {
		return nil, false, mysqlCodecInvalid("MySQL artifact purge lease deadline must be after claim time")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, false, fmt.Errorf("begin MySQL artifact purge claim: %w", err)
	}
	digest := s.digest([]byte(ref))
	recordID, meta, err := s.lockAndReadMySQLMarkDeleted(ctx, tx, digest, ref)
	if err != nil {
		return nil, false, finalizeMySQLPurge(tx, err)
	}
	row, exists, err := lockMySQLPurgeJob(ctx, tx, recordID)
	if err != nil {
		return nil, false, finalizeMySQLPurge(tx, err)
	}
	if !exists {
		return nil, false, finalizeMySQLPurge(tx, mysqlPurgeJobNotFound(ref))
	}
	job, err := decodeMySQLPurgeJob(meta, row)
	if err != nil {
		return nil, false, finalizeMySQLPurge(tx, err)
	}
	if job.Status == artifact.PurgeStatusPurged {
		if err := finalizeMySQLPurge(tx, nil); err != nil {
			return nil, false, err
		}
		return job, false, nil
	}
	if job.Status == artifact.PurgeStatusLeased && job.LeaseOwner != workerID && job.LeaseUntil.After(now) {
		if err := finalizeMySQLPurge(tx, nil); err != nil {
			return nil, false, err
		}
		return job, false, nil
	}
	if job.LeaseVersion == int64(^uint64(0)>>1) {
		return nil, false, finalizeMySQLPurge(tx, mysqlCodecInvalid("MySQL artifact purge lease version cannot be incremented"))
	}

	nextVersion := job.LeaseVersion + 1
	updated := *job
	updated.Status = artifact.PurgeStatusLeased
	updated.UpdatedAt = now
	updated.LeaseOwner = workerID
	updated.LeaseUntil = leaseUntil
	updated.LeaseVersion = nextVersion
	nowColumns := encodeRequiredMySQLTime(now)
	leaseColumns := encodeRequiredMySQLTime(leaseUntil)
	result, err := tx.ExecContext(
		ctx,
		mysqlPurgeClaimJobUpdate,
		[]byte(updated.Status),
		nowColumns.Seconds.Int64,
		nowColumns.Nanoseconds.Int64,
		[]byte(workerID),
		leaseColumns.Seconds.Int64,
		leaseColumns.Nanoseconds.Int64,
		nextVersion,
		recordID,
	)
	if err != nil {
		return nil, false, finalizeMySQLPurge(tx, fmt.Errorf("update claimed MySQL artifact purge job: %w", err))
	}
	if err := requireOneMySQLPurgeRow(result, "purge claim job update"); err != nil {
		return nil, false, finalizeMySQLPurge(tx, err)
	}
	if meta.PurgeStatus != artifact.PurgeStatusLeased {
		result, err = tx.ExecContext(ctx, mysqlPurgeMetadataStatusUpdate, []byte(artifact.PurgeStatusLeased), recordID)
		if err != nil {
			return nil, false, finalizeMySQLPurge(tx, fmt.Errorf("update claimed MySQL artifact purge metadata: %w", err))
		}
		if err := requireOneMySQLPurgeRow(result, "purge claim metadata update"); err != nil {
			return nil, false, finalizeMySQLPurge(tx, err)
		}
	}
	if err := finalizeMySQLPurge(tx, nil); err != nil {
		return nil, false, err
	}
	return &updated, true, nil
}

func (s *MySQLMetadataStore) MarkPurgeSucceeded(ctx context.Context, ref string, workerID string, leaseVersion int64, at time.Time) (*artifact.ArtifactMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if workerID == "" || leaseVersion <= 0 || at.IsZero() {
		return nil, mysqlCodecInvalid("MySQL artifact purge success requires worker, positive lease version, and completion time")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin MySQL artifact purge success: %w", err)
	}
	digest := s.digest([]byte(ref))
	recordID, meta, err := s.lockAndReadMySQLMarkDeleted(ctx, tx, digest, ref)
	if err != nil {
		return nil, finalizeMySQLPurge(tx, err)
	}
	row, exists, err := lockMySQLPurgeJob(ctx, tx, recordID)
	if err != nil {
		return nil, finalizeMySQLPurge(tx, err)
	}
	if !exists {
		return nil, finalizeMySQLPurge(tx, mysqlPurgeJobNotFound(ref))
	}
	job, err := decodeMySQLPurgeJob(meta, row)
	if err != nil {
		return nil, finalizeMySQLPurge(tx, err)
	}
	if job.Status == artifact.PurgeStatusPurged {
		if err := finalizeMySQLPurge(tx, nil); err != nil {
			return nil, err
		}
		return meta, nil
	}
	if job.Status != artifact.PurgeStatusLeased || job.LeaseOwner != workerID || job.LeaseVersion != leaseVersion {
		return nil, finalizeMySQLPurge(tx, mysqlPurgeLeaseConflict(ref))
	}

	atColumns := encodeRequiredMySQLTime(at)
	result, err := tx.ExecContext(
		ctx,
		mysqlPurgeSuccessJobUpdate,
		[]byte(artifact.PurgeStatusPurged),
		[]byte{},
		atColumns.Seconds.Int64,
		atColumns.Nanoseconds.Int64,
		[]byte{},
		recordID,
		[]byte(artifact.PurgeStatusLeased),
		[]byte(workerID),
		leaseVersion,
	)
	if err != nil {
		return nil, finalizeMySQLPurge(tx, fmt.Errorf("update successful MySQL artifact purge job: %w", err))
	}
	if err := requireMySQLPurgeFence(result, ref); err != nil {
		return nil, finalizeMySQLPurge(tx, err)
	}
	result, err = tx.ExecContext(
		ctx,
		mysqlPurgeSuccessMetadataUpdate,
		[]byte(artifact.PurgeStatusPurged),
		atColumns.Seconds.Int64,
		atColumns.Nanoseconds.Int64,
		recordID,
	)
	if err != nil {
		return nil, finalizeMySQLPurge(tx, fmt.Errorf("update successful MySQL artifact purge metadata: %w", err))
	}
	if err := requireOneMySQLPurgeRow(result, "purge success metadata update"); err != nil {
		return nil, finalizeMySQLPurge(tx, err)
	}
	immediate := *meta
	immediate.PurgeStatus = artifact.PurgeStatusPurged
	immediate.PurgedAt = at
	if err := finalizeMySQLPurge(tx, nil); err != nil {
		return nil, err
	}
	return &immediate, nil
}

func (s *MySQLMetadataStore) MarkPurgeFailed(ctx context.Context, ref string, workerID string, leaseVersion int64, message string, at time.Time) (*artifact.PurgeJob, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if workerID == "" || leaseVersion <= 0 || at.IsZero() {
		return nil, mysqlCodecInvalid("MySQL artifact purge failure requires worker, positive lease version, and update time")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin MySQL artifact purge failure: %w", err)
	}
	digest := s.digest([]byte(ref))
	recordID, meta, err := s.lockAndReadMySQLMarkDeleted(ctx, tx, digest, ref)
	if err != nil {
		return nil, finalizeMySQLPurge(tx, err)
	}
	row, exists, err := lockMySQLPurgeJob(ctx, tx, recordID)
	if err != nil {
		return nil, finalizeMySQLPurge(tx, err)
	}
	if !exists {
		return nil, finalizeMySQLPurge(tx, mysqlPurgeJobNotFound(ref))
	}
	job, err := decodeMySQLPurgeJob(meta, row)
	if err != nil {
		return nil, finalizeMySQLPurge(tx, err)
	}
	if job.Status == artifact.PurgeStatusPurged {
		if err := finalizeMySQLPurge(tx, nil); err != nil {
			return nil, err
		}
		return job, nil
	}
	if job.Status != artifact.PurgeStatusLeased || job.LeaseOwner != workerID || job.LeaseVersion != leaseVersion {
		return nil, finalizeMySQLPurge(tx, mysqlPurgeLeaseConflict(ref))
	}
	if job.Attempts == int(^uint(0)>>1) {
		return nil, finalizeMySQLPurge(tx, mysqlCodecInvalid("MySQL artifact purge attempts cannot be incremented"))
	}

	nextAttempts := job.Attempts + 1
	atColumns := encodeRequiredMySQLTime(at)
	result, err := tx.ExecContext(
		ctx,
		mysqlPurgeFailureJobUpdate,
		[]byte(artifact.PurgeStatusRetrying),
		nextAttempts,
		[]byte(message),
		atColumns.Seconds.Int64,
		atColumns.Nanoseconds.Int64,
		[]byte{},
		recordID,
		[]byte(artifact.PurgeStatusLeased),
		[]byte(workerID),
		leaseVersion,
	)
	if err != nil {
		return nil, finalizeMySQLPurge(tx, fmt.Errorf("update failed MySQL artifact purge job: %w", err))
	}
	if err := requireMySQLPurgeFence(result, ref); err != nil {
		return nil, finalizeMySQLPurge(tx, err)
	}
	result, err = tx.ExecContext(ctx, mysqlPurgeMetadataStatusUpdate, []byte(artifact.PurgeStatusRetrying), recordID)
	if err != nil {
		return nil, finalizeMySQLPurge(tx, fmt.Errorf("update failed MySQL artifact purge metadata: %w", err))
	}
	if err := requireOneMySQLPurgeRow(result, "purge failure metadata update"); err != nil {
		return nil, finalizeMySQLPurge(tx, err)
	}
	updated := *job
	updated.Status = artifact.PurgeStatusRetrying
	updated.Attempts = nextAttempts
	updated.LastError = message
	updated.UpdatedAt = at
	updated.LeaseOwner = ""
	updated.LeaseUntil = time.Time{}
	if err := finalizeMySQLPurge(tx, nil); err != nil {
		return nil, err
	}
	return &updated, nil
}

func mysqlPurgeLeaseConflict(ref string) error {
	return &artifact.Error{Code: artifact.ErrConflict, Message: "stale artifact purge lease: " + ref}
}
