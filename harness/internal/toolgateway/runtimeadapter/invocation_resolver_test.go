package runtimeadapter

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

func TestValidateDefinitionRouteAcceptsRuntimeBoundMCPMirror(t *testing.T) {
	definition := toolgateway.ToolDefinition{
		Name: "lookup", Version: "v1", Type: toolgateway.ToolTypeMCP,
		MCP: &toolgateway.MCPToolSpec{ServerID: "maps", SnapshotID: "runtime-bound", MCPToolName: "lookup"},
	}
	err := validateDefinitionRoute(definition, agentruntime.ToolInvocationRequest{
		ToolName: "lookup", Source: agentruntime.ToolSourceMCP, SourceRef: "maps", SnapshotRef: "mcp_snapshot_1",
	})
	if err != nil {
		t.Fatalf("runtime-bound MCP mirror rejected: %v", err)
	}
}

func TestRegistryInvocationResolverUsesFrozenModelContextAndDefinitionPolicy(t *testing.T) {
	definition := toolDefinition("weather", "v1", `{"type":"object"}`)
	definition.Retry = toolgateway.RetryPolicy{MaxAttempts: 2, Idempotent: true}
	registry := toolgateway.NewStaticRegistry([]toolgateway.ToolDefinition{definition})
	resolver := RegistryInvocationResolver{
		Registry: registry,
		Policy:   staticInvocationPolicy(toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskLow, MaxRetries: 1}),
	}
	req := registryInvocationRequest()
	pkg := registryModelContext(t, registry)
	ctx := governedInvocationContext(pkg, "runtime_step_1")

	resolved, err := resolver.Resolve(ctx, req)
	if err != nil {
		t.Fatalf("resolve invocation: %v", err)
	}
	if resolved.StepID != composeToolStepID(req.RunID, req.ToolCallID) || resolved.ParentStepID != "runtime_step_1" || resolved.Caller.AgentID != "agent_1" {
		t.Fatalf("trusted identity lost: %#v", resolved)
	}
	if resolved.Policy.RiskLevel != toolgateway.RiskLow || resolved.Policy.MaxRetries != 1 || resolved.Policy.IdempotencyKey != "run_1:call_1" {
		t.Fatalf("definition policy not projected: %#v", resolved.Policy)
	}
	if resolved.Metadata["harness.model_context_hash"] != pkg.ContextHash || resolved.Metadata["harness.agent_binding_id"] != "binding_1" ||
		resolved.Metadata["harness.config_hash"] != pkg.Run.ConfigHash ||
		resolved.Metadata["harness.tool_snapshot_id"] != pkg.Capabilities.ToolSnapshot.SnapshotID ||
		resolved.Metadata["harness.tool_capability_hash"] != pkg.Capabilities.ToolSnapshot.CapabilityHash ||
		resolved.Metadata["harness.tool_policy_hash"] != pkg.Capabilities.ToolSnapshot.PolicyHash {
		t.Fatalf("frozen package metadata missing: %#v", resolved.Metadata)
	}
}

func TestRegistryInvocationResolverRejectsToolOutsideFrozenPackage(t *testing.T) {
	registry := toolgateway.NewStaticRegistry([]toolgateway.ToolDefinition{
		toolDefinition("weather", "v1", `{"type":"object"}`),
	})
	resolver := RegistryInvocationResolver{Registry: registry, Policy: staticInvocationPolicy(toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskLow})}
	pkg := registryModelContext(t, registry)
	pkg.Capabilities.Tools = []string{"poi@v1"}
	pkg.ContextHash = agentruntime.ComputeModelContextHash(pkg)
	ctx := governedInvocationContext(pkg, "runtime_step_1")

	_, err := resolver.Resolve(ctx, registryInvocationRequest())
	if !errors.Is(err, ErrToolRouteUnauthorized) {
		t.Fatalf("unauthorized route error = %v", err)
	}
}

