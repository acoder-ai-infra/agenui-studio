package agentregistry

import (
	"sort"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/executionmode"
	"github.com/AGenUI/agenui-studio/harness/internal/gatewaycontract"
)

func normalizeConfig(cfg AgentConfig) AgentConfig {
	if cfg.ContextCompaction == (agentruntime.ContextCompactionPolicy{}) && cfg.Context.Compression == "summary_v1" {
		cfg.ContextCompaction = agentruntime.DefaultContextCompactionPolicy()
		cfg.Context.Compression = ""
	}
	if policy, err := agentruntime.NormalizeContextCompactionPolicy(cfg.ContextCompaction); err == nil {
		cfg.ContextCompaction = policy
	}
	cfg.Metadata = cloneStringMap(cfg.Metadata)
	for _, key := range []string{
		"system_prompt", "prompt_version", "prompt_hash", "prompt_snapshot_ref", "prompt_content_hash", "prompt_content_ref",
		"skill_refs", "mcp_servers", "description", "max_iterations", "max_turns",
	} {
		delete(cfg.Metadata, key)
	}
	cfg.Runtime.Candidates = append([]agentruntime.RuntimeType(nil), cfg.Runtime.Candidates...)
	cfg.DataPassing = cloneDataPassingPolicy(cfg.DataPassing)
	cfg.Tools = append([]VersionedRef(nil), cfg.Tools...)
	// 扩展绑定列表只拷贝不排序：声明顺序就是执行顺序，属于语义。
	cfg.Extensions.BeforeModelHooks = append([]string(nil), cfg.Extensions.BeforeModelHooks...)
	cfg.Extensions.ToolCallInterceptors = append([]string(nil), cfg.Extensions.ToolCallInterceptors...)
	cfg.Extensions.OutputValidators = append([]string(nil), cfg.Extensions.OutputValidators...)
	cfg.Capability.ExecutionModes = append([]ExecutionMode(nil), cfg.Capability.ExecutionModes...)
	cfg.Orchestration.Workflow = cloneWorkflowDefinition(cfg.Orchestration.Workflow)
	cfg.Orchestration.Graph = cloneGraphDefinition(cfg.Orchestration.Graph)
	cfg.Gateway = cloneGatewayTargetConfig(cfg.Gateway)
	if cfg.Status == "" {
		cfg.Status = AgentStatusEnabled
	}
	// 兼容旧 YAML 作者态字段；归一化后只向下游暴露 foundation 字段。
	if cfg.Runtime.Type == "" && cfg.RuntimePolicy.Provider != "" {
		cfg.Runtime.Type = agentruntime.RuntimeType(cfg.RuntimePolicy.Provider)
	}
	if cfg.PromptRef == "" {
		cfg.PromptRef = promptRef(cfg.Context.SystemPrompt)
	}
	// context.system_prompt 只兼容旧作者态引用别名；归一化后不再持久化，避免与正文混淆。
	cfg.Context.SystemPrompt = ""
	if len(cfg.Tools) == 0 {
		for _, name := range cfg.ToolPolicy.Allowlist {
			cfg.Tools = append(cfg.Tools, VersionedRef{Name: name})
		}
	}
	if len(cfg.SubAgents) == 0 {
		cfg.SubAgents = append([]string(nil), cfg.Orchestration.SubAgents...)
	}
	if cfg.ContextPolicyRef == "" {
		cfg.ContextPolicyRef = policyRef("context_policy", cfg.Context.PromptPack)
	}
	if cfg.GuardrailPolicyRef == "" {
		cfg.GuardrailPolicyRef = policyRef("guardrail", firstNonEmpty(cfg.Guardrails.Output, cfg.Guardrails.Input, cfg.Guardrails.Tool, cfg.Guardrails.Behavior))
	}
	if cfg.ProtocolPolicy.OutputFormat == "" {
		cfg.ProtocolPolicy.OutputFormat = firstString(cfg.Output.Formats)
	}
	if !cfg.ProtocolPolicy.Streaming {
		cfg.ProtocolPolicy.Streaming = cfg.RuntimePolicy.Stream
	}
	if cfg.FallbackPolicy.MaxRetries == 0 {
		cfg.FallbackPolicy.MaxRetries = cfg.Model.Retry
	}
	if len(cfg.FallbackPolicy.FallbackAgents) == 0 {
		cfg.FallbackPolicy.FallbackAgents = append([]string(nil), cfg.Orchestration.Fallback...)
	}
	if cfg.Orchestration.DefaultMode == "" {
		if mode, err := executionmode.FromRuntimeMode(cfg.Runtime.Mode); err == nil {
			cfg.Orchestration.DefaultMode = mode
		} else if len(cfg.Capability.ExecutionModes) > 0 {
			cfg.Orchestration.DefaultMode = cfg.Capability.ExecutionModes[0]
		} else {
			cfg.Orchestration.DefaultMode = executionmode.SingleAgent
		}
	}
	if len(cfg.Capability.ExecutionModes) == 0 {
		cfg.Capability.ExecutionModes = []ExecutionMode{cfg.Orchestration.DefaultMode}
	}
	if cfg.Runtime.Mode == "" {
		// 非法的作者态 mode 留给后续严格校验拒绝，不在归一化阶段猜测。
		if runtimeMode, err := executionmode.ToRuntimeMode(cfg.Orchestration.DefaultMode); err == nil {
			cfg.Runtime.Mode = runtimeMode
		}
	}

	cfg.Tags = sortedStrings(cfg.Tags)
	cfg.Capability.Intents = sortedStrings(cfg.Capability.Intents)
	cfg.Capability.Protocols = sortedStrings(cfg.Capability.Protocols)
	sort.Slice(cfg.Capability.ExecutionModes, func(i, j int) bool { return cfg.Capability.ExecutionModes[i] < cfg.Capability.ExecutionModes[j] })
	sort.Slice(cfg.Tools, func(i, j int) bool {
		if cfg.Tools[i].Name == cfg.Tools[j].Name {
			return cfg.Tools[i].Version < cfg.Tools[j].Version
		}
		return cfg.Tools[i].Name < cfg.Tools[j].Name
	})
	cfg.SubAgents = sortedStrings(cfg.SubAgents)
	cfg.Orchestration.SubAgents = sortedStrings(cfg.Orchestration.SubAgents)
	cfg.Orchestration.A2ATargets = sortedStrings(cfg.Orchestration.A2ATargets)
	cfg.Orchestration.Fallback = sortedStrings(cfg.Orchestration.Fallback)
	cfg.Model.Fallback = sortedStrings(cfg.Model.Fallback)
	cfg.ToolPolicy.Allowlist = sortedStrings(cfg.ToolPolicy.Allowlist)
	cfg.ToolPolicy.MCPServers = sortedStrings(cfg.ToolPolicy.MCPServers)
	cfg.ToolPolicy.HITLRequiredTools = sortedStrings(cfg.ToolPolicy.HITLRequiredTools)
	cfg.Skills.Allowlist = sortedStrings(cfg.Skills.Allowlist)
	cfg.Context.RAGNamespaces = sortedStrings(cfg.Context.RAGNamespaces)
	cfg.Memory.ReadNamespaces = sortedStrings(cfg.Memory.ReadNamespaces)
	cfg.Memory.WriteNamespaces = sortedStrings(cfg.Memory.WriteNamespaces)
	cfg.Output.Formats = sortedStrings(cfg.Output.Formats)
	cfg.Output.ArtifactTypes = sortedStrings(cfg.Output.ArtifactTypes)
	cfg.Observability.Redaction = sortedStrings(cfg.Observability.Redaction)
	cfg.FallbackPolicy.FallbackAgents = sortedStrings(cfg.FallbackPolicy.FallbackAgents)
	return cfg
}

