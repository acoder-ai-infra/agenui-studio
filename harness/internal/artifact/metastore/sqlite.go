package metastore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

// SQLiteMetadataStore is a durable, single-process artifact metadata store.
// Without a durable metadata store the ref→object mapping lived only
// in memory, so every service restart orphaned in-flight runs (their context
// snapshots became unresolvable and Resume failed with
// CONTEXT_SNAPSHOT_UNAVAILABLE). Persisting metadata here lets HITL runs survive
// restarts and resume normally.
//
// Each ArtifactMeta / PurgeJob is stored as a JSON blob (the canonical value)
// alongside indexed columns used only for lookups and List filtering. It does
// not provide the cross-node locking semantics of the MySQL store, but an
// operator may select the single-process topology explicitly in any environment.
type SQLiteMetadataStore struct {
	db *sql.DB
	mu sync.Mutex
}

// NewSQLiteMetadataStore creates the store and ensures its tables exist. The db
// is owned by the caller (the shared SQLite pool) and must not be closed
// by this store.
func NewSQLiteMetadataStore(db *sql.DB) (*SQLiteMetadataStore, error) {
	if db == nil {
		return nil, &artifact.Error{Code: artifact.ErrInvalidArgument, Message: "sqlite metadata database is required"}
	}
	s := &SQLiteMetadataStore{db: db}
	if err := s.ensureSchema(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

// ProductionReady reports that this store does not provide multi-instance
// transactional readiness.
func (*SQLiteMetadataStore) ProductionReady() bool { return false }

var (
	_ artifact.MetadataStore = (*SQLiteMetadataStore)(nil)
	_ artifact.PurgeStore    = (*SQLiteMetadataStore)(nil)
)

const sqliteMetadataSchema = `
CREATE TABLE IF NOT EXISTS artifact_metadata (
    artifact_ref    TEXT PRIMARY KEY,
    artifact_id     TEXT NOT NULL,
    idempotency_key TEXT,
    tenant_id       TEXT NOT NULL DEFAULT '',
    session_id      TEXT NOT NULL DEFAULT '',
    run_id          TEXT NOT NULL DEFAULT '',
    owner_module    TEXT NOT NULL DEFAULT '',
    owner_id        TEXT NOT NULL DEFAULT '',
    artifact_type   TEXT NOT NULL DEFAULT '',
    visibility      TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT '',
    expires_at_nano INTEGER,
    created_at_nano INTEGER NOT NULL DEFAULT 0,
    meta_json       TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS artifact_metadata_id_idx ON artifact_metadata(artifact_id);
CREATE UNIQUE INDEX IF NOT EXISTS artifact_metadata_idem_idx ON artifact_metadata(idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS artifact_metadata_scope_idx ON artifact_metadata(tenant_id, session_id, run_id);
CREATE TABLE IF NOT EXISTS artifact_purge_jobs (
    artifact_ref    TEXT PRIMARY KEY,
    status          TEXT NOT NULL DEFAULT '',
    tenant_id       TEXT NOT NULL DEFAULT '',
    session_id      TEXT NOT NULL DEFAULT '',
    run_id          TEXT NOT NULL DEFAULT '',
    created_at_nano INTEGER NOT NULL DEFAULT 0,
    job_json        TEXT NOT NULL
);`

func (s *SQLiteMetadataStore) ensureSchema(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, sqliteMetadataSchema); err != nil {
		return fmt.Errorf("create sqlite artifact metadata schema: %w", err)
	}
	return nil
}

// --- MetadataStore -----------------------------------------------------------

func (s *SQLiteMetadataStore) Create(ctx context.Context, meta artifact.ArtifactMeta, idempotencyKey string) (*artifact.ArtifactMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if idempotencyKey != "" {
		existing, err := s.getExact(ctx, "idempotency_key", idempotencyKey)
		if err == nil {
			return existing, nil
		}
		if !artifact.IsErrorCode(err, artifact.ErrNotFound) {
			return nil, err
		}
	}
	if _, err := s.getExact(ctx, "artifact_ref", meta.ArtifactRef); err == nil {
		return nil, &artifact.Error{Code: artifact.ErrConflict, Message: fmt.Sprintf("artifact ref already exists: %s", meta.ArtifactRef)}
	} else if !artifact.IsErrorCode(err, artifact.ErrNotFound) {
		return nil, err
	}
	if existing, err := s.getExact(ctx, "artifact_id", meta.ArtifactID); err == nil {
		if existing.ArtifactRef != meta.ArtifactRef {
			return nil, &artifact.Error{Code: artifact.ErrConflict, Message: fmt.Sprintf("artifact id already exists: %s", meta.ArtifactID)}
		}
	} else if !artifact.IsErrorCode(err, artifact.ErrNotFound) {
		return nil, err
	}

	blob, err := json.Marshal(meta)
	if err != nil {
		return nil, &artifact.Error{Code: artifact.ErrInvalidArgument, Message: fmt.Sprintf("marshal artifact metadata: %v", err)}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO artifact_metadata
		(artifact_ref, artifact_id, idempotency_key, tenant_id, session_id, run_id, owner_module, owner_id, artifact_type, visibility, status, expires_at_nano, created_at_nano, meta_json)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		meta.ArtifactRef, meta.ArtifactID, nullString(idempotencyKey),
		meta.TenantID, meta.SessionID, meta.RunID, string(meta.OwnerModule), meta.OwnerID,
		string(meta.ArtifactType), string(meta.Visibility), string(meta.Status),
		nanoOrNull(meta.ExpiresAt), createdNano(meta.CreatedAt), string(blob),
	); err != nil {
		return nil, fmt.Errorf("insert sqlite artifact metadata: %w", err)
	}
	// Return a faithful deep clone of the input, NOT a JSON round-trip: the Store
	// validates Create's result with reflect.DeepEqual against the in-memory
	// original, and JSON would strip time.Time monotonic clocks and coerce
	// map[string]any number types, spuriously failing that check.
	return cloneMeta(meta), nil
}

func (s *SQLiteMetadataStore) GetByRef(ctx context.Context, ref string) (*artifact.ArtifactMeta, error) {
	meta, err := s.getExact(ctx, "artifact_ref", ref)
	if artifact.IsErrorCode(err, artifact.ErrNotFound) {
		return nil, &artifact.Error{Code: artifact.ErrNotFound, Message: fmt.Sprintf("artifact not found: %s", ref)}
	}
	return meta, err
}

func (s *SQLiteMetadataStore) GetByID(ctx context.Context, artifactID string) (*artifact.ArtifactMeta, error) {
	meta, err := s.getExact(ctx, "artifact_id", artifactID)
	if artifact.IsErrorCode(err, artifact.ErrNotFound) {
		return nil, &artifact.Error{Code: artifact.ErrNotFound, Message: fmt.Sprintf("artifact not found: %s", artifactID)}
	}
	return meta, err
}

func (s *SQLiteMetadataStore) GetByIdempotencyKey(ctx context.Context, key string) (*artifact.ArtifactMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key == "" {
		return nil, &artifact.Error{Code: artifact.ErrNotFound, Message: "idempotency key not found"}
	}
	meta, err := s.getExact(ctx, "idempotency_key", key)
	if artifact.IsErrorCode(err, artifact.ErrNotFound) {
		return nil, &artifact.Error{Code: artifact.ErrNotFound, Message: "idempotency key not found"}
	}
	return meta, err
}

func (s *SQLiteMetadataStore) List(ctx context.Context, query artifact.ListQuery) ([]artifact.ArtifactMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sqlText := `SELECT meta_json FROM artifact_metadata`
	fragments := make([]string, 0, 8)
	args := make([]any, 0, 8)
	if !query.IncludeDeleted {
		fragments = append(fragments, "status <> ?")
		args = append(args, string(artifact.ArtifactStatusDeleted))
	}
	appendEq := func(column, value string) {
		fragments = append(fragments, column+" = ?")
		args = append(args, value)
	}
	if query.TenantID != "" {
		appendEq("tenant_id", query.TenantID)
	}
	if query.SessionID != "" {
		appendEq("session_id", query.SessionID)
	}
	if query.RunID != "" {
		appendEq("run_id", query.RunID)
	}
	if query.OwnerModule != "" {
		appendEq("owner_module", string(query.OwnerModule))
	}
	if query.OwnerID != "" {
		appendEq("owner_id", query.OwnerID)
	}
	if query.Type != "" {
		appendEq("artifact_type", string(query.Type))
	}
	if query.Visibility != "" {
		appendEq("visibility", string(query.Visibility))
	}
	if len(fragments) > 0 {
		sqlText += " WHERE " + joinAnd(fragments)
	}
	sqlText += " ORDER BY created_at_nano ASC, artifact_id ASC"

	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("query sqlite artifact metadata list: %w", err)
	}
	defer rows.Close()

	out := make([]artifact.ArtifactMeta, 0)
	for rows.Next() {
		var blob string
		if err := rows.Scan(&blob); err != nil {
			return nil, fmt.Errorf("scan sqlite artifact metadata list: %w", err)
		}
		meta, err := decodeMeta([]byte(blob))
		if err != nil {
			return nil, err
		}
		// ExpiredAtOrBefore mirrors the memory store: keep only artifacts with a
		// non-zero expiry at or before the cutoff.
		if !query.ExpiredAtOrBefore.IsZero() && (meta.ExpiresAt.IsZero() || meta.ExpiresAt.After(query.ExpiredAtOrBefore)) {
			continue
		}
		out = append(out, *meta)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read sqlite artifact metadata list rows: %w", err)
	}
	return out, nil
}

