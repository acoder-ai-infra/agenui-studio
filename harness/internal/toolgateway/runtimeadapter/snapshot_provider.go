package runtimeadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

var (
	ErrToolRegistryMissing        = errors.New("tool registry missing")
	ErrToolReferenceInvalid       = errors.New("tool reference must use exact name@version")
	ErrToolSnapshotInvalid        = errors.New("tool snapshot invalid")
	ErrToolInputSchemaUnavailable = errors.New("tool input schema unavailable")
)

// InputSchemaResolver resolves immutable schema artifacts owned by Tool
// Gateway. Runtime never reads arbitrary schema locations by itself.
type InputSchemaResolver interface {
	ResolveInputSchema(ctx context.Context, def toolgateway.ToolDefinition) (json.RawMessage, error)
}

type InputSchemaResolverFunc func(context.Context, toolgateway.ToolDefinition) (json.RawMessage, error)

func (f InputSchemaResolverFunc) ResolveInputSchema(ctx context.Context, def toolgateway.ToolDefinition) (json.RawMessage, error) {
	return f(ctx, def)
}

// ResolvedModelToolSnapshot is the tenant-scoped Tool Registry snapshot used
// to construct one immutable ModelContextPackage. CreatedAt is intentionally
// not projected into Runtime's capability contract.
type ResolvedModelToolSnapshot struct {
	SnapshotID        string
	SchemaArtifactRef string
	CapabilityHash    string
	PolicyHash        string
	ToolRefs          []toolgateway.ToolRef
	Definitions       []agentruntime.ModelToolDefinition
}

// ModelToolSnapshotProvider is the Tool Registry -> Runtime schema boundary.
// It resolves a principal-scoped snapshot before exposing any schema to a
// model; Invoke never performs a second, implicit "latest" lookup.
type ModelToolSnapshotProvider struct {
	Registry toolgateway.ToolRegistry
	Schemas  InputSchemaResolver
}

func (p ModelToolSnapshotProvider) ResolveToolDefinitions(ctx context.Context, run agentruntime.RunRequest, refs []string) ([]agentruntime.ModelToolDefinition, error) {
	resolved, err := p.ResolveGovernedToolDefinitions(ctx, run, refs)
	if err != nil {
		return nil, err
	}
	return cloneModelToolDefinitions(resolved.Definitions), nil
}

func (p ModelToolSnapshotProvider) ResolveGovernedToolDefinitions(
	ctx context.Context,
	run agentruntime.RunRequest,
	refs []string,
) (agentruntime.GovernedToolDefinitionSnapshot, error) {
	resolved, err := p.ResolveSnapshot(ctx, run, refs)
	if err != nil {
		return agentruntime.GovernedToolDefinitionSnapshot{}, err
	}
	toolRefs := make([]string, 0, len(resolved.ToolRefs))
	for _, ref := range resolved.ToolRefs {
		toolRefs = append(toolRefs, ref.Name+"@"+ref.Version)
	}
	return agentruntime.GovernedToolDefinitionSnapshot{
		Snapshot: agentruntime.ToolSchemaSnapshot{
			SnapshotID:        resolved.SnapshotID,
			ToolRefs:          toolRefs,
			SchemaArtifactRef: resolved.SchemaArtifactRef,
			CapabilityHash:    resolved.CapabilityHash,
			PolicyHash:        resolved.PolicyHash,
		},
		Definitions: cloneModelToolDefinitions(resolved.Definitions),
	}, nil
}