func compileEffectiveConfig(cfg AgentConfig, deps ResolvedDependencies, now time.Time, prompt *PromptSnapshot) (*EffectiveConfig, error) {
	cfg = normalizeConfig(cfg)
	gateway := compileEffectiveGatewayTarget(cfg)
	definition := cfg.ToAgentDefinition()
	toolRisk, err := effectiveToolRisk(cfg, deps)
	if err != nil {
		return nil, wrapError(CodePolicyViolation, "tool_policy.risk_level", "tool risk policy cannot lower catalog risk", err)
	}
	if toolRisk == "" {
		delete(definition.Metadata, "tool_risk_level")
	} else {
		definition.Metadata["tool_risk_level"] = toolRisk
	}
	definition.RequiredCapabilities = withToolGovernanceCapabilities(
		definition.RequiredCapabilities,
		toolRisk,
		cfg.ToolPolicy.HITLRequiredTools,
	)
	promptHash := hashLabel("prompt", cfg.PromptRef, cfg.PromptVersion, cfg.Context.PromptPack)
	if prompt != nil {
		if prompt.Ref != cfg.PromptRef || prompt.Version != cfg.PromptVersion || prompt.PromptHash == "" ||
			prompt.SnapshotRef == "" || prompt.ContentHash == "" {
			return nil, wrapError(CodeInvalidConfig, "prompt_ref", "resolved system prompt does not match agent config", ErrPromptInvalid)
		}
		promptHash = prompt.PromptHash
		setMetadata(definition.Metadata, "prompt_hash", prompt.PromptHash)
		setMetadata(definition.Metadata, "prompt_snapshot_ref", prompt.SnapshotRef)
		setMetadata(definition.Metadata, "prompt_content_hash", prompt.ContentHash)
	}
	effective := &EffectiveConfig{
		Definition:   definition,
		PromptHash:   promptHash,
		SchemaHash:   hashLabel("schema", cfg.Capability.InputSchema, cfg.Capability.OutputSchema),
		PolicyHash:   effectivePolicyHash(cfg, toolRisk),
		ResolvedDeps: normalizeDeps(deps),
		EffectiveAt:  now,
		SourceVersionIDs: []string{
			"agent:" + cfg.AgentID + ":" + cfg.Version,
		},
		Gateway: gateway,
	}
	if prompt != nil {
		effective.SourceVersionIDs = append(effective.SourceVersionIDs, "prompt:"+prompt.SnapshotRef)
	}
	hash, err := computeEffectiveConfigHash(*effective, cfg)
	if err != nil {
		return nil, err
	}
	effective.ConfigHash = hash
	effective.ConfigSnapshotRef = configSnapshotRef(cfg.AgentID, cfg.Version, hash)
	return effective, nil
}

