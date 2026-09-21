package agentregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/skill"
)

func TestAgentDefinitionEncodesFoundationCapabilityMetadata(t *testing.T) {
	cfg := extendedConfig("capability_agent", "v1")
	cfg.Metadata["skill_refs"] = `["forged@9.9.9"]`
	cfg.Metadata["mcp_servers"] = `["forged"]`
	cfg.Skills.Allowlist = []string{"route@1.0.0", "summary@2.1.0"}
	cfg.ToolPolicy.MCPServers = []string{"maps", "search"}

	if err := ValidateAgentConfig(cfg); err != nil {
		t.Fatalf("validate capability refs: %v", err)
	}
	definition := cfg.ToAgentDefinition()
	assertMetadataList(t, definition.Metadata["skill_refs"], cfg.Skills.Allowlist)
	assertMetadataList(t, definition.Metadata["mcp_servers"], cfg.ToolPolicy.MCPServers)

	// 平台保留字段只能由结构化配置生成，空配置不能通过 metadata 注入能力。
	cfg.Skills.Allowlist = nil
	cfg.ToolPolicy.MCPServers = nil
	definition = cfg.ToAgentDefinition()
	if _, ok := definition.Metadata["skill_refs"]; ok {
		t.Fatal("reserved skill_refs metadata bypassed registry config")
	}
	if _, ok := definition.Metadata["mcp_servers"]; ok {
		t.Fatal("reserved mcp_servers metadata bypassed registry config")
	}
}

func TestFoundationCapabilityRefsFailClosed(t *testing.T) {
	tests := []struct {
		name  string
		field string
		apply func(*AgentConfig)
	}{
		{name: "unversioned skill", field: "skills.allowlist", apply: func(cfg *AgentConfig) {
			cfg.Skills.Allowlist = []string{"route"}
		}},
		{name: "invalid skill semver", field: "skills.allowlist", apply: func(cfg *AgentConfig) {
			cfg.Skills.Allowlist = []string{"route@v1"}
		}},
		{name: "skill version range", field: "skills.version_range", apply: func(cfg *AgentConfig) {
			cfg.Skills.Allowlist = []string{"route@1.0.0"}
			cfg.Skills.VersionRange = ">=1.0.0 <2.0.0"
		}},
		{name: "mcp uri", field: "tool_policy.mcp_servers", apply: func(cfg *AgentConfig) {
			cfg.ToolPolicy.MCPServers = []string{"mcp://maps/v1"}
		}},
		{name: "blank mcp server", field: "tool_policy.mcp_servers", apply: func(cfg *AgentConfig) {
			cfg.ToolPolicy.MCPServers = []string{" "}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := extendedConfig("invalid_capability_agent", "v1")
			tt.apply(&cfg)
			err := ValidateAgentConfig(cfg)
			var registryErr *RegistryError
			if !errors.As(err, &registryErr) || registryErr.Code != CodeInvalidConfig || registryErr.Field != tt.field {
				t.Fatalf("invalid capability ref was accepted: %v", err)
			}
		})
	}
}

func TestCatalogDependencyResolverTracksFoundationCapabilities(t *testing.T) {
	cfg := extendedConfig("dependency_capability_agent", "v1")
	cfg.Skills.Allowlist = []string{"route@1.0.0"}
	cfg.ToolPolicy.MCPServers = []string{"maps"}
	resolver := CatalogDependencyResolver{
		Skills:     map[string]bool{"route@1.0.0": true},
		MCPServers: map[string]bool{"maps": true},
	}

	deps, err := resolver.Resolve(context.Background(), cfg)
	if err != nil {
		t.Fatalf("resolve capability dependencies: %v", err)
	}
	if !reflect.DeepEqual(deps.Skills, []string{"route@1.0.0"}) || !reflect.DeepEqual(deps.MCPServers, []string{"maps"}) {
		t.Fatalf("foundation dependencies were not frozen: %#v", deps)
	}

	resolver.MCPServers = map[string]bool{"other": true}
	_, err = resolver.Resolve(context.Background(), cfg)
	var registryErr *RegistryError
	if !errors.As(err, &registryErr) || registryErr.Code != CodeDependencyMissing || registryErr.Field != "tool_policy.mcp_servers" {
		t.Fatalf("missing MCP server must fail closed: %v", err)
	}

	resolver.MCPServers = map[string]bool{"maps": true}
	resolver.Skills = map[string]bool{"other@1.0.0": true}
	_, err = resolver.Resolve(context.Background(), cfg)
	registryErr = nil
	if !errors.As(err, &registryErr) || registryErr.Code != CodeDependencyMissing || registryErr.Field != "skills" {
		t.Fatalf("missing skill must fail closed: %v", err)
	}
}

