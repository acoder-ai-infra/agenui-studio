package modeladmin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ManagedProvider is a stored provider definition with its revision and audit
// metadata.
type ManagedProvider struct {
	Definition ProviderDefinition `json:"definition"`
	Revision   int64              `json:"revision"`
	CreatedAt  time.Time          `json:"created_at"`
	UpdatedAt  time.Time          `json:"updated_at"`
	UpdatedBy  string             `json:"updated_by"`
}

// SQLManagedRegistry is a tenant-scoped model provider store with
// optimistic-revision CAS, mirroring mcp.SQLManagedRegistry.
type SQLManagedRegistry struct {
	db  *sql.DB
	now func() time.Time
}

// NewSQLManagedRegistry constructs a store over the shared managed SQL database.
func NewSQLManagedRegistry(db *sql.DB) *SQLManagedRegistry {
	return &SQLManagedRegistry{db: db, now: time.Now}
}

// Save creates (expectedRevision==0) or updates (expectedRevision>0) a provider
// under optimistic concurrency. A lost update returns ErrManagedConflict.
func (r *SQLManagedRegistry) Save(ctx context.Context, tenantID, actor string, definition ProviderDefinition, expectedRevision int64) (ManagedProvider, error) {
	if r == nil || r.db == nil || tenantID == "" || actor == "" || expectedRevision < 0 {
		return ManagedProvider{}, errInvalid("database, tenant, actor and non-negative revision are required")
	}
	definition.TenantID = tenantID
	definition.Scope = ScopeTenant
	if err := validateManagedDefinition(definition); err != nil {
		return ManagedProvider{}, err
	}
	now := r.now().UTC()
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return ManagedProvider{}, err
	}
	defer func() { _ = tx.Rollback() }()
	providers, err := lockAndListTenantProviders(ctx, tx, tenantID)
	if err != nil {
		return ManagedProvider{}, err
	}
	targetIndex := -1
	for index := range providers {
		if providers[index].Definition.ID == definition.ID {
			targetIndex = index
			break
		}
	}
	if expectedRevision == 0 && targetIndex >= 0 {
		return ManagedProvider{}, ErrManagedConflict
	}
	if expectedRevision > 0 && (targetIndex < 0 || providers[targetIndex].Revision != expectedRevision) {
		return ManagedProvider{}, ErrManagedConflict
	}
	creating := targetIndex < 0
	// Preserve existing clients that did not need to mark the first provider as
	// default. A first create is promoted deterministically; subsequent creates
	// remain non-default unless the caller explicitly requests a switch.
	if creating && len(providers) == 0 && !definition.IsDefault {
		definition.IsDefault = true
		if definition.DefaultModel == "" && len(definition.Models) > 0 {
			definition.DefaultModel = definition.Models[0]
		}
		if err := validateManagedDefinition(definition); err != nil {
			return ManagedProvider{}, fmt.Errorf("first managed model provider becomes tenant default: %w", err)
		}
	}

	var saved ManagedProvider
	if creating {
		saved = ManagedProvider{Definition: definition, Revision: 1, CreatedAt: now, UpdatedAt: now, UpdatedBy: actor}
		providers = append(providers, saved)
		targetIndex = len(providers) - 1
	} else {
		saved = providers[targetIndex]
		saved.Definition = definition
		saved.Revision++
		saved.UpdatedAt = now
		saved.UpdatedBy = actor
		providers[targetIndex] = saved
	}
	var demoted []int
	if definition.IsDefault {
		for index := range providers {
			if index == targetIndex || !providers[index].Definition.IsDefault {
				continue
			}
			providers[index].Definition.IsDefault = false
			providers[index].Revision++
			providers[index].UpdatedAt = now
			providers[index].UpdatedBy = actor
			demoted = append(demoted, index)
		}
	}
	if err := validateTenantDefaultInvariant(providers); err != nil {
		return ManagedProvider{}, err
	}
	encoded, err := json.Marshal(definition)
	if err != nil {
		return ManagedProvider{}, err
	}

	// Persist demotions first; the transaction keeps the temporary zero-default
	// state invisible. Each row retains optimistic revision protection even
	// though the tenant write lock serializes competing default switches.
	for _, index := range demoted {
		provider := providers[index]
		if err := updateManagedProviderTx(ctx, tx, tenantID, provider, provider.Revision-1); err != nil {
			return ManagedProvider{}, err
		}
	}
	if creating {
		_, err = tx.ExecContext(ctx, `INSERT INTO model_providers (
tenant_id, provider_id, definition_json, revision, created_at_ms, updated_at_ms, updated_by, identity_digest
) VALUES (?, ?, ?, 1, ?, ?, ?, ?)`, tenantID, definition.ID, encoded, now.UnixMilli(), now.UnixMilli(), actor, identityDigest(tenantID, definition.ID))
	} else {
		result, updateErr := tx.ExecContext(ctx, `UPDATE model_providers SET definition_json=?, revision=revision+1, updated_at_ms=?, updated_by=?
WHERE identity_digest=? AND tenant_id=? AND provider_id=? AND revision=?`, encoded, now.UnixMilli(), actor,
			identityDigest(tenantID, definition.ID), tenantID, definition.ID, expectedRevision)
		if updateErr != nil {
			return ManagedProvider{}, updateErr
		}
		affected, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return ManagedProvider{}, rowsErr
		}
		if affected != 1 {
			return ManagedProvider{}, ErrManagedConflict
		}
	}
	if err != nil {
		return ManagedProvider{}, fmt.Errorf("save managed model provider: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ManagedProvider{}, err
	}
	return saved, nil
}

