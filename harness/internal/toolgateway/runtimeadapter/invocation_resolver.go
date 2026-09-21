package runtimeadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

var (
	ErrModelContextSnapshotMissing = errors.New("model context tool snapshot missing")
	ErrToolRouteUnauthorized       = errors.New("tool route is not authorized by model context")
	ErrToolStepIdentityMissing     = errors.New("trusted tool step identity missing")
	ErrToolStepIdentityInvalid     = errors.New("trusted tool step identity invalid")
	ErrInvocationPolicyMissing     = errors.New("trusted invocation policy resolver missing")
	ErrInvocationPolicyInvalid     = errors.New("trusted invocation policy invalid")
	ErrMCPBindingMissing           = errors.New("trusted MCP invocation binding resolver missing")
	ErrMCPBindingInvalid           = errors.New("trusted MCP invocation binding invalid")
)

// InvocationPolicyResolver may further tighten the immutable ToolDefinition
// policy with tenant grants, Agent binding policy and approval requirements.
// It is a trusted application adapter; model output must never implement it.
type InvocationPolicyResolver interface {
	ResolveInvocationPolicy(ctx context.Context, req agentruntime.ToolInvocationRequest, def toolgateway.ToolDefinition) (toolgateway.ToolCallPolicy, error)
}

type InvocationPolicyResolverFunc func(context.Context, agentruntime.ToolInvocationRequest, toolgateway.ToolDefinition) (toolgateway.ToolCallPolicy, error)

func (f InvocationPolicyResolverFunc) ResolveInvocationPolicy(ctx context.Context, req agentruntime.ToolInvocationRequest, def toolgateway.ToolDefinition) (toolgateway.ToolCallPolicy, error) {
	return f(ctx, req, def)
}

// MCPInvocationBinding is the trusted bridge from an immutable Runtime MCP
// snapshot route to the ToolDefinition mirror consumed by Tool Gateway. The
// policy is complete: RequireApproval=false is an explicit Agent/HITL policy
// decision, not an omitted default.
type MCPInvocationBinding struct {
	GatewayToolName    string
	GatewayToolVersion string
	Policy             toolgateway.ToolCallPolicy
}

// MCPInvocationBindingResolver owns the application-specific MCP capability
// catalog and Agent policy lookup. Model output never selects a Gateway mirror
// version or supplies its execution policy.
type MCPInvocationBindingResolver interface {
	ResolveMCPInvocationBinding(
		ctx context.Context,
		req agentruntime.ToolInvocationRequest,
		snapshot mcp.CapabilitySnapshot,
		tool mcp.Tool,
	) (MCPInvocationBinding, error)
}

type MCPInvocationBindingResolverFunc func(context.Context, agentruntime.ToolInvocationRequest, mcp.CapabilitySnapshot, mcp.Tool) (MCPInvocationBinding, error)

func (f MCPInvocationBindingResolverFunc) ResolveMCPInvocationBinding(
	ctx context.Context,
	req agentruntime.ToolInvocationRequest,
	snapshot mcp.CapabilitySnapshot,
	tool mcp.Tool,
) (MCPInvocationBinding, error) {
	return f(ctx, req, snapshot, tool)
}

// RegistryInvocationResolver builds Tool Gateway policy only from the frozen
// ModelContextPackage, Tool Registry and trusted authorization ports.
type RegistryInvocationResolver struct {
	Registry toolgateway.ToolRegistry
	Policy   InvocationPolicyResolver
	MCP      MCPInvocationBindingResolver
}

