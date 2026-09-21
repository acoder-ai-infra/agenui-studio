package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/skill"
)

func TestGovernedCapabilitiesReachCanonicalModelPackage(t *testing.T) {
	mcpRegistry, err := mcp.NewInMemoryRegistry(mcp.ServerDefinition{ID: "maps", Version: "1", Scope: mcp.ScopeTenant, TenantID: "tenant"})
	if err != nil {
		t.Fatal(err)
	}
	mcpService, err := mcp.NewService(mcpRegistry, mcp.StaticClientProvider{Value: runtimeMCPClient{}}, mcp.NewInMemorySnapshotStore(), func() string { return "mcp-snapshot" })
	if err != nil {
		t.Fatal(err)
	}
	skillService, err := skill.NewService(skill.NewInMemoryRepository(), nil, func() string { return "skill-snapshot" })
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = skillService.Publish(context.Background(), skill.Principal{TenantID: "tenant", AgentID: "publisher"}, skill.Definition{
		ID: "route", Version: "1.0.0", TenantID: "tenant", InjectionStrategy: skill.InjectSystem,
		Dependencies: skill.Dependencies{Tools: []string{"search@v1"}, MCPServers: []string{"maps"}},
		Policy:       skill.Policy{Scope: skill.ScopeTenant, AllowedAgents: []string{"agent_1"}},
	}, []byte("Always verify the destination before routing."))
	if err != nil {
		t.Fatal(err)
	}

	req := testRunRequest()
	req.TenantID = "tenant"
	req.UserID = "user"
	req.Definition.Metadata = map[string]string{
		"mcp_servers": `["maps"]`,
		"skill_refs":  `["route@1.0.0"]`,
	}
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.CapabilitySnapshots = GovernedCapabilitySnapshotProvider{MCP: mcpService, Skills: skillService}
	pkg, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: req, Trace: observability.TraceContext{TraceID: "trace"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Capabilities.MCPSnapshots) != 1 || len(pkg.Capabilities.SkillSnapshots) != 1 {
		t.Fatalf("governed snapshots missing: %#v", pkg.Capabilities)
	}
	if len(pkg.Capabilities.ToolDefinitions) != 1 || pkg.Capabilities.ToolDefinitions[0].Name != "maps.search" {
		t.Fatalf("MCP schema was not compiled into runtime-neutral model tools: %#v", pkg.Capabilities.ToolDefinitions)
	}
	if len(pkg.Capabilities.Tools) != 1 || pkg.Capabilities.Tools[0] != "search@v1" || len(pkg.Capabilities.MCPServers) != 1 {
		t.Fatalf("skill dependencies were not expanded and deduplicated: %#v", pkg.Capabilities)
	}
	if !hasModelMessage(pkg.Messages.ConversationWindow, "system", "Always verify the destination before routing.") {
		t.Fatalf("system-injected skill did not reach model messages: %#v", pkg.Messages.ConversationWindow)
	}
}

func TestGovernedCapabilitiesCompileRegistryAndSkillToolSchemas(t *testing.T) {
	provider := GovernedCapabilitySnapshotProvider{ToolSchemas: ToolDefinitionSnapshotProviderFunc(func(_ context.Context, _ RunRequest, refs []string) ([]ModelToolDefinition, error) {
		if len(refs) != 1 || refs[0] != "search@v1" {
			t.Fatalf("unexpected tool refs: %#v", refs)
		}
		return []ModelToolDefinition{{Name: "search", Schema: json.RawMessage(`{"type":"object"}`)}}, nil
	})}
	req := testRunRequest()
	req.Definition.ToolRefs = []string{"search@v1"}
	req.ConfigHash = "sha256:config"
	resolved, err := provider.Resolve(context.Background(), RuntimeContextAssemblyRequest{Run: req})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.ToolDefinitions) != 1 || resolved.ToolDefinitions[0].Name != "search" {
		t.Fatalf("tool schemas missing: %#v", resolved.ToolDefinitions)
	}
}

