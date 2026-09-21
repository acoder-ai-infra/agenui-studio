package agentregistry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	configDraftTable        = "agent_config_drafts"
	configVersionTable      = "agent_config_versions"
	configReleaseTable      = "agent_config_releases"
	configReleaseEventTable = "agent_config_release_events"
)

const (
	configDraftColumns   = "tenant_id, agent_id, config_json, content_hash, revision, created_at_ms, updated_at_ms, created_by, updated_by"
	configVersionColumns = "tenant_id, agent_id, version, config_json, content_hash, source_draft_revision, created_at_ms, created_by, prepared_json"
	configReleaseColumns = "tenant_id, environment, agent_id, version, content_hash, revision, created_at_ms, updated_at_ms, updated_by"
)

type SQLAgentConfigControlStore struct {
	db *sql.DB
}

// NewSQLAgentConfigControlStore is the default SQLite-compatible
// implementation. MySQL uses the same parameterized DML and differs from
// their migration DDL.
func NewSQLAgentConfigControlStore(db *sql.DB) *SQLAgentConfigControlStore {
	return &SQLAgentConfigControlStore{db: db}
}

func (s *SQLAgentConfigControlStore) SaveDraft(ctx context.Context, draft AgentConfigDraft, expectedRevision int64) (AgentConfigDraft, error) {
	if s == nil || s.db == nil {
		return AgentConfigDraft{}, invalidConfigControl("sql database is required")
	}
	encoded, hash, err := encodeAgentConfig(draft.Config)
	if err != nil {
		return AgentConfigDraft{}, err
	}
	if hash != draft.ContentHash {
		return AgentConfigDraft{}, invalidConfigControl("draft content hash mismatch")
	}
	digest := identityTupleDigest(draft.TenantID, draft.AgentID)
	if expectedRevision == 0 {
		_, err = s.db.ExecContext(ctx, `INSERT INTO `+configDraftTable+` (
tenant_id, agent_id, config_json, content_hash, revision, created_at_ms, updated_at_ms, created_by, updated_by, identity_digest
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			draft.TenantID, draft.AgentID, encoded, draft.ContentHash, int64(1),
			draft.CreatedAt.UnixMilli(), draft.UpdatedAt.UnixMilli(), draft.CreatedBy, draft.UpdatedBy, digest,
		)
		if err != nil {
			if defaultSQLConflictClassifier(err) {
				return AgentConfigDraft{}, ErrAgentConfigControlConflict
			}
			return AgentConfigDraft{}, fmt.Errorf("save agent config draft: %w", err)
		}
	} else {
		result, updateErr := s.db.ExecContext(ctx, `UPDATE `+configDraftTable+`
SET config_json=?, content_hash=?, revision=revision+1, updated_at_ms=?, updated_by=?
WHERE identity_digest=? AND tenant_id=? AND agent_id=? AND revision=?`,
			encoded, draft.ContentHash, draft.UpdatedAt.UnixMilli(), draft.UpdatedBy,
			digest, draft.TenantID, draft.AgentID, expectedRevision,
		)
		if updateErr != nil {
			return AgentConfigDraft{}, fmt.Errorf("update agent config draft: %w", updateErr)
		}
		if err := requireConfigControlUpdate(result); err != nil {
			return AgentConfigDraft{}, err
		}
	}
	return s.GetDraft(ctx, draft.TenantID, draft.AgentID)
}

func (s *SQLAgentConfigControlStore) GetDraft(ctx context.Context, tenantID, agentID string) (AgentConfigDraft, error) {
	if s == nil || s.db == nil {
		return AgentConfigDraft{}, invalidConfigControl("sql database is required")
	}
	return getAgentConfigDraft(ctx, s.db, tenantID, agentID)
}

func (s *SQLAgentConfigControlStore) ListDrafts(ctx context.Context, tenantID string) ([]AgentConfigDraft, error) {
	if s == nil || s.db == nil {
		return nil, invalidConfigControl("sql database is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+configDraftColumns+` FROM `+configDraftTable+`
WHERE tenant_id=? ORDER BY updated_at_ms DESC, agent_id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list agent config drafts: %w", err)
	}
	defer rows.Close()
	var drafts []AgentConfigDraft
	for rows.Next() {
		draft, err := scanAgentConfigDraft(rows)
		if err != nil {
			return nil, err
		}
		drafts = append(drafts, draft)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list agent config drafts: %w", err)
	}
	return drafts, nil
}

func (s *SQLAgentConfigControlStore) AgentIDManaged(ctx context.Context, agentID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, invalidConfigControl("sql database is required")
	}
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM `+configDraftTable+` WHERE agent_id=? LIMIT 1`, agentID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check managed agent id: %w", err)
	}
	return true, nil
}

func (s *SQLAgentConfigControlStore) PublishDraft(ctx context.Context, req PublishAgentConfigDraftRequest, event AgentConfigReleaseEvent) (version AgentConfigVersion, release AgentConfigRelease, err error) {
	if s == nil || s.db == nil {
		return version, release, invalidConfigControl("sql database is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return version, release, fmt.Errorf("begin agent config publish: %w", err)
	}
	defer rollback(tx)

	draft, err := getAgentConfigDraft(ctx, tx, req.TenantID, req.AgentID)
	if err != nil {
		return version, release, err
	}
	if draft.Revision != req.ExpectedDraftRevision {
		return version, release, ErrAgentConfigControlConflict
	}
	version = AgentConfigVersion{
		TenantID: req.TenantID, AgentID: req.AgentID, Version: draft.Config.Version,
		Config: draft.Config, ContentHash: draft.ContentHash, SourceDraftRevision: draft.Revision,
		CreatedAt: event.OccurredAt, CreatedBy: req.Actor, Prepared: clonePreparedAgent(req.Prepared),
	}
	version, err = createOrGetAgentConfigVersion(ctx, tx, version)
	if err != nil {
		return AgentConfigVersion{}, release, err
	}
	current, currentErr := getAgentConfigRelease(ctx, tx, req.TenantID, req.Environment, req.AgentID)
	if currentErr != nil && !errors.Is(currentErr, ErrAgentConfigControlNotFound) {
		return AgentConfigVersion{}, AgentConfigRelease{}, currentErr
	}
	event.ToVersion = version.Version
	event.ContentHash = version.ContentHash
	release, changed, err := setAgentConfigRelease(ctx, tx, req.TenantID, req.Environment, req.AgentID, version, req.ExpectedReleaseRevision, req.Actor, event.OccurredAt)
	if err != nil {
		return AgentConfigVersion{}, AgentConfigRelease{}, err
	}
	if changed {
		if currentErr == nil {
			event.FromVersion = current.Version
		}
		event.ReleaseRevision = release.Revision
		if err := insertAgentConfigReleaseEvent(ctx, tx, event); err != nil {
			return AgentConfigVersion{}, AgentConfigRelease{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return AgentConfigVersion{}, AgentConfigRelease{}, fmt.Errorf("commit agent config publish: %w", err)
	}
	return version, release, nil
}

func (s *SQLAgentConfigControlStore) PromoteVersion(ctx context.Context, req PromoteAgentConfigVersionRequest, event AgentConfigReleaseEvent) (release AgentConfigRelease, err error) {
	if s == nil || s.db == nil {
		return release, invalidConfigControl("sql database is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return release, fmt.Errorf("begin agent config promotion: %w", err)
	}
	defer rollback(tx)
	version, err := getAgentConfigVersion(ctx, tx, req.TenantID, req.AgentID, req.Version)
	if err != nil {
		return release, err
	}
	current, currentErr := getAgentConfigRelease(ctx, tx, req.TenantID, req.Environment, req.AgentID)
	if currentErr != nil && !errors.Is(currentErr, ErrAgentConfigControlNotFound) {
		return release, currentErr
	}
	release, changed, err := setAgentConfigRelease(ctx, tx, req.TenantID, req.Environment, req.AgentID, version, req.ExpectedReleaseRevision, req.Actor, event.OccurredAt)
	if err != nil {
		return AgentConfigRelease{}, err
	}
	if changed {
		if currentErr == nil {
			event.FromVersion = current.Version
		}
		event.ToVersion = version.Version
		event.ContentHash = version.ContentHash
		event.ReleaseRevision = release.Revision
		if err := insertAgentConfigReleaseEvent(ctx, tx, event); err != nil {
			return AgentConfigRelease{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return AgentConfigRelease{}, fmt.Errorf("commit agent config promotion: %w", err)
	}
	return release, nil
}

func (s *SQLAgentConfigControlStore) GetRelease(ctx context.Context, tenantID string, environment ConfigEnvironment, agentID string) (AgentConfigRelease, error) {
	if s == nil || s.db == nil {
		return AgentConfigRelease{}, invalidConfigControl("sql database is required")
	}
	return getAgentConfigRelease(ctx, s.db, tenantID, environment, agentID)
}

func (s *SQLAgentConfigControlStore) GetVersion(ctx context.Context, tenantID, agentID, version string) (AgentConfigVersion, error) {
	if s == nil || s.db == nil {
		return AgentConfigVersion{}, invalidConfigControl("sql database is required")
	}
	return getAgentConfigVersion(ctx, s.db, tenantID, agentID, version)
}

func (s *SQLAgentConfigControlStore) ListVersions(ctx context.Context, tenantID, agentID string) ([]AgentConfigVersion, error) {
	if s == nil || s.db == nil {
		return nil, invalidConfigControl("sql database is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+configVersionColumns+` FROM `+configVersionTable+`
WHERE tenant_id=? AND agent_id=? ORDER BY created_at_ms DESC, version DESC`, tenantID, agentID)
	if err != nil {
		return nil, fmt.Errorf("list agent config versions: %w", err)
	}
	defer rows.Close()
	var versions []AgentConfigVersion
	for rows.Next() {
		version, err := scanAgentConfigVersion(rows)
		if err != nil {
			return nil, err
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list agent config versions: %w", err)
	}
	return versions, nil
}

type configControlRow interface {
	Scan(...any) error
}

type configControlQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getAgentConfigDraft(ctx context.Context, queryer configControlQueryer, tenantID, agentID string) (AgentConfigDraft, error) {
	digest := identityTupleDigest(tenantID, agentID)
	row := queryer.QueryRowContext(ctx, `SELECT `+configDraftColumns+` FROM `+configDraftTable+`
WHERE identity_digest=? AND tenant_id=? AND agent_id=?`, digest, tenantID, agentID)
	draft, err := scanAgentConfigDraft(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentConfigDraft{}, ErrAgentConfigControlNotFound
	}
	return draft, err
}

func scanAgentConfigDraft(row configControlRow) (AgentConfigDraft, error) {
	var draft AgentConfigDraft
	var encoded []byte
	var createdAtMS, updatedAtMS int64
	if err := row.Scan(&draft.TenantID, &draft.AgentID, &encoded, &draft.ContentHash, &draft.Revision,
		&createdAtMS, &updatedAtMS, &draft.CreatedBy, &draft.UpdatedBy); err != nil {
		return AgentConfigDraft{}, err
	}
	if err := json.Unmarshal(encoded, &draft.Config); err != nil {
		return AgentConfigDraft{}, fmt.Errorf("decode agent config draft: %w", err)
	}
	_, hash, err := encodeAgentConfig(draft.Config)
	if err != nil || hash != draft.ContentHash || draft.Config.AgentID != draft.AgentID {
		return AgentConfigDraft{}, fmt.Errorf("%w: draft content projection mismatch", ErrStoreCorrupt)
	}
	draft.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	draft.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
	return draft, nil
}

func createOrGetAgentConfigVersion(ctx context.Context, tx *sql.Tx, version AgentConfigVersion) (AgentConfigVersion, error) {
	encoded, hash, err := encodeAgentConfig(version.Config)
	if err != nil {
		return AgentConfigVersion{}, err
	}
	if hash != version.ContentHash {
		return AgentConfigVersion{}, invalidConfigControl("version content hash mismatch")
	}
	prepared, err := json.Marshal(version.Prepared)
	if err != nil {
		return AgentConfigVersion{}, fmt.Errorf("encode prepared agent config: %w", err)
	}
	if version.Prepared == nil {
		prepared = nil
	}
	digest := identityTupleDigest(version.TenantID, version.AgentID, version.Version)
	_, err = tx.ExecContext(ctx, `INSERT INTO `+configVersionTable+` (
tenant_id, agent_id, version, config_json, content_hash, source_draft_revision, created_at_ms, created_by, prepared_json, identity_digest
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, version.TenantID, version.AgentID, version.Version, encoded,
		version.ContentHash, version.SourceDraftRevision, version.CreatedAt.UnixMilli(), version.CreatedBy, prepared, digest)
	if err == nil {
		return version, nil
	}
	if !defaultSQLConflictClassifier(err) {
		return AgentConfigVersion{}, fmt.Errorf("create agent config version: %w", err)
	}
	existing, getErr := getAgentConfigVersion(ctx, tx, version.TenantID, version.AgentID, version.Version)
	if getErr != nil {
		return AgentConfigVersion{}, getErr
	}
	if existing.ContentHash != version.ContentHash {
		return AgentConfigVersion{}, ErrAgentConfigVersionImmutable
	}
	return existing, nil
}