func (r RegistryInvocationResolver) Resolve(ctx context.Context, req agentruntime.ToolInvocationRequest) (ResolvedInvocation, error) {
	if r.Registry == nil {
		return ResolvedInvocation{}, ErrToolRegistryMissing
	}
	pkg, ok := agentruntime.ModelContextPackageFrom(ctx)
	if !ok || pkg.PackageID == "" || pkg.ContextHash == "" || pkg.Run.RunID != req.RunID ||
		pkg.ContextHash != agentruntime.ComputeModelContextHash(pkg) || pkg.Run.SessionID != req.SessionID ||
		pkg.Run.AgentID != req.AgentID || pkg.Run.AgentBindingID == "" || pkg.Run.ConfigSnapshotRef == "" || pkg.Run.ConfigHash == "" {
		return ResolvedInvocation{}, ErrModelContextSnapshotMissing
	}
	trusted, ok := observability.TraceContextFrom(ctx)
	if !ok || trusted.TraceID == "" || trusted.TenantID == "" || pkg.Security.TenantID != trusted.TenantID ||
		(pkg.Security.UserID != "" && pkg.Security.UserID != trusted.UserID) {
		return ResolvedInvocation{}, ErrModelContextSnapshotMissing
	}
	route, err := resolveAuthorizedToolRoute(pkg, req)
	if err != nil {
		return ResolvedInvocation{}, err
	}
	var frozenToolSnapshot *agentruntime.ToolSchemaSnapshot
	if req.Source == agentruntime.ToolSourceRegistry {
		frozenToolSnapshot, err = validateFrozenRegistrySnapshot(pkg, req)
		if err != nil {
			return ResolvedInvocation{}, err
		}
		if err := r.revalidateFrozenRegistrySnapshot(ctx, pkg, trusted, frozenToolSnapshot); err != nil {
			return ResolvedInvocation{}, err
		}
	}

	definition, gatewayToolName, gatewayToolVersion, trustedPolicy, err := r.resolveDefinitionAndPolicy(ctx, req, route)
	if err != nil {
		return ResolvedInvocation{}, err
	}
	policy, err := tightenPolicy(definitionPolicy(*definition, req), trustedPolicy, *definition, req)
	if err != nil {
		return ResolvedInvocation{}, err
	}
	if frozenToolSnapshot != nil {
		// Definition 与 Snapshot 来自两个旧接口，无法一次原子读取。Definition
		// 解析完成后再精确复核一次，封住两次读取之间的同版本变更窗口。
		if err := r.revalidateFrozenRegistrySnapshot(ctx, pkg, trusted, frozenToolSnapshot); err != nil {
			return ResolvedInvocation{}, err
		}
	}
	parentStepID := agentruntime.RuntimeParentStepIDFrom(ctx)
	if parentStepID == "" || req.ToolCallID == "" {
		return ResolvedInvocation{}, ErrToolStepIdentityMissing
	}
	metadata := map[string]string{
		"harness.model_context_package_id": pkg.PackageID,
		"harness.model_context_hash":       pkg.ContextHash,
		"harness.agent_binding_id":         pkg.Run.AgentBindingID,
		"harness.config_snapshot_ref":      pkg.Run.ConfigSnapshotRef,
		"harness.config_hash":              pkg.Run.ConfigHash,
	}
	if frozenToolSnapshot != nil {
		metadata["harness.tool_snapshot_id"] = frozenToolSnapshot.SnapshotID
		metadata["harness.tool_capability_hash"] = frozenToolSnapshot.CapabilityHash
		metadata["harness.tool_policy_hash"] = frozenToolSnapshot.PolicyHash
	}
	return ResolvedInvocation{
		StepID:       composeToolStepID(req.RunID, req.ToolCallID),
		ParentStepID: parentStepID,
		ToolName:     gatewayToolName,
		ToolVersion:  gatewayToolVersion,
		Caller:       toolgateway.ToolCaller{Type: "agent", AgentID: req.AgentID},
		Policy:       policy,
		Metadata:     metadata,
	}, nil
}

func (r RegistryInvocationResolver) revalidateFrozenRegistrySnapshot(
	ctx context.Context,
	pkg agentruntime.ModelContextPackage,
	trace observability.TraceContext,
	frozen *agentruntime.ToolSchemaSnapshot,
) error {
	refs, err := parseGatewayToolRefs(frozen.ToolRefs)
	if err != nil {
		return err
	}
	current, err := r.Registry.ResolveSnapshot(ctx, toolgateway.ResolveToolSnapshotRequest{
		TenantID: pkg.Security.TenantID,
		AgentID:  pkg.Run.AgentID,
		ToolRefs: refs,
		Trace:    trace,
	})
	if err != nil {
		return toolCapabilityDependencyError("revalidate frozen tool snapshot", err)
	}
	expected := &toolgateway.ToolSnapshot{
		SnapshotID:        frozen.SnapshotID,
		ToolRefs:          refs,
		SchemaArtifactRef: frozen.SchemaArtifactRef,
		CapabilityHash:    frozen.CapabilityHash,
		PolicyHash:        frozen.PolicyHash,
	}
	if !sameToolSnapshot(expected, current) {
		return ErrModelContextSnapshotMissing
	}
	return nil
}

