package providercontract

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompileCatalogLowersDiscriminatedUnionForSupportedProviders(t *testing.T) {
	canonical := json.RawMessage(`{"type":"object","x-harness-discriminator":"operation","oneOf":[{"type":"object","required":["operation","title"],"properties":{"operation":{"const":"begin"},"title":{"type":"string"}},"additionalProperties":false},{"type":"object","required":["operation","revision"],"properties":{"operation":{"const":"commit"},"revision":{"type":"string"}},"additionalProperties":false}]}`)
	for _, provider := range []Provider{ProviderOpenAICompatible, ProviderAnthropic} {
		t.Run(string(provider), func(t *testing.T) {
			result, err := CompileCatalog(provider, []ToolDefinition{{Name: "workspace", Version: "v1", InputSchema: canonical}})
			if err != nil {
				t.Fatal(err)
			}
			if len(result) != 1 || result[0].CompilerVersion == "" || result[0].CanonicalHash == result[0].CompiledHash {
				t.Fatalf("compiled report = %+v", result)
			}
			var wire map[string]any
			if json.Unmarshal(result[0].CompiledSchema, &wire) != nil {
				t.Fatal("compiled schema is invalid JSON")
			}
			for _, keyword := range []string{"oneOf", "anyOf", "allOf", "enum", "const", "not", "x-harness-discriminator"} {
				if _, exists := wire[keyword]; exists {
					t.Fatalf("wire schema retains forbidden root keyword %q: %#v", keyword, wire)
				}
			}
			if wire["type"] != "object" {
				t.Fatalf("wire schema = %#v", wire)
			}
			description, _ := wire["description"].(string)
			if !strings.Contains(description, "operation=begin requires title") || !strings.Contains(description, "operation=commit requires revision") {
				t.Fatalf("description = %q", description)
			}
		})
	}
}

func TestCompileCatalogKeepsOrdinaryOpenAISchemaBytes(t *testing.T) {
	canonical := json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`)
	result, err := CompileCatalog(ProviderOpenAICompatible, []ToolDefinition{{Name: "echo", Version: "v1", InputSchema: canonical}})
	if err != nil {
		t.Fatal(err)
	}
	if string(result[0].CompiledSchema) != string(canonical) || result[0].CanonicalHash != result[0].CompiledHash {
		t.Fatalf("OpenAI schema changed: %+v", result[0])
	}
}
