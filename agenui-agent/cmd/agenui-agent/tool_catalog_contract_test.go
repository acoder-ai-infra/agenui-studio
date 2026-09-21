package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/sdk/providercontract"
	"gopkg.in/yaml.v3"
)

func TestEnabledToolCatalogCompilesForSupportedProviders(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "configs", "harness", "catalogs", "tools.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Enabled     []string `yaml:"enabled"`
		Definitions []struct {
			Name        string         `yaml:"name"`
			Version     string         `yaml:"version"`
			InputSchema map[string]any `yaml:"input_schema"`
		} `yaml:"definitions"`
	}
	if err := yaml.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	definitions := make(map[string]providercontract.ToolDefinition, len(catalog.Definitions))
	for _, definition := range catalog.Definitions {
		encoded, err := json.Marshal(definition.InputSchema)
		if err != nil {
			t.Fatalf("marshal %s@%s schema: %v", definition.Name, definition.Version, err)
		}
		ref := definition.Name + "@" + definition.Version
		definitions[ref] = providercontract.ToolDefinition{Name: definition.Name, Version: definition.Version, InputSchema: encoded}
		branches, hasOneOf := definition.InputSchema["oneOf"].([]any)
		if !hasOneOf {
			continue
		}
		discriminator, _ := definition.InputSchema["x-harness-discriminator"].(string)
		if discriminator == "" {
			t.Errorf("tool %s must declare x-harness-discriminator", ref)
			continue
		}
		for index, value := range branches {
			branch, ok := value.(map[string]any)
			if !ok || branch["type"] != "object" {
				t.Errorf("tool %s oneOf branch %d must declare type object", definition.Name, index)
				continue
			}
			properties, _ := branch["properties"].(map[string]any)
			discriminatorSchema, _ := properties[discriminator].(map[string]any)
			if _, ok := discriminatorSchema["const"].(string); !ok {
				t.Errorf("tool %s oneOf branch %d must use string %s.const", ref, index, discriminator)
			}
			if !containsYAMLString(branch["required"], discriminator) {
				t.Errorf("tool %s oneOf branch %d must require %s", ref, index, discriminator)
			}
		}
	}
	enabled := make([]providercontract.ToolDefinition, 0, len(catalog.Enabled))
	for _, ref := range catalog.Enabled {
		definition, ok := definitions[ref]
		if !ok {
			t.Fatalf("enabled tool %s has no definition", ref)
		}
		enabled = append(enabled, definition)
	}
	for _, provider := range []providercontract.Provider{
		providercontract.ProviderOpenAICompatible,
		providercontract.ProviderAnthropic,
	} {
		compiled, err := providercontract.CompileCatalog(provider, enabled)
		if err != nil {
			t.Errorf("provider %s: %v", provider, err)
			continue
		}
		for _, definition := range compiled {
			var wire map[string]any
			if err := json.Unmarshal(definition.CompiledSchema, &wire); err != nil {
				t.Errorf("provider %s tool %s@%s emitted invalid JSON: %v", provider, definition.Name, definition.Version, err)
				continue
			}
			if wire["type"] != "object" {
				t.Errorf("provider %s tool %s@%s root type = %#v, want object", provider, definition.Name, definition.Version, wire["type"])
			}
			for _, keyword := range []string{"oneOf", "anyOf", "allOf", "enum", "const", "not", "x-harness-discriminator"} {
				if _, exists := wire[keyword]; exists {
					t.Errorf("provider %s tool %s@%s retains forbidden root keyword %q", provider, definition.Name, definition.Version, keyword)
				}
			}
		}
	}
}

func containsYAMLString(value any, want string) bool {
	values, _ := value.([]any)
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