func cloneGatewayTargetConfig(input *GatewayTargetConfig) *GatewayTargetConfig {
	if input == nil {
		return nil
	}
	out := *input
	out.Plugins = normalizeGatewayPluginConfigs(input.Plugins)
	if input.Remote != nil {
		remote := *input.Remote
		remote.AllowedEndpointOrigins = append([]string(nil), input.Remote.AllowedEndpointOrigins...)
		out.Remote = &remote
	}
	return &out
}

func cloneEffectiveGatewayTarget(input *EffectiveGatewayTarget) *EffectiveGatewayTarget {
	if input == nil {
		return nil
	}
	out := *input
	out.Plugins = cloneGatewayPluginConfigs(input.Plugins)
	if input.Remote != nil {
		remote := *input.Remote
		remote.AllowedEndpointOrigins = append([]string(nil), input.Remote.AllowedEndpointOrigins...)
		out.Remote = &remote
	}
	return &out
}

func compileEffectiveGatewayTarget(cfg AgentConfig) *EffectiveGatewayTarget {
	if cfg.Gateway == nil {
		return nil
	}
	return &EffectiveGatewayTarget{
		ProviderKind: cfg.Gateway.ProviderKind,
		Plugins:      cloneGatewayPluginConfigs(cfg.Gateway.Plugins),
		Remote:       cloneGatewayRemoteConfig(cfg.Gateway.Remote),
	}
}

func normalizeGatewayPluginConfigs(input []GatewayPluginConfig) []GatewayPluginConfig {
	out := make([]GatewayPluginConfig, len(input))
	for i := range input {
		out[i] = input[i].Clone()
		out[i].PluginID = strings.TrimSpace(out[i].PluginID)
		if canonical, err := gatewaycontract.CanonicalizePluginConfig(out[i].Config); err == nil {
			out[i].Config = canonical
		}
	}
	return out
}

func cloneGatewayPluginConfigs(input []GatewayPluginConfig) []GatewayPluginConfig {
	out := make([]GatewayPluginConfig, len(input))
	for i := range input {
		out[i] = input[i].Clone()
	}
	return out
}

func cloneGatewayRemoteConfig(input *GatewayRemoteConfig) *GatewayRemoteConfig {
	if input == nil {
		return nil
	}
	out := *input
	out.AllowedEndpointOrigins = append([]string(nil), input.AllowedEndpointOrigins...)
	return &out
}

func effectivePolicyHash(cfg AgentConfig, toolRisk string) string {
	parts := []string{cfg.GuardrailPolicyRef, cfg.ContextPolicyRef, toolRisk}
	parts = append(parts, cfg.ToolPolicy.HITLRequiredTools...)
	return hashLabel("policy", parts...)
}

