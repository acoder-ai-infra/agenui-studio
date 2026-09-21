// Package toolconfig provides tenant-scoped, runtime-editable configuration for
// tools that require per-tenant parameters (e.g. git_clone credentials/hosts or
// an HTTP tool's endpoint and auth headers). It mirrors the managed-MCP and
// managed model-provider registries: a tenant-scoped SQL store keyed by
// (tenant_id, tool_name) with optimistic-revision CAS.
//
// Only tools that declare a config_schema in the capability catalog are
// configurable here; tools with no config_schema (read_file, write_file, ...)
// need no per-tenant configuration and are not managed by this store.
//
// Secrets are never stored inline — a tool config only ever references the name
// of the environment variable that holds the credential, consistent with the
// managed MCP header_env and model-provider api_key_env conventions.
package toolconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// ScopeTenant marks a tool config as owned by a single tenant.
const ScopeTenant = "tenant"

var (
	// ErrToolConfigNotFound is returned when a tenant tool config does not exist.
	ErrToolConfigNotFound = errors.New("toolconfig: tenant tool config not found")
	// ErrManagedConflict is returned when an optimistic-revision write loses.
	ErrManagedConflict = errors.New("toolconfig: tool config revision conflict")
	// ErrInvalidDefinition is returned when a definition fails validation.
	ErrInvalidDefinition = errors.New("toolconfig: invalid tool config definition")

	// safeToolName mirrors the id regexes of the sibling managed registries.
	safeToolName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
)

// Definition is one tenant's configuration for a single configurable tool. The
// CRUD list is keyed by (tenant_id, tool_name); at most one row per tool per
// tenant (singleton). Config is the opaque, config_schema-validated JSON object
// interpreted by the tool at execution time; the store does not interpret it
// beyond requiring a valid JSON object.
type Definition struct {
	ToolName string          `json:"tool_name"`
	TenantID string          `json:"tenant_id,omitempty"`
	Scope    string          `json:"scope,omitempty"`
	Config   json.RawMessage `json:"config"`
}

func errInvalid(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidDefinition, message)
}

func validateManagedDefinition(definition Definition) error {
	if !safeToolName.MatchString(definition.ToolName) {
		return errInvalid("tool name must match ^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$")
	}
	if definition.Scope != ScopeTenant || definition.TenantID == "" {
		return errInvalid("managed tool config requires tenant scope")
	}
	if len(definition.Config) == 0 || !json.Valid(definition.Config) {
		return errInvalid("config must be a valid JSON object")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(definition.Config, &probe); err != nil {
		return errInvalid("config must be a JSON object")
	}
	return nil
}
