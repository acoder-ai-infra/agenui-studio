package metastore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

type mysqlDigestFunc func([]byte) [sha256.Size]byte

const (
	mysqlCreateKeyKindIdempotency uint8 = 1
	mysqlCreateKeyKindRef         uint8 = 2
	mysqlCreateKeyKindID          uint8 = 3
)

type MySQLMetadataStore struct {
	db                         *sql.DB
	digest                     mysqlDigestFunc
	afterCreateExactMiss       func(uint8)
	beforeMarkDeletedLockQuery func()
	afterMarkDeletedExactRead  func()
}

type mysqlMarkDeletedTxFinalizer interface {
	Commit() error
	Rollback() error
}

func finalizeMySQLMarkDeleted(finalizer mysqlMarkDeletedTxFinalizer, primary error) error {
	if primary != nil {
		rollbackErr := finalizer.Rollback()
		if rollbackErr == nil || errors.Is(rollbackErr, sql.ErrTxDone) {
			return primary
		}
		return errors.Join(primary, fmt.Errorf("rollback MySQL artifact metadata MarkDeleted: %w", rollbackErr))
	}
	if err := finalizer.Commit(); err != nil {
		return fmt.Errorf("commit MySQL artifact metadata MarkDeleted: %w", err)
	}
	return nil
}

func NewMySQLMetadataStore(db *sql.DB) (*MySQLMetadataStore, error) {
	return newMySQLMetadataStore(db, sha256.Sum256)
}

func newMySQLMetadataStore(db *sql.DB, digest mysqlDigestFunc) (*MySQLMetadataStore, error) {
	if db == nil {
		return nil, &artifact.Error{
			Code:    artifact.ErrInvalidArgument,
			Message: "MySQL metadata database is required",
		}
	}
	return &MySQLMetadataStore{db: db, digest: digest}, nil
}

func (s *MySQLMetadataStore) MarkDeleted(ctx context.Context, ref string, reason artifact.DeleteReason, at time.Time) (*artifact.ArtifactMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	refDigest := s.digest([]byte(ref))
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin MySQL artifact metadata MarkDeleted: %w", err)
	}

	recordID, locked, err := s.lockAndReadMySQLMarkDeleted(ctx, tx, refDigest, ref)
	if err != nil {
		return nil, finalizeMySQLMarkDeleted(tx, err)
	}
	if s.afterMarkDeletedExactRead != nil {
		s.afterMarkDeletedExactRead()
	}
	if locked.Status == artifact.ArtifactStatusDeleted {
		if err := finalizeMySQLMarkDeleted(tx, nil); err != nil {
			return nil, err
		}
		return locked, nil
	}

	deletedAt := encodeNullableMySQLTime(at)
	result, err := tx.ExecContext(
		ctx,
		mysqlMetadataMarkDeletedUpdate,
		[]byte(artifact.ArtifactStatusDeleted),
		mysqlBindNullInt64(deletedAt.Seconds),
		mysqlBindNullInt64(deletedAt.Nanoseconds),
		[]byte(reason),
		[]byte(artifact.PurgeStatusPending),
		recordID,
	)
	if err != nil {
		return nil, finalizeMySQLMarkDeleted(tx, fmt.Errorf("update MySQL artifact metadata tombstone: %w", err))
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return nil, finalizeMySQLMarkDeleted(tx, fmt.Errorf("read MySQL artifact metadata tombstone RowsAffected: %w", err))
	}
	if rowsAffected != 1 {
		return nil, finalizeMySQLMarkDeleted(tx, mysqlCodecInvalid("MySQL artifact metadata tombstone update did not affect exactly one row"))
	}

	immediate := *locked
	immediate.Status = artifact.ArtifactStatusDeleted
	immediate.DeletedAt = at
	immediate.DeleteReason = reason
	immediate.PurgeStatus = artifact.PurgeStatusPending
	immediate.PurgedAt = time.Time{}
	if err := finalizeMySQLMarkDeleted(tx, nil); err != nil {
		return nil, err
	}
	return &immediate, nil
}

var _ artifact.MetadataStore = (*MySQLMetadataStore)(nil)