func (s *SQLiteMetadataStore) MarkDeleted(ctx context.Context, ref string, reason artifact.DeleteReason, at time.Time) (*artifact.ArtifactMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	meta, err := s.getExact(ctx, "artifact_ref", ref)
	if err != nil {
		if artifact.IsErrorCode(err, artifact.ErrNotFound) {
			return nil, &artifact.Error{Code: artifact.ErrNotFound, Message: fmt.Sprintf("artifact not found: %s", ref)}
		}
		return nil, err
	}
	if meta.Status == artifact.ArtifactStatusDeleted {
		return meta, nil
	}
	meta.Status = artifact.ArtifactStatusDeleted
	meta.DeletedAt = at
	meta.DeleteReason = reason
	meta.PurgeStatus = artifact.PurgeStatusPending
	if err := s.updateMeta(ctx, *meta); err != nil {
		return nil, err
	}
	return meta, nil
}

// --- PurgeStore --------------------------------------------------------------

func (s *SQLiteMetadataStore) RequestPurge(ctx context.Context, ref string, reason artifact.DeleteReason, at time.Time) (*artifact.ArtifactMeta, *artifact.PurgeJob, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	meta, err := s.getExact(ctx, "artifact_ref", ref)
	if err != nil {
		if artifact.IsErrorCode(err, artifact.ErrNotFound) {
			return nil, nil, &artifact.Error{Code: artifact.ErrNotFound, Message: fmt.Sprintf("artifact not found: %s", ref)}
		}
		return nil, nil, err
	}
	if existing, ok, err := s.getJob(ctx, ref); err != nil {
		return nil, nil, err
	} else if ok {
		return meta, existing, nil
	}
	if meta.Status != artifact.ArtifactStatusDeleted {
		meta.Status = artifact.ArtifactStatusDeleted
		meta.DeletedAt = at
		meta.DeleteReason = reason
	}
	meta.PurgeStatus = artifact.PurgeStatusPending
	meta.PurgedAt = time.Time{}
	if err := s.updateMeta(ctx, *meta); err != nil {
		return nil, nil, err
	}
	job := artifact.PurgeJob{
		ArtifactRef: ref, TenantID: meta.TenantID, SessionID: meta.SessionID, RunID: meta.RunID,
		StorageKey: meta.StorageKey, Status: artifact.PurgeStatusPending, CreatedAt: at, UpdatedAt: at,
	}
	if err := s.upsertJob(ctx, job); err != nil {
		return nil, nil, err
	}
	return meta, &job, nil
}

