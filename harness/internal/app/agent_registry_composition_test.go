package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/AGenUI/agenui-studio/harness/internal/agentregistry"
	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/gatewaycontract"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

func TestBuildAgentRegistryProductionImportsAndRestartsIdempotently(t *testing.T) {
	ctx := context.Background()
	db := openAgentRegistrySQLite(t)
	path := writeAgentRegistryPromptCatalog(t, "first prompt")
	options := productionAgentRegistryOptions(db, path)

	service, resolver, first, err := buildAgentRegistry(ctx, options)
	if err != nil {
		t.Fatalf("first buildAgentRegistry() error = %v", err)
	}
	if service == nil || resolver == nil || first == nil {
		t.Fatalf("first build result = (%v, %v, %#v), want all values", service, resolver, first)
	}
	if first.Loaded != 1 || first.Registered != 1 || first.Unchanged != 0 {
		t.Fatalf("first bootstrap result = %#v", first)
	}
	if err := service.ValidateProduction(); err != nil {
		t.Fatalf("ValidateProduction() error = %v", err)
	}
	assertAgentRegistryPrompt(t, ctx, resolver, "first prompt")

	_, secondResolver, second, err := buildAgentRegistry(ctx, options)
	if err != nil {
		t.Fatalf("second buildAgentRegistry() error = %v", err)
	}
	if second == nil || second.Loaded != 1 || second.Registered != 0 || second.Unchanged != 1 {
		t.Fatalf("second bootstrap result = %#v", second)
	}
	assertAgentRegistryPrompt(t, ctx, secondResolver, "first prompt")
	assertAgentRegistryRowCount(t, db, "agent_registry_entries", 1)
	// prompts=file：prompt 只进内存 store，不写库（ADR-0001）。
	assertAgentRegistryRowCount(t, db, "agent_registry_prompt_versions", 0)
}

func TestFileAgentExtensionPreflightIgnoresUnrelatedHistoricalRegistryRows(t *testing.T) {
	ctx := context.Background()
	db := openAgentRegistrySQLite(t)
	promptPath := writeAgentRegistryPromptCatalog(t, "preflight prompt")
	current := agentRegistryTestConfig()
	legacy := current
	legacy.AgentID = "legacy_agent"
	legacy.AgentType = "legacy_agent"
	legacy.Version = "legacy-v1"
	seed := productionAgentRegistryOptions(db, promptPath)
	seed.Loader = agentRegistryTestLoader{configs: []agentregistry.AgentConfig{current, legacy}}
	service, _, _, err := buildAgentRegistry(ctx, seed)
	if err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE agent_registry_entries SET effective_json='{}' WHERE agent_id=?",
		legacy.AgentID,
	); err != nil {
		t.Fatalf("corrupt unrelated legacy fixture: %v", err)
	}
	if _, err := service.ListAgentConfigs(ctx); !errors.Is(err, agentregistry.ErrStoreCorrupt) {
		t.Fatalf("ListAgentConfigs() error = %v, want legacy ErrStoreCorrupt fixture", err)
	}

	loader := agentRegistryTestLoader{configs: []agentregistry.AgentConfig{current}}
	if err := preflightFileAgentExtensionBindings(ctx, loader, nil); err != nil {
		t.Fatalf("preflight current file configs: %v", err)
	}

	unknownExtension := current
	unknownExtension.Extensions.ToolCallInterceptors = []string{"missing.preflight.interceptor"}
	if err := preflightFileAgentExtensionBindings(
		ctx,
		agentRegistryTestLoader{configs: []agentregistry.AgentConfig{unknownExtension}},
		nil,
	); err == nil || !strings.Contains(err.Error(), "missing.preflight.interceptor") {
		t.Fatalf("unknown current file extension error = %v, want fail-closed binding preflight", err)
	}
}