func (s *MySQLMetadataStore) lockAndReadMySQLMarkDeleted(
	ctx context.Context,
	tx *sql.Tx,
	refDigest [sha256.Size]byte,
	ref string,
) (uint64, *artifact.ArtifactMeta, error) {
	if s.beforeMarkDeletedLockQuery != nil {
		s.beforeMarkDeletedLockQuery()
	}
	rows, err := tx.QueryContext(ctx, mysqlMetadataMarkDeletedSelect, refDigest[:], []byte(ref))
	if err != nil {
		return 0, nil, fmt.Errorf("query locked MySQL artifact metadata tombstone: %w", err)
	}

	type lockedRecord struct {
		recordID uint64
		meta     *artifact.ArtifactMeta
	}
	records := make([]lockedRecord, 0, 2)
	var readErr error
	for rows.Next() {
		recordID, row, err := scanMySQLMarkDeletedRow(rows)
		if err != nil {
			readErr = err
			break
		}
		meta, err := decodeMySQLArtifactRow(row)
		if err != nil {
			readErr = err
			break
		}
		records = append(records, lockedRecord{recordID: recordID, meta: meta})
	}
	if err := rows.Err(); err != nil {
		readErr = errors.Join(readErr, fmt.Errorf("read locked MySQL artifact metadata tombstone rows: %w", err))
	}
	if err := rows.Close(); err != nil {
		readErr = errors.Join(readErr, fmt.Errorf("close locked MySQL artifact metadata tombstone rows: %w", err))
	}
	if readErr != nil {
		return 0, nil, readErr
	}
	if len(records) == 0 {
		return 0, nil, mysqlMetadataNotFound()
	}
	if len(records) != 1 {
		return 0, nil, mysqlCodecInvalid("multiple exact MySQL artifact metadata rows for MarkDeleted")
	}
	return records[0].recordID, records[0].meta, nil
}

func (s *MySQLMetadataStore) Create(ctx context.Context, meta artifact.ArtifactMeta, idempotencyKey string) (*artifact.ArtifactMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if idempotencyKey != "" {
		existing, err := s.GetByIdempotencyKey(ctx, idempotencyKey)
		if err == nil {
			return existing, nil
		}
		if !artifact.IsErrorCode(err, artifact.ErrNotFound) {
			return nil, err
		}
	}

	row, immediate, err := prepareMySQLArtifactRow(meta)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return s.recoverMySQLCreate(ctx, idempotencyKey, err)
	}

	if idempotencyKey != "" {
		existing, err := s.lockAndReadMySQLCreateExact(
			ctx,
			tx,
			mysqlCreateKeyKindIdempotency,
			mysqlMetadataSelectByIdempotencyKey,
			idempotencyKey,
		)
		if err == nil {
			return finishMySQLCreateWinner(tx, existing)
		}
		if !artifact.IsErrorCode(err, artifact.ErrNotFound) {
			return s.failMySQLCreate(ctx, tx, idempotencyKey, err, shouldRecoverMySQLCreate(err))
		}
	}

	_, err = s.lockAndReadMySQLCreateExact(
		ctx,
		tx,
		mysqlCreateKeyKindRef,
		mysqlMetadataSelectByRef,
		meta.ArtifactRef,
	)
	if err == nil {
		return s.failMySQLCreate(ctx, tx, idempotencyKey, mysqlCreateConflict("artifact ref", meta.ArtifactRef), false)
	}
	if !artifact.IsErrorCode(err, artifact.ErrNotFound) {
		return s.failMySQLCreate(ctx, tx, idempotencyKey, err, shouldRecoverMySQLCreate(err))
	}

	_, err = s.lockAndReadMySQLCreateExact(
		ctx,
		tx,
		mysqlCreateKeyKindID,
		mysqlMetadataSelectByID,
		meta.ArtifactID,
	)
	if err == nil {
		return s.failMySQLCreate(ctx, tx, idempotencyKey, mysqlCreateConflict("artifact ID", meta.ArtifactID), false)
	}
	if !artifact.IsErrorCode(err, artifact.ErrNotFound) {
		return s.failMySQLCreate(ctx, tx, idempotencyKey, err, shouldRecoverMySQLCreate(err))
	}

	if _, err := tx.ExecContext(ctx, mysqlMetadataInsert, s.mysqlCreateInsertArguments(row, idempotencyKey)...); err != nil {
		return s.failMySQLCreate(
			ctx,
			tx,
			idempotencyKey,
			fmt.Errorf("insert MySQL artifact metadata: %w", err),
			true,
		)
	}
	if err := tx.Commit(); err != nil {
		return s.recoverMySQLCreate(ctx, idempotencyKey, fmt.Errorf("commit MySQL artifact metadata Create: %w", err))
	}
	return immediate, nil
}