func TestRegistryInvocationResolverOnlyAllowsPolicyToTighten(t *testing.T) {
	definition := toolDefinition("weather", "v1", `{"type":"object"}`)
	definition.RiskLevel = toolgateway.RiskHigh
	definition.Timeout = time.Second
	definition.Retry = toolgateway.RetryPolicy{MaxAttempts: 2, Idempotent: true}
	definition.Permissions.RequiredScopes = []string{"weather.read"}
	registry := toolgateway.NewStaticRegistry([]toolgateway.ToolDefinition{definition})
	resolver := RegistryInvocationResolver{
		Registry: registry,
		Policy: InvocationPolicyResolverFunc(func(context.Context, agentruntime.ToolInvocationRequest, toolgateway.ToolDefinition) (toolgateway.ToolCallPolicy, error) {
			return toolgateway.ToolCallPolicy{
				RiskLevel: toolgateway.RiskLow, Timeout: 5 * time.Second, MaxRetries: 99,
				AllowFallback: true, Scopes: []string{"weather.read", "unrelated.admin"},
			}, nil
		}),
	}

	resolved, err := resolver.Resolve(governedInvocationContext(registryModelContext(t, registry), "runtime_step_1"), registryInvocationRequest())
	if err != nil {
		t.Fatalf("resolve tightened policy: %v", err)
	}
	if resolved.Policy.RiskLevel != toolgateway.RiskLow || !resolved.Policy.RequireApproval || resolved.Policy.Timeout != time.Second ||
		resolved.Policy.MaxRetries != 1 || resolved.Policy.AllowFallback || len(resolved.Policy.Scopes) != 1 || resolved.Policy.Scopes[0] != "weather.read" {
		t.Fatalf("policy was weakened: %#v", resolved.Policy)
	}
}

func TestRegistryInvocationResolverCannotRaiseAllowedRisk(t *testing.T) {
	definition := toolDefinition("weather", "v1", `{"type":"object"}`)
	definition.RiskLevel = toolgateway.RiskLow
	registry := toolgateway.NewStaticRegistry([]toolgateway.ToolDefinition{definition})
	resolver := RegistryInvocationResolver{
		Registry: registry,
		Policy:   staticInvocationPolicy(toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskHigh}),
	}

	resolved, err := resolver.Resolve(governedInvocationContext(registryModelContext(t, registry), "runtime_step_1"), registryInvocationRequest())
	if err != nil {
		t.Fatalf("resolve invocation: %v", err)
	}
	if resolved.Policy.RiskLevel != toolgateway.RiskLow {
		t.Fatalf("allowed risk was widened: %#v", resolved.Policy)
	}
}

func TestRegistryInvocationResolverRejectsMissingOrIncompleteAgentPolicy(t *testing.T) {
	definition := toolDefinition("weather", "v1", `{"type":"object"}`)
	registry := toolgateway.NewStaticRegistry([]toolgateway.ToolDefinition{definition})
	ctx := governedInvocationContext(registryModelContext(t, registry), "runtime_step_1")

	resolver := RegistryInvocationResolver{Registry: registry}
	if _, err := resolver.Resolve(ctx, registryInvocationRequest()); !errors.Is(err, ErrInvocationPolicyMissing) {
		t.Fatalf("missing Agent/HITL policy error = %v", err)
	}

	resolver.Policy = staticInvocationPolicy(toolgateway.ToolCallPolicy{})
	if _, err := resolver.Resolve(ctx, registryInvocationRequest()); !errors.Is(err, ErrInvocationPolicyInvalid) {
		t.Fatalf("incomplete Agent/HITL policy error = %v", err)
	}
}

func TestRegistryInvocationResolverRequiresModelContextAndRuntimeStep(t *testing.T) {
	registry := toolgateway.NewStaticRegistry([]toolgateway.ToolDefinition{
		toolDefinition("weather", "v1", `{"type":"object"}`),
	})
	resolver := RegistryInvocationResolver{Registry: registry, Policy: staticInvocationPolicy(toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskLow})}
	req := registryInvocationRequest()

	if _, err := resolver.Resolve(context.Background(), req); !errors.Is(err, ErrModelContextSnapshotMissing) {
		t.Fatalf("missing package error = %v", err)
	}
	ctx := governedInvocationContext(registryModelContext(t, registry), "")
	if _, err := resolver.Resolve(ctx, req); !errors.Is(err, ErrToolStepIdentityMissing) {
		t.Fatalf("missing step error = %v", err)
	}
}

