package agentregistry

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
)

func TestRegistryFreezesResolvedSystemPromptWithoutPersistingContent(t *testing.T) {
	ctx := context.Background()
	promptStore := NewMemoryPromptStore()
	content := "你是旅行规划助手。回答前先核对日期和目的地。"
	if err := promptStore.Create(ctx, PromptVersion{
		Ref: "prompt://travel/system", Version: "v4", Content: content,
		Variables: []string{"current_date", "destination"},
	}); err != nil {
		t.Fatalf("create prompt: %v", err)
	}
	prompts, err := NewPromptResolver(promptStore)
	if err != nil {
		t.Fatalf("new prompt resolver: %v", err)
	}
	store := NewMemoryStore()
	service := NewService(WithStore(store), WithPromptResolver(prompts))
	if _, err := service.RuntimeSystemPromptResolver(); err != nil {
		t.Fatalf("runtime prompt resolver: %v", err)
	}
	cfg := extendedConfig("travel_prompt_agent", "v1")
	cfg.PromptRef = "prompt://travel/system"
	cfg.PromptVersion = "v4"
	// 旧作者态字段只是引用别名，绝不能再被当成正文写入 Runtime metadata。
	cfg.Context.SystemPrompt = "travel_system_v4"
	cfg.Metadata["prompt_hash"] = "forged"
	cfg.Metadata["prompt_content_hash"] = "forged"
	cfg.Metadata["prompt_snapshot_ref"] = "forged"
	cfg.Metadata["prompt_content_ref"] = "https://secret.invalid/prompt?token=forged"
	validation, err := service.ValidateAgent(ctx, cfg)
	if err != nil {
		t.Fatalf("validate agent: %v", err)
	}
	if validation.ResolvedPrompt == nil || validation.ResolvedPrompt.Content != "" {
		t.Fatalf("validation result leaked prompt content: %#v", validation.ResolvedPrompt)
	}

	if _, err := service.RegisterAgent(ctx, cfg); err != nil {
		t.Fatalf("register agent: %v", err)
	}
	effective, err := service.ResolveEffectiveConfig(ctx, ResolveRequest{AgentID: cfg.AgentID, Version: cfg.Version})
	if err != nil {
		t.Fatalf("resolve effective config: %v", err)
	}
	resolved, err := prompts.Resolve(ctx, PromptKey{Ref: cfg.PromptRef, Version: cfg.PromptVersion})
	if err != nil {
		t.Fatalf("resolve prompt: %v", err)
	}
	if effective.PromptHash != resolved.PromptHash {
		t.Fatalf("prompt hash = %q, want %q", effective.PromptHash, resolved.PromptHash)
	}
	metadata := effective.Definition.Metadata
	if metadata["prompt_version"] != cfg.PromptVersion || metadata["prompt_hash"] != resolved.PromptHash ||
		metadata["prompt_snapshot_ref"] != resolved.SnapshotRef || metadata["prompt_content_hash"] != resolved.ContentHash {
		t.Fatalf("prompt snapshot was not frozen: %#v", metadata)
	}
	if metadata["system_prompt"] != "" || metadata["prompt_content_ref"] != "" {
		t.Fatalf("system prompt alias leaked into runtime metadata: %#v", metadata)
	}
	runtimeResolver, err := service.RuntimeSystemPromptResolver()
	if err != nil {
		t.Fatalf("runtime prompt resolver: %v", err)
	}
	assembler := agentruntime.NewDefaultRuntimeContextAssembler(nil)
	assembler.SystemPrompts = runtimeResolver
	pkg, err := assembler.Build(ctx, agentruntime.RuntimeContextAssemblyRequest{
		Run: agentruntime.RunRequest{
			SessionID: "session_prompt", RunID: "run_prompt", Definition: effective.Definition,
			Input: []agentruntime.Message{{Role: "user", Content: "规划一次旅行"}},
		},
	})
	if err != nil {
		t.Fatalf("build runtime context: %v", err)
	}
	if len(pkg.Messages.ConversationWindow) == 0 || pkg.Messages.ConversationWindow[0].Role != "system" ||
		pkg.Messages.ConversationWindow[0].Content != content {
		t.Fatalf("runtime did not inject frozen system prompt: %#v", pkg.Messages.ConversationWindow)
	}

	record, err := store.Get(ctx, StoreLookup{AgentID: cfg.AgentID, Version: cfg.Version})
	if err != nil {
		t.Fatalf("get stored agent: %v", err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal stored agent: %v", err)
	}
	if strings.Contains(string(encoded), content) {
		t.Fatal("system prompt content must not be copied into agent registry entries")
	}
	if record.Config.Context.SystemPrompt != "" {
		t.Fatalf("legacy system prompt alias was persisted: %#v", record.Config.Context)
	}
}

func TestReloadFreezesResolvedSystemPrompt(t *testing.T) {
	ctx := context.Background()
	promptStore := NewMemoryPromptStore()
	cfg := extendedConfig("reload_prompt_agent", "v1")
	if err := promptStore.Create(ctx, PromptVersion{
		Ref: cfg.PromptRef, Version: cfg.PromptVersion, Content: "你是重载后的固定系统提示词。",
	}); err != nil {
		t.Fatalf("create prompt: %v", err)
	}
	resolver, err := NewPromptResolver(promptStore)
	if err != nil {
		t.Fatalf("new prompt resolver: %v", err)
	}
	service := NewService(
		WithLoader(staticLoader{configs: []AgentConfig{cfg}}),
		WithPromptResolver(resolver),
	)
	if _, err := service.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	effective, err := service.ResolveEffectiveConfig(ctx, ResolveRequest{AgentID: cfg.AgentID, Version: cfg.Version})
	if err != nil {
		t.Fatalf("resolve effective config: %v", err)
	}
	prompt, err := resolver.Resolve(ctx, PromptKey{Ref: cfg.PromptRef, Version: cfg.PromptVersion})
	if err != nil {
		t.Fatalf("resolve prompt: %v", err)
	}
	if effective.PromptHash != prompt.PromptHash ||
		effective.Definition.Metadata["prompt_snapshot_ref"] != prompt.SnapshotRef {
		t.Fatalf("reload did not freeze prompt snapshot: %#v", effective)
	}
}

func TestRegistryRequiresPromptResolverByDefault(t *testing.T) {
	_, err := NewService().RegisterAgent(context.Background(), extendedConfig("strict_prompt_agent", "v1"))
	var registryErr *RegistryError
	if !errors.As(err, &registryErr) || registryErr.Code != CodeDependencyMissing || registryErr.Field != "prompt_resolver" {
		t.Fatalf("missing resolver error = %v", err)
	}
}

func TestRegistryPromptResolverFailsClosed(t *testing.T) {
	ctx := context.Background()
	resolver, err := NewPromptResolver(NewMemoryPromptStore())
	if err != nil {
		t.Fatalf("new prompt resolver: %v", err)
	}
	service := NewService(WithPromptResolver(resolver))
	cfg := extendedConfig("missing_prompt_agent", "v1")

	_, err = service.RegisterAgent(ctx, cfg)
	var registryErr *RegistryError
	if !errors.As(err, &registryErr) || registryErr.Code != CodeDependencyMissing || registryErr.Field != "prompt_ref" {
		t.Fatalf("missing prompt error = %v", err)
	}

	cfg.PromptVersion = ""
	_, err = service.RegisterAgent(ctx, cfg)
	if !errors.As(err, &registryErr) || registryErr.Code != CodeInvalidConfig || registryErr.Field != "prompt_version" {
		t.Fatalf("unversioned prompt error = %v", err)
	}
}

func TestCompileUsesResolvedPromptHash(t *testing.T) {
	cfg := extendedConfig("compiled_prompt_agent", "v1")
	first := &PromptSnapshot{
		Ref: cfg.PromptRef, Version: cfg.PromptVersion,
		SnapshotRef: "prompt-snapshot://first", ContentHash: PromptContentHash("first"), PromptHash: "sha256:first",
	}
	second := *first
	second.SnapshotRef = "prompt-snapshot://second"
	second.ContentHash = PromptContentHash("second")
	second.PromptHash = "sha256:second"

	now := time.Unix(1, 0).UTC()
	one, err := compileEffectiveConfig(cfg, ResolvedDependencies{}, now, first)
	if err != nil {
		t.Fatalf("compile first prompt: %v", err)
	}
	two, err := compileEffectiveConfig(cfg, ResolvedDependencies{}, now, &second)
	if err != nil {
		t.Fatalf("compile second prompt: %v", err)
	}
	if one.ConfigHash == two.ConfigHash || one.ConfigSnapshotRef == two.ConfigSnapshotRef {
		t.Fatalf("prompt content change must change config snapshot: %#v %#v", one, two)
	}
}

func TestLegacySystemPromptFieldOnlyAcceptsReferenceAlias(t *testing.T) {
	ctx := context.Background()
	valid := extendedConfig("legacy_prompt_alias", "v1")
	valid.PromptRef = ""
	valid.Context.SystemPrompt = "travel/system_v4"
	service := NewService(WithLegacyUnresolvedPrompts())
	if _, err := service.RegisterAgent(ctx, valid); err != nil {
		t.Fatalf("register legacy reference alias: %v", err)
	}
	effective, err := service.ResolveEffectiveConfig(ctx, ResolveRequest{AgentID: valid.AgentID, Version: valid.Version})
	if err != nil {
		t.Fatalf("resolve legacy alias: %v", err)
	}
	if effective.Definition.PromptRef != "prompt://travel/system_v4" || effective.Definition.Metadata["system_prompt"] != "" {
		t.Fatalf("legacy alias normalization drift: %#v", effective.Definition)
	}

	raw := extendedConfig("legacy_prompt_body", "v1")
	raw.PromptRef = ""
	raw.Context.SystemPrompt = "你是助手。\n不要把正文当引用。"
	if _, err := NewService(WithLegacyUnresolvedPrompts()).RegisterAgent(ctx, raw); !errors.Is(err, ErrPromptRefRequired) {
		t.Fatalf("inline system prompt error = %v", err)
	}
}