func TestBuildAgentRegistryDatabasePromptsSkipYAMLImport(t *testing.T) {
	// prompts=database：数据库是唯一事实源，启动不再导入 YAML（ADR-0002）。
	ctx := context.Background()
	db := openAgentRegistrySQLite(t)
	path := writeAgentRegistryPromptCatalog(t, "yaml prompt must be ignored")
	seeded, err := agentregistry.NewSQLPromptStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := seeded.Create(ctx, agentregistry.PromptVersion{
		Ref: "prompt://composition/system", Version: "v1", Content: "database prompt",
	}); err != nil {
		t.Fatalf("seed database prompt: %v", err)
	}

	options := productionAgentRegistryOptions(db, path)
	options.PromptsFromDatabase = true
	_, resolver, _, err := buildAgentRegistry(ctx, options)
	if err != nil {
		t.Fatalf("buildAgentRegistry(prompts=database) error = %v", err)
	}
	assertAgentRegistryPrompt(t, ctx, resolver, "database prompt")
	// YAML 内容未被导入：库中仍只有预置的那一条。
	assertAgentRegistryRowCount(t, db, "agent_registry_prompt_versions", 1)

	// path 在 database 模式下可以完全缺省。
	options.PromptCatalogPath = ""
	if _, _, _, err := buildAgentRegistry(ctx, options); err != nil {
		t.Fatalf("buildAgentRegistry(prompts=database, no path) error = %v", err)
	}
}

func TestBuildAgentRegistryProductionRequiresDependencies(t *testing.T) {
	db := openAgentRegistrySQLite(t)
	path := writeAgentRegistryPromptCatalog(t, "required dependencies")
	valid := productionAgentRegistryOptions(db, path)
	tests := map[string]func(*agentRegistryBuildOptions){
		"database":       func(options *agentRegistryBuildOptions) { options.Database = nil },
		"tool registry":  func(options *agentRegistryBuildOptions) { options.ToolRegistry = nil },
		"loader":         func(options *agentRegistryBuildOptions) { options.Loader = nil },
		"prompt catalog": func(options *agentRegistryBuildOptions) { options.PromptCatalogPath = "" },
	}
	for name, remove := range tests {
		t.Run(name, func(t *testing.T) {
			options := valid
			remove(&options)
			service, resolver, result, err := buildAgentRegistry(context.Background(), options)
			if err == nil {
				t.Fatal("missing production dependency must fail closed")
			}
			if service != nil || resolver != nil || result != nil {
				t.Fatalf("missing dependency returned partial composition = (%v, %v, %#v)", service, resolver, result)
			}
		})
	}
}

func TestBuildAgentRegistryProductionPersistsGatewayTarget(t *testing.T) {
	ctx := context.Background()
	db := openAgentRegistrySQLite(t)
	path := writeAgentRegistryPromptCatalog(t, "gateway prompt")
	target := agentRegistryTestConfig()
	target.AgentID = "gateway_target"
	target.AgentType = "gateway_target"
	target.DataPassing = agentruntime.DataPassingPolicy{ScopedDataKeys: []string{"locale"}}
	target.Gateway = &agentregistry.GatewayTargetConfig{
		ProviderKind: gatewaycontract.SubAgentProviderLocalAgent,
		Plugins: []agentregistry.GatewayPluginConfig{
			{PluginID: "basic_validator"},
		},
	}
	parent := agentRegistryTestConfig()
	parent.AgentID = "gateway_parent"
	parent.AgentType = "gateway_parent"
	parent.SubAgents = []string{target.AgentID}
	options := productionAgentRegistryOptions(db, path)
	options.Loader = agentRegistryTestLoader{configs: []agentregistry.AgentConfig{target, parent}}

	service, _, first, err := buildAgentRegistry(ctx, options)
	if err != nil {
		t.Fatalf("first buildAgentRegistry() error = %v", err)
	}
	if first == nil || first.Registered != 2 {
		t.Fatalf("first bootstrap result = %#v", first)
	}
	firstTarget := resolveAgentRegistryGatewayTarget(t, ctx, service, parent, target.AgentID)

	restarted, _, second, err := buildAgentRegistry(ctx, options)
	if err != nil {
		t.Fatalf("second buildAgentRegistry() error = %v", err)
	}
	if second == nil || second.Unchanged != 2 {
		t.Fatalf("second bootstrap result = %#v", second)
	}
	secondTarget := resolveAgentRegistryGatewayTarget(t, ctx, restarted, parent, target.AgentID)
	if secondTarget.Effective.ConfigHash != firstTarget.Effective.ConfigHash ||
		secondTarget.Gateway.ProviderKind != firstTarget.Gateway.ProviderKind ||
		len(secondTarget.Gateway.Plugins) != 1 || secondTarget.Gateway.Plugins[0].PluginID != "basic_validator" {
		t.Fatalf("Gateway target drifted after restart: first=%#v second=%#v", firstTarget, secondTarget)
	}
	if got := secondTarget.Effective.Definition.DataPassing.ScopedDataKeys; len(got) != 1 || got[0] != "locale" {
		t.Fatalf("persisted data-passing policy = %#v, want [locale]", got)
	}
}