// Delete removes a provider under optimistic concurrency. A revision mismatch or
// missing row returns ErrManagedConflict.
func (r *SQLManagedRegistry) Delete(ctx context.Context, tenantID, providerID string, expectedRevision int64) error {
	if r == nil || r.db == nil || tenantID == "" || providerID == "" || expectedRevision < 1 {
		return errInvalid("database, tenant, provider id and positive revision are required")
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	providers, err := lockAndListTenantProviders(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	targetIndex := -1
	for index := range providers {
		if providers[index].Definition.ID == providerID {
			targetIndex = index
			break
		}
	}
	if targetIndex < 0 {
		return ErrProviderNotFound
	}
	if providers[targetIndex].Revision != expectedRevision {
		return ErrManagedConflict
	}
	remaining := append([]ManagedProvider(nil), providers[:targetIndex]...)
	remaining = append(remaining, providers[targetIndex+1:]...)
	if err := validateTenantDefaultInvariant(remaining); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM model_providers
WHERE identity_digest=? AND tenant_id=? AND provider_id=? AND revision=?`,
		identityDigest(tenantID, providerID), tenantID, providerID, expectedRevision)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrManagedConflict
	}
	return tx.Commit()
}

// lockAndListTenantProviders serializes all mutations of one tenant using a
// no-op range UPDATE inside the transaction. On InnoDB the tenant index range
// receives next-key locks (including the empty-tenant gap); on SQLite the UPDATE
// acquires the database write reservation. This keeps default switching safe
// across processes without relying on an in-memory mutex or a schema change.
func lockAndListTenantProviders(ctx context.Context, tx *sql.Tx, tenantID string) ([]ManagedProvider, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE model_providers SET updated_at_ms=updated_at_ms WHERE tenant_id=?`, tenantID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT definition_json, revision, created_at_ms, updated_at_ms, updated_by
FROM model_providers WHERE tenant_id=? ORDER BY provider_id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var providers []ManagedProvider
	for rows.Next() {
		provider, err := scanManagedProvider(rows)
		if err != nil {
			return nil, err
		}
		if provider.Definition.TenantID != tenantID {
			return nil, fmt.Errorf("managed model provider %q definition tenant %q differs from row tenant %q", provider.Definition.ID, provider.Definition.TenantID, tenantID)
		}
		if err := ValidateProviderDefinition(provider.Definition); err != nil {
			return nil, fmt.Errorf("managed model provider %q definition: %w", provider.Definition.ID, err)
		}
		providers = append(providers, provider)
	}
	return providers, rows.Err()
}

func updateManagedProviderTx(ctx context.Context, tx *sql.Tx, tenantID string, provider ManagedProvider, expectedRevision int64) error {
	encoded, err := json.Marshal(provider.Definition)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE model_providers SET definition_json=?, revision=revision+1, updated_at_ms=?, updated_by=?
WHERE identity_digest=? AND tenant_id=? AND provider_id=? AND revision=?`, encoded, provider.UpdatedAt.UnixMilli(), provider.UpdatedBy,
		identityDigest(tenantID, provider.Definition.ID), tenantID, provider.Definition.ID, expectedRevision)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrManagedConflict
	}
	return nil
}

func validateTenantDefaultInvariant(providers []ManagedProvider) error {
	if len(providers) == 0 {
		return nil
	}
	defaults := 0
	for _, provider := range providers {
		if provider.Definition.IsDefault {
			defaults++
		}
	}
	if defaults != 1 {
		return errInvalid("a non-empty tenant must have exactly one default provider; set the replacement is_default=true to switch atomically")
	}
	return nil
}

// GetManaged returns one provider for a tenant.
func (r *SQLManagedRegistry) GetManaged(ctx context.Context, tenantID, providerID string) (ManagedProvider, error) {
	row := r.db.QueryRowContext(ctx, `SELECT definition_json, revision, created_at_ms, updated_at_ms, updated_by
FROM model_providers WHERE identity_digest=? AND tenant_id=? AND provider_id=?`, identityDigest(tenantID, providerID), tenantID, providerID)
	return scanManagedProvider(row)
}

// List returns all providers for a tenant, ordered by id.
func (r *SQLManagedRegistry) List(ctx context.Context, tenantID string) ([]ManagedProvider, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT definition_json, revision, created_at_ms, updated_at_ms, updated_by
FROM model_providers WHERE tenant_id=? ORDER BY provider_id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ManagedProvider
	for rows.Next() {
		provider, err := scanManagedProvider(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, provider)
	}
	return result, rows.Err()
}

// TenantProviderSnapshot is one consistent read of a tenant's executable model
// provider definitions and their content-addressed generation.
type TenantProviderSnapshot struct {
	Fingerprint string
	Providers   []ManagedProvider
}

// TenantSnapshot reads providers and computes their fingerprint from the same
// rows, avoiding a fingerprint/List TOCTOU. The digest includes provider ID,
// revision and definition JSON, so same-millisecond updates and delete/recreate
// sequences cannot leave a changed executable definition cached. No schema
// column is required.
func (r *SQLManagedRegistry) TenantSnapshot(ctx context.Context, tenantID string) (TenantProviderSnapshot, error) {
	if r == nil || r.db == nil {
		return TenantProviderSnapshot{}, errInvalid("database is required")
	}
	rows, err := r.db.QueryContext(ctx, `SELECT provider_id, definition_json, revision, created_at_ms, updated_at_ms, updated_by
FROM model_providers WHERE tenant_id=? ORDER BY provider_id`, tenantID)
	if err != nil {
		return TenantProviderSnapshot{}, err
	}
	defer rows.Close()

	hash := sha256.New()
	var snapshot TenantProviderSnapshot
	for rows.Next() {
		var provider ManagedProvider
		var providerID string
		var encoded []byte
		var createdAtMS, updatedAtMS int64
		if err := rows.Scan(&providerID, &encoded, &provider.Revision, &createdAtMS, &updatedAtMS, &provider.UpdatedBy); err != nil {
			return TenantProviderSnapshot{}, err
		}
		if err := json.Unmarshal(encoded, &provider.Definition); err != nil {
			return TenantProviderSnapshot{}, err
		}
		if provider.Definition.ID != providerID {
			return TenantProviderSnapshot{}, fmt.Errorf("managed model provider row id %q differs from definition id %q", providerID, provider.Definition.ID)
		}
		if provider.Definition.TenantID != tenantID {
			return TenantProviderSnapshot{}, fmt.Errorf("managed model provider %q definition tenant %q differs from row tenant %q", providerID, provider.Definition.TenantID, tenantID)
		}
		if err := ValidateProviderDefinition(provider.Definition); err != nil {
			return TenantProviderSnapshot{}, fmt.Errorf("managed model provider %q definition: %w", providerID, err)
		}
		provider.CreatedAt = time.UnixMilli(createdAtMS).UTC()
		provider.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
		snapshot.Providers = append(snapshot.Providers, provider)
		writeFingerprintPart(hash, []byte(providerID))
		var revision [8]byte
		binary.BigEndian.PutUint64(revision[:], uint64(provider.Revision))
		writeFingerprintPart(hash, revision[:])
		writeFingerprintPart(hash, encoded)
	}
	if err := rows.Err(); err != nil {
		return TenantProviderSnapshot{}, err
	}
	if len(snapshot.Providers) > 0 {
		snapshot.Fingerprint = "mp1_" + base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
	}
	return snapshot, nil
}

// TenantFingerprint is retained for lightweight change checks and tests. New
// resolver code should consume TenantSnapshot so definitions and generation
// come from one database read.
func (r *SQLManagedRegistry) TenantFingerprint(ctx context.Context, tenantID string) (fingerprint string, count int64, err error) {
	snapshot, err := r.TenantSnapshot(ctx, tenantID)
	if err != nil {
		return "", 0, err
	}
	return snapshot.Fingerprint, int64(len(snapshot.Providers)), nil
}

func writeFingerprintPart(hash interface{ Write([]byte) (int, error) }, value []byte) {
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(value)))
	_, _ = hash.Write(size[:])
	_, _ = hash.Write(value)
}

type managedScanner interface{ Scan(...any) error }

func scanManagedProvider(row managedScanner) (ManagedProvider, error) {
	var provider ManagedProvider
	var encoded []byte
	var createdAtMS, updatedAtMS int64
	if err := row.Scan(&encoded, &provider.Revision, &createdAtMS, &updatedAtMS, &provider.UpdatedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ManagedProvider{}, ErrProviderNotFound
		}
		return ManagedProvider{}, err
	}
	if err := json.Unmarshal(encoded, &provider.Definition); err != nil {
		return ManagedProvider{}, err
	}
	provider.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	provider.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
	return provider, nil
}

func identityDigest(values ...string) []byte {
	h := sha256.New()
	for _, value := range values {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(value))
	}
	return h.Sum(nil)
}
