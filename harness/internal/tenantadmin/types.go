// Package tenantadmin provides a platform-level, runtime-editable tenant
// directory. Unlike modeladmin/mcp/skill (which are tenant-scoped), this is a
// GLOBAL registry: one row per tenant, keyed by tenant_id, with a short
// alphanumeric secret key used by the console to log in and switch tenants.
//
// It mirrors the managed-registry pattern from internal/modeladmin: a SQL store
// with optimistic-revision CAS. The secret key is stored (and returned) in
// plaintext by design — it is the tenant login credential surfaced in the Owner
// console.
package tenantadmin

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Tenant lifecycle states.
const (
	StatusActive = "active"
	StatusPaused = "paused"
)

const (
	DefaultTenantID    = "public"
	DefaultTenantName  = "Public"
	DefaultTenantOwner = "system"
	DefaultTenantPlan  = "Sandbox"
)

var (
	// ErrTenantNotFound is returned when a tenant does not exist.
	ErrTenantNotFound = errors.New("tenantadmin: tenant not found")
	// ErrTenantConflict is returned when an optimistic-revision write loses.
	ErrTenantConflict = errors.New("tenantadmin: tenant revision conflict")
	// ErrDuplicateID is returned when creating a tenant whose id already exists.
	ErrDuplicateID = errors.New("tenantadmin: tenant id already exists")
	// ErrDuplicateKey is returned when a generated/assigned secret key collides.
	ErrDuplicateKey = errors.New("tenantadmin: tenant secret key already exists")
	// ErrTenantPaused is returned when a login is attempted against a paused tenant.
	ErrTenantPaused = errors.New("tenantadmin: tenant is paused")
	// ErrInvalidDefinition is returned when a definition fails validation.
	ErrInvalidDefinition = errors.New("tenantadmin: invalid tenant definition")

	safeTenantID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
)

// TenantDefinition is the editable management metadata for one tenant. It does
// NOT own model providers or quota — those remain config/modeladmin driven. The
// SecretKey is a 6-char alphanumeric login credential generated on create.
type TenantDefinition struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Owner     string `json:"owner,omitempty"`
	Plan      string `json:"plan,omitempty"`
	Status    string `json:"status"`
	SecretKey string `json:"secret_key"`
}

// ManagedTenant is a stored tenant with its revision and audit metadata.
type ManagedTenant struct {
	Definition TenantDefinition `json:"definition"`
	Revision   int64            `json:"revision"`
	CreatedAt  time.Time        `json:"created_at"`
	UpdatedAt  time.Time        `json:"updated_at"`
	UpdatedBy  string           `json:"updated_by"`
}

func errInvalid(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidDefinition, message)
}

// normalizeStatus defaults an empty status to active.
func normalizeStatus(status string) string {
	if status == "" {
		return StatusActive
	}
	return status
}

// validateDefinition checks the editable metadata. SecretKey is assigned by the
// store, not the caller, so it is not validated here.
func validateDefinition(definition TenantDefinition) error {
	if !safeTenantID.MatchString(definition.ID) {
		return errInvalid("tenant id must match ^[a-z0-9][a-z0-9-]{0,63}$")
	}
	if definition.Name == "" {
		return errInvalid("name is required")
	}
	switch normalizeStatus(definition.Status) {
	case StatusActive, StatusPaused:
	default:
		return errInvalid("status must be active or paused")
	}
	return nil
}
