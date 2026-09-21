package agentregistry

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

func TestValidateAgentConfigRequiresExactToolVersions(t *testing.T) {
	tests := map[string]VersionedRef{
		"missing name":    {Version: "v1"},
		"missing version": {Name: "search"},
		"embedded at":     {Name: "search@latest", Version: "v1"},
		"whitespace":      {Name: " search", Version: "v1"},
	}
	for name, ref := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Tools = []VersionedRef{ref}
			assertRegistryError(t, ValidateAgentConfig(cfg), CodeInvalidConfig, "tools")
		})
	}

	cfg := validConfig()
	cfg.Tools = append(cfg.Tools, cfg.Tools[0])
	assertRegistryError(t, ValidateAgentConfig(cfg), CodeInvalidConfig, "tools")

	cfg = validConfig()
	cfg.Tools = append(cfg.Tools, VersionedRef{Name: "search", Version: "v2"})
	assertRegistryError(t, ValidateAgentConfig(cfg), CodeInvalidConfig, "tools")

	cfg.Tools = []VersionedRef{{Name: "search"}}
	_, err := (ToolCatalogResolver{Registry: newGetOnlyToolRegistry(catalogTool("search", "v1", toolgateway.RiskLow))}).Resolve(context.Background(), cfg)
	assertRegistryError(t, err, CodeInvalidConfig, "tools")
}

func TestValidateAgentConfigRequiresCanonicalToolRisk(t *testing.T) {
	for _, risk := range []string{"LOW", "R1_READ", "write_high"} {
		t.Run(risk, func(t *testing.T) {
			cfg := validConfig()
			cfg.ToolPolicy.RiskLevel = risk
			assertRegistryError(t, ValidateAgentConfig(cfg), CodeInvalidConfig, "tool_policy.risk_level")
		})
	}
	for _, risk := range []string{"", "low", "medium", "high"} {
		t.Run("valid_"+risk, func(t *testing.T) {
			cfg := validConfig()
			cfg.ToolPolicy.RiskLevel = risk
			if err := ValidateAgentConfig(cfg); err != nil {
				t.Fatalf("ValidateAgentConfig() error = %v", err)
			}
		})
	}
}

func TestValidateAgentConfigRejectsEinoRuntimeToolName(t *testing.T) {
	for name, runtime := range map[string]agentruntime.RuntimeSpec{
		"explicit":  {Type: agentruntime.RuntimeTypeEino, Mode: agentruntime.RuntimeModeDeepAgent},
		"auto":      {Type: agentruntime.RuntimeTypeAuto, Mode: agentruntime.RuntimeModeDeepAgent},
		"preferred": {Type: agentruntime.RuntimeTypeNative, Mode: agentruntime.RuntimeModeDeepAgent, Preferred: agentruntime.RuntimeTypeEino},
		"candidate": {Type: agentruntime.RuntimeTypeNative, Mode: agentruntime.RuntimeModeDeepAgent, Candidates: []agentruntime.RuntimeType{agentruntime.RuntimeTypeEino}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Runtime = runtime
			cfg.Tools = []VersionedRef{{Name: "task", Version: "v1"}}
			assertRegistryError(t, ValidateAgentConfig(cfg), CodeInvalidConfig, "tools")
		})
	}

	cfg := validConfig()
	cfg.Tools = []VersionedRef{{Name: "task", Version: "v1"}}
	cfg.Runtime = agentruntime.RuntimeSpec{Type: agentruntime.RuntimeTypeNative, Mode: agentruntime.RuntimeModeDirect}
	if err := ValidateAgentConfig(cfg); err != nil {
		t.Fatalf("native Runtime should not reserve Eino names: %v", err)
	}
}

func TestValidateAgentConfigRequiresExactDeclaredHITLTools(t *testing.T) {
	for _, value := range []string{"search", " search@v1", "search@v1@latest", "missing@v1"} {
		t.Run(value, func(t *testing.T) {
			cfg := validConfig()
			cfg.ToolPolicy.HITLRequiredTools = []string{value}
			assertRegistryError(t, ValidateAgentConfig(cfg), CodeInvalidConfig, "tool_policy.hitl_required_tools")
		})
	}
	cfg := validConfig()
	cfg.ToolPolicy.HITLRequiredTools = []string{"search@v1", "search@v1"}
	assertRegistryError(t, ValidateAgentConfig(cfg), CodeInvalidConfig, "tool_policy.hitl_required_tools")
}