func TestBuildAgentRegistryLocalWithoutDatabaseUsesMemoryStores(t *testing.T) {
	path := writeAgentRegistryPromptCatalog(t, "local prompt")
	service, resolver, result, err := buildAgentRegistry(context.Background(), agentRegistryBuildOptions{
		Loader:            agentRegistryTestLoader{configs: []agentregistry.AgentConfig{agentRegistryTestConfig()}},
		PromptCatalogPath: path,
	})
	if err != nil {
		t.Fatalf("buildAgentRegistry(local) error = %v", err)
	}
	if result == nil || result.Registered != 1 {
		t.Fatalf("local bootstrap result = %#v", result)
	}
	if !errors.Is(service.ValidateProduction(), agentregistry.ErrProductionMemoryStore) {
		t.Fatalf("local service unexpectedly passed production validation: %v", service.ValidateProduction())
	}
	assertAgentRegistryPrompt(t, context.Background(), resolver, "local prompt")
}

func TestBuildFormalCompositionAdminPromptStoreAlwaysTargetsDatabase(t *testing.T) {
	// 管理接口管理的是数据库记录：prompts=file 时运行时只读本地 YAML，但管理面
	// PromptStore 必须指向数据库，不能写进程内存（本地文件改动经重启生效）。
	ctx := context.Background()
	db := openAgentRegistrySQLite(t)
	promptPath := writeAgentRegistryPromptCatalog(t, "yaml prompt")

	// adminStore 模拟 composition 在 prompts=file 时单独构建的管理面 SQL store。
	adminStore, err := agentregistry.NewSQLPromptStore(db)
	if err != nil {
		t.Fatal(err)
	}

	options := agentRegistryBuildOptions{
		Loader:            agentRegistryTestLoader{configs: []agentregistry.AgentConfig{agentRegistryTestConfig()}},
		PromptCatalogPath: promptPath,
	}
	// prompts=file 的运行时 store 是内存实现：YAML 内容可解析，但不落库。
	_, resolver, _, err := buildAgentRegistry(ctx, options)
	if err != nil {
		t.Fatalf("buildAgentRegistry(prompts=file) error = %v", err)
	}
	assertAgentRegistryPrompt(t, ctx, resolver, "yaml prompt")
	assertAgentRegistryRowCount(t, db, "agent_registry_prompt_versions", 0)

	// 管理面 store 写入必须落库。
	managed := agentregistry.PromptVersion{Ref: "prompt://admin/system", Version: "v1", Content: "written through admin API"}
	if err := adminStore.Create(ctx, managed); err != nil {
		t.Fatalf("admin prompt write must reach the database: %v", err)
	}
	assertAgentRegistryRowCount(t, db, "agent_registry_prompt_versions", 1)
	// 而运行时 resolver（内存）看不到管理面写入的记录——两条通道互不影响。
	if _, err := agentregistry.PromptStoreFromResolver(resolver).Get(ctx, agentregistry.PromptKey{Ref: managed.Ref, Version: managed.Version}); err == nil {
		t.Fatal("file-mode runtime resolver must not see admin database writes")
	}
}

