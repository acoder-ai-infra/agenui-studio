package toolconfig

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ManagedToolConfig is a stored tool config definition with its revision and
// audit metadata.
type ManagedToolConfig struct {
	Definition Definition `json:"definition"`
	Revision   int64      `json:"revision"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	UpdatedBy  string     `json:"updated_by"`
}

// SQLManagedRegistry is a tenant-scoped tool config store with
// optimistic-revision CAS, mirroring mcp.SQLManagedRegistry and
// modeladmin.SQLManagedRegistry.
type SQLManagedRegistry struct {
	db  *sql.DB
	now func() time.Time
}

// NewSQLManagedRegistry constructs a store over the shared managed SQL database.
func NewSQLManagedRegistry(db *sql.DB) *SQLManagedRegistry {
	return &SQLManagedRegistry{db: db, now: time.Now}
}

// Save creates (expectedRevision==0) or updates (expectedRevision>0) a tenant's
// tool config under optimistic concurrency. A lost update returns
// ErrManagedConflict.
func (r *SQLManagedRegistry) Save(ctx context.Context, tenantID, actor string, definition Definition, expectedRevision int64) (ManagedToolConfig, error) {
	if r == nil || r.db == nil || tenantID == "" || actor == "" || expectedRevision < 0 {
		return ManagedToolConfig{}, errInvalid("database, tenant, actor and non-negative revision are required")
	}
	definition.TenantID = tenantID
	definition.Scope = ScopeTenant
	if err := validateManagedDefinition(definition); err != nil {
		return ManagedToolConfig{}, err
	}
	encoded, err := json.Marshal(definition)
	if err != nil {
		return ManagedToolConfig{}, err
	}
	now := r.now().UTC()
	if expectedRevision == 0 {
		_, err = r.db.ExecContext(ctx, `INSERT INTO tool_configs (
tenant_id, tool_name, definition_json, revision, created_at_ms, updated_at_ms, updated_by, identity_digest
) VALUES (?, ?, ?, 1, ?, ?, ?, ?)`, tenantID, definition.ToolName, encoded, now.UnixMilli(), now.UnixMilli(), actor, identityDigest(tenantID, definition.ToolName))
	} else {
		result, updateErr := r.db.ExecContext(ctx, `UPDATE tool_configs SET definition_json=?, revision=revision+1, updated_at_ms=?, updated_by=?
WHERE identity_digest=? AND tenant_id=? AND tool_name=? AND revision=?`, encoded, now.UnixMilli(), actor,
			identityDigest(tenantID, definition.ToolName), tenantID, definition.ToolName, expectedRevision)
		if updateErr != nil {
			return ManagedToolConfig{}, updateErr
		}
		affected, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return ManagedToolConfig{}, rowsErr
		}
		if affected != 1 {
			return ManagedToolConfig{}, ErrManagedConflict
		}
	}
	if err != nil {
		if _, getErr := r.GetManaged(ctx, tenantID, definition.ToolName); getErr == nil {
			return ManagedToolConfig{}, ErrManagedConflict
		}
		return ManagedToolConfig{}, fmt.Errorf("save managed tool config: %w", err)
	}
	return r.GetManaged(ctx, tenantID, definition.ToolName)
}

// Delete removes a tenant's tool config under optimistic concurrency. A revision
// mismatch or missing row returns ErrManagedConflict / ErrToolConfigNotFound.
func (r *SQLManagedRegistry) Delete(ctx context.Context, tenantID, toolName string, expectedRevision int64) error {
	if r == nil || r.db == nil || tenantID == "" || toolName == "" || expectedRevision < 1 {
		return errInvalid("database, tenant, tool name and positive revision are required")
	}
	result, err := r.db.ExecContext(ctx, `DELETE FROM tool_configs
WHERE identity_digest=? AND tenant_id=? AND tool_name=? AND revision=?`,
		identityDigest(tenantID, toolName), tenantID, toolName, expectedRevision)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		if _, getErr := r.GetManaged(ctx, tenantID, toolName); errors.Is(getErr, ErrToolConfigNotFound) {
			return ErrToolConfigNotFound
		}
		return ErrManagedConflict
	}
	return nil
}

// GetManaged returns one tenant's config for a tool.
func (r *SQLManagedRegistry) GetManaged(ctx context.Context, tenantID, toolName string) (ManagedToolConfig, error) {
	if r == nil || r.db == nil {
		return ManagedToolConfig{}, errInvalid("database is required")
	}
	row := r.db.QueryRowContext(ctx, `SELECT definition_json, revision, created_at_ms, updated_at_ms, updated_by
FROM tool_configs WHERE identity_digest=? AND tenant_id=? AND tool_name=?`, identityDigest(tenantID, toolName), tenantID, toolName)
	return scanManagedToolConfig(row)
}

// List returns all tool configs for a tenant, ordered by tool name.
func (r *SQLManagedRegistry) List(ctx context.Context, tenantID string) ([]ManagedToolConfig, error) {
	if r == nil || r.db == nil {
		return nil, errInvalid("database is required")
	}
	rows, err := r.db.QueryContext(ctx, `SELECT definition_json, revision, created_at_ms, updated_at_ms, updated_by
FROM tool_configs WHERE tenant_id=? ORDER BY tool_name`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ManagedToolConfig
	for rows.Next() {
		config, err := scanManagedToolConfig(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, config)
	}
	return result, rows.Err()
}

type managedScanner interface{ Scan(...any) error }

func scanManagedToolConfig(row managedScanner) (ManagedToolConfig, error) {
	var config ManagedToolConfig
	var encoded []byte
	var createdAtMS, updatedAtMS int64
	if err := row.Scan(&encoded, &config.Revision, &createdAtMS, &updatedAtMS, &config.UpdatedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ManagedToolConfig{}, ErrToolConfigNotFound
		}
		return ManagedToolConfig{}, err
	}
	if err := json.Unmarshal(encoded, &config.Definition); err != nil {
		return ManagedToolConfig{}, err
	}
	config.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	config.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
	return config, nil
}

func identityDigest(values ...string) []byte {
	h := sha256.New()
	for _, value := range values {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(value))
	}
	return h.Sum(nil)
}