func compileConfigSnapshots(cfg AgentConfig, base EffectiveConfig) ([]ConfigSnapshotRecord, error) {
	cfg = normalizeConfig(cfg)
	snapshots := make([]ConfigSnapshotRecord, 0, len(cfg.Capability.ExecutionModes))
	for _, mode := range cfg.Capability.ExecutionModes {
		effective := cloneEffectiveConfig(base)
		definition, err := applyExecutionMode(cfg, effective.Definition, mode)
		if err != nil {
			return nil, err
		}
		effective.Definition = definition
		effective.Definition.RequiredCapabilities = withToolGovernanceCapabilities(
			effective.Definition.RequiredCapabilities,
			base.Definition.Metadata["tool_risk_level"],
			cfg.ToolPolicy.HITLRequiredTools,
		)
		hash, err := computeEffectiveConfigHash(effective, cfg)
		if err != nil {
			return nil, err
		}
		effective.ConfigHash = hash
		effective.ConfigSnapshotRef = configSnapshotRef(cfg.AgentID, cfg.Version, hash)
		snapshots = append(snapshots, ConfigSnapshotRecord{
			Ref: effective.ConfigSnapshotRef, AgentID: cfg.AgentID, Version: cfg.Version,
			ExecutionMode: mode, ConfigHash: hash, Effective: effective, CreatedAt: effective.EffectiveAt,
		})
	}
	return snapshots, nil
}

func configSnapshotRef(agentID, version, hash string) string {
	return "agent-config://" + agentID + "/" + version + "/" + hash
}

func compileCapabilityCard(cfg AgentConfig, effective *EffectiveConfig) (*CapabilityCard, error) {
	cfg = normalizeConfig(cfg)
	card := &CapabilityCard{
		AgentID:        cfg.AgentID,
		AgentType:      cfg.AgentType,
		Version:        cfg.Version,
		Owner:          cfg.Owner,
		Description:    cfg.Description,
		Intents:        append([]string(nil), cfg.Capability.Intents...),
		ExecutionModes: append([]ExecutionMode(nil), cfg.Capability.ExecutionModes...),
		InputSchema:    cfg.Capability.InputSchema,
		OutputSchema:   cfg.Capability.OutputSchema,
		RiskLevel:      effective.Definition.Metadata["tool_risk_level"],
		Protocols:      append([]string(nil), cfg.Capability.Protocols...),
		Status:         cfg.Status,
		ConfigHash:     effective.ConfigHash,
		OutputFormat:   cfg.ProtocolPolicy.OutputFormat,
		Tags:           append([]string(nil), cfg.Tags...),
		ScoreHints:     ScoreHints{HealthScore: 1, HistoricalSuccess: 1, CostLatencyScore: 1},
	}
	cardHash, err := computeCardHash(*card)
	if err != nil {
		return nil, err
	}
	card.CardHash = cardHash
	return card, nil
}

func executionMode(cfg AgentConfig) ExecutionMode {
	if cfg.Orchestration.DefaultMode != "" {
		return cfg.Orchestration.DefaultMode
	}
	if len(cfg.Capability.ExecutionModes) > 0 {
		return cfg.Capability.ExecutionModes[0]
	}
	return ExecutionModeSingleAgent
}

func requiredCapabilities(cfg AgentConfig, mode ExecutionMode) []string {
	required := make(map[string]struct{}, 9)
	add := func(capability string) {
		if capability != "" {
			required[capability] = struct{}{}
		}
	}
	if cfg.ProtocolPolicy.Streaming || cfg.RuntimePolicy.Stream {
		add("streaming")
	}
	if len(cfg.Tools) > 0 || len(cfg.ToolPolicy.Allowlist) > 0 {
		add("tool_call")
	}
	if len(cfg.SubAgents) > 0 || len(cfg.Orchestration.SubAgents) > 0 {
		add("sub_agent")
	}
	if cfg.RuntimePolicy.Checkpoint {
		add("checkpoint")
		add("resume")
	}
	if len(cfg.ToolPolicy.MCPServers) > 0 {
		add("mcp")
		add("tool_call")
	}
	switch mode {
	case executionmode.DeepAgent:
		add("deep_agent")
	case executionmode.Workflow:
		add("workflow")
		addNodeCapabilities(required, workflowNodes(cfg.Orchestration.Workflow))
	case executionmode.Graph:
		// Runtime 当前没有 graph capability，保留该需求可使候选 Runtime 动态 fail closed。
		add("graph")
		addNodeCapabilities(required, graphNodes(cfg.Orchestration.Graph))
	}
	out := make([]string, 0, len(required))
	for capability := range required {
		out = append(out, capability)
	}
	sort.Strings(out)
	return out
}

