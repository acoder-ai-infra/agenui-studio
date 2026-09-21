package agentregistry

import (
	"context"
	"errors"
	"testing"
)

func TestCatalogDependencyResolverPinsVersionedRefs(t *testing.T) {
	cfg := extendedConfig("dependency_agent", "v1")
	cfg.Model.Primary = "model-a"
	resolver := CatalogDependencyResolver{
		Tools:   map[string]bool{"search@v1": true},
		Models:  map[string]bool{"model-a": true},
		Prompts: map[string]bool{cfg.PromptRef + "@" + cfg.PromptVersion: true},
	}

	deps, err := resolver.Resolve(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(deps.Tools) != 1 || deps.Tools[0] != "search@v1" {
		t.Fatalf("versioned tool ref lost: %#v", deps.Tools)
	}

	cfg.Tools[0].Version = "v2"
	_, err = resolver.Resolve(context.Background(), cfg)
	var registryErr *RegistryError
	if !errors.As(err, &registryErr) || registryErr.Code != CodeDependencyMissing {
		t.Fatalf("unregistered tool version must fail closed: %v", err)
	}
}

func TestCatalogDependencyResolverRejectsMissingBehaviorGuardrail(t *testing.T) {
	cfg := extendedConfig("guardrail_agent", "v1")
	cfg.Guardrails.Behavior = "guardrail://behavior/v2"
	resolver := CatalogDependencyResolver{Guardrails: map[string]bool{"guardrail://behavior/v1": true}}

	_, err := resolver.Resolve(context.Background(), cfg)
	var registryErr *RegistryError
	if !errors.As(err, &registryErr) || registryErr.Code != CodeDependencyMissing {
		t.Fatalf("missing behavior guardrail must fail closed: %v", err)
	}
}

func TestCatalogDependencyResolverPinsGraphStateSchema(t *testing.T) {
	cfg := extendedConfig("graph_schema_agent", "v1")
	cfg.Orchestration.Graph = testGraphDefinition()
	resolver := CatalogDependencyResolver{Schemas: map[string]bool{"schema://trip/state@v1": true}}

	deps, err := resolver.Resolve(context.Background(), cfg)
	if err != nil {
		t.Fatalf("resolve graph state schema: %v", err)
	}
	if len(deps.Schemas) != 1 || deps.Schemas[0] != "schema://trip/state@v1" {
		t.Fatalf("graph state schema was not pinned: %#v", deps.Schemas)
	}

	cfg.Orchestration.Graph.StateSchemaRef = "schema://trip/state@v2"
	_, err = resolver.Resolve(context.Background(), cfg)
	var registryErr *RegistryError
	if !errors.As(err, &registryErr) || registryErr.Code != CodeDependencyMissing {
		t.Fatalf("missing graph state schema must fail closed: %v", err)
	}
}