func TestGovernedCapabilitiesCarryToolSnapshotIntoModelContext(t *testing.T) {
	provider := GovernedCapabilitySnapshotProvider{ToolSchemas: governedToolSnapshotProviderFunc(func(_ context.Context, _ RunRequest, refs []string) (GovernedToolDefinitionSnapshot, error) {
		return GovernedToolDefinitionSnapshot{
			Snapshot: ToolSchemaSnapshot{
				SnapshotID: "tool_snapshot_1", ToolRefs: append([]string(nil), refs...),
				CapabilityHash: "sha256:capability", PolicyHash: "sha256:policy",
			},
			Definitions: []ModelToolDefinition{{Name: "search", Schema: json.RawMessage(`{"type":"object"}`)}},
		}, nil
	})}
	req := testRunRequest()
	req.Definition.ToolRefs = []string{"search@v1"}
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.CapabilitySnapshots = provider
	pkg, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: req})
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Capabilities.ToolSnapshot == nil || pkg.Capabilities.ToolSnapshot.SnapshotID != "tool_snapshot_1" ||
		pkg.Capabilities.ToolSnapshot.PolicyHash != "sha256:policy" || pkg.Run.ConfigHash != req.ConfigHash || pkg.ContextHash != ComputeModelContextHash(pkg) {
		t.Fatalf("governed tool snapshot missing from package: %#v", pkg.Capabilities.ToolSnapshot)
	}
	pkg.Run.ConfigHash = "sha256:changed"
	if pkg.ContextHash == ComputeModelContextHash(pkg) {
		t.Fatal("config hash mutation must invalidate model context hash")
	}
	pkg.Run.ConfigHash = req.ConfigHash
	providerSnapshot := pkg.Capabilities.ToolSnapshot
	providerSnapshot.ToolRefs[0] = "mutated@v9"
	if pkg.ContextHash == ComputeModelContextHash(pkg) {
		t.Fatal("tool snapshot mutation must invalidate model context hash")
	}
}

func TestGovernedCapabilitiesRejectMismatchedToolSnapshot(t *testing.T) {
	provider := GovernedCapabilitySnapshotProvider{ToolSchemas: governedToolSnapshotProviderFunc(func(context.Context, RunRequest, []string) (GovernedToolDefinitionSnapshot, error) {
		return GovernedToolDefinitionSnapshot{Snapshot: ToolSchemaSnapshot{
			SnapshotID: "tool_snapshot_1", ToolRefs: []string{"other@v1"},
			CapabilityHash: "sha256:capability", PolicyHash: "sha256:policy",
		}}, nil
	})}
	req := testRunRequest()
	req.Definition.ToolRefs = []string{"search@v1"}
	_, err := provider.Resolve(context.Background(), RuntimeContextAssemblyRequest{Run: req})
	if !errors.Is(err, ErrProductionToolSnapshot) {
		t.Fatalf("mismatched tool snapshot error = %v", err)
	}
}

func TestAssemblerRejectsInvalidToolSnapshotFromAnyProvider(t *testing.T) {
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.CapabilitySnapshots = capabilityOverflowProvider{snapshot: CapabilitySnapshot{
		Tools:        []string{"search@v1"},
		ToolSnapshot: &ToolSchemaSnapshot{SnapshotID: "snapshot", ToolRefs: []string{"other@v1"}, CapabilityHash: "sha256:cap", PolicyHash: "sha256:policy"},
	}}
	_, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: testRunRequest()})
	if !errors.Is(err, ErrProductionToolSnapshot) {
		t.Fatalf("invalid arbitrary provider snapshot error = %v", err)
	}
}

func TestAssemblerRejectsGovernedToolDefinitionDrift(t *testing.T) {
	tests := map[string][]ModelToolDefinition{
		"missing": nil,
		"extra": {
			{Name: "search", Schema: json.RawMessage(`{"type":"object"}`)},
			{Name: "evil", Schema: json.RawMessage(`{"type":"object"}`)},
		},
		"wrong name":   {{Name: "evil", Schema: json.RawMessage(`{"type":"object"}`)}},
		"empty schema": {{Name: "search"}},
	}
	for name, definitions := range tests {
		t.Run(name, func(t *testing.T) {
			assembler := NewDefaultRuntimeContextAssembler(nil)
			assembler.CapabilitySnapshots = capabilityOverflowProvider{snapshot: CapabilitySnapshot{
				Tools: []string{"search@v1"},
				ToolSnapshot: &ToolSchemaSnapshot{
					SnapshotID: "snapshot", ToolRefs: []string{"search@v1"},
					CapabilityHash: "sha256:cap", PolicyHash: "sha256:policy",
				},
				ToolDefinitions: definitions,
			}}
			_, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: testRunRequest()})
			if !errors.Is(err, ErrCapabilityToolSchemaInvalid) {
				t.Fatalf("governed definition drift error = %v", err)
			}
		})
	}
}