func (s *MySQLMetadataStore) lockAndReadMySQLCreateExact(
	ctx context.Context,
	tx *sql.Tx,
	kind uint8,
	query string,
	raw string,
) (*artifact.ArtifactMeta, error) {
	digest := s.digest([]byte(raw))
	if _, err := tx.ExecContext(ctx, mysqlMetadataKeyLockUpsert, int64(kind), digest[:]); err != nil {
		return nil, fmt.Errorf("upsert MySQL artifact metadata key lock kind %d: %w", kind, err)
	}
	var lockedDigest []byte
	if err := tx.QueryRowContext(ctx, mysqlMetadataKeyLockSelect, int64(kind), digest[:]).Scan(&lockedDigest); err != nil {
		return nil, fmt.Errorf("lock MySQL artifact metadata key kind %d: %w", kind, err)
	}

	existing, err := s.getExactFrom(ctx, tx, query, raw)
	if artifact.IsErrorCode(err, artifact.ErrNotFound) && s.afterCreateExactMiss != nil {
		s.afterCreateExactMiss(kind)
	}
	return existing, err
}

func finishMySQLCreateWinner(tx *sql.Tx, winner *artifact.ArtifactMeta) (*artifact.ArtifactMeta, error) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return nil, fmt.Errorf("rollback MySQL artifact metadata Create after idempotency winner: %w", err)
	}
	return winner, nil
}

func (s *MySQLMetadataStore) failMySQLCreate(
	ctx context.Context,
	tx *sql.Tx,
	idempotencyKey string,
	primary error,
	recoverWinner bool,
) (*artifact.ArtifactMeta, error) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return nil, errors.Join(primary, fmt.Errorf("rollback MySQL artifact metadata Create: %w", err))
	}
	if recoverWinner {
		return s.recoverMySQLCreate(ctx, idempotencyKey, primary)
	}
	return nil, primary
}

func (s *MySQLMetadataStore) recoverMySQLCreate(
	ctx context.Context,
	idempotencyKey string,
	primary error,
) (*artifact.ArtifactMeta, error) {
	if idempotencyKey != "" {
		winner, err := s.GetByIdempotencyKey(ctx, idempotencyKey)
		if err == nil {
			return winner, nil
		}
	}
	return nil, primary
}

func shouldRecoverMySQLCreate(err error) bool {
	return !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) &&
		!artifact.IsErrorCode(err, artifact.ErrConflict)
}

func mysqlCreateConflict(kind, raw string) error {
	return &artifact.Error{
		Code:    artifact.ErrConflict,
		Message: fmt.Sprintf("%s already exists: %s", kind, raw),
	}
}

func (s *MySQLMetadataStore) mysqlCreateInsertArguments(row mysqlArtifactRow, idempotencyKey string) []any {
	var idempotencyRaw any
	var idempotencyDigest any
	if idempotencyKey != "" {
		idempotencyRaw = []byte(idempotencyKey)
		digest := s.digest([]byte(idempotencyKey))
		idempotencyDigest = append([]byte(nil), digest[:]...)
	}
	return []any{
		row.artifactID,
		s.mysqlDigestBytes(row.artifactID),
		row.artifactRef,
		s.mysqlDigestBytes(row.artifactRef),
		idempotencyRaw,
		idempotencyDigest,
		row.tenantID,
		s.mysqlDigestBytes(row.tenantID),
		row.userID,
		row.sessionID,
		s.mysqlDigestBytes(row.sessionID),
		row.runID,
		s.mysqlDigestBytes(row.runID),
		row.stepID,
		row.ownerModule,
		s.mysqlDigestBytes(row.ownerModule),
		row.ownerID,
		s.mysqlDigestBytes(row.ownerID),
		row.artifactType,
		s.mysqlDigestBytes(row.artifactType),
		row.mimeType,
		row.name,
		row.sizeBytes,
		row.artifactHash,
		row.visibility,
		s.mysqlDigestBytes(row.visibility),
		row.storageBackend,
		row.storageKey,
		row.previewPayload,
		row.retentionPolicy,
		mysqlBindNullInt64(row.expiresAt.Seconds),
		mysqlBindNullInt64(row.expiresAt.Nanoseconds),
		row.createdBy,
		mysqlBindNullInt64(row.createdAt.Seconds),
		mysqlBindNullInt64(row.createdAt.Nanoseconds),
		row.status,
		row.derivedFromPayload,
		row.schemaVersion,
		mysqlBindNullInt64(row.deletedAt.Seconds),
		mysqlBindNullInt64(row.deletedAt.Nanoseconds),
		row.deleteReason,
		row.purgeStatus,
		mysqlBindNullInt64(row.purgedAt.Seconds),
		mysqlBindNullInt64(row.purgedAt.Nanoseconds),
		row.metadataPayload,
	}
}