func TestToolCatalogResolverUsesGetAndFreezesAuthoringRisk(t *testing.T) {
	registry := newGetOnlyToolRegistry(
		catalogTool("search", "v1", toolgateway.RiskLow),
		catalogTool("charge", "v2", toolgateway.RiskMedium),
	)
	cfg := validConfig()
	cfg.Tools = []VersionedRef{{Name: "charge", Version: "v2"}, {Name: "search", Version: "v1"}}

	deps, err := (ToolCatalogResolver{Registry: registry}).Resolve(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got, want := deps.Tools, []string{"charge@v2", "search@v1"}; !equalStrings(got, want) {
		t.Fatalf("tools = %#v, want %#v", got, want)
	}
	if deps.AuthoringMaxToolRisk != "medium" {
		t.Fatalf("authoring max risk = %q, want medium", deps.AuthoringMaxToolRisk)
	}
	if registry.getCalls != 2 {
		t.Fatalf("Get() calls = %d, want 2", registry.getCalls)
	}
}

func TestToolCatalogResolverFailsClosedForUnavailableOrInvalidDefinition(t *testing.T) {
	tests := map[string]struct {
		definition toolgateway.ToolDefinition
		ref        VersionedRef
	}{
		"missing version": {
			definition: catalogTool("search", "v1", toolgateway.RiskLow),
			ref:        VersionedRef{Name: "search", Version: "v2"},
		},
		"disabled": {
			definition: func() toolgateway.ToolDefinition {
				def := catalogTool("search", "v1", toolgateway.RiskLow)
				def.Disabled = true
				return def
			}(),
			ref: VersionedRef{Name: "search", Version: "v1"},
		},
		"invalid risk": {
			definition: catalogTool("search", "v1", toolgateway.RiskLevel("R1_READ")),
			ref:        VersionedRef{Name: "search", Version: "v1"},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Tools = []VersionedRef{test.ref}
			_, err := (ToolCatalogResolver{Registry: newGetOnlyToolRegistry(test.definition)}).Resolve(context.Background(), cfg)
			assertRegistryError(t, err, CodeDependencyMissing, "tools")
		})
	}
}

func TestToolCatalogResolverRejectsDisabledDefinitionFromRawRegistry(t *testing.T) {
	definition := catalogTool("search", "v1", toolgateway.RiskLow)
	definition.Disabled = true
	registry := &rawToolRegistry{definition: definition}
	cfg := validConfig()

	_, err := (ToolCatalogResolver{Registry: registry}).Resolve(context.Background(), cfg)
	assertRegistryError(t, err, CodeDependencyMissing, "tools")
	if registry.getCalls != 1 {
		t.Fatalf("Get() calls = %d, want 1", registry.getCalls)
	}
}

func TestToolCatalogResolverRejectsAgentOutsideToolAllowlist(t *testing.T) {
	definition := catalogTool("search", "v1", toolgateway.RiskLow)
	definition.Permissions.AllowedAgents = []string{"another_agent"}
	cfg := validConfig()

	_, err := (ToolCatalogResolver{Registry: newGetOnlyToolRegistry(definition)}).Resolve(context.Background(), cfg)
	assertRegistryError(t, err, CodePolicyViolation, "tools")
}

func TestToolCatalogResolverAuthorizesAnyAgentWithWildcard(t *testing.T) {
	definition := catalogTool("search", "v1", toolgateway.RiskLow)
	definition.Permissions.AllowedAgents = []string{"*"}
	cfg := validConfig()

	deps, err := (ToolCatalogResolver{Registry: newGetOnlyToolRegistry(definition)}).Resolve(context.Background(), cfg)
	if err != nil {
		t.Fatalf("wildcard allowed_agents must authorize any agent: %v", err)
	}
	if len(deps.Tools) != 1 || deps.Tools[0] != "search@v1" {
		t.Fatalf("unexpected resolved tools: %#v", deps.Tools)
	}
}

func TestToolCatalogRiskCannotBeDowngradedAndHighRiskRequiresHITL(t *testing.T) {
	registry := newGetOnlyToolRegistry(catalogTool("search", "v1", toolgateway.RiskHigh))
	service := NewService(
		WithLegacyUnresolvedPrompts(),
		WithDependencyResolver(ToolCatalogResolver{Registry: registry}),
	)

	downgraded := validConfig()
	downgraded.ToolPolicy.RiskLevel = "low"
	downgraded.ToolPolicy.HITLRequiredTools = []string{"search@v1"}
	_, err := service.ValidateAgent(context.Background(), downgraded)
	assertRegistryError(t, err, CodePolicyViolation, "tool_policy.risk_level")

	missingHITL := validConfig()
	_, err = service.ValidateAgent(context.Background(), missingHITL)
	assertRegistryError(t, err, CodePolicyViolation, "tool_policy.hitl_required_tools")

	partialRegistry := newGetOnlyToolRegistry(
		catalogTool("search", "v1", toolgateway.RiskLow),
		catalogTool("payment", "v1", toolgateway.RiskHigh),
	)
	partialService := NewService(
		WithLegacyUnresolvedPrompts(),
		WithDependencyResolver(ToolCatalogResolver{Registry: partialRegistry}),
	)
	partial := validConfig()
	partial.Tools = []VersionedRef{{Name: "search", Version: "v1"}, {Name: "payment", Version: "v1"}}
	partial.ToolPolicy.HITLRequiredTools = []string{"search@v1"}
	_, err = partialService.ValidateAgent(context.Background(), partial)
	assertRegistryError(t, err, CodePolicyViolation, "tool_policy.hitl_required_tools")
}

