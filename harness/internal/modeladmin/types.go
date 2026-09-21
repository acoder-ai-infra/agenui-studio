// Package modeladmin provides tenant-scoped, runtime-editable model gateway
// provider configuration. It mirrors the managed-MCP registry pattern: a
// tenant-scoped SQL store with optimistic-revision CAS. Provider secrets are
// never stored inline — only the name of the environment variable that holds
// the credential (APIKeyEnv), consistent with models.yaml and managed MCP.
package modeladmin

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"

	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

// ScopeTenant marks a managed provider as owned by a single tenant.
const ScopeTenant = "tenant"

var (
	// ErrProviderNotFound is returned when a managed provider does not exist.
	ErrProviderNotFound = errors.New("modeladmin: managed provider not found")
	// ErrManagedConflict is returned when an optimistic-revision write loses.
	ErrManagedConflict = errors.New("modeladmin: managed provider revision conflict")
	// ErrInvalidDefinition is returned when a definition fails validation.
	ErrInvalidDefinition = errors.New("modeladmin: invalid managed provider definition")

	safeProviderID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	safeEnvName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ProviderDefinition is one model provider available to a tenant. The CRUD list
// is keyed by ID (one row per provider). One provider per tenant may set
// IsDefault to become the tenant's primary route; its DefaultModel is the
// primary model and its Quota governs the tenant.
type ProviderDefinition struct {
	ID           string                    `json:"id"`
	DisplayName  string                    `json:"display_name,omitempty"`
	TenantID     string                    `json:"tenant_id,omitempty"`
	Scope        string                    `json:"scope,omitempty"`
	Version      string                    `json:"version"`
	Protocol     string                    `json:"protocol"`
	ProviderKind modelgateway.ProviderKind `json:"provider_kind,omitempty"`
	BaseURL      string                    `json:"base_url,omitempty"`
	// APIKeyEnv references an environment variable holding the secret (preferred:
	// no secret in the DB). APIKey stores the secret inline (plaintext in the DB —
	// convenient but the value is persisted, returned by the read API and shown in
	// the console). If both are set, APIKeyEnv wins.
	APIKeyEnv string   `json:"api_key_env,omitempty"`
	APIKey    string   `json:"api_key,omitempty"`
	Models    []string `json:"models,omitempty"`
	// PromptCacheSessionAffinity opts an Anthropic provider into the gateway's
	// trusted, tenant-scoped prompt-cache session affinity header. It is stored
	// inside definition_json, so adding it does not require a schema migration.
	PromptCacheSessionAffinity bool                                    `json:"prompt_cache_session_affinity,omitempty"`
	Costs                      map[string]modelgateway.ModelCostTable  `json:"costs,omitempty"`
	Capability                 modelgateway.ModelCapability            `json:"capability,omitempty"`
	Capabilities               map[string]modelgateway.ModelCapability `json:"capabilities,omitempty"`
	IsDefault                  bool                                    `json:"is_default,omitempty"`
	DefaultModel               string                                  `json:"default_model,omitempty"`
	Quota                      modelgateway.TenantQuota                `json:"quota,omitempty"`
}

func errInvalid(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidDefinition, message)
}

// ValidateProviderDefinition applies the canonical managed-provider contract to
// definitions read from storage as well as definitions entering CRUD. Runtime
// assembly must not assume persisted JSON is valid merely because it decodes.
func ValidateProviderDefinition(definition ProviderDefinition) error {
	return validateManagedDefinition(definition)
}

// mockProtocol reports whether the protocol is a local/test stub that needs no
// endpoint or credential.
func mockProtocol(protocol string) bool {
	return protocol == "mock" || protocol == "scenario_mock"
}

func validateManagedDefinition(definition ProviderDefinition) error {
	if !safeProviderID.MatchString(definition.ID) {
		return errInvalid("provider id must match ^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$")
	}
	if definition.Version == "" {
		return errInvalid("version is required")
	}
	if definition.Scope != ScopeTenant || definition.TenantID == "" {
		return errInvalid("managed provider requires tenant scope")
	}
	switch definition.Protocol {
	case "openai_compatible", "anthropic", "", "mock", "scenario_mock", "dashscope_rerank":
	default:
		return errInvalid("protocol must be openai_compatible, anthropic, dashscope_rerank or a mock variant")
	}
	if !modelgateway.IsKnownProviderKind(definition.ProviderKind) {
		return errInvalid("provider_kind is unsupported")
	}
	if definition.ProviderKind == modelgateway.ProviderKindSessionAffinity &&
		!modelgateway.PromptCacheSessionAffinityAllowed(definition.ProviderKind, definition.Protocol, definition.BaseURL) {
		return errInvalid("session_affinity requires the session-affinity provider endpoint")
	}
	if definition.PromptCacheSessionAffinity &&
		!modelgateway.PromptCacheSessionAffinityAllowed(definition.ProviderKind, definition.Protocol, definition.BaseURL) {
		return errInvalid("prompt_cache_session_affinity requires the session-affinity provider")
	}
	if !mockProtocol(definition.Protocol) {
		endpoint, err := url.ParseRequestURI(definition.BaseURL)
		if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil {
			return errInvalid("base_url must be an http(s) URL without userinfo")
		}
		// Accept either an inline plaintext key or a reference to an env var.
		if definition.APIKey == "" {
			if definition.APIKeyEnv == "" || !safeEnvName.MatchString(definition.APIKeyEnv) {
				return errInvalid("either api_key (inline) or a valid api_key_env is required")
			}
		} else if definition.APIKeyEnv != "" && !safeEnvName.MatchString(definition.APIKeyEnv) {
			return errInvalid("api_key_env must be a valid environment variable name")
		}
		if len(definition.Models) == 0 {
			return errInvalid("at least one model must be declared")
		}
	}
	if definition.IsDefault {
		model := definition.DefaultModel
		if model == "" && len(definition.Models) > 0 {
			model = definition.Models[0]
		}
		if model == "" {
			return errInvalid("default provider requires default_model")
		}
		if len(definition.Models) > 0 && !containsString(definition.Models, model) {
			return errInvalid("default_model must appear in models")
		}
	}
	return nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
