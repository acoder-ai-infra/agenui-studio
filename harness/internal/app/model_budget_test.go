package app

import (
	"context"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

func TestConfigModelTokenBudgetAllocatorUsesTenantModelCapability(t *testing.T) {
	allocator := configModelTokenBudgetAllocator{models: ModelConfig{
		Default: TenantConfig{
			DefaultProvider: "mock", DefaultModel: "mock-model",
			Providers: []ProviderConfig{{Name: "mock", Capability: modelgateway.ModelCapability{Limits: modelgateway.ModelLimits{MaxContextTokens: 16000, MaxOutputTokens: 4096}}}},
		},
		Tenants: map[string]TenantConfig{
			"small": {
				DefaultProvider: "small-provider", DefaultModel: "small-model",
				Providers: []ProviderConfig{{Name: "small-provider", Capabilities: map[string]modelgateway.ModelCapability{
					"small-model": {Limits: modelgateway.ModelLimits{MaxContextTokens: 8000, MaxOutputTokens: 1000}},
					"large-model": {Limits: modelgateway.ModelLimits{MaxContextTokens: 128000, MaxOutputTokens: 32768}},
				}}},
			},
		},
	}}
	for _, tc := range []struct {
		tenant     string
		wantInput  int
		wantOutput int
	}{
		{tenant: "small", wantInput: 7000, wantOutput: 1000},
		{tenant: "unknown", wantInput: 11904, wantOutput: 4096},
	} {
		budget, err := allocator.Allocate(context.Background(), agentruntime.RuntimeContextAssemblyRequest{Run: agentruntime.RunRequest{TenantID: tc.tenant}}, agentruntime.ContextSnapshot{}, agentruntime.CapabilitySnapshot{})
		if err != nil {
			t.Fatalf("tenant %s: %v", tc.tenant, err)
		}
		if budget.MaxInputTokens != tc.wantInput || budget.ReservedOutputTokens != tc.wantOutput {
			t.Fatalf("tenant %s budget=%+v", tc.tenant, budget)
		}
	}

	large := "large-model"
	budget, err := allocator.Allocate(context.Background(), agentruntime.RuntimeContextAssemblyRequest{
		Run: agentruntime.RunRequest{
			TenantID: "small",
			Definition: agentruntime.AgentDefinition{
				ModelOptions: agentruntime.ModelCallOptions{Model: &large},
			},
		},
	}, agentruntime.ContextSnapshot{}, agentruntime.CapabilitySnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if budget.MaxInputTokens != 95232 || budget.ReservedOutputTokens != 32768 {
		t.Fatalf("model hint budget=%+v", budget)
	}
}

func TestConfigModelTokenBudgetAllocatorFailsClosedWithoutPhysicalLimit(t *testing.T) {
	allocator := configModelTokenBudgetAllocator{models: ModelConfig{Default: TenantConfig{
		DefaultProvider: "mock", DefaultModel: "mock-model", Providers: []ProviderConfig{{Name: "mock"}},
	}}}
	if _, err := allocator.Allocate(context.Background(), agentruntime.RuntimeContextAssemblyRequest{}, agentruntime.ContextSnapshot{}, agentruntime.CapabilitySnapshot{}); err == nil {
		t.Fatal("missing max_context_tokens was accepted")
	}
}

func TestValidateModelCostAttributionChecksEveryRoutableModel(t *testing.T) {
	cfg := ModelConfig{Default: TenantConfig{
		DefaultProvider: "mock",
		DefaultModel:    "model-a",
		Quota:           modelgateway.TenantQuota{CostBudget: 10},
		Providers: []ProviderConfig{{
			Name:     "mock",
			Models:   []string{"model-a", "model-b"},
			Costs:    map[string]modelgateway.ModelCostTable{"model-a": {Currency: "USD", InputPer1K: 1}},
			Protocol: "mock",
		}},
	}}
	err := validateModelCostAttribution(cfg)
	if err == nil || !strings.Contains(err.Error(), "mock/model-b") {
		t.Fatalf("missing model-b cost was accepted: %v", err)
	}

	cfg.Default.Providers[0].Costs["model-b"] = modelgateway.ModelCostTable{Currency: "CNY", InputPer1K: 1}
	err = validateModelCostAttribution(cfg)
	if err == nil || !strings.Contains(err.Error(), "cannot aggregate currencies") {
		t.Fatalf("mixed currencies were accepted: %v", err)
	}

	cfg.Default.Providers[0].Costs["model-b"] = modelgateway.ModelCostTable{Currency: "USD", InputPer1K: 1}
	if err := validateModelCostAttribution(cfg); err != nil {
		t.Fatalf("valid per-model cost table rejected: %v", err)
	}
}