func getAgentConfigVersion(ctx context.Context, queryer configControlQueryer, tenantID, agentID, version string) (AgentConfigVersion, error) {
	digest := identityTupleDigest(tenantID, agentID, version)
	row := queryer.QueryRowContext(ctx, `SELECT `+configVersionColumns+` FROM `+configVersionTable+`
WHERE identity_digest=? AND tenant_id=? AND agent_id=? AND version=?`, digest, tenantID, agentID, version)
	result, err := scanAgentConfigVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentConfigVersion{}, ErrAgentConfigControlNotFound
	}
	return result, err
}

func scanAgentConfigVersion(row configControlRow) (AgentConfigVersion, error) {
	var version AgentConfigVersion
	var encoded, prepared []byte
	var createdAtMS int64
	if err := row.Scan(&version.TenantID, &version.AgentID, &version.Version, &encoded, &version.ContentHash,
		&version.SourceDraftRevision, &createdAtMS, &version.CreatedBy, &prepared); err != nil {
		return AgentConfigVersion{}, err
	}
	if err := json.Unmarshal(encoded, &version.Config); err != nil {
		return AgentConfigVersion{}, fmt.Errorf("decode agent config version: %w", err)
	}
	if len(prepared) > 0 {
		if err := json.Unmarshal(prepared, &version.Prepared); err != nil {
			return AgentConfigVersion{}, fmt.Errorf("decode prepared agent config: %w", err)
		}
	}
	_, hash, err := encodeAgentConfig(version.Config)
	if err != nil || hash != version.ContentHash || version.Config.AgentID != version.AgentID || version.Config.Version != version.Version {
		return AgentConfigVersion{}, fmt.Errorf("%w: version content projection mismatch", ErrStoreCorrupt)
	}
	version.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	return version, nil
}