func TestContextAssemblyRejectsDuplicateExecutableToolNames(t *testing.T) {
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.CapabilitySnapshots = capabilityOverflowProvider{snapshot: CapabilitySnapshot{
		ToolDefinitions: []ModelToolDefinition{{Name: "search", Schema: json.RawMessage(`{"type":"object"}`)}},
		MCPSnapshots:    []mcp.CapabilitySnapshot{{ID: "mcp", ServerID: "maps", Tools: []mcp.Tool{{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`)}}}},
	}}
	_, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: testRunRequest()})
	if !errors.Is(err, ErrCapabilityToolSchemaInvalid) {
		t.Fatalf("duplicate model tool names were accepted: %v", err)
	}
}

func TestContextAssemblyRejectsMissingMCPToolSchema(t *testing.T) {
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.CapabilitySnapshots = capabilityOverflowProvider{snapshot: CapabilitySnapshot{
		MCPSnapshots: []mcp.CapabilitySnapshot{{
			ID: "mcp", ServerID: "maps", CapabilityHash: "sha256:mcp",
			Tools: []mcp.Tool{{Name: "maps_search"}},
		}},
	}}
	_, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: testRunRequest()})
	if !errors.Is(err, ErrCapabilityToolSchemaInvalid) {
		t.Fatalf("missing MCP schema was accepted: %v", err)
	}
}

func TestProductionMCPSnapshotValidationFailsClosed(t *testing.T) {
	valid := func() CapabilitySnapshot {
		return CapabilitySnapshot{
			MCPServers: []string{"maps"},
			MCPSnapshots: []mcp.CapabilitySnapshot{{
				ID: "mcp_snapshot_1", ServerID: "maps", ServerVersion: "v1",
				PrincipalHash: "sha256:principal", PolicyHash: "sha256:policy", CapabilityHash: "sha256:capability",
				Tools: []mcp.Tool{{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`)}},
			}},
		}
	}
	if err := validateGovernedMCPSnapshots(valid()); err != nil {
		t.Fatalf("valid governed MCP snapshot rejected: %v", err)
	}
	tests := map[string]func(*CapabilitySnapshot){
		"missing snapshot": func(snapshot *CapabilitySnapshot) {
			snapshot.MCPSnapshots = nil
		},
		"principal hash missing": func(snapshot *CapabilitySnapshot) {
			snapshot.MCPSnapshots[0].PrincipalHash = ""
		},
		"server mismatch": func(snapshot *CapabilitySnapshot) {
			snapshot.MCPSnapshots[0].ServerID = "other"
		},
		"duplicate tool": func(snapshot *CapabilitySnapshot) {
			snapshot.MCPSnapshots[0].Tools = append(snapshot.MCPSnapshots[0].Tools, snapshot.MCPSnapshots[0].Tools[0])
		},
		"invalid schema": func(snapshot *CapabilitySnapshot) {
			snapshot.MCPSnapshots[0].Tools[0].InputSchema = json.RawMessage(`{`)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			snapshot := valid()
			mutate(&snapshot)
			if err := validateGovernedMCPSnapshots(snapshot); !errors.Is(err, ErrProductionMCPSnapshot) {
				t.Fatalf("invalid governed MCP snapshot error=%v", err)
			}
		})
	}
}

func TestContextAssemblyRejectsRuntimeReservedMCPToolName(t *testing.T) {
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.CapabilitySnapshots = capabilityOverflowProvider{snapshot: CapabilitySnapshot{
		MCPSnapshots: []mcp.CapabilitySnapshot{{
			ID: "mcp", ServerID: "runtime-tools", CapabilityHash: "sha256:mcp",
			Tools: []mcp.Tool{{Name: "task", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		}},
	}}
	req := testRunRequest()
	req.Definition.Runtime = RuntimeSpec{Type: RuntimeTypeEino, Mode: RuntimeModeDeepAgent}
	_, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: req})
	if !errors.Is(err, ErrCapabilityToolSchemaInvalid) {
		t.Fatalf("runtime-reserved MCP tool name was accepted: %v", err)
	}
}

