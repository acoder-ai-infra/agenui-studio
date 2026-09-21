package agentregistry

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/gatewaycontract"
)

func TestResolveGatewayTargetUsesFrozenParentAuthorization(t *testing.T) {
	ctx := context.Background()
	service := newLegacyTestService()
	target := directAgentConfig("hotel", "v1")
	target.Gateway = validGatewayTargetConfig()
	parent := directAgentConfig("planner", "v1")
	parent.SubAgents = []string{"hotel"}
	for _, cfg := range []AgentConfig{target, parent} {
		if _, err := service.RegisterAgent(ctx, cfg); err != nil {
			t.Fatalf("register %s: %v", cfg.AgentID, err)
		}
	}
	parentEffective, err := service.ResolveEffectiveConfig(ctx, ResolveRequest{AgentID: "planner", Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := service.ResolveGatewayTarget(ctx, ResolveGatewayTargetRequest{
		ParentAgentID: "planner", ParentAgentVersion: "v1",
		ParentConfigSnapshotRef: parentEffective.ConfigSnapshotRef, ParentConfigHash: parentEffective.ConfigHash,
		SubAgentRef: "hotel",
	})
	if err != nil {
		t.Fatalf("resolve target: %v", err)
	}
	if resolved.Effective.Definition.AgentID != "hotel" || resolved.Effective.Definition.Version != "v1" ||
		resolved.Effective.ConfigHash == "" || resolved.Gateway.ProviderKind != gatewaycontract.SubAgentProviderLocalAgent {
		t.Fatalf("target facts not frozen: %+v", resolved)
	}

	_, err = service.ResolveGatewayTarget(ctx, ResolveGatewayTargetRequest{
		ParentAgentID: "planner", ParentAgentVersion: "v1",
		ParentConfigSnapshotRef: parentEffective.ConfigSnapshotRef, ParentConfigHash: "sha256:stale",
		SubAgentRef: "hotel",
	})
	if !errors.Is(err, ErrConfigDrift) {
		t.Fatalf("stale parent hash accepted: %v", err)
	}

	parentUnauthorized := directAgentConfig("other", "v1")
	if _, err := service.RegisterAgent(ctx, parentUnauthorized); err != nil {
		t.Fatal(err)
	}
	other, err := service.ResolveEffectiveConfig(ctx, ResolveRequest{AgentID: "other", Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ResolveGatewayTarget(ctx, ResolveGatewayTargetRequest{
		ParentAgentID: "other", ParentAgentVersion: "v1", ParentConfigSnapshotRef: other.ConfigSnapshotRef,
		ParentConfigHash: other.ConfigHash, SubAgentRef: "hotel",
	}); errorCode(err) != CodePolicyViolation {
		t.Fatalf("undeclared target accepted: %v", err)
	}
}

func TestGatewayTargetConfigAndDataPassingAreHashBound(t *testing.T) {
	base := directAgentConfig("hotel", "v1")
	base.Gateway = validGatewayTargetConfig()
	base.Gateway.Plugins = []GatewayPluginConfig{{PluginID: "basic_validator", Config: json.RawMessage(`{"level":1}`)}}
	base.DataPassing.ScopedDataKeys = []string{"locale"}
	first, err := compileEffectiveConfig(base, ResolvedDependencies{}, time.Unix(1, 0), nil)
	if err != nil {
		t.Fatal(err)
	}

	changedData := base
	changedData.DataPassing.ScopedDataKeys = []string{"locale", "currency"}
	second, err := compileEffectiveConfig(changedData, ResolvedDependencies{}, time.Unix(1, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ConfigHash == second.ConfigHash || len(first.Definition.DataPassing.ScopedDataKeys) != 1 || len(second.Definition.DataPassing.ScopedDataKeys) != 2 {
		t.Fatalf("data-passing policy is not frozen into config hash: first=%s second=%s", first.ConfigHash, second.ConfigHash)
	}

	changedGateway := base
	changedGateway.Gateway = &GatewayTargetConfig{
		ProviderKind: gatewaycontract.SubAgentProviderRemoteA2A,
		Remote: &GatewayRemoteConfig{
			BaseURL: "https://agent.example.com", Transport: "JSONRPC", TimeoutMS: 30_000,
		},
	}
	third, err := compileEffectiveConfig(changedGateway, ResolvedDependencies{}, time.Unix(1, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ConfigHash == third.ConfigHash {
		t.Fatalf("Gateway target is not hash-bound: first=%s third=%s", first.ConfigHash, third.ConfigHash)
	}

	changedPlugin := base
	changedPlugin.Gateway = cloneGatewayTargetConfig(base.Gateway)
	changedPlugin.Gateway.Plugins[0].Config = json.RawMessage(`{"level":2}`)
	fourth, err := compileEffectiveConfig(changedPlugin, ResolvedDependencies{}, time.Unix(1, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ConfigHash == fourth.ConfigHash {
		t.Fatalf("Gateway plugin config is not hash-bound: first=%s fourth=%s", first.ConfigHash, fourth.ConfigHash)
	}
	changedPlugin.Gateway.Plugins[0].Config[9] = '9'
	if got := string(fourth.Gateway.Plugins[0].Config); got != `{"level":2}` {
		t.Fatalf("effective plugin config shares author bytes: %s", got)
	}
}

func TestGatewayTargetConfigRejectsDuplicatePluginIDs(t *testing.T) {
	cfg := directAgentConfig("hotel", "v1")
	cfg.Gateway = validGatewayTargetConfig()
	cfg.Gateway.Plugins = []GatewayPluginConfig{{PluginID: "basic_validator"}, {PluginID: "basic_validator"}}
	if err := ValidateAgentConfig(cfg); err == nil {
		t.Fatal("duplicate gateway plugin ids must fail closed")
	}
}

func TestGatewayPluginNestedObjectOrderDoesNotChangeConfigHash(t *testing.T) {
	left := directAgentConfig("hotel", "v1")
	left.Gateway = validGatewayTargetConfig()
	left.Gateway.Plugins = []GatewayPluginConfig{{PluginID: "nested", Config: json.RawMessage(`{"outer":{"b":2,"a":1}}`)}}
	right := left
	right.Gateway = cloneGatewayTargetConfig(left.Gateway)
	right.Gateway.Plugins[0].Config = json.RawMessage(`{"outer":{"a":1,"b":2}}`)

	leftEffective, err := compileEffectiveConfig(left, ResolvedDependencies{}, time.Unix(1, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	rightEffective, err := compileEffectiveConfig(right, ResolvedDependencies{}, time.Unix(1, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if leftEffective.ConfigHash != rightEffective.ConfigHash {
		t.Fatalf("nested object order changed ConfigHash: left=%s right=%s", leftEffective.ConfigHash, rightEffective.ConfigHash)
	}
}

func validGatewayTargetConfig() *GatewayTargetConfig {
	return &GatewayTargetConfig{ProviderKind: gatewaycontract.SubAgentProviderLocalAgent}
}