func setAgentConfigRelease(ctx context.Context, tx *sql.Tx, tenantID string, environment ConfigEnvironment, agentID string, version AgentConfigVersion, expectedRevision int64, actor string, now time.Time) (AgentConfigRelease, bool, error) {
	current, err := getAgentConfigRelease(ctx, tx, tenantID, environment, agentID)
	if errors.Is(err, ErrAgentConfigControlNotFound) {
		if expectedRevision != 0 {
			return AgentConfigRelease{}, false, ErrAgentConfigControlConflict
		}
		release := AgentConfigRelease{
			TenantID: tenantID, Environment: environment, AgentID: agentID, Version: version.Version,
			ContentHash: version.ContentHash, Revision: 1, CreatedAt: now, UpdatedAt: now, UpdatedBy: actor,
		}
		digest := identityTupleDigest(tenantID, string(environment), agentID)
		_, insertErr := tx.ExecContext(ctx, `INSERT INTO `+configReleaseTable+` (
tenant_id, environment, agent_id, version, content_hash, revision, created_at_ms, updated_at_ms, updated_by, identity_digest
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, tenantID, environment, agentID, version.Version, version.ContentHash,
			release.Revision, now.UnixMilli(), now.UnixMilli(), actor, digest)
		if insertErr != nil {
			if defaultSQLConflictClassifier(insertErr) {
				return AgentConfigRelease{}, false, ErrAgentConfigControlConflict
			}
			return AgentConfigRelease{}, false, fmt.Errorf("create agent config release: %w", insertErr)
		}
		return release, true, nil
	}
	if err != nil {
		return AgentConfigRelease{}, false, err
	}
	if current.Version == version.Version && current.ContentHash == version.ContentHash {
		if expectedRevision == 0 || expectedRevision == current.Revision {
			return current, false, nil
		}
	}
	if current.Revision != expectedRevision {
		return AgentConfigRelease{}, false, ErrAgentConfigControlConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+configReleaseTable+`
SET version=?, content_hash=?, revision=revision+1, updated_at_ms=?, updated_by=?
WHERE identity_digest=? AND tenant_id=? AND environment=? AND agent_id=? AND revision=?`,
		version.Version, version.ContentHash, now.UnixMilli(), actor,
		identityTupleDigest(tenantID, string(environment), agentID), tenantID, environment, agentID, expectedRevision)
	if err != nil {
		return AgentConfigRelease{}, false, fmt.Errorf("update agent config release: %w", err)
	}
	if err := requireConfigControlUpdate(result); err != nil {
		return AgentConfigRelease{}, false, err
	}
	current.Version = version.Version
	current.ContentHash = version.ContentHash
	current.Revision++
	current.UpdatedAt = now
	current.UpdatedBy = actor
	return current, true, nil
}

func getAgentConfigRelease(ctx context.Context, queryer configControlQueryer, tenantID string, environment ConfigEnvironment, agentID string) (AgentConfigRelease, error) {
	digest := identityTupleDigest(tenantID, string(environment), agentID)
	row := queryer.QueryRowContext(ctx, `SELECT `+configReleaseColumns+` FROM `+configReleaseTable+`
WHERE identity_digest=? AND tenant_id=? AND environment=? AND agent_id=?`, digest, tenantID, environment, agentID)
	var release AgentConfigRelease
	var createdAtMS, updatedAtMS int64
	if err := row.Scan(&release.TenantID, &release.Environment, &release.AgentID, &release.Version, &release.ContentHash,
		&release.Revision, &createdAtMS, &updatedAtMS, &release.UpdatedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AgentConfigRelease{}, ErrAgentConfigControlNotFound
		}
		return AgentConfigRelease{}, err
	}
	release.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	release.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
	return release, nil
}

func insertAgentConfigReleaseEvent(ctx context.Context, tx *sql.Tx, event AgentConfigReleaseEvent) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO `+configReleaseEventTable+` (
event_id, tenant_id, environment, agent_id, from_version, to_version, content_hash, release_revision, actor, reason, occurred_at_ms, event_id_digest
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, event.EventID, event.TenantID, event.Environment, event.AgentID,
		event.FromVersion, event.ToVersion, event.ContentHash, event.ReleaseRevision, event.Actor, event.Reason,
		event.OccurredAt.UnixMilli(), identityTupleDigest(event.EventID))
	if err != nil {
		return fmt.Errorf("append agent config release event: %w", err)
	}
	return nil
}

func requireConfigControlUpdate(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrAgentConfigControlConflict
	}
	return nil
}

var _ AgentConfigControlStore = (*SQLAgentConfigControlStore)(nil)

func clonePreparedAgent(input *PreparedAgent) *PreparedAgent {
	if input == nil {
		return nil
	}
	return &PreparedAgent{
		Card: cloneCapabilityCard(input.Card), Effective: cloneEffectiveConfig(input.Effective),
		ConfigSnapshots: cloneConfigSnapshotRecords(input.ConfigSnapshots), SubAgentVersions: cloneStringStringMap(input.SubAgentVersions),
	}
}

func cloneStringStringMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}