func TestBuildAgentRegistryDatabasePromptsPersistManagedPromptsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	db := openAgentRegistrySQLite(t)
	// prompts=database 时启动不导入 YAML，agent 引用的 prompt 必须预先在库中。
	seeded, err := agentregistry.NewSQLPromptStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := seeded.Create(ctx, agentregistry.PromptVersion{
		Ref: "prompt://composition/system", Version: "v1", Content: "database prompt",
	}); err != nil {
		t.Fatalf("seed database prompt: %v", err)
	}
	options := agentRegistryBuildOptions{
		Database:            db,
		PromptsFromDatabase: true,
		Loader:              agentRegistryTestLoader{configs: []agentregistry.AgentConfig{agentRegistryTestConfig()}},
	}

	_, resolver, _, err := buildAgentRegistry(ctx, options)
	if err != nil {
		t.Fatalf("first buildAgentRegistry(prompts=database) error = %v", err)
	}
	store := agentregistry.PromptStoreFromResolver(resolver)
	if store == nil {
		t.Fatal("database prompt build did not expose its PromptStore")
	}
	managed := agentregistry.PromptVersion{
		Ref: "prompt://tenant-local/agent-composition/system", Version: "v2", Content: "managed local prompt",
	}
	if err := store.Create(ctx, managed); err != nil {
		t.Fatalf("Create(managed prompt) error = %v", err)
	}

	_, restartedResolver, _, err := buildAgentRegistry(ctx, options)
	if err != nil {
		t.Fatalf("second buildAgentRegistry(prompts=database) error = %v", err)
	}
	restarted := agentregistry.PromptStoreFromResolver(restartedResolver)
	got, err := restarted.Get(ctx, agentregistry.PromptKey{Ref: managed.Ref, Version: managed.Version})
	if err != nil {
		t.Fatalf("Get(managed prompt after restart) error = %v", err)
	}
	if got.Content != managed.Content {
		t.Fatalf("managed prompt content = %q, want %q", got.Content, managed.Content)
	}
	// 预置的 agent prompt + 管理面写入的 prompt，共两条。
	assertAgentRegistryRowCount(t, db, "agent_registry_prompt_versions", 2)
}

func TestBuildAgentRegistryDatabaseOnlySkipsAgentConfigLoader(t *testing.T) {
	ctx := context.Background()
	db := openAgentRegistrySQLite(t)
	path := writeAgentRegistryPromptCatalog(t, "db only prompt")
	service, resolver, result, err := buildAgentRegistry(ctx, agentRegistryBuildOptions{
		DatabaseOnly:      true,
		Database:          db,
		ToolRegistry:      toolgateway.NewStaticRegistry(nil),
		Loader:            erroringAgentRegistryLoader{},
		PromptCatalogPath: path,
	})
	if err != nil {
		t.Fatalf("buildAgentRegistry(database only) error = %v", err)
	}
	if service == nil || resolver == nil {
		t.Fatalf("database-only build result = (%v, %v)", service, resolver)
	}
	if result != nil {
		t.Fatalf("database-only build unexpectedly bootstrapped from files: %#v", result)
	}
	assertAgentRegistryPrompt(t, ctx, resolver, "db only prompt")
	assertAgentRegistryRowCount(t, db, "agent_registry_entries", 0)
}

