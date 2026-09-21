package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/modeladmin"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	_ "modernc.org/sqlite"
)

func TestValidateProviderBuildable(t *testing.T) {
	ctx := context.Background()

	// mock protocol needs neither endpoint nor credential.
	if err := validateProviderBuildable(ctx, modeladmin.ProviderDefinition{
		ID: "m", TenantID: "t", Scope: modeladmin.ScopeTenant, Version: "v1", Protocol: "mock",
	}); err != nil {
		t.Fatalf("mock provider should validate: %v", err)
	}

	// openai_compatible with an unset api_key_env must fail (dry-run catches it
	// before the config reaches live traffic).
	if err := validateProviderBuildable(ctx, modeladmin.ProviderDefinition{
		ID: "o", TenantID: "t", Scope: modeladmin.ScopeTenant, Version: "v1", Protocol: "openai_compatible",
		BaseURL: "https://x.example.test/v1", APIKeyEnv: "HARNESS_UNSET_KEY_XYZ",
		Models: []string{"m1"}, IsDefault: true, DefaultModel: "m1",
	}); err == nil {
		t.Fatal("expected error when api_key_env is unset")
	}

	// With the secret present it builds a usable client.
	t.Setenv("HARNESS_TEST_MP_KEY", "secret")
	if err := validateProviderBuildable(ctx, modeladmin.ProviderDefinition{
		ID: "o", TenantID: "t", Scope: modeladmin.ScopeTenant, Version: "v1", Protocol: "openai_compatible",
		BaseURL: "https://x.example.test/v1", APIKeyEnv: "HARNESS_TEST_MP_KEY",
		Models: []string{"m1"}, IsDefault: true, DefaultModel: "m1",
	}); err != nil {
		t.Fatalf("valid provider should build: %v", err)
	}

	// Session affinity is an Anthropic-only provider contract. Managed-provider
	// dry-run validation must reject it before an incompatible row can be saved.
	if err := validateProviderBuildable(ctx, modeladmin.ProviderDefinition{
		ID: "o-cache", TenantID: "t", Scope: modeladmin.ScopeTenant, Version: "v1", Protocol: "openai_compatible",
		BaseURL: "https://x.example.test/v1", APIKeyEnv: "HARNESS_TEST_MP_KEY",
		Models: []string{"m1"}, PromptCacheSessionAffinity: true,
	}); err == nil {
		t.Fatal("expected non-anthropic prompt cache session affinity to fail")
	}

	if err := validateProviderBuildable(ctx, modeladmin.ProviderDefinition{
		ID: "a-cache", TenantID: "t", Scope: modeladmin.ScopeTenant, Version: "v1", Protocol: "anthropic",
		BaseURL: "http://localhost/open_api/anthropic", APIKeyEnv: "HARNESS_TEST_MP_KEY",
		Models: []string{"claude-sonnet"}, ProviderKind: modelgateway.ProviderKindSessionAffinity,
		PromptCacheSessionAffinity: true,
	}); err != nil {
		t.Fatalf("valid anthropic prompt cache session affinity should build: %v", err)
	}
}

