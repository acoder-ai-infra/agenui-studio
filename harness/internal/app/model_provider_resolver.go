package app

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/AGenUI/agenui-studio/harness/internal/modeladmin"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// facadeAssembler materializes a TenantConfig into an executable Facade. It is
// shared between the startup loop and the runtime resolver so both paths build
// identical Facades.
type facadeAssembler func(tenantID string, tc TenantConfig) (*modelgateway.Facade, []string)

// managedModelResolver serves per-tenant Facades from the managed model
// provider store. It caches the built Facade keyed by a cheap tenant
// fingerprint (provider id + revision + definition), so the model-serving hot path only
// rebuilds when a tenant's managed providers actually change. A tenant with no
// managed providers falls back to the managed "default" tenant, not models.yaml.
type managedModelResolver struct {
	store    *modeladmin.SQLManagedRegistry
	assemble facadeAssembler
	logger   observability.StructuredLogger

	mu    sync.RWMutex
	cache map[string]cachedFacade
}

type cachedFacade struct {
	fingerprint string
	facade      *modelgateway.Facade
}

func newManagedModelResolver(store *modeladmin.SQLManagedRegistry, assemble facadeAssembler, logger observability.StructuredLogger) *managedModelResolver {
	return &managedModelResolver{store: store, assemble: assemble, logger: logger, cache: map[string]cachedFacade{}}
}

var _ modelgateway.TenantFacadeResolver = (*managedModelResolver)(nil)

// Facade returns the tenant's managed Facade. Only an explicit empty tenant
// snapshot falls back to the database-backed "default" tenant. Read, secret,
// validation and assembly failures propagate and fail closed.
func (r *managedModelResolver) Facade(ctx context.Context, tenantID string) (*modelgateway.Facade, bool, error) {
	if tenantID == "" {
		return nil, false, nil
	}
	if r == nil || r.store == nil || r.assemble == nil {
		return nil, false, errors.New("managed model resolver is not configured")
	}
	facade, ok, err := r.facadeForTenant(ctx, tenantID)
	if err != nil || ok {
		return facade, ok, err
	}
	if tenantID != "default" {
		return r.facadeForTenant(ctx, "default")
	}
	return nil, false, nil
}

func (r *managedModelResolver) facadeForTenant(ctx context.Context, tenantID string) (*modelgateway.Facade, bool, error) {
	snapshot, err := r.store.TenantSnapshot(ctx, tenantID)
	if err != nil {
		r.warn(ctx, "managed model fingerprint failed", tenantID, err)
		return nil, false, fmt.Errorf("managed model tenant %q snapshot: %w", tenantID, err)
	}
	if len(snapshot.Providers) == 0 {
		return nil, false, nil
	}
	fingerprint := snapshot.Fingerprint
	r.mu.RLock()
	cached, ok := r.cache[tenantID]
	r.mu.RUnlock()
	if ok && cached.fingerprint == fingerprint {
		return cached.facade, true, nil
	}
	tc, err := managedTenantConfig(snapshot.Providers)
	if err != nil {
		r.warn(ctx, "managed model config assembly failed", tenantID, err)
		return nil, false, fmt.Errorf("managed model tenant %q config: %w", tenantID, err)
	}
	facade, names := r.assemble(tenantID, tc)
	if len(names) == 0 || facade == nil || facade.Router == nil {
		r.warn(ctx, "managed model config built no usable provider", tenantID, nil)
		return nil, false, fmt.Errorf("managed model tenant %q built no usable default route", tenantID)
	}
	r.mu.Lock()
	r.cache[tenantID] = cachedFacade{fingerprint: fingerprint, facade: facade}
	r.mu.Unlock()
	return facade, true, nil
}

func (r *managedModelResolver) warn(ctx context.Context, msg, tenantID string, err error) {
	if r.logger == nil {
		return
	}
	fields := []observability.Field{observability.String("tenant_id", tenantID)}
	if err != nil {
		fields = append(fields, observability.String("error", err.Error()))
	}
	r.logger.Warn(ctx, msg, fields...)
}