type authorizedToolRoute struct {
	mcpSnapshot  *mcp.CapabilitySnapshot
	mcpTool      *mcp.Tool
	httpSnapshot *agentruntime.HTTPToolSnapshot
}

func resolveAuthorizedToolRoute(pkg agentruntime.ModelContextPackage, req agentruntime.ToolInvocationRequest) (authorizedToolRoute, error) {
	switch req.Source {
	case agentruntime.ToolSourceRegistry:
		if req.ToolVersion == "" || req.SourceRef != req.ToolName+"@"+req.ToolVersion || !containsExactRef(pkg.Capabilities.Tools, req.SourceRef) ||
			pkg.Capabilities.ToolSnapshot == nil || !containsExactRef(pkg.Capabilities.ToolSnapshot.ToolRefs, req.SourceRef) {
			return authorizedToolRoute{}, ErrToolRouteUnauthorized
		}
		for _, definition := range pkg.Capabilities.ToolDefinitions {
			if definition.Name == req.ToolName && len(definition.Schema) > 0 {
				return authorizedToolRoute{}, nil
			}
		}
		return authorizedToolRoute{}, ErrModelContextSnapshotMissing
	case agentruntime.ToolSourceMCP:
		if req.ToolVersion != "" {
			return authorizedToolRoute{}, ErrToolRouteUnauthorized
		}
		var resolved authorizedToolRoute
		for _, snapshot := range pkg.Capabilities.MCPSnapshots {
			if snapshot.ServerID != req.SourceRef || snapshot.ID != req.SnapshotRef {
				continue
			}
			if snapshot.CapabilityHash == "" || snapshot.PolicyHash == "" || resolved.mcpSnapshot != nil {
				return authorizedToolRoute{}, ErrModelContextSnapshotMissing
			}
			for _, tool := range snapshot.Tools {
				if tool.Name == req.ToolName {
					if resolved.mcpTool != nil || len(tool.InputSchema) == 0 {
						return authorizedToolRoute{}, ErrModelContextSnapshotMissing
					}
					frozenSnapshot := cloneMCPCapabilitySnapshot(snapshot)
					frozenTool := cloneMCPTool(tool)
					resolved.mcpSnapshot = &frozenSnapshot
					resolved.mcpTool = &frozenTool
				}
			}
		}
		if resolved.mcpSnapshot == nil || resolved.mcpTool == nil {
			return authorizedToolRoute{}, ErrToolRouteUnauthorized
		}
		return resolved, nil
	case agentruntime.ToolSourceHTTPTool:
		if req.ToolVersion != "" {
			return authorizedToolRoute{}, ErrToolRouteUnauthorized
		}
		var resolved authorizedToolRoute
		for _, snapshot := range pkg.Capabilities.HTTPToolSnapshots {
			if snapshot.Name != req.SourceRef || snapshot.DefinitionHash != req.SnapshotRef {
				continue
			}
			if snapshot.Name != req.ToolName || len(snapshot.InputSchema) == 0 || resolved.httpSnapshot != nil {
				return authorizedToolRoute{}, ErrModelContextSnapshotMissing
			}
			frozen := cloneHTTPToolSnapshot(snapshot)
			resolved.httpSnapshot = &frozen
		}
		if resolved.httpSnapshot == nil {
			return authorizedToolRoute{}, ErrToolRouteUnauthorized
		}
		return resolved, nil
	default:
		return authorizedToolRoute{}, ErrToolRouteUnauthorized
	}
}

func cloneHTTPToolSnapshot(input agentruntime.HTTPToolSnapshot) agentruntime.HTTPToolSnapshot {
	output := input
	output.InputSchema = append(json.RawMessage(nil), input.InputSchema...)
	if input.HeaderEnv != nil {
		output.HeaderEnv = make(map[string]string, len(input.HeaderEnv))
		for k, v := range input.HeaderEnv {
			output.HeaderEnv[k] = v
		}
	}
	return output
}