func TestManagedTenantConfigSelectsDefault(t *testing.T) {
	t.Setenv("HARNESS_TEST_MP_KEY", "secret")
	tc, err := managedTenantConfig([]modeladmin.ManagedProvider{
		{Definition: canonicalManagedTestDefinition(modeladmin.ProviderDefinition{ID: "a", Protocol: "openai_compatible", BaseURL: "https://a.example.test/v1", APIKeyEnv: "HARNESS_TEST_MP_KEY", Models: []string{"m1"}})},
		{Definition: canonicalManagedTestDefinition(modeladmin.ProviderDefinition{ID: "b", Protocol: "openai_compatible", BaseURL: "https://b.example.test/v1", APIKeyEnv: "HARNESS_TEST_MP_KEY", Models: []string{"m2"}, IsDefault: true, DefaultModel: "m2"})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tc.Providers) != 2 || tc.DefaultProvider != "b" || tc.DefaultModel != "m2" {
		t.Fatalf("tenant config = %#v", tc)
	}
}

func TestManagedTenantConfigCopiesAndValidatesPromptCacheSessionAffinity(t *testing.T) {
	t.Setenv("HARNESS_TEST_MP_KEY", "secret")
	models := []string{"claude-sonnet"}
	costs := map[string]modelgateway.ModelCostTable{"claude-sonnet": {Currency: "USD", InputPer1K: 1}}
	capabilities := map[string]modelgateway.ModelCapability{"claude-sonnet": {Chat: true}}
	providers := []modeladmin.ManagedProvider{{Definition: canonicalManagedTestDefinition(modeladmin.ProviderDefinition{
		ID: "anthropic-cache", Protocol: "anthropic", BaseURL: "http://localhost/open_api/anthropic",
		APIKeyEnv: "HARNESS_TEST_MP_KEY", Models: models, PromptCacheSessionAffinity: true,
		ProviderKind: modelgateway.ProviderKindSessionAffinity,
		Costs:        costs, Capabilities: capabilities, IsDefault: true, DefaultModel: "claude-sonnet",
	})}}

	tc, err := managedTenantConfig(providers)
	if err != nil {
		t.Fatal(err)
	}
	if len(tc.Providers) != 1 || !tc.Providers[0].PromptCacheSessionAffinity || tc.Providers[0].ProviderKind != modelgateway.ProviderKindSessionAffinity {
		t.Fatalf("tenant config = %#v", tc)
	}
	models[0] = "mutated-after-assembly"
	costs["claude-sonnet"] = modelgateway.ModelCostTable{Currency: "CNY", InputPer1K: 99}
	capabilities["claude-sonnet"] = modelgateway.ModelCapability{Chat: false}
	if tc.Providers[0].Models[0] != "claude-sonnet" {
		t.Fatalf("managed provider models alias source: %#v", tc.Providers[0].Models)
	}
	if got := tc.Providers[0].Costs["claude-sonnet"]; got.Currency != "USD" || got.InputPer1K != 1 {
		t.Fatalf("managed provider costs alias source: %#v", got)
	}
	if got := tc.Providers[0].Capabilities["claude-sonnet"]; !got.Chat {
		t.Fatalf("managed provider capabilities alias source: %#v", got)
	}

	_, err = managedTenantConfig([]modeladmin.ManagedProvider{{Definition: canonicalManagedTestDefinition(modeladmin.ProviderDefinition{
		ID: "openai-cache", Protocol: "openai_compatible", BaseURL: "https://openai.example.test/v1",
		APIKeyEnv: "HARNESS_TEST_MP_KEY", Models: []string{"m1"}, PromptCacheSessionAffinity: true,
	})}})
	if err == nil {
		t.Fatal("expected managed non-anthropic prompt cache session affinity to fail closed")
	}
}

func TestManagedModelResolverFallsBackToManagedDefaultTenant(t *testing.T) {
	store := newModelProviderTestStore(t)
	if _, err := store.Save(context.Background(), "default", "test", modeladmin.ProviderDefinition{
		ID: "mock", Version: "v1", Protocol: "mock", IsDefault: true, DefaultModel: "mock-model",
	}, 0); err != nil {
		t.Fatal(err)
	}
	var assembledTenant string
	resolver := newManagedModelResolver(store, func(tenantID string, tc TenantConfig) (*modelgateway.Facade, []string) {
		assembledTenant = tenantID
		return resolverTestFacade(tc.DefaultProvider, tc.DefaultModel), []string{tc.DefaultProvider}
	}, nil)

	if _, ok, err := resolver.Facade(context.Background(), "tenant-without-row"); err != nil || !ok {
		t.Fatal("expected resolver to use managed default tenant")
	}
	if assembledTenant != "default" {
		t.Fatalf("assembled tenant = %q, want default", assembledTenant)
	}
}

func TestManagedModelResolverDoesNotFallbackOnTenantConfigError(t *testing.T) {
	store := newModelProviderTestStore(t)
	ctx := context.Background()
	if _, err := store.Save(ctx, "default", "test", modeladmin.ProviderDefinition{
		ID: "mock", Version: "v1", Protocol: "mock", IsDefault: true, DefaultModel: "mock-model",
	}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(ctx, "tenant-a", "test", modeladmin.ProviderDefinition{
		ID: "broken", Version: "v1", Protocol: "openai_compatible",
		BaseURL: "https://example.test/v1", APIKeyEnv: "HARNESS_INTENTIONALLY_UNSET_PROVIDER_KEY",
		Models: []string{"m1"}, IsDefault: true, DefaultModel: "m1",
	}, 0); err != nil {
		t.Fatal(err)
	}
	var assembled []string
	resolver := newManagedModelResolver(store, func(tenantID string, tc TenantConfig) (*modelgateway.Facade, []string) {
		assembled = append(assembled, tenantID)
		return resolverTestFacade(tc.DefaultProvider, tc.DefaultModel), []string{tc.DefaultProvider}
	}, nil)

	if _, ok, err := resolver.Facade(ctx, "tenant-a"); err == nil || ok {
		t.Fatalf("invalid tenant config must fail closed: ok=%v err=%v", ok, err)
	}
	if len(assembled) != 0 {
		t.Fatalf("invalid tenant unexpectedly assembled fallback: %v", assembled)
	}
}

func TestManagedModelResolverFingerprintIncludesRevision(t *testing.T) {
	store, db := newModelProviderTestStoreAndDB(t)
	ctx := context.Background()
	t.Setenv("HARNESS_TEST_MP_KEY", "secret")
	definition := modeladmin.ProviderDefinition{
		ID: "claude", Version: "v1", Protocol: "anthropic",
		BaseURL: "http://localhost/open_api/anthropic", APIKeyEnv: "HARNESS_TEST_MP_KEY",
		ProviderKind: modelgateway.ProviderKindSessionAffinity,
		Models:       []string{"claude-sonnet"}, IsDefault: true, DefaultModel: "claude-sonnet",
		PromptCacheSessionAffinity: true,
	}
	saved, err := store.Save(ctx, "tenant-a", "test", definition, 0)
	if err != nil {
		t.Fatal(err)
	}
	var assembledAffinity []bool
	resolver := newManagedModelResolver(store, func(_ string, tc TenantConfig) (*modelgateway.Facade, []string) {
		assembledAffinity = append(assembledAffinity, tc.Providers[0].PromptCacheSessionAffinity)
		return resolverTestFacade(tc.DefaultProvider, tc.DefaultModel), []string{tc.DefaultProvider}
	}, nil)
	if _, ok, err := resolver.Facade(ctx, "tenant-a"); err != nil || !ok {
		t.Fatalf("first resolve: ok=%v err=%v", ok, err)
	}
	var originalUpdated int64
	if err := db.QueryRowContext(ctx, `SELECT updated_at_ms FROM model_providers WHERE tenant_id=? AND provider_id=?`, "tenant-a", "claude").Scan(&originalUpdated); err != nil {
		t.Fatal(err)
	}
	definition.PromptCacheSessionAffinity = false
	if _, err := store.Save(ctx, "tenant-a", "test", definition, saved.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE model_providers SET updated_at_ms=? WHERE tenant_id=? AND provider_id=?`, originalUpdated, "tenant-a", "claude"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := resolver.Facade(ctx, "tenant-a"); err != nil || !ok {
		t.Fatalf("second resolve: ok=%v err=%v", ok, err)
	}
	if len(assembledAffinity) != 2 || !assembledAffinity[0] || assembledAffinity[1] {
		t.Fatalf("facade was not rebuilt on same-millisecond revision: %v", assembledAffinity)
	}
}

func TestManagedModelResolverDoesNotServeCachedFacadeAfterSnapshotFailure(t *testing.T) {
	store, db := newModelProviderTestStoreAndDB(t)
	ctx := context.Background()
	if _, err := store.Save(ctx, "tenant-a", "test", modeladmin.ProviderDefinition{
		ID: "mock", Version: "v1", Protocol: "mock", IsDefault: true, DefaultModel: "mock-model",
	}, 0); err != nil {
		t.Fatal(err)
	}
	assembleCalls := 0
	resolver := newManagedModelResolver(store, func(_ string, tc TenantConfig) (*modelgateway.Facade, []string) {
		assembleCalls++
		return resolverTestFacade(tc.DefaultProvider, tc.DefaultModel), []string{tc.DefaultProvider}
	}, nil)
	if _, ok, err := resolver.Facade(ctx, "tenant-a"); err != nil || !ok {
		t.Fatalf("initial resolve: ok=%v err=%v", ok, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := resolver.Facade(ctx, "tenant-a"); err == nil || ok {
		t.Fatalf("corrupt snapshot reused cached facade: ok=%v err=%v", ok, err)
	}
	if assembleCalls != 1 {
		t.Fatalf("snapshot failure unexpectedly assembled a facade: calls=%d", assembleCalls)
	}
}

func TestManagedModelResolverRejectsFacadeWithoutDefaultRoute(t *testing.T) {
	store := newModelProviderTestStore(t)
	ctx := context.Background()
	if _, err := store.Save(ctx, "tenant-a", "test", modeladmin.ProviderDefinition{
		ID: "mock", Version: "v1", Protocol: "mock", IsDefault: true, DefaultModel: "mock-model",
	}, 0); err != nil {
		t.Fatal(err)
	}
	resolver := newManagedModelResolver(store, func(_ string, _ TenantConfig) (*modelgateway.Facade, []string) {
		return &modelgateway.Facade{}, []string{"mock"}
	}, nil)
	if _, ok, err := resolver.Facade(ctx, "tenant-a"); err == nil || ok {
		t.Fatalf("facade without default route accepted: ok=%v err=%v", ok, err)
	}
}

func TestManagedModelResolverRejectsSemanticallyInvalidStoredProvider(t *testing.T) {
	store, db := newModelProviderTestStoreAndDB(t)
	ctx := context.Background()
	if _, err := store.Save(ctx, "tenant-a", "test", modeladmin.ProviderDefinition{
		ID: "default-mock", Version: "v1", Protocol: "mock", IsDefault: true, DefaultModel: "mock-model",
	}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(ctx, "tenant-a", "test", modeladmin.ProviderDefinition{
		ID: "secondary", Version: "v1", Protocol: "mock",
	}, 0); err != nil {
		t.Fatal(err)
	}
	secondary, err := store.GetManaged(ctx, "tenant-a", "secondary")
	if err != nil {
		t.Fatal(err)
	}
	secondary.Definition.Protocol = "unknown_protocol"
	encoded, err := json.Marshal(secondary.Definition)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE model_providers SET definition_json=? WHERE tenant_id=? AND provider_id=?`, encoded, "tenant-a", "secondary"); err != nil {
		t.Fatal(err)
	}
	assembleCalls := 0
	resolver := newManagedModelResolver(store, func(_ string, tc TenantConfig) (*modelgateway.Facade, []string) {
		assembleCalls++
		return resolverTestFacade(tc.DefaultProvider, tc.DefaultModel), []string{tc.DefaultProvider}
	}, nil)
	if _, ok, err := resolver.Facade(ctx, "tenant-a"); err == nil || ok {
		t.Fatalf("semantically invalid stored provider was silently skipped: ok=%v err=%v", ok, err)
	}
	if assembleCalls != 0 {
		t.Fatalf("invalid stored provider reached facade assembly: calls=%d", assembleCalls)
	}
}

func TestManagedModelResolverConcurrentCachedReads(t *testing.T) {
	store := newModelProviderTestStore(t)
	ctx := context.Background()
	if _, err := store.Save(ctx, "tenant-a", "test", modeladmin.ProviderDefinition{
		ID: "mock", Version: "v1", Protocol: "mock", IsDefault: true, DefaultModel: "mock-model",
	}, 0); err != nil {
		t.Fatal(err)
	}
	var assembleCalls atomic.Int64
	resolver := newManagedModelResolver(store, func(_ string, tc TenantConfig) (*modelgateway.Facade, []string) {
		assembleCalls.Add(1)
		return resolverTestFacade(tc.DefaultProvider, tc.DefaultModel), []string{tc.DefaultProvider}
	}, nil)
	if _, ok, err := resolver.Facade(ctx, "tenant-a"); err != nil || !ok {
		t.Fatalf("warm resolve: ok=%v err=%v", ok, err)
	}

	const workers = 64
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok, err := resolver.Facade(ctx, "tenant-a"); err != nil {
				errs <- err
			} else if !ok {
				errs <- fmt.Errorf("concurrent resolve returned miss")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := assembleCalls.Load(); got != 1 {
		t.Fatalf("unchanged cached facade assembled %d times, want 1", got)
	}
}

func resolverTestFacade(provider, model string) *modelgateway.Facade {
	return &modelgateway.Facade{Router: modelgateway.NewStaticRouter(modelgateway.Route{
		Primary: modelgateway.ModelTarget{Provider: provider, Model: model},
	}, nil)}
}

func canonicalManagedTestDefinition(definition modeladmin.ProviderDefinition) modeladmin.ProviderDefinition {
	definition.TenantID = "t"
	definition.Scope = modeladmin.ScopeTenant
	if definition.Version == "" {
		definition.Version = "v1"
	}
	return definition
}

func newModelProviderTestStore(t *testing.T) *modeladmin.SQLManagedRegistry {
	store, _ := newModelProviderTestStoreAndDB(t)
	return store
}

func newModelProviderTestStoreAndDB(t *testing.T) (*modeladmin.SQLManagedRegistry, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "modelproviders.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := modeladmin.ApplySQLiteSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return modeladmin.NewSQLManagedRegistry(db), db
}