func TestBuildFormalCompositionRejectsProductionMemorySnapshotStoreBeforeDatabaseWrites(t *testing.T) {
	db, recorder := newSharedSQLTestDatabase(t)

	_, err := buildFormalComposition(
		context.Background(),
		HarnessConfig{Environment: EnvironmentProduction, Features: HarnessFeatureConfig{AllowInProcessCapabilities: true}},
		ModelConfig{},
		storage.Stores{},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		formalCompositionDependencies{
			database:         db,
			contextSnapshots: contextpkg.NewInMemorySnapshotStore(),
		},
	)
	if err == nil || !strings.Contains(err.Error(), "in-memory ContextSnapshot store") {
		t.Fatalf("buildFormalComposition() error = %v, want production memory snapshot rejection", err)
	}
	if got := recorder.execCalls.Load(); got != 0 {
		t.Fatalf("production dependency validation executed %d SQL statements, want 0", got)
	}
}

func TestBuildFormalCompositionValidatesConfiguredCapabilitiesBeforeDatabaseWrites(t *testing.T) {
	db, recorder := newSharedSQLTestDatabase(t)

	_, err := buildFormalComposition(
		context.Background(),
		HarnessConfig{Environment: EnvironmentTesting, Components: fileSourcedCapabilityComponents(), Features: HarnessFeatureConfig{AllowInProcessCapabilities: true}},
		ModelConfig{},
		storage.Stores{},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		formalCompositionDependencies{
			database:         db,
			contextSnapshots: durableSnapshotStoreStub{},
		},
	)
	if err == nil || !strings.Contains(err.Error(), "required component config path is empty") {
		t.Fatalf("buildFormalComposition() error = %v, want configured capability validation", err)
	}
	if got := recorder.execCalls.Load(); got != 0 {
		t.Fatalf("testing capability preflight executed %d SQL statements, want 0", got)
	}
}

func TestBuildFormalCompositionAllowsProductionFileCapabilitiesUntilConfigValidation(t *testing.T) {
	db, recorder := newSharedSQLTestDatabase(t)

	_, err := buildFormalComposition(
		context.Background(),
		HarnessConfig{Environment: EnvironmentProduction, Components: fileSourcedCapabilityComponents(), Features: HarnessFeatureConfig{AllowInProcessCapabilities: true}},
		ModelConfig{},
		storage.Stores{},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		formalCompositionDependencies{
			database:         db,
			contextSnapshots: durableSnapshotStoreStub{},
		},
	)
	if err == nil || !strings.Contains(err.Error(), "required component config path is empty") {
		t.Fatalf("buildFormalComposition() error = %v, want file capability config validation", err)
	}
	if got := recorder.execCalls.Load(); got != 0 {
		t.Fatalf("production capability preflight executed %d SQL statements, want 0", got)
	}
}

type durableSnapshotStoreStub struct{}

func (durableSnapshotStoreStub) Save(_ context.Context, snapshot contextpkg.Snapshot) (string, error) {
	return snapshot.ID, nil
}

func (durableSnapshotStoreStub) Load(context.Context, string) (contextpkg.Snapshot, error) {
	return contextpkg.Snapshot{}, contextpkg.ErrSnapshotNotFound
}

type agentRegistryTestLoader struct {
	configs []agentregistry.AgentConfig
}

func (loader agentRegistryTestLoader) Load(context.Context) ([]agentregistry.AgentConfig, error) {
	return append([]agentregistry.AgentConfig(nil), loader.configs...), nil
}

type erroringAgentRegistryLoader struct{}

func (erroringAgentRegistryLoader) Load(context.Context) ([]agentregistry.AgentConfig, error) {
	return nil, errors.New("agent config loader should not run")
}

func productionAgentRegistryOptions(db *sql.DB, promptPath string) agentRegistryBuildOptions {
	return agentRegistryBuildOptions{
		Production:        true,
		Database:          db,
		ToolRegistry:      toolgateway.NewStaticRegistry(nil),
		Loader:            agentRegistryTestLoader{configs: []agentregistry.AgentConfig{agentRegistryTestConfig()}},
		PromptCatalogPath: promptPath,
	}
}