func (s *MySQLMetadataStore) mysqlDigestBytes(raw []byte) []byte {
	digest := s.digest(raw)
	return append([]byte(nil), digest[:]...)
}

func mysqlBindNullInt64(value sql.NullInt64) any {
	if !value.Valid {
		return nil
	}
	return value.Int64
}

func (s *MySQLMetadataStore) GetByRef(ctx context.Context, ref string) (*artifact.ArtifactMeta, error) {
	return s.getExact(ctx, mysqlMetadataSelectByRef, ref)
}

func (s *MySQLMetadataStore) GetByID(ctx context.Context, artifactID string) (*artifact.ArtifactMeta, error) {
	return s.getExact(ctx, mysqlMetadataSelectByID, artifactID)
}

func (s *MySQLMetadataStore) GetByIdempotencyKey(ctx context.Context, key string) (*artifact.ArtifactMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key == "" {
		return nil, mysqlMetadataNotFound()
	}
	return s.getExact(ctx, mysqlMetadataSelectByIdempotencyKey, key)
}

func (s *MySQLMetadataStore) List(ctx context.Context, listQuery artifact.ListQuery) ([]artifact.ArtifactMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	query, args := s.mysqlListQuery(listQuery)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query MySQL artifact metadata list: %w", err)
	}
	defer rows.Close()

	result := make([]artifact.ArtifactMeta, 0)
	seenIDs := make(map[string]struct{})
	seenRefs := make(map[string]struct{})
	for rows.Next() {
		row, err := scanMySQLArtifactRow(rows)
		if err != nil {
			return nil, err
		}
		meta, err := decodeMySQLArtifactRow(row)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seenIDs[meta.ArtifactID]; duplicate {
			return nil, mysqlCodecInvalid("duplicate raw ArtifactID in selected MySQL artifact metadata rows")
		}
		if _, duplicate := seenRefs[meta.ArtifactRef]; duplicate {
			return nil, mysqlCodecInvalid("duplicate raw ArtifactRef in selected MySQL artifact metadata rows")
		}
		seenIDs[meta.ArtifactID] = struct{}{}
		seenRefs[meta.ArtifactRef] = struct{}{}
		result = append(result, *meta)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read MySQL artifact metadata list rows: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *MySQLMetadataStore) mysqlListQuery(listQuery artifact.ListQuery) (string, []any) {
	fragments := make([]string, 0, 9)
	args := make([]any, 0, 18)
	if !listQuery.IncludeDeleted {
		fragments = append(fragments, "status <> ?")
		args = append(args, []byte(artifact.ArtifactStatusDeleted))
	}
	appendExact := func(fragment string, raw string) {
		digest := s.digest([]byte(raw))
		fragments = append(fragments, fragment)
		args = append(args, append([]byte(nil), digest[:]...), []byte(raw))
	}
	if listQuery.TenantID != "" {
		appendExact("tenant_id_hash = ? AND tenant_id = ?", listQuery.TenantID)
	}
	if listQuery.SessionID != "" {
		appendExact("session_id_hash = ? AND session_id = ?", listQuery.SessionID)
	}
	if listQuery.RunID != "" {
		appendExact("run_id_hash = ? AND run_id = ?", listQuery.RunID)
	}
	if listQuery.OwnerModule != "" {
		appendExact("owner_module_hash = ? AND owner_module = ?", string(listQuery.OwnerModule))
	}
	if listQuery.OwnerID != "" {
		appendExact("owner_id_hash = ? AND owner_id = ?", listQuery.OwnerID)
	}
	if listQuery.Type != "" {
		appendExact("artifact_type_hash = ? AND artifact_type = ?", string(listQuery.Type))
	}
	if listQuery.Visibility != "" {
		appendExact("visibility_hash = ? AND visibility = ?", string(listQuery.Visibility))
	}
	if !listQuery.ExpiredAtOrBefore.IsZero() {
		seconds := listQuery.ExpiredAtOrBefore.Unix()
		fragments = append(fragments, "expires_at_sec IS NOT NULL AND (expires_at_sec < ? OR (expires_at_sec = ? AND expires_at_nano <= ?))")
		args = append(args, seconds, seconds, int64(listQuery.ExpiredAtOrBefore.Nanosecond()))
	}

	query := mysqlMetadataListSelect
	if len(fragments) != 0 {
		query += " WHERE " + strings.Join(fragments, " AND ")
	}
	return query + mysqlMetadataListOrder, args
}