func withToolGovernanceCapabilities(required []string, risk string, hitlTools []string) []string {
	if risk != "medium" && risk != "high" && len(hitlTools) == 0 {
		return append([]string(nil), required...)
	}
	set := make(map[string]struct{}, len(required)+3)
	for _, capability := range required {
		set[capability] = struct{}{}
	}
	set["checkpoint"] = struct{}{}
	set["control_request"] = struct{}{}
	set["resume"] = struct{}{}
	out := make([]string, 0, len(set))
	for capability := range set {
		out = append(out, capability)
	}
	sort.Strings(out)
	return out
}

func addNodeCapabilities(required map[string]struct{}, nodes []agentruntime.WorkflowNode) {
	for _, node := range nodes {
		if node.ToolRef != "" {
			required["tool_call"] = struct{}{}
		}
		if node.AgentID != "" {
			required["sub_agent"] = struct{}{}
		}
	}
}

func workflowNodes(definition *agentruntime.WorkflowDefinition) []agentruntime.WorkflowNode {
	if definition == nil {
		return nil
	}
	return definition.Nodes
}

func graphNodes(definition *agentruntime.GraphDefinition) []agentruntime.WorkflowNode {
	if definition == nil {
		return nil
	}
	return definition.Nodes
}

func cloneWorkflowDefinition(definition *agentruntime.WorkflowDefinition) *agentruntime.WorkflowDefinition {
	if definition == nil {
		return nil
	}
	clone := *definition
	clone.Nodes = cloneWorkflowNodes(definition.Nodes)
	clone.Edges = append([]agentruntime.WorkflowEdge(nil), definition.Edges...)
	return &clone
}

func cloneGraphDefinition(definition *agentruntime.GraphDefinition) *agentruntime.GraphDefinition {
	if definition == nil {
		return nil
	}
	clone := *definition
	clone.Nodes = cloneWorkflowNodes(definition.Nodes)
	clone.Edges = append([]agentruntime.WorkflowEdge(nil), definition.Edges...)
	return &clone
}

func cloneWorkflowNodes(nodes []agentruntime.WorkflowNode) []agentruntime.WorkflowNode {
	clones := make([]agentruntime.WorkflowNode, len(nodes))
	for index, node := range nodes {
		clones[index] = node
		clones[index].InputKeys = append([]string(nil), node.InputKeys...)
		clones[index].OutputKeys = append([]string(nil), node.OutputKeys...)
		clones[index].Config = cloneStringMap(node.Config)
	}
	return clones
}

func promptRef(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, "://") {
		return name
	}
	for _, char := range name {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' ||
			char == '-' || char == '_' || char == '.' || char == '/' {
			continue
		}
		return ""
	}
	return "prompt://" + name
}

func policyRef(prefix, name string) string {
	if name == "" || strings.Contains(name, "://") {
		return name
	}
	return prefix + "://" + name
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func normalizeDeps(deps ResolvedDependencies) ResolvedDependencies {
	deps.Models = sortedStrings(deps.Models)
	deps.Tools = sortedUniqueStrings(deps.Tools)
	deps.AuthoringHighRiskTools = sortedUniqueStrings(deps.AuthoringHighRiskTools)
	deps.MCPServers = sortedStrings(deps.MCPServers)
	deps.HTTPTools = sortedStrings(deps.HTTPTools)
	deps.Prompts = sortedStrings(deps.Prompts)
	deps.Schemas = sortedStrings(deps.Schemas)
	deps.Skills = sortedStrings(deps.Skills)
	deps.Guardrails = sortedStrings(deps.Guardrails)
	deps.Agents = sortedStrings(deps.Agents)
	deps.Protocols = sortedStrings(deps.Protocols)
	return deps
}

func sortedUniqueStrings(values []string) []string {
	values = sortedStrings(values)
	out := values[:0]
	for _, value := range values {
		if value == "" || len(out) > 0 && out[len(out)-1] == value {
			continue
		}
		out = append(out, value)
	}
	return out
}

func hashLabel(prefix string, parts ...string) string {
	hasValue := false
	for _, part := range parts {
		if part != "" {
			hasValue = true
			break
		}
	}
	if !hasValue {
		return ""
	}
	// 字段位置属于语义，不能通过排序或丢弃空值把不同配置压成同一摘要。
	hash, _ := hashJSON(prefix, struct {
		Domain string   `json:"domain"`
		Parts  []string `json:"parts"`
	}{Domain: prefix, Parts: append([]string(nil), parts...)})
	return hash
}