func (s *SQLiteMetadataStore) GetPurgeJob(ctx context.Context, ref string) (*artifact.PurgeJob, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	job, ok, err := s.getJob(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &artifact.Error{Code: artifact.ErrNotFound, Message: fmt.Sprintf("purge job not found: %s", ref)}
	}
	return job, nil
}

func (s *SQLiteMetadataStore) ListPendingPurges(ctx context.Context, query artifact.PurgeQuery) ([]artifact.PurgeJob, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := query.Limit
	if limit <= 0 {
		limit = 100
	}
	readyAt := query.ReadyAt
	if readyAt.IsZero() {
		readyAt = time.Now()
	}
	sqlText := `SELECT job_json FROM artifact_purge_jobs`
	fragments := make([]string, 0, 4)
	args := make([]any, 0, 4)
	if query.TenantID != "" {
		fragments = append(fragments, "tenant_id = ?")
		args = append(args, query.TenantID)
	}
	if query.SessionID != "" {
		fragments = append(fragments, "session_id = ?")
		args = append(args, query.SessionID)
	}
	if query.RunID != "" {
		fragments = append(fragments, "run_id = ?")
		args = append(args, query.RunID)
	}
	if len(fragments) > 0 {
		sqlText += " WHERE " + joinAnd(fragments)
	}
	sqlText += " ORDER BY created_at_nano ASC, artifact_ref ASC"

	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("query sqlite purge jobs: %w", err)
	}
	defer rows.Close()

	jobs := make([]artifact.PurgeJob, 0, limit)
	for rows.Next() {
		var blob string
		if err := rows.Scan(&blob); err != nil {
			return nil, fmt.Errorf("scan sqlite purge job: %w", err)
		}
		var job artifact.PurgeJob
		if err := json.Unmarshal([]byte(blob), &job); err != nil {
			return nil, &artifact.Error{Code: artifact.ErrInvalidArgument, Message: fmt.Sprintf("decode purge job: %v", err)}
		}
		claimable := job.Status == artifact.PurgeStatusPending ||
			job.Status == artifact.PurgeStatusRetrying ||
			(job.Status == artifact.PurgeStatusLeased && !job.LeaseUntil.After(readyAt))
		if !claimable {
			continue
		}
		jobs = append(jobs, job)
		if len(jobs) >= limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read sqlite purge job rows: %w", err)
	}
	return jobs, nil
}

