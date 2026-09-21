package agentregistry

import (
	"context"
	"strings"
)

type CatalogDependencyResolver struct {
	Tools      map[string]bool
	Models     map[string]bool
	Prompts    map[string]bool
	Schemas    map[string]bool
	Skills     map[string]bool
	MCPServers map[string]bool
	HTTPTools  map[string]bool
	Guardrails map[string]bool
	Agents     map[string]bool
	Protocols  map[string]bool
}

func (r CatalogDependencyResolver) Resolve(_ context.Context, input AgentConfig) (ResolvedDependencies, error) {
	cfg := normalizeConfig(input)
	var deps ResolvedDependencies
	check := func(catalog map[string]bool, field, value string) error {
		if value == "" || catalog == nil {
			return nil
		}
		if !catalog[value] {
			return newError(CodeDependencyMissing, field, "dependency not found: "+value)
		}
		return nil
	}
	add := func(target *[]string, values ...string) {
		for _, value := range values {
			if value != "" {
				*target = append(*target, value)
			}
		}
	}

	if err := check(r.Models, "model.primary", cfg.Model.Primary); err != nil {
		return deps, err
	}
	add(&deps.Models, cfg.Model.Primary)
	for _, model := range cfg.Model.Fallback {
		if err := check(r.Models, "model.fallback", model); err != nil {
			return deps, err
		}
		add(&deps.Models, model)
	}
	for _, tool := range cfg.Tools {
		ref := versionedDependency(tool.Name, tool.Version)
		if err := check(r.Tools, "tools", ref); err != nil {
			return deps, err
		}
		add(&deps.Tools, ref)
	}
	for _, prompt := range []string{versionedDependency(cfg.PromptRef, cfg.PromptVersion), cfg.Context.PromptPack} {
		if err := check(r.Prompts, "prompt_ref", prompt); err != nil {
			return deps, err
		}
		add(&deps.Prompts, prompt)
	}
	graphStateSchema := ""
	if cfg.Orchestration.Graph != nil {
		graphStateSchema = cfg.Orchestration.Graph.StateSchemaRef
	}
	for _, schema := range []string{cfg.Capability.InputSchema, cfg.Capability.OutputSchema, graphStateSchema} {
		if err := check(r.Schemas, "capability.schema", schema); err != nil {
			return deps, err
		}
		add(&deps.Schemas, schema)
	}
	for _, skill := range cfg.Skills.Allowlist {
		if err := check(r.Skills, "skills", skill); err != nil {
			return deps, err
		}
		add(&deps.Skills, skill)
	}
	for _, serverID := range cfg.ToolPolicy.MCPServers {
		if err := check(r.MCPServers, "tool_policy.mcp_servers", serverID); err != nil {
			return deps, err
		}
		add(&deps.MCPServers, serverID)
	}
	for _, toolName := range cfg.ToolPolicy.HTTPTools {
		if err := check(r.HTTPTools, "tool_policy.http_tools", toolName); err != nil {
			return deps, err
		}
		add(&deps.HTTPTools, toolName)
	}
	for _, guardrail := range []string{cfg.GuardrailPolicyRef, cfg.Guardrails.Input, cfg.Guardrails.Tool, cfg.Guardrails.Output, cfg.Guardrails.Behavior} {
		if err := check(r.Guardrails, "guardrails", guardrail); err != nil {
			return deps, err
		}
		add(&deps.Guardrails, guardrail)
	}
	for _, agent := range append(append([]string(nil), cfg.SubAgents...), cfg.FallbackPolicy.FallbackAgents...) {
		if err := check(r.Agents, "sub_agents", agent); err != nil {
			return deps, err
		}
		add(&deps.Agents, agent)
	}
	for _, protocol := range append(append([]string(nil), cfg.Capability.Protocols...), cfg.Output.Formats...) {
		protocol = strings.ToLower(protocol)
		if err := check(r.Protocols, "protocols", protocol); err != nil {
			return deps, err
		}
		add(&deps.Protocols, protocol)
	}
	return normalizeDeps(deps), nil
}

type CompositeDependencyResolver []DependencyResolver

func (r CompositeDependencyResolver) Resolve(ctx context.Context, cfg AgentConfig) (ResolvedDependencies, error) {
	var out ResolvedDependencies
	for _, resolver := range r {
		if resolver == nil {
			continue
		}
		deps, err := resolver.Resolve(ctx, cfg)
		if err != nil {
			return out, err
		}
		out.Models = append(out.Models, deps.Models...)
		out.Tools = append(out.Tools, deps.Tools...)
		out.AuthoringMaxToolRisk = maxToolRisk(out.AuthoringMaxToolRisk, deps.AuthoringMaxToolRisk)
		out.AuthoringHighRiskTools = append(out.AuthoringHighRiskTools, deps.AuthoringHighRiskTools...)
		out.MCPServers = append(out.MCPServers, deps.MCPServers...)
		out.HTTPTools = append(out.HTTPTools, deps.HTTPTools...)
		out.Prompts = append(out.Prompts, deps.Prompts...)
		out.Schemas = append(out.Schemas, deps.Schemas...)
		out.Skills = append(out.Skills, deps.Skills...)
		out.Guardrails = append(out.Guardrails, deps.Guardrails...)
		out.Agents = append(out.Agents, deps.Agents...)
		out.Protocols = append(out.Protocols, deps.Protocols...)
	}
	return normalizeDeps(out), nil
}

func versionedDependency(name, version string) string {
	if name == "" || version == "" {
		return name
	}
	return name + "@" + version
}