func TestToolCatalogRiskIsProjectedIntoDefinitionCardAndConfigHash(t *testing.T) {
	registry := newGetOnlyToolRegistry(catalogTool("search", "v1", toolgateway.RiskMedium))
	service := NewService(
		WithLegacyUnresolvedPrompts(),
		WithDependencyResolver(CompositeDependencyResolver{
			CatalogDependencyResolver{},
			ToolCatalogResolver{Registry: registry},
		}),
	)
	cfg := validConfig()

	registered, err := service.RegisterAgent(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RegisterAgent() error = %v", err)
	}
	effective, err := service.ResolveEffectiveConfig(context.Background(), ResolveRequest{AgentID: cfg.AgentID, Version: cfg.Version})
	if err != nil {
		t.Fatalf("ResolveEffectiveConfig() error = %v", err)
	}
	if effective.Definition.Metadata["tool_risk_level"] != "medium" || effective.ResolvedDeps.AuthoringMaxToolRisk != "medium" {
		t.Fatalf("trusted tool risk was not frozen: %#v", effective)
	}
	if got := effective.ResolvedDeps.Tools; len(got) != 1 || got[0] != "search@v1" {
		t.Fatalf("composite resolver duplicated tool refs: %#v", got)
	}
	if registered.CapabilityCard.RiskLevel != "medium" || registered.ConfigHash == "" {
		t.Fatalf("capability card/config hash did not use trusted risk: %#v", registered)
	}
}

func TestRiskyDirectActionRequiresCheckpointAndResumeCapabilities(t *testing.T) {
	service := NewService(
		WithLegacyUnresolvedPrompts(),
		WithDependencyResolver(ToolCatalogResolver{Registry: newGetOnlyToolRegistry(
			catalogTool("search", "v1", toolgateway.RiskHigh),
		)}),
	)
	cfg := directAgentConfig("agent_1", "v1")
	cfg.ToolPolicy.RiskLevel = "high"
	cfg.ToolPolicy.HITLRequiredTools = []string{"search@v1"}
	if _, err := service.RegisterAgent(context.Background(), cfg); err != nil {
		t.Fatalf("register risky direct Agent: %v", err)
	}
	effective, err := service.ResolveEffectiveConfig(context.Background(), ResolveRequest{
		AgentID: cfg.AgentID, Version: cfg.Version, ExecutionMode: ExecutionModeDirectAction,
	})
	if err != nil {
		t.Fatalf("resolve risky direct Agent: %v", err)
	}
	for _, required := range []string{"checkpoint", "control_request", "resume"} {
		if !containsString(effective.Definition.RequiredCapabilities, required) {
			t.Fatalf("risky direct Agent missing %q: %#v", required, effective.Definition.RequiredCapabilities)
		}
	}
}

func catalogTool(name, version string, risk toolgateway.RiskLevel) toolgateway.ToolDefinition {
	return toolgateway.ToolDefinition{
		Name: name, Version: version, Type: toolgateway.ToolTypeFunction,
		InputSchema: json.RawMessage(`{"type":"object"}`),
		RiskLevel:   risk, Visibility: observability.VisibilityInternal,
		Permissions: toolgateway.ToolPermissions{AllowedAgents: []string{"agent_1"}},
		Function:    &toolgateway.FunctionToolSpec{HandlerName: name},
	}
}

type getOnlyToolRegistry struct {
	registry *toolgateway.StaticRegistry
	getCalls int
}

// rawToolRegistry 模拟不做 Disabled 过滤的生产目录实现，确保发布校验不依赖具体后端。
type rawToolRegistry struct {
	definition toolgateway.ToolDefinition
	getCalls   int
}

func (r *rawToolRegistry) Get(context.Context, string, string) (*toolgateway.ToolDefinition, error) {
	r.getCalls++
	definition := r.definition
	return &definition, nil
}

func (*rawToolRegistry) ResolveSnapshot(context.Context, toolgateway.ResolveToolSnapshotRequest) (*toolgateway.ToolSnapshot, error) {
	panic("raw registry must not resolve snapshots during authoring validation")
}

func newGetOnlyToolRegistry(definitions ...toolgateway.ToolDefinition) *getOnlyToolRegistry {
	return &getOnlyToolRegistry{registry: toolgateway.NewStaticRegistry(definitions)}
}

func (r *getOnlyToolRegistry) Get(ctx context.Context, name, version string) (*toolgateway.ToolDefinition, error) {
	r.getCalls++
	return r.registry.Get(ctx, name, version)
}

func (*getOnlyToolRegistry) ResolveSnapshot(context.Context, toolgateway.ResolveToolSnapshotRequest) (*toolgateway.ToolSnapshot, error) {
	panic("Agent Registry must not resolve tenant-scoped tool snapshots")
}

func assertRegistryError(t *testing.T, err error, code ErrorCode, field string) {
	t.Helper()
	var registryErr *RegistryError
	if !errors.As(err, &registryErr) || registryErr.Code != code || registryErr.Field != field {
		t.Fatalf("error = %v, want code=%s field=%s", err, code, field)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