func validateFrozenRegistrySnapshot(
	pkg agentruntime.ModelContextPackage,
	req agentruntime.ToolInvocationRequest,
) (*agentruntime.ToolSchemaSnapshot, error) {
	frozen := pkg.Capabilities.ToolSnapshot
	if frozen == nil || frozen.SnapshotID == "" || frozen.CapabilityHash == "" || frozen.PolicyHash == "" ||
		len(frozen.ToolRefs) == 0 || !sameExactRefs(pkg.Capabilities.Tools, frozen.ToolRefs) {
		return nil, ErrModelContextSnapshotMissing
	}
	if _, err := parseGatewayToolRefs(frozen.ToolRefs); err != nil {
		return nil, ErrModelContextSnapshotMissing
	}
	if !containsExactRef(frozen.ToolRefs, req.SourceRef) {
		return nil, ErrModelContextSnapshotMissing
	}
	cloned := *frozen
	cloned.ToolRefs = append([]string(nil), frozen.ToolRefs...)
	return &cloned, nil
}

func (r RegistryInvocationResolver) resolveDefinitionAndPolicy(
	ctx context.Context,
	req agentruntime.ToolInvocationRequest,
	route authorizedToolRoute,
) (*toolgateway.ToolDefinition, string, string, toolgateway.ToolCallPolicy, error) {
	switch req.Source {
	case agentruntime.ToolSourceRegistry:
		if r.Policy == nil {
			return nil, "", "", toolgateway.ToolCallPolicy{}, ErrInvocationPolicyMissing
		}
		definition, err := r.Registry.Get(ctx, req.ToolName, req.ToolVersion)
		if err != nil {
			return nil, "", "", toolgateway.ToolCallPolicy{}, fmt.Errorf("resolve frozen tool definition: %w", err)
		}
		if definition == nil || definition.Name != req.ToolName || definition.Version != req.ToolVersion {
			return nil, "", "", toolgateway.ToolCallPolicy{}, ErrToolRouteUnauthorized
		}
		if err := validateDefinitionRoute(*definition, req); err != nil {
			return nil, "", "", toolgateway.ToolCallPolicy{}, err
		}
		policy, err := r.Policy.ResolveInvocationPolicy(ctx, req, *definition)
		if err != nil {
			return nil, "", "", toolgateway.ToolCallPolicy{}, fmt.Errorf("resolve trusted tool policy: %w", err)
		}
		return definition, definition.Name, definition.Version, policy, nil
	case agentruntime.ToolSourceMCP:
		if r.MCP == nil {
			return nil, "", "", toolgateway.ToolCallPolicy{}, ErrMCPBindingMissing
		}
		if route.mcpSnapshot == nil || route.mcpTool == nil {
			return nil, "", "", toolgateway.ToolCallPolicy{}, ErrModelContextSnapshotMissing
		}
		binding, err := r.MCP.ResolveMCPInvocationBinding(ctx, req, *route.mcpSnapshot, *route.mcpTool)
		if err != nil {
			return nil, "", "", toolgateway.ToolCallPolicy{}, fmt.Errorf("resolve trusted MCP binding: %w", err)
		}
		if !validExactIdentity(binding.GatewayToolName) || !validExactIdentity(binding.GatewayToolVersion) {
			return nil, "", "", toolgateway.ToolCallPolicy{}, fmt.Errorf("%w: gateway mirror identity is invalid", ErrMCPBindingInvalid)
		}
		definition, err := r.Registry.Get(ctx, binding.GatewayToolName, binding.GatewayToolVersion)
		if err != nil {
			return nil, "", "", toolgateway.ToolCallPolicy{}, fmt.Errorf("resolve MCP gateway mirror: %w", err)
		}
		if definition == nil || definition.Name != binding.GatewayToolName || definition.Version != binding.GatewayToolVersion {
			return nil, "", "", toolgateway.ToolCallPolicy{}, fmt.Errorf("%w: gateway mirror identity changed", ErrMCPBindingInvalid)
		}
		if err := validateDefinitionRoute(*definition, req); err != nil {
			return nil, "", "", toolgateway.ToolCallPolicy{}, err
		}
		// Foundation's frozen source binding currently requires the Gateway
		// mirror name to remain the MCP protocol tool name. The version is
		// supplied exclusively by this trusted mapping, never by model output.
		if definition.Name != req.ToolName {
			return nil, "", "", toolgateway.ToolCallPolicy{}, fmt.Errorf("%w: gateway mirror name differs from MCP tool", ErrMCPBindingInvalid)
		}
		return definition, definition.Name, definition.Version, binding.Policy, nil
	case agentruntime.ToolSourceHTTPTool:
		if r.Policy == nil {
			return nil, "", "", toolgateway.ToolCallPolicy{}, ErrInvocationPolicyMissing
		}
		if route.httpSnapshot == nil {
			return nil, "", "", toolgateway.ToolCallPolicy{}, ErrModelContextSnapshotMissing
		}
		version := agentruntime.HTTPToolMirrorVersion(route.httpSnapshot.DefinitionHash)
		definition, err := r.Registry.Get(ctx, req.ToolName, version)
		if err != nil {
			return nil, "", "", toolgateway.ToolCallPolicy{}, fmt.Errorf("resolve http tool mirror: %w", err)
		}
		if definition == nil || definition.Name != req.ToolName || definition.Version != version {
			return nil, "", "", toolgateway.ToolCallPolicy{}, ErrToolRouteUnauthorized
		}
		if err := validateDefinitionRoute(*definition, req); err != nil {
			return nil, "", "", toolgateway.ToolCallPolicy{}, err
		}
		policy, err := r.Policy.ResolveInvocationPolicy(ctx, req, *definition)
		if err != nil {
			return nil, "", "", toolgateway.ToolCallPolicy{}, fmt.Errorf("resolve trusted http tool policy: %w", err)
		}
		return definition, definition.Name, definition.Version, policy, nil
	default:
		return nil, "", "", toolgateway.ToolCallPolicy{}, ErrToolRouteUnauthorized
	}
}