func TestRegistryInvocationResolverRejectsIncompleteToolSnapshot(t *testing.T) {
	definition := toolDefinition("weather", "v1", `{"type":"object"}`)
	registry := toolgateway.NewStaticRegistry([]toolgateway.ToolDefinition{definition})
	pkg := registryModelContext(t, registry)
	pkg.Capabilities.ToolSnapshot.PolicyHash = ""
	pkg.ContextHash = agentruntime.ComputeModelContextHash(pkg)
	resolver := RegistryInvocationResolver{
		Registry: registry,
		Policy:   staticInvocationPolicy(toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskLow}),
	}

	_, err := resolver.Resolve(governedInvocationContext(pkg, "runtime_step_1"), registryInvocationRequest())
	if !errors.Is(err, ErrModelContextSnapshotMissing) {
		t.Fatalf("snapshot drift error = %v", err)
	}
}

func TestRegistryInvocationResolverRejectsSameVersionDefinitionMutation(t *testing.T) {
	registry := &mutableInvocationRegistry{definition: toolDefinition("weather", "v1", `{"type":"object"}`)}
	pkg := registryModelContext(t, registry)
	registry.mu.Lock()
	registry.definition.InputSchema = json.RawMessage(`{"type":"object","required":["admin"]}`)
	registry.definition.RiskLevel = toolgateway.RiskHigh
	registry.mu.Unlock()
	resolver := RegistryInvocationResolver{
		Registry: registry,
		Policy:   staticInvocationPolicy(toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskLow}),
	}

	_, err := resolver.Resolve(governedInvocationContext(pkg, "runtime_step_1"), registryInvocationRequest())
	if !errors.Is(err, ErrModelContextSnapshotMissing) {
		t.Fatalf("same-version Tool definition mutation was accepted: %v", err)
	}
}

func TestRegistryInvocationResolverRejectsMutationBetweenSnapshotAndDefinitionReads(t *testing.T) {
	registry := &mutableInvocationRegistry{definition: toolDefinition("weather", "v1", `{"type":"object"}`)}
	pkg := registryModelContext(t, registry)
	registry.mu.Lock()
	registry.mutateAfterSnapshot = func(definition *toolgateway.ToolDefinition) {
		definition.InputSchema = json.RawMessage(`{"type":"object","required":["admin"]}`)
		definition.RiskLevel = toolgateway.RiskHigh
	}
	registry.mu.Unlock()
	resolver := RegistryInvocationResolver{
		Registry: registry,
		Policy:   staticInvocationPolicy(toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskHigh}),
	}

	_, err := resolver.Resolve(governedInvocationContext(pkg, "runtime_step_1"), registryInvocationRequest())
	if !errors.Is(err, ErrModelContextSnapshotMissing) {
		t.Fatalf("definition mutation between snapshot checks was accepted: %v", err)
	}
}

func TestRegistryInvocationResolverRejectsMissingConfigHash(t *testing.T) {
	definition := toolDefinition("weather", "v1", `{"type":"object"}`)
	registry := toolgateway.NewStaticRegistry([]toolgateway.ToolDefinition{definition})
	pkg := registryModelContext(t, registry)
	pkg.Run.ConfigHash = ""
	pkg.ContextHash = agentruntime.ComputeModelContextHash(pkg)
	resolver := RegistryInvocationResolver{
		Registry: registry,
		Policy:   staticInvocationPolicy(toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskLow}),
	}

	_, err := resolver.Resolve(governedInvocationContext(pkg, "runtime_step_1"), registryInvocationRequest())
	if !errors.Is(err, ErrModelContextSnapshotMissing) {
		t.Fatalf("missing config hash error = %v", err)
	}
}

