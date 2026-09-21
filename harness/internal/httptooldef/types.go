// Package httptooldef provides tenant-scoped, runtime-editable HTTP tool
// DEFINITIONS. Unlike internal/toolconfig (which stores per-tenant config
// VALUES for platform-defined catalog tools), this package lets a tenant author
// a complete REST tool from scratch — name, method, base URL, model-facing
// input/output JSON Schemas, and credential header→env mappings — stored per
// tenant in DB and made visible to that tenant's agents at runtime.
//
// It mirrors the managed-MCP / model-provider registries: a tenant-scoped SQL
// store keyed by (tenant_id, tool_name) with optimistic-revision CAS. Secrets
// are never stored — only the names of the environment variables that hold the
// credential header values.
package httptooldef

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// ScopeTenant marks a definition as owned by a single tenant.
const ScopeTenant = "tenant"

var (
	// ErrNotFound is returned when a tenant HTTP tool definition does not exist.
	ErrNotFound = errors.New("httptooldef: tenant http tool not found")
	// ErrManagedConflict is returned when an optimistic-revision write loses.
	ErrManagedConflict = errors.New("httptooldef: http tool revision conflict")
	// ErrInvalidDefinition is returned when a definition fails validation.
	ErrInvalidDefinition = errors.New("httptooldef: invalid http tool definition")

	safeToolName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	safeEnvName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Definition is one tenant's complete definition of an HTTP tool. The tool's
// model-facing contract (InputSchema/OutputSchema) is authored by the tenant;
// the credential lives only as HeaderEnv (header name → environment variable
// name), never inline.
type Definition struct {
	ToolName     string            `json:"tool_name"`
	TenantID     string            `json:"tenant_id,omitempty"`
	Scope        string            `json:"scope,omitempty"`
	DisplayName  string            `json:"display_name,omitempty"`
	Description  string            `json:"description,omitempty"`
	Method       string            `json:"method"`
	BaseURL      string            `json:"base_url"`
	InputSchema  json.RawMessage   `json:"input_schema,omitempty"`
	OutputSchema json.RawMessage   `json:"output_schema,omitempty"`
	HeaderEnv    map[string]string `json:"header_env,omitempty"`
	ResponseMode string            `json:"response_mode"`
	Write        bool              `json:"write,omitempty"`
	RiskLevel    string            `json:"risk_level,omitempty"`
	TimeoutMS    int               `json:"timeout_ms,omitempty"`
}

func errInvalid(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidDefinition, message)
}

func validateManagedDefinition(definition Definition) error {
	if !safeToolName.MatchString(definition.ToolName) {
		return errInvalid("tool name must match ^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$")
	}
	if definition.Scope != ScopeTenant || definition.TenantID == "" {
		return errInvalid("managed http tool requires tenant scope")
	}
	switch strings.ToUpper(strings.TrimSpace(definition.Method)) {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
	default:
		return errInvalid("method must be GET, POST, PUT, PATCH or DELETE")
	}
	endpoint, err := url.ParseRequestURI(definition.BaseURL)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil {
		return errInvalid("base_url must be an http(s) URL without embedded credentials")
	}
	switch definition.ResponseMode {
	case "json", "text", "bytes":
	default:
		return errInvalid("response_mode must be json, text or bytes")
	}
	switch definition.RiskLevel {
	case "", "low", "medium", "high":
	default:
		return errInvalid("risk_level must be low, medium or high")
	}
	method := strings.ToUpper(strings.TrimSpace(definition.Method))
	if definition.Write && definition.RiskLevel != "high" {
		return errInvalid("write operations require risk_level high")
	}
	if definition.Write && method == "GET" {
		return errInvalid("GET cannot be a write operation")
	}
	if (method == "PUT" || method == "PATCH" || method == "DELETE") && !definition.Write {
		return errInvalid("method " + method + " must be declared as a write operation")
	}
	if len(definition.InputSchema) > 0 && !json.Valid(definition.InputSchema) {
		return errInvalid("input_schema must be valid JSON")
	}
	if len(definition.OutputSchema) > 0 && !json.Valid(definition.OutputSchema) {
		return errInvalid("output_schema must be valid JSON")
	}
	for header, envName := range definition.HeaderEnv {
		if strings.TrimSpace(header) != header || header == "" || strings.ContainsAny(header, "\r\n") {
			return errInvalid("header_env keys must be valid header names")
		}
		if !safeEnvName.MatchString(envName) {
			return errInvalid("header_env values must be valid environment variable names")
		}
	}
	return nil
}

// Hash returns a stable content hash over the fields that determine the tool's
// runtime behaviour and model-facing contract. It is frozen into the per-run
// ModelContextPackage so execution and resume can reject definitions that
// drifted since the snapshot was taken (mirrors MCP capability/policy hashes).
func (d Definition) Hash() string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			_, _ = h.Write([]byte{0})
			_, _ = h.Write([]byte(p))
		}
	}
	write(d.TenantID, d.ToolName, d.Method, d.BaseURL, d.ResponseMode, d.RiskLevel)
	if d.Write {
		write("write")
	}
	write(string(d.InputSchema), string(d.OutputSchema))
	keys := make([]string, 0, len(d.HeaderEnv))
	for k := range d.HeaderEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		write(k, d.HeaderEnv[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}