func validateDefinitionRoute(def toolgateway.ToolDefinition, req agentruntime.ToolInvocationRequest) error {
	switch req.Source {
	case agentruntime.ToolSourceRegistry:
		if def.Name != req.ToolName || def.Version != req.ToolVersion {
			return ErrToolRouteUnauthorized
		}
	case agentruntime.ToolSourceMCP:
		if def.Type != toolgateway.ToolTypeMCP || def.MCP == nil || def.MCP.ServerID != req.SourceRef ||
			(def.MCP.SnapshotID != "runtime-bound" && def.MCP.SnapshotID != req.SnapshotRef) || def.MCP.MCPToolName != req.ToolName {
			return fmt.Errorf("%w: gateway mirror route differs from frozen MCP route", ErrMCPBindingInvalid)
		}
	case agentruntime.ToolSourceHTTPTool:
		if def.Type != toolgateway.ToolTypeHTTP || def.HTTP == nil || def.Name != req.ToolName {
			return ErrToolRouteUnauthorized
		}
	default:
		return ErrToolRouteUnauthorized
	}
	return nil
}

func definitionPolicy(def toolgateway.ToolDefinition, req agentruntime.ToolInvocationRequest) toolgateway.ToolCallPolicy {
	maxRetries := 0
	if def.Retry.Idempotent && def.Retry.MaxAttempts > 1 {
		maxRetries = def.Retry.MaxAttempts - 1
	}
	return toolgateway.ToolCallPolicy{
		RiskLevel:       def.RiskLevel,
		RequireApproval: def.RiskLevel == toolgateway.RiskHigh,
		Timeout:         def.Timeout,
		MaxRetries:      maxRetries,
		IdempotencyKey:  req.RunID + ":" + req.ToolCallID,
	}
}

