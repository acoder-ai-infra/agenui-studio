// Package providercontract validates and compiles canonical model-tool schemas
// for configured Provider wire dialects. Business runtimes keep ownership of
// canonical tool semantics; the Harness owns provider compatibility.
package providercontract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway/anthropic"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway/openai"
)

type Provider string

const (
	ProviderOpenAICompatible Provider = "openai_compatible"
	ProviderAnthropic        Provider = "anthropic"
)

type ToolDefinition struct {
	Name        string
	Version     string
	InputSchema json.RawMessage
}

type CompiledToolDefinition struct {
	Name            string
	Version         string
	Provider        Provider
	CompilerVersion string
	CanonicalHash   string
	CompiledHash    string
	CompiledSchema  json.RawMessage
}

func ValidateCatalog(provider Provider, definitions []ToolDefinition) error {
	_, err := CompileCatalog(provider, definitions)
	return err
}

func CompileCatalog(provider Provider, definitions []ToolDefinition) ([]CompiledToolDefinition, error) {
	compiler, err := compilerFor(provider)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(definitions))
	compiled := make([]CompiledToolDefinition, 0, len(definitions))
	for index, definition := range definitions {
		definition.Name = strings.TrimSpace(definition.Name)
		definition.Version = strings.TrimSpace(definition.Version)
		if definition.Name == "" || definition.Version == "" || len(definition.InputSchema) == 0 || !json.Valid(definition.InputSchema) {
			return nil, fmt.Errorf("provider contract %s: definition %d requires name, version and valid input schema", provider, index)
		}
		ref := definition.Name + "@" + definition.Version
		if _, duplicate := seen[ref]; duplicate {
			return nil, fmt.Errorf("provider contract %s: duplicate tool %s", provider, ref)
		}
		seen[ref] = struct{}{}
		wire, compileErr := compiler.Compile(definition.InputSchema)
		if compileErr != nil {
			return nil, fmt.Errorf("provider contract %s: compile %s: %w", provider, ref, compileErr)
		}
		compiled = append(compiled, CompiledToolDefinition{
			Name: definition.Name, Version: definition.Version, Provider: provider,
			CompilerVersion: compiler.Version(),
			CanonicalHash:   hash(definition.InputSchema), CompiledHash: hash(wire),
			CompiledSchema: append(json.RawMessage(nil), wire...),
		})
	}
	return compiled, nil
}

func compilerFor(provider Provider) (mg.ToolSchemaCompiler, error) {
	switch provider {
	case ProviderOpenAICompatible:
		return openai.NewToolSchemaCompiler(), nil
	case ProviderAnthropic:
		return anthropic.NewToolSchemaCompiler(), nil
	default:
		return nil, fmt.Errorf("provider contract: unsupported provider %q", provider)
	}
}

func hash(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}
