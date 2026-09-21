package tenantadmin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SQLManagedRegistry is the platform-global tenant directory with
// optimistic-revision CAS. It is NOT tenant-scoped: rows are keyed by tenant_id
// and every tenant is visible to the Owner console.
type SQLManagedRegistry struct {
	db  *sql.DB
	now func() time.Time
}

// NewSQLManagedRegistry constructs a store over the shared managed SQL database.
func NewSQLManagedRegistry(db *sql.DB) *SQLManagedRegistry {
	return &SQLManagedRegistry{db: db, now: time.Now}
}

// createKeyAttempts bounds secret-key regeneration on UNIQUE collisions. With a
// 55-char alphabet and 6 chars the space is ~2.7e10, so a collision is rare and
// a handful of retries is ample.
const createKeyAttempts = 10

// Create inserts a new tenant with revision 1 and a freshly generated unique
// secret key. A duplicate tenant_id returns ErrDuplicateID.
func (r *SQLManagedRegistry) Create(ctx context.Context, actor string, definition TenantDefinition) (ManagedTenant, error) {
	if r == nil || r.db == nil || actor == "" {
		return ManagedTenant{}, errInvalid("database and actor are required")
	}
	definition.Status = normalizeStatus(definition.Status)
	if err := validateDefinition(definition); err != nil {
		return ManagedTenant{}, err
	}
	if _, err := r.Get(ctx, definition.ID); err == nil {
		return ManagedTenant{}, ErrDuplicateID
	} else if !errors.Is(err, ErrTenantNotFound) {
		return ManagedTenant{}, err
	}
	now := r.now().UTC()
	for attempt := 0; attempt < createKeyAttempts; attempt++ {
		key, err := GenerateSecretKey()
		if err != nil {
			return ManagedTenant{}, err
		}
		definition.SecretKey = key
		_, execErr := r.db.ExecContext(ctx, `INSERT INTO tenants (
tenant_id, name, owner, plan, status, secret_key, revision, created_at_ms, updated_at_ms, updated_by
) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`,
			definition.ID, definition.Name, definition.Owner, definition.Plan, definition.Status, definition.SecretKey,
			now.UnixMilli(), now.UnixMilli(), actor)
		if execErr == nil {
			return r.Get(ctx, definition.ID)
		}
		// A racing insert of the same id wins → duplicate id, not a key collision.
		if _, getErr := r.Get(ctx, definition.ID); getErr == nil {
			return ManagedTenant{}, ErrDuplicateID
		}
		// Otherwise assume the UNIQUE(secret_key) constraint fired; retry with a new key.
	}
	return ManagedTenant{}, ErrDuplicateKey
}

// EnsureDefaultTenant materializes the built-in development/default tenant in
// the durable tenant directory. It never overwrites an existing public tenant.
func (r *SQLManagedRegistry) EnsureDefaultTenant(ctx context.Context, actor string) (ManagedTenant, error) {
	if r == nil || r.db == nil || actor == "" {
		return ManagedTenant{}, errInvalid("database and actor are required")
	}
	if existing, err := r.Get(ctx, DefaultTenantID); err == nil {
		return existing, nil
	} else if !errors.Is(err, ErrTenantNotFound) {
		return ManagedTenant{}, err
	}
	now := r.now().UTC()
	for attempt := 0; attempt < createKeyAttempts; attempt++ {
		key, err := GenerateSecretKey()
		if err != nil {
			return ManagedTenant{}, err
		}
		_, execErr := r.db.ExecContext(ctx, `INSERT INTO tenants (
tenant_id, name, owner, plan, status, secret_key, revision, created_at_ms, updated_at_ms, updated_by
) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`,
			DefaultTenantID, DefaultTenantName, DefaultTenantOwner, DefaultTenantPlan, StatusActive, key,
			now.UnixMilli(), now.UnixMilli(), actor)
		if execErr == nil {
			return r.Get(ctx, DefaultTenantID)
		}
		if existing, getErr := r.Get(ctx, DefaultTenantID); getErr == nil {
			return existing, nil
		}
	}
	return ManagedTenant{}, ErrDuplicateKey
}

// Update mutates the editable metadata (name/owner/plan/status) under optimistic
// concurrency. The tenant id and secret key are immutable here. A lost update or
// missing row returns ErrTenantConflict / ErrTenantNotFound.
func (r *SQLManagedRegistry) Update(ctx context.Context, actor string, definition TenantDefinition, expectedRevision int64) (ManagedTenant, error) {
	if r == nil || r.db == nil || actor == "" || expectedRevision < 1 {
		return ManagedTenant{}, errInvalid("database, actor and positive revision are required")
	}
	definition.Status = normalizeStatus(definition.Status)
	if err := validateDefinition(definition); err != nil {
		return ManagedTenant{}, err
	}
	now := r.now().UTC()
	result, err := r.db.ExecContext(ctx, `UPDATE tenants
SET name=?, owner=?, plan=?, status=?, revision=revision+1, updated_at_ms=?, updated_by=?
WHERE tenant_id=? AND revision=?`,
		definition.Name, definition.Owner, definition.Plan, definition.Status, now.UnixMilli(), actor,
		definition.ID, expectedRevision)
	if err != nil {
		return ManagedTenant{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return ManagedTenant{}, err
	}
	if affected != 1 {
		if _, getErr := r.Get(ctx, definition.ID); errors.Is(getErr, ErrTenantNotFound) {
			return ManagedTenant{}, ErrTenantNotFound
		}
		return ManagedTenant{}, ErrTenantConflict
	}
	return r.Get(ctx, definition.ID)
}

// Delete removes a tenant under optimistic concurrency. A revision mismatch or
// missing row returns ErrTenantConflict / ErrTenantNotFound.
func (r *SQLManagedRegistry) Delete(ctx context.Context, tenantID string, expectedRevision int64) error {
	if r == nil || r.db == nil || tenantID == "" || expectedRevision < 1 {
		return errInvalid("database, tenant id and positive revision are required")
	}
	result, err := r.db.ExecContext(ctx, `DELETE FROM tenants WHERE tenant_id=? AND revision=?`, tenantID, expectedRevision)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		if _, getErr := r.Get(ctx, tenantID); errors.Is(getErr, ErrTenantNotFound) {
			return ErrTenantNotFound
		}
		return ErrTenantConflict
	}
	return nil
}

