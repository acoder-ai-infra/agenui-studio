package app

import (
	"context"
	"fmt"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/modeladmin"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

// configModelTokenBudgetAllocator freezes the selected model's physical window
// into each model context package. Agent-level model hints take precedence over
// the tenant default because Model Gateway will later route fail-closed to that
// exact model.
type configModelTokenBudgetAllocator struct {
	models ModelConfig
}

func (a configModelTokenBudgetAllocator) Allocate(_ context.Context, req agentruntime.RuntimeContextAssemblyRequest, _ agentruntime.ContextSnapshot, _ agentruntime.CapabilitySnapshot) (agentruntime.ModelTokenBudget, error) {
	tenant, ok := a.models.Tenants[req.Run.TenantID]
	if !ok {
		tenant = a.models.Default
		if tenant.DefaultProvider == "" {
			tenant = a.models.Tenants["default"]
		}
	}
	model := tenant.DefaultModel
	if req.Run.Definition.ModelOptions.Model != nil && *req.Run.Definition.ModelOptions.Model != "" {
		model = *req.Run.Definition.ModelOptions.Model
	}
	return allocateModelTokenBudget(tenant, req.Run.TenantID, model)
}

type managedModelTokenBudgetAllocator struct {
	store *modeladmin.SQLManagedRegistry
}

func managedTokenBudgetAllocator(store *modeladmin.SQLManagedRegistry) agentruntime.TokenBudgetAllocator {
	if store == nil {
		return nil
	}
	return managedModelTokenBudgetAllocator{store: store}
}

func (a managedModelTokenBudgetAllocator) Allocate(ctx context.Context, req agentruntime.RuntimeContextAssemblyRequest, _ agentruntime.ContextSnapshot, _ agentruntime.CapabilitySnapshot) (agentruntime.ModelTokenBudget, error) {
	if a.store == nil {
		return agentruntime.ModelTokenBudget{}, fmt.Errorf("managed model provider store is required")
	}
	tenant, err := a.tenantConfig(ctx, req.Run.TenantID)
	if err != nil {
		return agentruntime.ModelTokenBudget{}, err
	}
	model := tenant.DefaultModel
	if req.Run.Definition.ModelOptions.Model != nil && *req.Run.Definition.ModelOptions.Model != "" {
		model = *req.Run.Definition.ModelOptions.Model
	}
	return allocateModelTokenBudget(tenant, req.Run.TenantID, model)
}

func (a managedModelTokenBudgetAllocator) tenantConfig(ctx context.Context, tenantID string) (TenantConfig, error) {
	providers, err := a.store.List(ctx, tenantID)
	if err != nil {
		return TenantConfig{}, err
	}
	if len(providers) == 0 && tenantID != "default" {
		providers, err = a.store.List(ctx, "default")
		if err != nil {
			return TenantConfig{}, err
		}
	}
	if len(providers) == 0 {
		return TenantConfig{}, fmt.Errorf("no managed model providers configured for tenant %q", tenantID)
	}
	return managedTenantConfig(providers)
}

func allocateModelTokenBudget(tenant TenantConfig, tenantID, model string) (agentruntime.ModelTokenBudget, error) {
	for _, provider := range orderedProvidersForModel(tenant, model) {
		limits := capabilityForConfiguredModel(provider, model).Limits
		if limits.MaxContextTokens <= 0 {
			return agentruntime.ModelTokenBudget{}, fmt.Errorf("model capability max_context_tokens is required for %s/%s", provider.Name, model)
		}
		reserved := limits.MaxOutputTokens
		if reserved <= 0 {
			reserved = 2048
		}
		maxInput := limits.MaxContextTokens - reserved
		if maxInput <= 0 {
			return agentruntime.ModelTokenBudget{}, fmt.Errorf("invalid model token budget for %s/%s: context=%d output=%d", provider.Name, model, limits.MaxContextTokens, reserved)
		}
		return agentruntime.ModelTokenBudget{MaxInputTokens: maxInput, ReservedOutputTokens: reserved}, nil
	}
	return agentruntime.ModelTokenBudget{}, fmt.Errorf("model %q is not configured for tenant %q", model, tenantID)
}

func orderedProvidersForModel(tenant TenantConfig, model string) []ProviderConfig {
	providers := make([]ProviderConfig, 0, len(tenant.Providers))
	for _, provider := range tenant.Providers {
		if provider.Name == tenant.DefaultProvider && providerServesModel(provider, model) {
			providers = append(providers, provider)
			break
		}
	}
	for _, provider := range tenant.Providers {
		if provider.Name == tenant.DefaultProvider {
			continue
		}
		if providerServesModel(provider, model) {
			providers = append(providers, provider)
		}
	}
	return providers
}

func providerServesModel(provider ProviderConfig, model string) bool {
	if model == "" {
		return false
	}
	for _, candidate := range provider.Models {
		if candidate == model {
			return true
		}
	}
	return len(provider.Models) == 0 && provider.Name != ""
}

func capabilityForConfiguredModel(provider ProviderConfig, model string) modelgateway.ModelCapability {
	if provider.Capabilities != nil {
		if capability, ok := provider.Capabilities[model]; ok {
			return normalizeConfiguredCapability(capability, provider.Name, model)
		}
	}
	return normalizeConfiguredCapability(provider.Capability, provider.Name, model)
}

func normalizeConfiguredCapability(capability modelgateway.ModelCapability, providerName, model string) modelgateway.ModelCapability {
	if capability.Provider == "" {
		capability.Provider = providerName
	}
	if capability.Model == "" {
		capability.Model = model
	}
	if !capability.Chat {
		capability.Chat = true
	}
	if !capability.Streaming {
		capability.Streaming = true
	}
	if !capability.ToolCalling {
		capability.ToolCalling = true
	}
	if !capability.StructuredOutput {
		capability.StructuredOutput = true
	}
	if !capability.JSONSchema {
		capability.JSONSchema = true
	}
	return capability
}