func TestRegistryInvocationResolverValidatesFrozenMCPRoute(t *testing.T) {
	definition := toolDefinition("lookup", "v1", `{"type":"object"}`)
	definition.Version = "mcp-v7"
	definition.Type = toolgateway.ToolTypeMCP
	definition.Function = nil
	definition.MCP = &toolgateway.MCPToolSpec{ServerID: "maps", SnapshotID: "mcp_snapshot_1", MCPToolName: "lookup"}
	registry := toolgateway.NewStaticRegistry([]toolgateway.ToolDefinition{definition})
	resolver := RegistryInvocationResolver{
		Registry: registry,
		MCP: MCPInvocationBindingResolverFunc(func(_ context.Context, got agentruntime.ToolInvocationRequest, snapshot mcp.CapabilitySnapshot, tool mcp.Tool) (MCPInvocationBinding, error) {
			if got.ToolVersion != "" || snapshot.ID != "mcp_snapshot_1" || snapshot.ServerID != "maps" || tool.Name != "lookup" {
				t.Fatalf("unexpected frozen MCP route: req=%#v snapshot=%#v tool=%#v", got, snapshot, tool)
			}
			return MCPInvocationBinding{
				GatewayToolName: "lookup", GatewayToolVersion: "mcp-v7",
				Policy: toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskLow},
			}, nil
		}),
	}
	pkg := mcpModelContext(t)
	req := mcpInvocationRequest()

	resolved, err := resolver.Resolve(governedInvocationContext(pkg, "runtime_step_1"), req)
	if err != nil {
		t.Fatalf("resolve MCP invocation: %v", err)
	}
	if resolved.Policy.RiskLevel != toolgateway.RiskLow {
		t.Fatalf("MCP policy = %#v", resolved.Policy)
	}
	if resolved.ToolName != "lookup" || resolved.ToolVersion != "mcp-v7" {
		t.Fatalf("MCP Gateway mirror = %q@%q", resolved.ToolName, resolved.ToolVersion)
	}
}

func TestRegistryInvocationResolverMCPFailsClosedWithoutBindingOrWithRuntimeVersion(t *testing.T) {
	definition := toolDefinition("lookup", "mcp-v7", `{"type":"object"}`)
	definition.Type = toolgateway.ToolTypeMCP
	definition.Function = nil
	definition.MCP = &toolgateway.MCPToolSpec{ServerID: "maps", SnapshotID: "mcp_snapshot_1", MCPToolName: "lookup"}
	registry := toolgateway.NewStaticRegistry([]toolgateway.ToolDefinition{definition})
	pkg := mcpModelContext(t)
	req := mcpInvocationRequest()

	resolver := RegistryInvocationResolver{Registry: registry}
	if _, err := resolver.Resolve(governedInvocationContext(pkg, "runtime_step_1"), req); !errors.Is(err, ErrMCPBindingMissing) {
		t.Fatalf("missing MCP binding error = %v", err)
	}
	resolver.MCP = MCPInvocationBindingResolverFunc(func(context.Context, agentruntime.ToolInvocationRequest, mcp.CapabilitySnapshot, mcp.Tool) (MCPInvocationBinding, error) {
		return MCPInvocationBinding{GatewayToolName: "lookup", GatewayToolVersion: "mcp-v7"}, nil
	})
	if _, err := resolver.Resolve(governedInvocationContext(pkg, "runtime_step_1"), req); !errors.Is(err, ErrInvocationPolicyInvalid) {
		t.Fatalf("incomplete MCP policy error = %v", err)
	}

	req.ToolVersion = "model-supplied"
	if _, err := resolver.Resolve(governedInvocationContext(pkg, "runtime_step_1"), req); !errors.Is(err, ErrToolRouteUnauthorized) {
		t.Fatalf("model-supplied MCP version error = %v", err)
	}
}

func registryInvocationRequest() agentruntime.ToolInvocationRequest {
	return agentruntime.ToolInvocationRequest{
		SessionID: "session_1", RunID: "run_1", AgentID: "agent_1", ToolCallID: "call_1",
		ToolName: "weather", ToolVersion: "v1", Source: agentruntime.ToolSourceRegistry,
		SourceRef: "weather@v1", Arguments: json.RawMessage(`{"city":"hangzhou"}`),
	}
}

func mcpInvocationRequest() agentruntime.ToolInvocationRequest {
	return agentruntime.ToolInvocationRequest{
		SessionID: "session_1", RunID: "run_1", AgentID: "agent_1", ToolCallID: "call_1",
		ToolName: "lookup", Source: agentruntime.ToolSourceMCP,
		SourceRef: "maps", SnapshotRef: "mcp_snapshot_1", Arguments: json.RawMessage(`{"query":"west lake"}`),
	}
}

func mcpModelContext(t *testing.T) agentruntime.ModelContextPackage {
	t.Helper()
	registry := toolgateway.NewStaticRegistry([]toolgateway.ToolDefinition{toolDefinition("weather", "v1", `{"type":"object"}`)})
	pkg := registryModelContext(t, registry)
	pkg.Capabilities.Tools = nil
	pkg.Capabilities.ToolSnapshot = nil
	pkg.Capabilities.ToolDefinitions = nil
	pkg.Capabilities.MCPSnapshots = []mcp.CapabilitySnapshot{{
		ID: "mcp_snapshot_1", ServerID: "maps", CapabilityHash: "sha256:mcp", PolicyHash: "sha256:policy",
		Tools: []mcp.Tool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}}
	pkg.ContextHash = agentruntime.ComputeModelContextHash(pkg)
	return pkg
}