// RegenerateKey rotates a tenant's secret key under optimistic concurrency.
func (r *SQLManagedRegistry) RegenerateKey(ctx context.Context, actor, tenantID string, expectedRevision int64) (ManagedTenant, error) {
	if r == nil || r.db == nil || actor == "" || tenantID == "" || expectedRevision < 1 {
		return ManagedTenant{}, errInvalid("database, actor, tenant id and positive revision are required")
	}
	now := r.now().UTC()
	for attempt := 0; attempt < createKeyAttempts; attempt++ {
		key, err := GenerateSecretKey()
		if err != nil {
			return ManagedTenant{}, err
		}
		result, execErr := r.db.ExecContext(ctx, `UPDATE tenants
SET secret_key=?, revision=revision+1, updated_at_ms=?, updated_by=?
WHERE tenant_id=? AND revision=?`, key, now.UnixMilli(), actor, tenantID, expectedRevision)
		if execErr != nil {
			// Assume UNIQUE(secret_key) collision; retry with a new key.
			continue
		}
		affected, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return ManagedTenant{}, rowsErr
		}
		if affected != 1 {
			if _, getErr := r.Get(ctx, tenantID); errors.Is(getErr, ErrTenantNotFound) {
				return ManagedTenant{}, ErrTenantNotFound
			}
			return ManagedTenant{}, ErrTenantConflict
		}
		return r.Get(ctx, tenantID)
	}
	return ManagedTenant{}, ErrDuplicateKey
}

// Get returns one tenant by id.
func (r *SQLManagedRegistry) Get(ctx context.Context, tenantID string) (ManagedTenant, error) {
	if r == nil || r.db == nil {
		return ManagedTenant{}, errInvalid("database is required")
	}
	row := r.db.QueryRowContext(ctx, `SELECT tenant_id, name, owner, plan, status, secret_key, revision, created_at_ms, updated_at_ms, updated_by
FROM tenants WHERE tenant_id=?`, tenantID)
	return scanManagedTenant(row)
}

// List returns all tenants, newest-updated first.
func (r *SQLManagedRegistry) List(ctx context.Context) ([]ManagedTenant, error) {
	if r == nil || r.db == nil {
		return nil, errInvalid("database is required")
	}
	rows, err := r.db.QueryContext(ctx, `SELECT tenant_id, name, owner, plan, status, secret_key, revision, created_at_ms, updated_at_ms, updated_by
FROM tenants ORDER BY updated_at_ms DESC, tenant_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ManagedTenant
	for rows.Next() {
		tenant, err := scanManagedTenant(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, tenant)
	}
	return result, rows.Err()
}

// ResolveSecretKey looks up the tenant that owns a login key. A paused tenant
// returns ErrTenantPaused; an unknown key returns ErrTenantNotFound.
func (r *SQLManagedRegistry) ResolveSecretKey(ctx context.Context, secretKey string) (ManagedTenant, error) {
	if r == nil || r.db == nil {
		return ManagedTenant{}, errInvalid("database is required")
	}
	if secretKey == "" {
		return ManagedTenant{}, ErrTenantNotFound
	}
	row := r.db.QueryRowContext(ctx, `SELECT tenant_id, name, owner, plan, status, secret_key, revision, created_at_ms, updated_at_ms, updated_by
FROM tenants WHERE secret_key=?`, secretKey)
	tenant, err := scanManagedTenant(row)
	if err != nil {
		return ManagedTenant{}, err
	}
	if tenant.Definition.Status == StatusPaused {
		return ManagedTenant{}, ErrTenantPaused
	}
	return tenant, nil
}

type managedScanner interface{ Scan(...any) error }

func scanManagedTenant(row managedScanner) (ManagedTenant, error) {
	var tenant ManagedTenant
	var createdAtMS, updatedAtMS int64
	def := &tenant.Definition
	if err := row.Scan(&def.ID, &def.Name, &def.Owner, &def.Plan, &def.Status, &def.SecretKey,
		&tenant.Revision, &createdAtMS, &updatedAtMS, &tenant.UpdatedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ManagedTenant{}, ErrTenantNotFound
		}
		return ManagedTenant{}, fmt.Errorf("scan tenant: %w", err)
	}
	tenant.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	tenant.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
	return tenant, nil
}