func (s *SQLiteMetadataStore) ClaimPurge(ctx context.Context, ref string, workerID string, now time.Time, leaseUntil time.Time) (*artifact.PurgeJob, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok, err := s.getJob(ctx, ref)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, &artifact.Error{Code: artifact.ErrNotFound, Message: fmt.Sprintf("purge job not found: %s", ref)}
	}
	if job.Status == artifact.PurgeStatusPurged {
		return job, false, nil
	}
	if job.Status == artifact.PurgeStatusLeased && job.LeaseOwner != workerID && job.LeaseUntil.After(now) {
		return job, false, nil
	}
	if job.Status != artifact.PurgeStatusPending && job.Status != artifact.PurgeStatusRetrying && job.Status != artifact.PurgeStatusLeased {
		return job, false, nil
	}
	job.Status = artifact.PurgeStatusLeased
	job.LeaseOwner = workerID
	job.LeaseUntil = leaseUntil
	job.LeaseVersion++
	job.UpdatedAt = now
	if err := s.upsertJob(ctx, *job); err != nil {
		return nil, false, err
	}
	if err := s.setMetaPurgeStatus(ctx, ref, artifact.PurgeStatusLeased); err != nil {
		return nil, false, err
	}
	return job, true, nil
}

func (s *SQLiteMetadataStore) MarkPurgeSucceeded(ctx context.Context, ref string, workerID string, leaseVersion int64, at time.Time) (*artifact.ArtifactMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	job, jobOK, err := s.getJob(ctx, ref)
	if err != nil {
		return nil, err
	}
	meta, err := s.getExact(ctx, "artifact_ref", ref)
	metaOK := err == nil
	if err != nil && !artifact.IsErrorCode(err, artifact.ErrNotFound) {
		return nil, err
	}
	if !metaOK || !jobOK {
		return nil, &artifact.Error{Code: artifact.ErrNotFound, Message: fmt.Sprintf("purge state not found: %s", ref)}
	}
	if job.Status == artifact.PurgeStatusPurged {
		return meta, nil
	}
	if job.Status != artifact.PurgeStatusLeased || job.LeaseOwner != workerID || job.LeaseVersion != leaseVersion {
		return nil, &artifact.Error{Code: artifact.ErrConflict, Message: fmt.Sprintf("stale purge lease: %s", ref)}
	}
	job.Status = artifact.PurgeStatusPurged
	job.LastError = ""
	job.LeaseOwner = ""
	job.LeaseUntil = time.Time{}
	job.UpdatedAt = at
	if err := s.upsertJob(ctx, *job); err != nil {
		return nil, err
	}
	meta.PurgeStatus = artifact.PurgeStatusPurged
	meta.PurgedAt = at
	if err := s.updateMeta(ctx, *meta); err != nil {
		return nil, err
	}
	return meta, nil
}

func (s *SQLiteMetadataStore) MarkPurgeFailed(ctx context.Context, ref string, workerID string, leaseVersion int64, message string, at time.Time) (*artifact.PurgeJob, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok, err := s.getJob(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &artifact.Error{Code: artifact.ErrNotFound, Message: fmt.Sprintf("purge job not found: %s", ref)}
	}
	if job.Status == artifact.PurgeStatusPurged {
		return job, nil
	}
	if job.Status != artifact.PurgeStatusLeased || job.LeaseOwner != workerID || job.LeaseVersion != leaseVersion {
		return nil, &artifact.Error{Code: artifact.ErrConflict, Message: fmt.Sprintf("stale purge lease: %s", ref)}
	}
	job.Status = artifact.PurgeStatusRetrying
	job.Attempts++
	job.LastError = message
	job.LeaseOwner = ""
	job.LeaseUntil = time.Time{}
	job.UpdatedAt = at
	if err := s.upsertJob(ctx, *job); err != nil {
		return nil, err
	}
	if err := s.setMetaPurgeStatus(ctx, ref, artifact.PurgeStatusRetrying); err != nil {
		return nil, err
	}
	return job, nil
}