// managedTenantConfig assembles a TenantConfig from a tenant's managed provider
// records, resolving each provider's api_key_env secret. The provider marked
// IsDefault supplies the tenant's default provider/model and quota.
func managedTenantConfig(providers []modeladmin.ManagedProvider) (TenantConfig, error) {
	var tc TenantConfig
	defaultCount := 0
	for _, managed := range providers {
		def := managed.Definition
		if err := modeladmin.ValidateProviderDefinition(def); err != nil {
			return TenantConfig{}, fmt.Errorf("managed provider %q: %w", def.ID, err)
		}
		pc := ProviderConfig{
			Name:                       def.ID,
			Protocol:                   def.Protocol,
			ProviderKind:               def.ProviderKind,
			BaseURL:                    def.BaseURL,
			APIKey:                     def.APIKey,
			APIKeyEnv:                  def.APIKeyEnv,
			Models:                     append([]string(nil), def.Models...),
			PromptCacheSessionAffinity: def.PromptCacheSessionAffinity,
			Costs:                      cloneModelCosts(def.Costs),
			Capability:                 def.Capability,
			Capabilities:               cloneModelCapabilities(def.Capabilities),
		}
		// resolveProviderSecret overrides APIKey from the env var when APIKeyEnv is
		// set; otherwise the inline APIKey (if any) is used as-is.
		if err := resolveProviderSecret(&pc); err != nil {
			return TenantConfig{}, err
		}
		tc.Providers = append(tc.Providers, pc)
		if def.IsDefault {
			defaultCount++
			if defaultCount > 1 {
				return TenantConfig{}, fmt.Errorf("managed model config declares multiple default providers")
			}
			tc.DefaultProvider = def.ID
			tc.DefaultModel = def.DefaultModel
			if tc.DefaultModel == "" && len(def.Models) > 0 {
				tc.DefaultModel = def.Models[0]
			}
			tc.Quota = def.Quota
		}
	}
	if len(providers) > 0 && defaultCount != 1 {
		return TenantConfig{}, fmt.Errorf("managed model config requires exactly one default provider")
	}
	if err := validateModelProviderOptions(ModelConfig{Default: tc}); err != nil {
		return TenantConfig{}, err
	}
	return tc, nil
}

// managedProviderNames 枚举给定租户在托管库中已有的 provider 名称，仅用于
// 启动日志与巡检展示；database 模式的运行期路由由 resolver 懒加载。
func managedProviderNames(ctx context.Context, store *modeladmin.SQLManagedRegistry, tenantIDs []string) (map[string][]string, []string) {
	byTenant := make(map[string][]string, len(tenantIDs))
	var all []string
	for _, tenantID := range tenantIDs {
		providers, err := store.List(ctx, tenantID)
		if err != nil {
			continue
		}
		for _, provider := range providers {
			name := provider.Definition.ID
			byTenant[tenantID] = append(byTenant[tenantID], name)
			all = appendUnique(all, []string{name})
		}
	}
	return byTenant, all
}

func cloneModelCosts(input map[string]modelgateway.ModelCostTable) map[string]modelgateway.ModelCostTable {
	if len(input) == 0 {
		return nil
	}
	out := make(map[string]modelgateway.ModelCostTable, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func cloneModelCapabilities(input map[string]modelgateway.ModelCapability) map[string]modelgateway.ModelCapability {
	if len(input) == 0 {
		return nil
	}
	out := make(map[string]modelgateway.ModelCapability, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

// validateProviderBuildable dry-runs a single managed provider definition: it
// resolves the api_key_env secret and confirms buildFacade produces a usable
// client. The management save handler calls this before persisting so a broken
// config never reaches live tenant traffic.
func validateProviderBuildable(_ context.Context, def modeladmin.ProviderDefinition) error {
	if err := modeladmin.ValidateProviderDefinition(def); err != nil {
		return err
	}
	tc := TenantConfig{}
	pc := ProviderConfig{
		Name:                       def.ID,
		Protocol:                   def.Protocol,
		ProviderKind:               def.ProviderKind,
		BaseURL:                    def.BaseURL,
		APIKey:                     def.APIKey,
		APIKeyEnv:                  def.APIKeyEnv,
		Models:                     append([]string(nil), def.Models...),
		PromptCacheSessionAffinity: def.PromptCacheSessionAffinity,
		Costs:                      cloneModelCosts(def.Costs),
		Capability:                 def.Capability,
		Capabilities:               cloneModelCapabilities(def.Capabilities),
	}
	if err := validateModelProviderOptions(ModelConfig{Default: TenantConfig{Providers: []ProviderConfig{pc}}}); err != nil {
		return err
	}
	if err := resolveProviderSecret(&pc); err != nil {
		return err
	}
	tc.Providers = []ProviderConfig{pc}
	if def.IsDefault {
		tc.DefaultProvider = def.ID
		tc.DefaultModel = def.DefaultModel
		if tc.DefaultModel == "" && len(def.Models) > 0 {
			tc.DefaultModel = def.Models[0]
		}
	}
	_, names := buildFacade(def.TenantID, tc)
	if len(names) == 0 {
		return fmt.Errorf("provider %q built no usable client (check protocol, base_url and api_key_env)", def.ID)
	}
	return nil
}