func TestRegistryCapabilitiesResolveThroughFoundation(t *testing.T) {
	ctx := context.Background()
	cfg := extendedConfig("agent_1", "v1")
	cfg.Skills.Allowlist = []string{"route@1.0.0"}
	cfg.ToolPolicy.MCPServers = []string{"maps"}

	registry := newLegacyTestService()
	if _, err := registry.RegisterAgent(ctx, cfg); err != nil {
		t.Fatalf("register agent: %v", err)
	}
	effective, err := registry.ResolveEffectiveConfig(ctx, ResolveRequest{AgentID: cfg.AgentID, Version: cfg.Version})
	if err != nil {
		t.Fatalf("resolve effective config: %v", err)
	}

	skillService, err := skill.NewService(skill.NewInMemoryRepository(), nil, func() string { return "skill-snapshot" })
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = skillService.Publish(ctx, skill.Principal{TenantID: "tenant", AgentID: "publisher"}, skill.Definition{
		ID: "route", Version: "1.0.0", TenantID: "tenant", InjectionStrategy: skill.InjectSystem,
		Policy: skill.Policy{Scope: skill.ScopeTenant, AllowedAgents: []string{cfg.AgentID}},
	}, []byte("route safely"))
	if err != nil {
		t.Fatalf("publish skill: %v", err)
	}
	mcpRegistry, err := mcp.NewInMemoryRegistry(mcp.ServerDefinition{
		ID: "maps", Version: "1", Scope: mcp.ScopeTenant, TenantID: "tenant", AllowedAgents: []string{cfg.AgentID},
	})
	if err != nil {
		t.Fatal(err)
	}
	sequence := 0
	mcpService, err := mcp.NewService(
		mcpRegistry,
		mcp.StaticClientProvider{Value: registryMCPClient{}},
		mcp.NewInMemorySnapshotStore(),
		func() string {
			sequence++
			return fmt.Sprintf("mcp-snapshot-%d", sequence)
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	resolved, err := (agentruntime.GovernedCapabilitySnapshotProvider{MCP: mcpService, Skills: skillService}).Resolve(
		ctx,
		agentruntime.RuntimeContextAssemblyRequest{Run: agentruntime.RunRequest{
			SessionID: "session", RunID: "run", TenantID: "tenant", UserID: "user", Definition: effective.Definition,
		}},
	)
	if err != nil {
		t.Fatalf("foundation resolve capabilities: %v", err)
	}
	if len(resolved.SkillResolutions) != 1 || resolved.SkillResolutions[0].Root.SkillID != "route" {
		t.Fatalf("skill config did not reach foundation: %#v", resolved.SkillResolutions)
	}
	if len(resolved.ContextFragments) != 1 || resolved.ContextFragments[0].Content != "route safely" {
		t.Fatalf("skill instructions did not reach governed context: %#v", resolved.ContextFragments)
	}
	if len(resolved.MCPSnapshots) != 1 || resolved.MCPSnapshots[0].ServerID != "maps" {
		t.Fatalf("MCP config did not reach foundation: %#v", resolved.MCPSnapshots)
	}
}

func assertMetadataList(t *testing.T, encoded string, want []string) {
	t.Helper()
	var got []string
	if err := json.Unmarshal([]byte(encoded), &got); err != nil {
		t.Fatalf("metadata is not a JSON string array: %q: %v", encoded, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata list = %#v, want %#v", got, want)
	}
}

type registryMCPClient struct{}

func (registryMCPClient) ListTools(context.Context) ([]mcp.Tool, error) {
	return []mcp.Tool{{Name: "maps.search", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil
}

func (registryMCPClient) CallTool(context.Context, string, json.RawMessage, mcp.CallOptions) (mcp.ToolResult, error) {
	return mcp.ToolResult{Content: []byte("ok")}, nil
}