func registryModelContext(t *testing.T, registry toolgateway.ToolRegistry) agentruntime.ModelContextPackage {
	t.Helper()
	trace := observability.TraceContext{TraceID: "trace_1", TenantID: "tenant_1", UserID: "user_1", AgentID: "agent_1"}
	snapshot, err := registry.ResolveSnapshot(context.Background(), toolgateway.ResolveToolSnapshotRequest{
		TenantID: "tenant_1", AgentID: "agent_1",
		ToolRefs: []toolgateway.ToolRef{{Name: "weather", Version: "v1"}},
		Trace:    trace,
	})
	if err != nil {
		t.Fatalf("resolve test Tool snapshot: %v", err)
	}
	pkg := agentruntime.ModelContextPackage{
		PackageID: "package_1", ContextHash: "sha256:context",
		Run: agentruntime.ModelContextRun{
			SessionID: "session_1", RunID: "run_1", AgentBindingID: "binding_1",
			AgentID: "agent_1", ConfigSnapshotRef: "agent-config://agent_1/v1/hash", ConfigHash: "sha256:config",
		},
		Capabilities: agentruntime.ModelContextCapabilities{
			Tools: []string{"weather@v1"},
			ToolSnapshot: &agentruntime.ToolSchemaSnapshot{
				SnapshotID: snapshot.SnapshotID, ToolRefs: []string{"weather@v1"},
				SchemaArtifactRef: snapshot.SchemaArtifactRef,
				CapabilityHash:    snapshot.CapabilityHash, PolicyHash: snapshot.PolicyHash,
			},
			ToolDefinitions: []agentruntime.ModelToolDefinition{{
				Name: "weather", Schema: json.RawMessage(`{"type":"object"}`),
			}},
		},
		Security: agentruntime.ModelContextSecurity{TenantID: "tenant_1", UserID: "user_1"},
	}
	pkg.ContextHash = agentruntime.ComputeModelContextHash(pkg)
	return pkg
}

func staticInvocationPolicy(policy toolgateway.ToolCallPolicy) InvocationPolicyResolver {
	return InvocationPolicyResolverFunc(func(context.Context, agentruntime.ToolInvocationRequest, toolgateway.ToolDefinition) (toolgateway.ToolCallPolicy, error) {
		return policy, nil
	})
}

func governedInvocationContext(pkg agentruntime.ModelContextPackage, parentStepID string) context.Context {
	ctx := agentruntime.WithModelContextPackage(context.Background(), pkg)
	ctx = agentruntime.WithRuntimeParentStepID(ctx, parentStepID)
	return observability.WithTraceContext(ctx, observability.TraceContext{
		TraceID: "trace_1", TenantID: "tenant_1", UserID: "user_1", SessionID: "session_1", RunID: "run_1", AgentID: "agent_1",
	})
}

type mutableInvocationRegistry struct {
	mu                  sync.RWMutex
	definition          toolgateway.ToolDefinition
	mutateAfterSnapshot func(*toolgateway.ToolDefinition)
}

func (r *mutableInvocationRegistry) Get(ctx context.Context, name, version string) (*toolgateway.ToolDefinition, error) {
	r.mu.RLock()
	definition := r.definition
	r.mu.RUnlock()
	return toolgateway.NewStaticRegistry([]toolgateway.ToolDefinition{definition}).Get(ctx, name, version)
}

func (r *mutableInvocationRegistry) ResolveSnapshot(ctx context.Context, req toolgateway.ResolveToolSnapshotRequest) (*toolgateway.ToolSnapshot, error) {
	r.mu.RLock()
	definition := r.definition
	r.mu.RUnlock()
	snapshot, err := toolgateway.NewStaticRegistry([]toolgateway.ToolDefinition{definition}).ResolveSnapshot(ctx, req)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.mutateAfterSnapshot != nil {
		r.mutateAfterSnapshot(&r.definition)
		r.mutateAfterSnapshot = nil
	}
	r.mu.Unlock()
	return snapshot, nil
}