func tightenPolicy(base, override toolgateway.ToolCallPolicy, def toolgateway.ToolDefinition, req agentruntime.ToolInvocationRequest) (toolgateway.ToolCallPolicy, error) {
	if !canonicalRisk(base.RiskLevel) || !canonicalRisk(override.RiskLevel) || override.Timeout < 0 || override.MaxRetries < 0 {
		return toolgateway.ToolCallPolicy{}, ErrInvocationPolicyInvalid
	}
	canonicalIdempotencyKey := req.RunID + ":" + req.ToolCallID
	if override.IdempotencyKey != "" && override.IdempotencyKey != canonicalIdempotencyKey {
		return toolgateway.ToolCallPolicy{}, ErrInvocationPolicyInvalid
	}
	result := base
	// RiskLevel is the caller's allowed maximum risk, so a lower value is the
	// tighter value. It is not the risk classification of this definition.
	if riskRank(override.RiskLevel) < riskRank(result.RiskLevel) {
		result.RiskLevel = override.RiskLevel
	}
	result.RequireApproval = result.RequireApproval || override.RequireApproval
	if override.Timeout > 0 && (result.Timeout <= 0 || override.Timeout < result.Timeout) {
		result.Timeout = override.Timeout
	}
	if override.MaxRetries >= 0 && override.MaxRetries < result.MaxRetries {
		result.MaxRetries = override.MaxRetries
	}
	result.AllowFallback = false
	result.Scopes = requiredGrantedScopes(def.Permissions.RequiredScopes, override.Scopes)
	result.IdempotencyKey = canonicalIdempotencyKey
	// Definition-owned high risk and retry bounds are immutable upper limits.
	if def.RiskLevel == toolgateway.RiskHigh {
		result.RequireApproval = true
	}
	maxRetries := 0
	if def.Retry.Idempotent && def.Retry.MaxAttempts > 1 {
		maxRetries = def.Retry.MaxAttempts - 1
	}
	if result.MaxRetries > maxRetries {
		result.MaxRetries = maxRetries
	}
	return result, nil
}

func canonicalRisk(value toolgateway.RiskLevel) bool {
	return value == toolgateway.RiskLow || value == toolgateway.RiskMedium || value == toolgateway.RiskHigh
}

func riskRank(value toolgateway.RiskLevel) int {
	switch value {
	case toolgateway.RiskHigh:
		return 3
	case toolgateway.RiskMedium:
		return 2
	case toolgateway.RiskLow:
		return 1
	default:
		return 0
	}
}

func containsExactRef(refs []string, target string) bool {
	for _, ref := range refs {
		if strings.TrimSpace(ref) == target {
			return true
		}
	}
	return false
}

func parseGatewayToolRefs(values []string) ([]toolgateway.ToolRef, error) {
	result := make([]toolgateway.ToolRef, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" || value != strings.TrimSpace(value) || strings.Count(value, "@") != 1 {
			return nil, ErrModelContextSnapshotMissing
		}
		name, version, ok := strings.Cut(value, "@")
		if !ok || !validExactIdentity(name) || !validExactIdentity(version) {
			return nil, ErrModelContextSnapshotMissing
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, ErrModelContextSnapshotMissing
		}
		seen[value] = struct{}{}
		result = append(result, toolgateway.ToolRef{Name: name, Version: version})
	}
	return result, nil
}

func sameExactRefs(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[string]int, len(left))
	for _, ref := range left {
		counts[ref]++
	}
	for _, ref := range right {
		counts[ref]--
		if counts[ref] < 0 {
			return false
		}
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}

func validExactIdentity(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && !strings.ContainsAny(value, "@#")
}

func requiredGrantedScopes(required, granted []string) []string {
	grantedSet := make(map[string]struct{}, len(granted))
	for _, scope := range granted {
		grantedSet[scope] = struct{}{}
	}
	result := make([]string, 0, len(required))
	for _, scope := range required {
		if _, ok := grantedSet[scope]; ok {
			result = append(result, scope)
		}
	}
	return result
}

func cloneMCPCapabilitySnapshot(input mcp.CapabilitySnapshot) mcp.CapabilitySnapshot {
	output := input
	output.Tools = make([]mcp.Tool, len(input.Tools))
	for index, tool := range input.Tools {
		output.Tools[index] = cloneMCPTool(tool)
	}
	return output
}

func cloneMCPTool(input mcp.Tool) mcp.Tool {
	output := input
	output.InputSchema = append([]byte(nil), input.InputSchema...)
	return output
}

var _ InvocationResolver = RegistryInvocationResolver{}