func (p ModelToolSnapshotProvider) ResolveSnapshot(ctx context.Context, run agentruntime.RunRequest, refs []string) (ResolvedModelToolSnapshot, error) {
	if p.Registry == nil {
		return ResolvedModelToolSnapshot{}, ErrToolRegistryMissing
	}
	if run.TenantID == "" || run.Definition.AgentID == "" || run.Trace.TraceID == "" ||
		run.Trace.TenantID != run.TenantID || (run.Trace.AgentID != "" && run.Trace.AgentID != run.Definition.AgentID) {
		return ResolvedModelToolSnapshot{}, fmt.Errorf("%w: trusted tenant, Agent and trace are required", ErrToolSnapshotInvalid)
	}
	requested, err := parseExactToolRefs(refs)
	if err != nil {
		return ResolvedModelToolSnapshot{}, err
	}
	if len(requested) == 0 {
		return ResolvedModelToolSnapshot{}, nil
	}
	snapshot, err := p.Registry.ResolveSnapshot(ctx, toolgateway.ResolveToolSnapshotRequest{
		TenantID: run.TenantID,
		AgentID:  run.Definition.AgentID,
		ToolRefs: cloneToolRefs(requested),
		Trace:    run.Trace,
	})
	if err != nil {
		return ResolvedModelToolSnapshot{}, toolCapabilityDependencyError("resolve governed tool snapshot", err)
	}
	if err := validateToolSnapshot(snapshot, requested); err != nil {
		return ResolvedModelToolSnapshot{}, err
	}

	definitions := make([]agentruntime.ModelToolDefinition, 0, len(snapshot.ToolRefs))
	for _, ref := range snapshot.ToolRefs {
		definition, err := p.Registry.Get(ctx, ref.Name, ref.Version)
		if err != nil {
			return ResolvedModelToolSnapshot{}, toolCapabilityDependencyError("resolve tool definition "+ref.Name+"@"+ref.Version, err)
		}
		if definition == nil || definition.Name != ref.Name || definition.Version != ref.Version {
			return ResolvedModelToolSnapshot{}, fmt.Errorf("%w: definition drift for %s@%s", ErrToolSnapshotInvalid, ref.Name, ref.Version)
		}
		if !containsToolAgent(definition.Permissions.AllowedAgents, run.Definition.AgentID) {
			return ResolvedModelToolSnapshot{}, fmt.Errorf("%w: Agent is not authorized for %s@%s", ErrToolSnapshotInvalid, ref.Name, ref.Version)
		}
		schema, err := p.resolveInputSchema(ctx, *definition)
		if err != nil {
			return ResolvedModelToolSnapshot{}, err
		}
		definitions = append(definitions, agentruntime.ModelToolDefinition{
			Name:        definition.Name,
			Description: definition.Description,
			Schema:      schema,
		})
	}
	// ToolRegistry exposes snapshot identity and definitions through separate
	// methods. Re-resolve once after schema reads so a same-version mutation
	// racing package assembly is rejected before the model sees the tool.
	verified, err := p.Registry.ResolveSnapshot(ctx, toolgateway.ResolveToolSnapshotRequest{
		TenantID: run.TenantID,
		AgentID:  run.Definition.AgentID,
		ToolRefs: cloneToolRefs(requested),
		Trace:    run.Trace,
	})
	if err != nil {
		return ResolvedModelToolSnapshot{}, toolCapabilityDependencyError("verify governed tool snapshot", err)
	}
	if err := validateToolSnapshot(verified, requested); err != nil || !sameToolSnapshot(snapshot, verified) {
		return ResolvedModelToolSnapshot{}, ErrToolSnapshotInvalid
	}
	return ResolvedModelToolSnapshot{
		SnapshotID:        snapshot.SnapshotID,
		SchemaArtifactRef: snapshot.SchemaArtifactRef,
		CapabilityHash:    snapshot.CapabilityHash,
		PolicyHash:        snapshot.PolicyHash,
		ToolRefs:          cloneToolRefs(snapshot.ToolRefs),
		Definitions:       cloneModelToolDefinitions(definitions),
	}, nil
}

func sameToolSnapshot(left, right *toolgateway.ToolSnapshot) bool {
	if left == nil || right == nil || left.SnapshotID != right.SnapshotID ||
		left.SchemaArtifactRef != right.SchemaArtifactRef || left.CapabilityHash != right.CapabilityHash || left.PolicyHash != right.PolicyHash {
		return false
	}
	leftRefs := cloneToolRefs(left.ToolRefs)
	rightRefs := cloneToolRefs(right.ToolRefs)
	sort.Slice(leftRefs, func(i, j int) bool {
		if leftRefs[i].Name == leftRefs[j].Name {
			return leftRefs[i].Version < leftRefs[j].Version
		}
		return leftRefs[i].Name < leftRefs[j].Name
	})
	sort.Slice(rightRefs, func(i, j int) bool {
		if rightRefs[i].Name == rightRefs[j].Name {
			return rightRefs[i].Version < rightRefs[j].Version
		}
		return rightRefs[i].Name < rightRefs[j].Name
	})
	if len(leftRefs) != len(rightRefs) {
		return false
	}
	for index := range leftRefs {
		if leftRefs[index] != rightRefs[index] {
			return false
		}
	}
	return true
}