// --- helpers -----------------------------------------------------------------

// getExact reads a single artifact row by an exact column match. column must be
// a trusted literal ("artifact_ref" / "artifact_id" / "idempotency_key").
func (s *SQLiteMetadataStore) getExact(ctx context.Context, column, value string) (*artifact.ArtifactMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var blob string
	err := s.db.QueryRowContext(ctx, `SELECT meta_json FROM artifact_metadata WHERE `+column+` = ?`, value).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &artifact.Error{Code: artifact.ErrNotFound, Message: "artifact metadata not found"}
	}
	if err != nil {
		return nil, fmt.Errorf("query sqlite artifact metadata by %s: %w", column, err)
	}
	return decodeMeta([]byte(blob))
}

func (s *SQLiteMetadataStore) updateMeta(ctx context.Context, meta artifact.ArtifactMeta) error {
	blob, err := json.Marshal(meta)
	if err != nil {
		return &artifact.Error{Code: artifact.ErrInvalidArgument, Message: fmt.Sprintf("marshal artifact metadata: %v", err)}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE artifact_metadata
		SET status = ?, expires_at_nano = ?, meta_json = ?
		WHERE artifact_ref = ?`,
		string(meta.Status), nanoOrNull(meta.ExpiresAt), string(blob), meta.ArtifactRef,
	); err != nil {
		return fmt.Errorf("update sqlite artifact metadata: %w", err)
	}
	return nil
}

func (s *SQLiteMetadataStore) setMetaPurgeStatus(ctx context.Context, ref string, status artifact.PurgeStatus) error {
	meta, err := s.getExact(ctx, "artifact_ref", ref)
	if err != nil {
		if artifact.IsErrorCode(err, artifact.ErrNotFound) {
			return nil
		}
		return err
	}
	meta.PurgeStatus = status
	return s.updateMeta(ctx, *meta)
}

func (s *SQLiteMetadataStore) getJob(ctx context.Context, ref string) (*artifact.PurgeJob, bool, error) {
	var blob string
	err := s.db.QueryRowContext(ctx, `SELECT job_json FROM artifact_purge_jobs WHERE artifact_ref = ?`, ref).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("query sqlite purge job: %w", err)
	}
	var job artifact.PurgeJob
	if err := json.Unmarshal([]byte(blob), &job); err != nil {
		return nil, false, &artifact.Error{Code: artifact.ErrInvalidArgument, Message: fmt.Sprintf("decode purge job: %v", err)}
	}
	return &job, true, nil
}

func (s *SQLiteMetadataStore) upsertJob(ctx context.Context, job artifact.PurgeJob) error {
	blob, err := json.Marshal(job)
	if err != nil {
		return &artifact.Error{Code: artifact.ErrInvalidArgument, Message: fmt.Sprintf("marshal purge job: %v", err)}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO artifact_purge_jobs
		(artifact_ref, status, tenant_id, session_id, run_id, created_at_nano, job_json)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(artifact_ref) DO UPDATE SET
			status = excluded.status,
			tenant_id = excluded.tenant_id,
			session_id = excluded.session_id,
			run_id = excluded.run_id,
			created_at_nano = excluded.created_at_nano,
			job_json = excluded.job_json`,
		job.ArtifactRef, string(job.Status), job.TenantID, job.SessionID, job.RunID, createdNano(job.CreatedAt), string(blob),
	); err != nil {
		return fmt.Errorf("upsert sqlite purge job: %w", err)
	}
	return nil
}

func decodeMeta(blob []byte) (*artifact.ArtifactMeta, error) {
	var meta artifact.ArtifactMeta
	if err := json.Unmarshal(blob, &meta); err != nil {
		return nil, &artifact.Error{Code: artifact.ErrInvalidArgument, Message: fmt.Sprintf("decode artifact metadata: %v", err)}
	}
	return &meta, nil
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nanoOrNull(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixNano()
}

func createdNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func joinAnd(fragments []string) string {
	out := ""
	for i, fragment := range fragments {
		if i > 0 {
			out += " AND "
		}
		out += fragment
	}
	return out
}
