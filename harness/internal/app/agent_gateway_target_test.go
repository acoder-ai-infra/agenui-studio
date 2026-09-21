package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/agentgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/agentregistry"
	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/gatewaycontract"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	storagemem "github.com/AGenUI/agenui-studio/harness/internal/storage/memory"
)

func TestRegistryGatewayTargetResolverVerifiesActualParentRun(t *testing.T) {
	ctx := context.Background()
	registry := agentregistry.NewService(agentregistry.WithLegacyUnresolvedPrompts())
	target := gatewayRegistryAgent("hotel")
	target.Gateway.Plugins = []agentregistry.GatewayPluginConfig{{PluginID: "basic_validator", Config: json.RawMessage(`{}`)}}
	parent := gatewayRegistryAgent("planner")
	parent.SubAgents = []string{"hotel"}
	for _, cfg := range []agentregistry.AgentConfig{target, parent} {
		if _, err := registry.RegisterAgent(ctx, cfg); err != nil {
			t.Fatalf("register %s: %v", cfg.AgentID, err)
		}
	}
	parentEffective, err := registry.ResolveEffectiveConfig(ctx, agentregistry.ResolveRequest{AgentID: "planner", Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	stores := storagemem.New().Stores()
	if err := stores.Runs.Create(ctx, &storage.Run{
		RunID: "parent-run", SessionID: "session", TenantID: "tenant", AgentID: "planner",
		Status: storage.RunStatusRunning, ConfigSnapshotRef: parentEffective.ConfigSnapshotRef,
	}); err != nil {
		t.Fatal(err)
	}
	resolver := registryGatewayTargetResolver{registry: registry, runs: stores.Runs}
	req := agentgateway.ResolveTargetRequest{
		TenantID: "tenant", SessionID: "session", ParentRunID: "parent-run",
		ParentAgentID: "planner", ParentAgentVersion: "v1",
		ParentConfigSnapshotRef: parentEffective.ConfigSnapshotRef, ParentConfigHash: parentEffective.ConfigHash,
		SubAgentRef: "hotel",
	}
	resolved, err := resolver.Resolve(ctx, req)
	if err != nil || resolved.Binding.TargetAgentID != "hotel" || resolved.Binding.ConfigHash == "" || len(resolved.Plugins) != 1 ||
		resolved.Plugins[0].PluginID != "basic_validator" ||
		resolved.Binding.SchemaVersion != agentgateway.ProviderBindingSchemaV2 {
		t.Fatalf("resolve trusted parent: target=%+v err=%v", resolved, err)
	}
	req.SessionID = "forged"
	if _, err := resolver.Resolve(ctx, req); !errors.Is(err, agentgateway.ErrTargetUnauthorized) {
		t.Fatalf("forged parent identity accepted: %v", err)
	}
	if err := stores.Runs.Create(ctx, &storage.Run{
		RunID: "child-parent-run", ParentRunID: "root-run", SessionID: "session", TenantID: "tenant", AgentID: "planner",
		Status: storage.RunStatusRunning, ConfigSnapshotRef: parentEffective.ConfigSnapshotRef,
	}); err != nil {
		t.Fatal(err)
	}
	req.SessionID = "session"
	req.ParentRunID = "child-parent-run"
	if _, err := resolver.Resolve(ctx, req); !errors.Is(err, agentgateway.ErrNestingLimit) {
		t.Fatalf("platform child run was allowed to resolve a grandchild: %v", err)
	}
}

func gatewayRegistryAgent(id string) agentregistry.AgentConfig {
	cfg := agentregistry.AgentConfig{
		AgentID: id, AgentType: id + "_assistant", Version: "v1", Status: agentregistry.AgentStatusEnabled,
		Runtime:       agentruntime.RuntimeSpec{Type: agentruntime.RuntimeTypeNative, Mode: agentruntime.RuntimeModeDirect},
		PromptRef:     "prompt://" + id,
		Capability:    agentregistry.CapabilityConfig{ExecutionModes: []agentregistry.ExecutionMode{agentregistry.ExecutionModeDirectAction}},
		Orchestration: agentregistry.OrchestrationConfig{DefaultMode: agentregistry.ExecutionModeDirectAction},
	}
	if id == "hotel" {
		cfg.Gateway = &agentregistry.GatewayTargetConfig{
			ProviderKind: gatewaycontract.SubAgentProviderLocalAgent,
		}
	}
	return cfg
}