func (p ModelToolSnapshotProvider) resolveInputSchema(ctx context.Context, def toolgateway.ToolDefinition) (json.RawMessage, error) {
	schema := append(json.RawMessage(nil), def.InputSchema...)
	if len(schema) == 0 {
		if p.Schemas == nil || strings.TrimSpace(def.InputSchemaRef) == "" {
			return nil, fmt.Errorf("%w: %s@%s", ErrToolInputSchemaUnavailable, def.Name, def.Version)
		}
		var err error
		schema, err = p.Schemas.ResolveInputSchema(ctx, def)
		if err != nil {
			return nil, toolCapabilityDependencyError("resolve input schema "+def.Name+"@"+def.Version, err)
		}
		schema = append(json.RawMessage(nil), schema...)
	}
	if len(schema) == 0 || !json.Valid(schema) {
		return nil, fmt.Errorf("%w: %s@%s", ErrToolInputSchemaUnavailable, def.Name, def.Version)
	}
	return schema, nil
}

func toolCapabilityDependencyError(operation string, err error) error {
	if err == nil {
		return nil
	}
	var toolErr *toolgateway.ToolError
	if errors.As(err, &toolErr) && !toolErr.Retryable {
		return err
	}
	return fmt.Errorf("%w: %s: %w", agentruntime.ErrProductionCapabilityUnavailable, operation, err)
}

func parseExactToolRefs(values []string) ([]toolgateway.ToolRef, error) {
	refs := make([]toolgateway.ToolRef, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" || value != strings.TrimSpace(value) || strings.Count(value, "@") != 1 {
			return nil, fmt.Errorf("%w: %q", ErrToolReferenceInvalid, value)
		}
		name, version, ok := strings.Cut(value, "@")
		if !ok || name == "" || version == "" || name != strings.TrimSpace(name) || version != strings.TrimSpace(version) {
			return nil, fmt.Errorf("%w: %q", ErrToolReferenceInvalid, value)
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, fmt.Errorf("%w: duplicate %q", ErrToolReferenceInvalid, value)
		}
		seen[value] = struct{}{}
		refs = append(refs, toolgateway.ToolRef{Name: name, Version: version})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Name == refs[j].Name {
			return refs[i].Version < refs[j].Version
		}
		return refs[i].Name < refs[j].Name
	})
	return refs, nil
}

func validateToolSnapshot(snapshot *toolgateway.ToolSnapshot, requested []toolgateway.ToolRef) error {
	if snapshot == nil || snapshot.SnapshotID == "" || snapshot.CapabilityHash == "" || snapshot.PolicyHash == "" {
		return ErrToolSnapshotInvalid
	}
	actual := cloneToolRefs(snapshot.ToolRefs)
	sort.Slice(actual, func(i, j int) bool {
		if actual[i].Name == actual[j].Name {
			return actual[i].Version < actual[j].Version
		}
		return actual[i].Name < actual[j].Name
	})
	if len(actual) != len(requested) {
		return fmt.Errorf("%w: tool ref count drift", ErrToolSnapshotInvalid)
	}
	for index := range requested {
		if actual[index] != requested[index] || actual[index].Name == "" || actual[index].Version == "" {
			return fmt.Errorf("%w: tool ref drift", ErrToolSnapshotInvalid)
		}
	}
	return nil
}

func cloneToolRefs(input []toolgateway.ToolRef) []toolgateway.ToolRef {
	return append([]toolgateway.ToolRef(nil), input...)
}

func cloneModelToolDefinitions(input []agentruntime.ModelToolDefinition) []agentruntime.ModelToolDefinition {
	output := append([]agentruntime.ModelToolDefinition(nil), input...)
	for index := range output {
		output[index].Schema = append(json.RawMessage(nil), input[index].Schema...)
	}
	return output
}

func containsToolAgent(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

var _ agentruntime.ToolDefinitionSnapshotProvider = ModelToolSnapshotProvider{}
var _ agentruntime.GovernedToolDefinitionSnapshotProvider = ModelToolSnapshotProvider{}