func (s *MySQLMetadataStore) getExact(ctx context.Context, query string, raw string) (*artifact.ArtifactMeta, error) {
	return s.getExactFrom(ctx, s.db, query, raw)
}

type mysqlExactQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *MySQLMetadataStore) getExactFrom(
	ctx context.Context,
	queryer mysqlExactQueryer,
	query string,
	raw string,
) (*artifact.ArtifactMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	digest := s.digest([]byte(raw))
	rows, err := queryer.QueryContext(ctx, query, digest[:], []byte(raw))
	if err != nil {
		return nil, fmt.Errorf("query exact MySQL artifact metadata: %w", err)
	}
	defer rows.Close()

	var result *artifact.ArtifactMeta
	for rows.Next() {
		row, err := scanMySQLArtifactRow(rows)
		if err != nil {
			return nil, err
		}
		if result != nil {
			return nil, mysqlCodecInvalid("multiple exact MySQL artifact metadata rows")
		}
		result, err = decodeMySQLArtifactRow(row)
		if err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read exact MySQL artifact metadata rows: %w", err)
	}
	if result == nil {
		return nil, mysqlMetadataNotFound()
	}
	return result, nil
}

type mysqlRowScanner interface {
	Scan(...any) error
}

func scanMySQLArtifactRow(scanner mysqlRowScanner) (mysqlArtifactRow, error) {
	var row mysqlArtifactRow
	if err := scanner.Scan(mysqlArtifactRowScanDestinations(&row)...); err != nil {
		return mysqlArtifactRow{}, mysqlCodecInvalidCause("invalid MySQL artifact metadata row", err)
	}
	if mysqlArtifactRowHasNullRequiredRawColumn(row) {
		return mysqlArtifactRow{}, mysqlCodecInvalid("required MySQL artifact metadata column is NULL")
	}
	return row, nil
}

func scanMySQLMarkDeletedRow(scanner mysqlRowScanner) (uint64, mysqlArtifactRow, error) {
	var recordID uint64
	var row mysqlArtifactRow
	destinations := make([]any, 0, 1+len(mysqlArtifactRowScanDestinations(&row)))
	destinations = append(destinations, &recordID)
	destinations = append(destinations, mysqlArtifactRowScanDestinations(&row)...)
	if err := scanner.Scan(destinations...); err != nil {
		return 0, mysqlArtifactRow{}, mysqlCodecInvalidCause("invalid MySQL artifact metadata MarkDeleted row", err)
	}
	if mysqlArtifactRowHasNullRequiredRawColumn(row) {
		return 0, mysqlArtifactRow{}, mysqlCodecInvalid("required MySQL artifact metadata column is NULL")
	}
	return recordID, row, nil
}

func mysqlArtifactRowScanDestinations(row *mysqlArtifactRow) []any {
	return []any{
		&row.artifactID,
		&row.artifactRef,
		&row.tenantID,
		&row.userID,
		&row.sessionID,
		&row.runID,
		&row.stepID,
		&row.ownerModule,
		&row.ownerID,
		&row.artifactType,
		&row.mimeType,
		&row.name,
		&row.sizeBytes,
		&row.artifactHash,
		&row.visibility,
		&row.storageBackend,
		&row.storageKey,
		&row.previewPayload,
		&row.retentionPolicy,
		&row.expiresAt.Seconds,
		&row.expiresAt.Nanoseconds,
		&row.createdBy,
		&row.createdAt.Seconds,
		&row.createdAt.Nanoseconds,
		&row.status,
		&row.derivedFromPayload,
		&row.schemaVersion,
		&row.deletedAt.Seconds,
		&row.deletedAt.Nanoseconds,
		&row.deleteReason,
		&row.purgeStatus,
		&row.purgedAt.Seconds,
		&row.purgedAt.Nanoseconds,
		&row.metadataPayload,
	}
}

func mysqlArtifactRowHasNullRequiredRawColumn(row mysqlArtifactRow) bool {
	for _, value := range [][]byte{
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
		row.artifactHash,
		row.visibility,
		row.storageBackend,
		row.storageKey,
		row.previewPayload,
		row.retentionPolicy,
		row.createdBy,
		row.status,
		row.schemaVersion,
		row.deleteReason,
		row.purgeStatus,
	} {
		if value == nil {
			return true
		}
	}
	return false
}

func mysqlMetadataNotFound() error {
	return &artifact.Error{
		Code:    artifact.ErrNotFound,
		Message: "artifact metadata not found",
	}
}
