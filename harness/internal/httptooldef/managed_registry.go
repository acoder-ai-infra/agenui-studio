package httptooldef

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ManagedHTTPTool is a stored HTTP tool definition with its revision and audit
// metadata.
type ManagedHTTPTool struct {
	Definition Definition `json:"definition"`
	Revision   int64      `json:"revision"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	UpdatedBy  string     `json:"updated_by"`
}

// SQLManagedRegistry is a tenant-scoped HTTP tool definition store with
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
// HTTP tool definition under optimistic concurrency.
func (r *SQLManagedRegistry) Save(ctx context.Context, tenantID, actor string, definition Definition, expectedRevision int64) (ManagedHTTPTool, error) {
	if r == nil || r.db == nil || tenantID == "" || actor == "" || expectedRevision < 0 {
		return ManagedHTTPTool{}, errInvalid("database, tenant, actor and non-negative revision are required")
	}
	definition.TenantID = tenantID
	definition.Scope = ScopeTenant
	if err := validateManagedDefinition(definition); err != nil {
		return ManagedHTTPTool{}, err
	}
	encoded, err := json.Marshal(definition)
	if err != nil {
		return ManagedHTTPTool{}, err
	}
	now := r.now().UTC()
	if expectedRevision == 0 {
		_, err = r.db.ExecContext(ctx, `INSERT INTO http_tools (
tenant_id, tool_name, definition_json, revision, created_at_ms, updated_at_ms, updated_by, identity_digest
) VALUES (?, ?, ?, 1, ?, ?, ?, ?)`, tenantID, definition.ToolName, encoded, now.UnixMilli(), now.UnixMilli(), actor, identityDigest(tenantID, definition.ToolName))
	} else {
		result, updateErr := r.db.ExecContext(ctx, `UPDATE http_tools SET definition_json=?, revision=revision+1, updated_at_ms=?, updated_by=?
WHERE identity_digest=? AND tenant_id=? AND tool_name=? AND revision=?`, encoded, now.UnixMilli(), actor,
			identityDigest(tenantID, definition.ToolName), tenantID, definition.ToolName, expectedRevision)
		if updateErr != nil {
			return ManagedHTTPTool{}, updateErr
		}
		affected, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return ManagedHTTPTool{}, rowsErr
		}
		if affected != 1 {
			return ManagedHTTPTool{}, ErrManagedConflict
		}
	}
	if err != nil {
		if _, getErr := r.GetManaged(ctx, tenantID, definition.ToolName); getErr == nil {
			return ManagedHTTPTool{}, ErrManagedConflict
		}
		return ManagedHTTPTool{}, fmt.Errorf("save managed http tool: %w", err)
	}
	return r.GetManaged(ctx, tenantID, definition.ToolName)
}

// Delete removes a tenant's HTTP tool under optimistic concurrency.
func (r *SQLManagedRegistry) Delete(ctx context.Context, tenantID, toolName string, expectedRevision int64) error {
	if r == nil || r.db == nil || tenantID == "" || toolName == "" || expectedRevision < 1 {
		return errInvalid("database, tenant, tool name and positive revision are required")
	}
	result, err := r.db.ExecContext(ctx, `DELETE FROM http_tools
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
		if _, getErr := r.GetManaged(ctx, tenantID, toolName); errors.Is(getErr, ErrNotFound) {
			return ErrNotFound
		}
		return ErrManagedConflict
	}
	return nil
}

// GetManaged returns one tenant's HTTP tool with revision/audit metadata.
func (r *SQLManagedRegistry) GetManaged(ctx context.Context, tenantID, toolName string) (ManagedHTTPTool, error) {
	if r == nil || r.db == nil {
		return ManagedHTTPTool{}, errInvalid("database is required")
	}
	row := r.db.QueryRowContext(ctx, `SELECT definition_json, revision, created_at_ms, updated_at_ms, updated_by
FROM http_tools WHERE identity_digest=? AND tenant_id=? AND tool_name=?`, identityDigest(tenantID, toolName), tenantID, toolName)
	return scanManagedHTTPTool(row)
}

// GetForPrincipal returns one tenant's HTTP tool DEFINITION, keyed strictly by
// tenant. An empty tenant yields ErrNotFound — managed tools are never
// reachable without a tenant, mirroring mcp.SQLManagedRegistry.GetForPrincipal.
func (r *SQLManagedRegistry) GetForPrincipal(ctx context.Context, tenantID, toolName string) (Definition, error) {
	if tenantID == "" {
		return Definition{}, ErrNotFound
	}
	managed, err := r.GetManaged(ctx, tenantID, toolName)
	if err != nil {
		return Definition{}, err
	}
	return managed.Definition, nil
}

// List returns all HTTP tools for a tenant, ordered by tool name.
func (r *SQLManagedRegistry) List(ctx context.Context, tenantID string) ([]ManagedHTTPTool, error) {
	if r == nil || r.db == nil {
		return nil, errInvalid("database is required")
	}
	rows, err := r.db.QueryContext(ctx, `SELECT definition_json, revision, created_at_ms, updated_at_ms, updated_by
FROM http_tools WHERE tenant_id=? ORDER BY tool_name`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ManagedHTTPTool
	for rows.Next() {
		tool, err := scanManagedHTTPTool(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, tool)
	}
	return result, rows.Err()
}

type managedScanner interface{ Scan(...any) error }

func scanManagedHTTPTool(row managedScanner) (ManagedHTTPTool, error) {
	var tool ManagedHTTPTool
	var encoded []byte
	var createdAtMS, updatedAtMS int64
	if err := row.Scan(&encoded, &tool.Revision, &createdAtMS, &updatedAtMS, &tool.UpdatedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ManagedHTTPTool{}, ErrNotFound
		}
		return ManagedHTTPTool{}, err
	}
	if err := json.Unmarshal(encoded, &tool.Definition); err != nil {
		return ManagedHTTPTool{}, err
	}
	tool.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	tool.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
	return tool, nil
}

func identityDigest(values ...string) []byte {
	h := sha256.New()
	for _, value := range values {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(value))
	}
	return h.Sum(nil)
}