func agentRegistryTestConfig() agentregistry.AgentConfig {
	return agentregistry.AgentConfig{
		AgentID:       "composition_agent",
		AgentType:     "assistant",
		Version:       "v1",
		Status:        agentregistry.AgentStatusEnabled,
		Runtime:       agentruntime.RuntimeSpec{Type: agentruntime.RuntimeTypeMock, Mode: agentruntime.RuntimeModeReact},
		PromptRef:     "prompt://composition/system",
		PromptVersion: "v1",
		Capability: agentregistry.CapabilityConfig{
			ExecutionModes: []agentregistry.ExecutionMode{agentregistry.ExecutionModeSingleAgent},
		},
		Orchestration: agentregistry.OrchestrationConfig{DefaultMode: agentregistry.ExecutionModeSingleAgent},
		ProtocolPolicy: agentregistry.ProtocolPolicy{
			Streaming: true, OutputFormat: "markdown",
		},
	}
}

func resolveAgentRegistryGatewayTarget(
	t *testing.T,
	ctx context.Context,
	service *agentregistry.Service,
	parent agentregistry.AgentConfig,
	targetID string,
) agentregistry.ResolvedGatewayTarget {
	t.Helper()
	effective, err := service.ResolveEffectiveConfig(ctx, agentregistry.ResolveRequest{
		AgentID: parent.AgentID,
		Version: parent.Version,
	})
	if err != nil {
		t.Fatalf("ResolveEffectiveConfig(parent) error = %v", err)
	}
	resolved, err := service.ResolveGatewayTarget(ctx, agentregistry.ResolveGatewayTargetRequest{
		ParentAgentID:           parent.AgentID,
		ParentAgentVersion:      parent.Version,
		ParentConfigSnapshotRef: effective.ConfigSnapshotRef,
		ParentConfigHash:        effective.ConfigHash,
		SubAgentRef:             targetID,
	})
	if err != nil {
		t.Fatalf("ResolveGatewayTarget() error = %v", err)
	}
	return resolved
}

func openAgentRegistrySQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite) error = %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store, err := agentregistry.NewSQLStore(db)
	if err != nil {
		t.Fatalf("NewSQLStore() error = %v", err)
	}
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("SQLStore.EnsureSchema() error = %v", err)
	}
	prompts, err := agentregistry.NewSQLPromptStore(db)
	if err != nil {
		t.Fatalf("NewSQLPromptStore() error = %v", err)
	}
	if err := prompts.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("SQLPromptStore.EnsureSchema() error = %v", err)
	}
	return db
}

func writeAgentRegistryPromptCatalog(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prompts.yaml")
	writeAgentRegistryPromptCatalogAt(t, path, content)
	return path
}

func writeAgentRegistryPromptCatalogAt(t *testing.T, path, content string) {
	t.Helper()
	source := fmt.Sprintf(`schema_version: harness.prompts.v1
prompts:
  - prompt_ref: prompt://composition/system
    version: v1
    content: %q
`, content)
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatalf("write prompt catalog: %v", err)
	}
}

func assertAgentRegistryPrompt(
	t *testing.T,
	ctx context.Context,
	resolver agentregistry.PromptResolver,
	want string,
) {
	t.Helper()
	prompt, err := resolver.Resolve(ctx, agentregistry.PromptKey{Ref: "prompt://composition/system", Version: "v1"})
	if err != nil {
		t.Fatalf("Resolve(prompt) error = %v", err)
	}
	if prompt.Content != want {
		t.Fatalf("resolved prompt content = %q, want %q", prompt.Content, want)
	}
}

func assertAgentRegistryRowCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); err != nil {
		t.Fatalf("count %s rows: %v", table, err)
	}
	if got != want {
		t.Fatalf("%s row count = %d, want %d", table, got, want)
	}
}