func TestContextAssemblyUsesResolvedRuntimeForReservedToolNames(t *testing.T) {
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.CapabilitySnapshots = capabilityOverflowProvider{snapshot: CapabilitySnapshot{
		MCPSnapshots: []mcp.CapabilitySnapshot{{
			ID: "mcp", ServerID: "runtime-tools", CapabilityHash: "sha256:mcp",
			Tools: []mcp.Tool{{Name: "task", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		}},
	}}
	req := testRunRequest()
	req.Definition.Runtime = RuntimeSpec{Type: RuntimeTypeAuto, Mode: RuntimeModeDeepAgent, Candidates: []RuntimeType{RuntimeTypeNative, RuntimeTypeEino}}
	pkg, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{
		Run: req, Handle: AgentHandle{Runtime: RuntimeTypeNative},
	})
	if err != nil {
		t.Fatalf("native resolved runtime should not reserve Eino tool names: %v", err)
	}
	if pkg.Run.Runtime != string(RuntimeTypeNative) {
		t.Fatalf("package runtime = %q, want native", pkg.Run.Runtime)
	}
}

func TestEinoBuildsMCPProxyFromFrozenSnapshot(t *testing.T) {
	runtime := &EinoRuntime{Environment: RuntimeEnvironment{Tools: deepFactoryToolInvoker{}}}
	proxies, err := runtime.platformToolProxies(context.Background(), testRunRequest(), ModelContextPackage{
		Capabilities: ModelContextCapabilities{MCPSnapshots: []mcp.CapabilitySnapshot{{
			ID: "snapshot-1", ServerID: "maps", ServerVersion: "1", CapabilityHash: "sha256:test",
			Tools: []mcp.Tool{{Name: "maps_search", Description: "search maps", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`)}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 1 {
		t.Fatalf("mcp proxy count=%d", len(proxies))
	}
	proxy, ok := proxies[0].(*EinoToolProxy)
	if !ok {
		t.Fatalf("unexpected proxy type: %T", proxies[0])
	}
	if proxy.request.Source != ToolSourceMCP || proxy.request.SourceRef != "maps" || proxy.request.SnapshotRef != "snapshot-1" {
		t.Fatalf("mcp route evidence missing: %#v", proxy.request)
	}
	info, err := proxy.Info(context.Background())
	if err != nil || info.Name != "maps_search" || info.ParamsOneOf == nil {
		t.Fatalf("mcp schema was not materialized: info=%#v err=%v", info, err)
	}
}

func TestEinoBuildsRegistryAndMCPProxiesFromFrozenPackage(t *testing.T) {
	runtime := &EinoRuntime{
		Environment: RuntimeEnvironment{Tools: deepFactoryToolInvoker{}},
		ToolInfos: EinoToolInfoResolverFunc(func(context.Context, []string) ([]EinoResolvedTool, error) {
			t.Fatal("production package must not call the legacy ToolInfo resolver")
			return nil, nil
		}),
	}
	pkg := ModelContextPackage{Capabilities: ModelContextCapabilities{
		Tools: []string{"search@v2"},
		ToolSnapshot: &ToolSchemaSnapshot{
			SnapshotID: "tool-snapshot", ToolRefs: []string{"search@v2"},
			CapabilityHash: "sha256:tool-cap", PolicyHash: "sha256:tool-policy",
		},
		ToolDefinitions: []ModelToolDefinition{
			{Name: "search", Description: "frozen search", Schema: json.RawMessage(`{"type":"object"}`)},
			{Name: "maps_search", Description: "frozen maps", Schema: json.RawMessage(`{"type":"object"}`)},
		},
		MCPSnapshots: []mcp.CapabilitySnapshot{{
			ID: "mcp-snapshot", ServerID: "maps", CapabilityHash: "sha256:mcp-cap",
			Tools: []mcp.Tool{{Name: "maps_search", Description: "frozen maps", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		}},
	}}
	proxies, err := runtime.platformToolProxies(context.Background(), testRunRequest(), pkg)
	if err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 2 {
		t.Fatalf("proxy count = %d, want 2", len(proxies))
	}
	info, err := proxies[0].Info(context.Background())
	if err != nil || info.Name != "search" || info.Desc != "frozen search" || info.ParamsOneOf == nil {
		t.Fatalf("registry proxy did not consume frozen definition: info=%#v err=%v", info, err)
	}
}

func TestContextAssemblyRejectsCapabilitySchemaOverflow(t *testing.T) {
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.TokenBudget = contextpkg.ContextBudget{Limit: 100, ResponseBuffer: 10}
	assembler.CapabilitySnapshots = capabilityOverflowProvider{snapshot: CapabilitySnapshot{
		MCPSnapshots: []mcp.CapabilitySnapshot{{
			ID: "snapshot", ServerID: "maps", CapabilityHash: "sha256:test",
			Tools: []mcp.Tool{{Name: "search", InputSchema: json.RawMessage(`{"type":"object","description":"` + strings.Repeat("large", 100) + `"}`)}},
		}},
	}}
	_, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: testRunRequest(), Trace: observability.TraceContext{TraceID: "trace"}})
	if !errors.Is(err, contextpkg.ErrBudgetExceeded) {
		t.Fatalf("capability overflow was accepted: %v", err)
	}
}

func TestGovernedCapabilitiesFailClosedForUnboundOnDemandSkill(t *testing.T) {
	service := newRuntimeSkillService(t, skill.InjectOnDemand)
	provider := GovernedCapabilitySnapshotProvider{Skills: service}
	req := testRunRequest()
	req.TenantID = "tenant"
	req.Definition.Metadata = map[string]string{"skill_refs": `["route@1.0.0"]`}
	_, err := provider.Resolve(context.Background(), RuntimeContextAssemblyRequest{Run: req})
	if !errors.Is(err, ErrSkillExecutionAdapterMissing) {
		t.Fatalf("unbound on-demand skill was accepted: %v", err)
	}

	provider.SkillBindings = skillBindingProviderFunc(func(context.Context, RunRequest, skill.Resolution) ([]string, error) {
		return []string{"skill.route@1.0.0"}, nil
	})
	resolved, err := provider.Resolve(context.Background(), RuntimeContextAssemblyRequest{Run: req})
	if err != nil || len(resolved.Tools) != 1 || resolved.Tools[0] != "skill.route@1.0.0" {
		t.Fatalf("bound on-demand skill failed: %#v %v", resolved, err)
	}
}

func TestGovernedCapabilitiesClassifyOnDemandSkillBindingFailures(t *testing.T) {
	service := newRuntimeSkillService(t, skill.InjectOnDemand)
	req := testRunRequest()
	req.TenantID = "tenant"
	req.Definition.Metadata = map[string]string{"skill_refs": `["route@1.0.0"]`}

	provider := GovernedCapabilitySnapshotProvider{
		Skills: service,
		SkillBindings: skillBindingProviderFunc(func(context.Context, RunRequest, skill.Resolution) ([]string, error) {
			return nil, errors.New("tool catalog temporarily unavailable")
		}),
	}
	_, err := provider.Resolve(context.Background(), RuntimeContextAssemblyRequest{Run: req})
	if !errors.Is(err, ErrProductionCapabilityUnavailable) {
		t.Fatalf("temporary binding failure was not retryable: %v", err)
	}

	provider.SkillBindings = skillBindingProviderFunc(func(context.Context, RunRequest, skill.Resolution) ([]string, error) {
		return nil, ErrSkillExecutionAdapterMissing
	})
	_, err = provider.Resolve(context.Background(), RuntimeContextAssemblyRequest{Run: req})
	if !errors.Is(err, ErrSkillExecutionAdapterMissing) || errors.Is(err, ErrProductionCapabilityUnavailable) {
		t.Fatalf("structural binding failure was made retryable: %v", err)
	}
}

func TestLedgerSnapshotProviderUsesOnlyDurableFacts(t *testing.T) {
	ledger := contextpkg.NewInMemoryMessageLedger()
	_, _ = ledger.Append(context.Background(), "session_1", contextpkg.Message{ID: "m1", Role: contextpkg.RoleUser, Content: "durable"})
	manager := contextpkg.SnapshotManager{
		Ledger: ledger,
		Store:  contextpkg.NewInMemorySnapshotStore(),
		IDs:    func() string { return "snapshot-1" },
	}
	frozen, err := manager.Materialize(context.Background(), "session_1", "run_1")
	if err != nil {
		t.Fatal(err)
	}
	// 冻结之后写入的消息不得穿越旧 snapshot 进入本次模型调用。
	_, _ = ledger.Append(context.Background(), "session_1", contextpkg.Message{ID: "m2", Role: contextpkg.RoleAssistant, Content: "future"})
	provider := LedgerContextSnapshotProvider{Manager: manager}
	req := testRunRequest()
	req.ContextSnapshotRef = frozen.ID
	snapshot, err := provider.Materialize(context.Background(), RuntimeContextAssemblyRequest{Run: req})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Ref != "snapshot-1" || len(snapshot.Messages) != 1 || snapshot.Messages[0].Content != "durable" {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
}

func TestLedgerSnapshotProviderCarriesFrozenSummaryIntoModelPackage(t *testing.T) {
	ledger := contextpkg.NewInMemoryMessageLedger()
	stored, _ := ledger.Append(context.Background(), "session_1", contextpkg.Message{ID: "m1", Role: contextpkg.RoleUser, Content: "current"})
	manager := contextpkg.SnapshotManager{
		Ledger: ledger,
		Hot: runtimeHotContextReader{view: contextpkg.HotContextView{
			Messages: []contextpkg.Message{stored},
			Fragments: []contextpkg.ContextFragment{{
				Slot: contextpkg.SlotSummary, Stability: contextpkg.StabilitySemi, Priority: 70,
				Role: contextpkg.RoleSystem, Content: "earlier turns summary", TokenCost: 4,
			}},
			Version: 3,
		}},
		Store: contextpkg.NewInMemorySnapshotStore(), IDs: func() string { return "snapshot-summary" },
	}
	frozen, err := manager.Materialize(context.Background(), "session_1", "run_1")
	if err != nil {
		t.Fatal(err)
	}
	provider := LedgerContextSnapshotProvider{Manager: manager}
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.ContextSnapshots = provider
	req := testRunRequest()
	req.ContextSnapshotRef = frozen.ID
	req.Input = []Message{{ID: stored.ID, IdempotencyKey: stored.IdempotencyKey, Role: "user", Content: stored.Content}}
	pkg, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: req, Trace: observability.TraceContext{TraceID: "trace"}})
	if err != nil {
		t.Fatal(err)
	}
	if !hasModelMessage(pkg.Messages.ConversationWindow, "system", "earlier turns summary") {
		t.Fatalf("frozen summary did not reach model package: %#v", pkg.Messages.ConversationWindow)
	}
	if countModelMessages(pkg.Messages.ConversationWindow, "user", "current") != 1 {
		t.Fatalf("frozen current input was duplicated: %#v", pkg.Messages.ConversationWindow)
	}
}

func TestLedgerSnapshotProviderRejectsMissingOrMismatchedIdentity(t *testing.T) {
	ledger := contextpkg.NewInMemoryMessageLedger()
	_, _ = ledger.Append(context.Background(), "session_1", contextpkg.Message{ID: "m1", Role: contextpkg.RoleUser, Content: "current"})
	manager := contextpkg.SnapshotManager{
		Ledger: ledger, Store: contextpkg.NewInMemorySnapshotStore(), IDs: func() string { return "snapshot-identity" },
	}
	frozen, err := manager.Materialize(context.Background(), "session_1", "run_1")
	if err != nil {
		t.Fatal(err)
	}
	provider := LedgerContextSnapshotProvider{Manager: manager}

	missingRef := testRunRequest()
	wrongSession := testRunRequest()
	wrongSession.SessionID, wrongSession.ContextSnapshotRef = "other", frozen.ID
	wrongRun := testRunRequest()
	wrongRun.RunID, wrongRun.ContextSnapshotRef = "other", frozen.ID
	for _, tc := range []struct {
		name string
		req  RunRequest
	}{
		{name: "missing ref", req: missingRef},
		{name: "wrong session", req: wrongSession},
		{name: "wrong run", req: wrongRun},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := provider.Materialize(context.Background(), RuntimeContextAssemblyRequest{Run: tc.req})
			if !errors.Is(err, ErrProductionContextSnapshot) {
				t.Fatalf("snapshot identity error=%v", err)
			}
		})
	}
}

func TestLedgerSnapshotProviderRejectsTamperedHash(t *testing.T) {
	ledger := contextpkg.NewInMemoryMessageLedger()
	_, _ = ledger.Append(context.Background(), "session_1", contextpkg.Message{ID: "m1", Role: contextpkg.RoleUser, Content: "current"})
	manager := contextpkg.SnapshotManager{
		Ledger: ledger, Store: contextpkg.NewInMemorySnapshotStore(), IDs: func() string { return "snapshot-hash" },
	}
	frozen, err := manager.Materialize(context.Background(), "session_1", "run_1")
	if err != nil {
		t.Fatal(err)
	}
	frozen.ContentHash = "sha256:tampered"
	provider := LedgerContextSnapshotProvider{Manager: contextpkg.SnapshotManager{
		Ledger: ledger, Store: fixedRuntimeSnapshotStore{snapshot: frozen},
	}}
	req := testRunRequest()
	req.ContextSnapshotRef = frozen.ID
	_, err = provider.Materialize(context.Background(), RuntimeContextAssemblyRequest{Run: req})
	if !errors.Is(err, ErrProductionContextSnapshot) {
		t.Fatalf("tampered snapshot hash error=%v", err)
	}
}

type runtimeHotContextReader struct{ view contextpkg.HotContextView }

type fixedRuntimeSnapshotStore struct{ snapshot contextpkg.Snapshot }

func (s fixedRuntimeSnapshotStore) Save(_ context.Context, snapshot contextpkg.Snapshot) (string, error) {
	return snapshot.ID, nil
}

func (s fixedRuntimeSnapshotStore) Load(context.Context, string) (contextpkg.Snapshot, error) {
	return s.snapshot, nil
}

func countModelMessages(messages []ModelContextMessage, role, content string) int {
	count := 0
	for _, message := range messages {
		if message.Role == role && message.Content == content {
			count++
		}
	}
	return count
}

func (r runtimeHotContextReader) Load(context.Context, string, int) (contextpkg.HotContextView, error) {
	return r.view, nil
}

type capabilityOverflowProvider struct{ snapshot CapabilitySnapshot }

func (p capabilityOverflowProvider) Resolve(context.Context, RuntimeContextAssemblyRequest) (CapabilitySnapshot, error) {
	return p.snapshot, nil
}

type governedToolSnapshotProviderFunc func(context.Context, RunRequest, []string) (GovernedToolDefinitionSnapshot, error)

func (f governedToolSnapshotProviderFunc) ResolveGovernedToolDefinitions(ctx context.Context, run RunRequest, refs []string) (GovernedToolDefinitionSnapshot, error) {
	return f(ctx, run, refs)
}

func (f governedToolSnapshotProviderFunc) ResolveToolDefinitions(ctx context.Context, run RunRequest, refs []string) ([]ModelToolDefinition, error) {
	resolved, err := f(ctx, run, refs)
	return resolved.Definitions, err
}

type skillBindingProviderFunc func(context.Context, RunRequest, skill.Resolution) ([]string, error)

func (f skillBindingProviderFunc) Bind(ctx context.Context, run RunRequest, resolution skill.Resolution) ([]string, error) {
	return f(ctx, run, resolution)
}

func newRuntimeSkillService(t *testing.T, strategy skill.InjectionStrategy) *skill.Service {
	t.Helper()
	service, err := skill.NewService(skill.NewInMemoryRepository(), nil, func() string { return "skill-snapshot" })
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = service.Publish(context.Background(), skill.Principal{TenantID: "tenant", AgentID: "publisher"}, skill.Definition{
		ID: "route", Version: "1.0.0", TenantID: "tenant", InjectionStrategy: strategy,
		Policy: skill.Policy{Scope: skill.ScopeTenant, AllowedAgents: []string{"agent_1"}},
	}, []byte("route skill"))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

type runtimeMCPClient struct{}

func (runtimeMCPClient) ListTools(context.Context) ([]mcp.Tool, error) {
	return []mcp.Tool{{Name: "maps.search", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil
}
func (runtimeMCPClient) CallTool(context.Context, string, json.RawMessage, mcp.CallOptions) (mcp.ToolResult, error) {
	return mcp.ToolResult{Content: []byte("ok")}, nil
}
