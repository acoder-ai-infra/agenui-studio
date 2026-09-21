package app

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildSystemInfo_RedactsSecrets(t *testing.T) {
	tenants := map[string]TenantConfig{
		"acme": {
			DefaultProvider: "dashscope",
			DefaultModel:    "qwen",
			Providers: []ProviderConfig{
				{Name: "dashscope", Protocol: "openai_compatible", BaseURL: "https://x", APIKey: "sk-super-secret-key", Models: []string{"qwen"}},
			},
		},
	}
	si := buildSystemInfo("jwt", "sqlite", "local", true, tenants, &ArtifactConfig{ObjectBackend: "file", MetadataBackend: "memory"},
		CapabilityCatalog{Enabled: []string{"skill.b", "skill.a"}},
		CapabilityCatalog{Enabled: []string{"tool.echo"}},
		CapabilityCatalog{Enabled: []string{"mcp.local"}},
		CapabilityCatalog{},
	)

	if len(si.Tenants) != 1 || len(si.Tenants[0].Providers) != 1 {
		t.Fatalf("unexpected snapshot shape: %+v", si)
	}
	p := si.Tenants[0].Providers[0]
	if !p.HasAPIKey {
		t.Fatal("HasAPIKey should be true when key set")
	}

	raw, err := json.Marshal(si)
	if err != nil {
		t.Fatal(err)
	}
	blob := string(raw)
	if strings.Contains(blob, "sk-super-secret-key") {
		t.Fatalf("API key leaked into snapshot JSON: %s", blob)
	}
	// The redacted flag "has_api_key" is fine; a bare "api_key" field is not.
	if strings.Contains(blob, `"api_key"`) || strings.Contains(blob, `"apiKey"`) {
		t.Fatalf("snapshot exposes an api_key field: %s", blob)
	}
	if !strings.Contains(blob, `"has_api_key":true`) {
		t.Fatalf("expected has_api_key flag: %s", blob)
	}
	if got := strings.Join(si.Capabilities["skills"], ","); got != "skill.a,skill.b" {
		t.Fatalf("capability snapshot is not stable: %q", got)
	}
	if si.Artifact.ObjectBackend != "file" || si.Artifact.MetadataBackend != "memory" {
		t.Fatalf("artifact backend snapshot=%#v", si.Artifact)
	}
}
