package app

import (
	"sort"

	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

// SystemInfo is the read-only, redacted snapshot of the running configuration
// exposed to the debug console (GET /api/v1/debug/info). It deliberately carries
// NO secret material: auth JWT secret and provider API keys never appear here —
// providers expose only HasAPIKey. Built once at composition time in Build.
type SystemInfo struct {
	AuthMode       string              `json:"auth_mode"`       // "jwt" | "insecure"
	StorageBackend string              `json:"storage_backend"` // "sqlite" | "memory" | "mysql"
	RedisEnabled   bool                `json:"redis_enabled"`
	ServerEnv      string              `json:"server_env"`
	Tenants        []TenantInfo        `json:"tenants"`
	Capabilities   map[string][]string `json:"capabilities,omitempty"`
	Artifact       ArtifactInfo        `json:"artifact"`
}

type ArtifactInfo struct {
	ObjectBackend   string `json:"object_backend"`
	MetadataBackend string `json:"metadata_backend"`
}

// TenantInfo is a tenant's model routing snapshot.
type TenantInfo struct {
	TenantID        string                   `json:"tenant_id"`
	DefaultProvider string                   `json:"default_provider,omitempty"`
	DefaultModel    string                   `json:"default_model,omitempty"`
	Providers       []ProviderInfo           `json:"providers"`
	Quota           modelgateway.TenantQuota `json:"quota"`
}

// ProviderInfo describes one configured provider WITHOUT its API key.
type ProviderInfo struct {
	Name       string                       `json:"name"`
	Protocol   string                       `json:"protocol"`
	BaseURL    string                       `json:"base_url,omitempty"`
	Models     []string                     `json:"models,omitempty"`
	HasAPIKey  bool                         `json:"has_api_key"`
	Capability modelgateway.ModelCapability `json:"capability"`
}

// buildSystemInfo assembles the redacted snapshot from resolved config + modes.
// Tenants are sorted by ID for stable output.
func buildSystemInfo(authMode, storageBackend, serverEnv string, redisEnabled bool, tenants map[string]TenantConfig, artifactConfig *ArtifactConfig, catalogs ...CapabilityCatalog) SystemInfo {
	si := SystemInfo{
		AuthMode:       authMode,
		StorageBackend: storageBackend,
		RedisEnabled:   redisEnabled,
		ServerEnv:      serverEnv,
		Capabilities:   capabilityInfo(catalogs),
		Artifact:       artifactInfo(artifactConfig),
	}
	ids := make([]string, 0, len(tenants))
	for id := range tenants {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		tc := tenants[id]
		ti := TenantInfo{
			TenantID:        id,
			DefaultProvider: tc.DefaultProvider,
			DefaultModel:    tc.DefaultModel,
			Quota:           tc.Quota,
		}
		for _, p := range tc.Providers {
			ti.Providers = append(ti.Providers, ProviderInfo{
				Name:       p.Name,
				Protocol:   p.Protocol,
				BaseURL:    p.BaseURL,
				Models:     p.Models,
				HasAPIKey:  p.APIKey != "",
				Capability: p.Capability,
			})
		}
		si.Tenants = append(si.Tenants, ti)
	}
	return si
}

func artifactInfo(config *ArtifactConfig) ArtifactInfo {
	if config == nil {
		return ArtifactInfo{ObjectBackend: "memory", MetadataBackend: "memory"}
	}
	return ArtifactInfo{ObjectBackend: config.ObjectBackend, MetadataBackend: config.MetadataBackend}
}

func capabilityInfo(catalogs []CapabilityCatalog) map[string][]string {
	names := []string{"skills", "tools", "mcp"}
	result := make(map[string][]string)
	for index, catalog := range catalogs {
		if index >= len(names) || len(catalog.Enabled) == 0 {
			continue
		}
		result[names[index]] = append([]string(nil), catalog.Enabled...)
		sort.Strings(result[names[index]])
	}
	return result
}
