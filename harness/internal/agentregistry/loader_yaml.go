package agentregistry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/executionmode"
	"github.com/AGenUI/agenui-studio/harness/internal/gatewaycontract"
	"github.com/AGenUI/agenui-studio/harness/internal/processpresentation"
	"gopkg.in/yaml.v3"
)

type YAMLLoader struct{ Dir string }

func NewYAMLLoader(dir string) YAMLLoader { return YAMLLoader{Dir: dir} }

func (l YAMLLoader) Load(ctx context.Context) ([]AgentConfig, error) {
	dir := l.Dir
	if dir == "" {
		dir = "agents"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, wrapError(CodeLoadFailed, dir, "read agent config directory", err)
	}
	var configs []AgentConfig
	for _, entry := range entries {
		if entry.IsDir() || (!strings.HasSuffix(entry.Name(), ".yaml") && !strings.HasSuffix(entry.Name(), ".yml")) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		loaded, err := loadYAMLFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		configs = append(configs, loaded...)
	}
	return configs, nil
}

// manifestDTO 只存在于配置入口，显式把正式文档中的作者态 YAML 编译成 canonical AgentConfig。
// 这样兼容 provider/allowlist 等配置写法，同时不会污染 AgentConfig 的 JSON/hash 语义。
type manifestDTO struct {
	AgentID             string                                `yaml:"agent_id"`
	AgentType           string                                `yaml:"agent_type"`
	Version             string                                `yaml:"version"`
	Status              AgentStatus                           `yaml:"status"`
	Name                string                                `yaml:"name"`
	Description         string                                `yaml:"description"`
	Owner               string                                `yaml:"owner"`
	Tags                []string                              `yaml:"tags"`
	Capability          CapabilityConfig                      `yaml:"capability"`
	Runtime             manifestRuntime                       `yaml:"runtime"`
	Prompt              manifestPrompt                        `yaml:"prompt"`
	PromptRef           string                                `yaml:"prompt_ref"`
	PromptVersion       string                                `yaml:"prompt_version"`
	Tools               yaml.Node                             `yaml:"tools"`
	ToolPolicy          ToolsConfig                           `yaml:"tool_policy"`
	SubAgents           yaml.Node                             `yaml:"sub_agents"`
	DataPassing         agentruntime.DataPassingPolicy        `yaml:"data_passing"`
	ContextPolicy       manifestPolicyRef                     `yaml:"context_policy"`
	ContextPolicyRef    string                                `yaml:"context_policy_ref"`
	ContextCompaction   agentruntime.ContextCompactionPolicy  `yaml:"context_compaction"`
	GuardrailPolicy     manifestPolicyRef                     `yaml:"guardrail_policy"`
	GuardrailPolicyRef  string                                `yaml:"guardrail_policy_ref"`
	ProtocolPolicy      ProtocolPolicy                        `yaml:"protocol_policy"`
	FallbackPolicy      FallbackPolicy                        `yaml:"fallback_policy"`
	Metadata            map[string]string                     `yaml:"metadata"`
	ProcessPresentation processpresentation.StagePresentation `yaml:"process_presentation"`
	Orchestration       manifestOrchestration                 `yaml:"orchestration"`
	Model               manifestModel                         `yaml:"model"`
	Skills              SkillsConfig                          `yaml:"skills"`
	Context             ContextConfig                         `yaml:"context"`
	Memory              MemoryConfig                          `yaml:"memory"`
	Guardrails          GuardrailsConfig                      `yaml:"guardrails"`
	Output              OutputConfig                          `yaml:"output"`
	Observability       ObservabilityConfig                   `yaml:"observability"`
	ObservabilityPolicy manifestObservabilityPolicy           `yaml:"observability_policy"`
	Release             ReleaseConfig                         `yaml:"release"`
	Gateway             *manifestGateway                      `yaml:"gateway"`
	// Extensions 声明 agent 级扩展绑定（声明顺序即执行顺序）。
	Extensions ExtensionsConfig `yaml:"extensions"`
}

type manifestGateway struct {
	ProviderKind gatewaycontract.SubAgentProviderKind `yaml:"provider_kind"`
	Plugins      []manifestGatewayPlugin              `yaml:"plugins"`
	Remote       *GatewayRemoteConfig                 `yaml:"remote"`
}

type manifestGatewayPlugin struct {
	PluginID string    `yaml:"plugin_id"`
	Config   yaml.Node `yaml:"config"`
}

func (m *manifestGateway) toConfig() (*GatewayTargetConfig, error) {
	if m == nil {
		return nil, nil
	}
	plugins := make([]GatewayPluginConfig, 0, len(m.Plugins))
	for _, input := range m.Plugins {
		plugin := GatewayPluginConfig{PluginID: input.PluginID, Config: json.RawMessage(`{}`)}
		if input.Config.Kind != 0 {
			config, err := yamlNodeJSON(input.Config)
			if err != nil {
				return nil, fmt.Errorf("gateway.plugins.config: %w", err)
			}
			plugin.Config, err = gatewaycontract.CanonicalizePluginConfig(config)
			if err != nil {
				return nil, fmt.Errorf("gateway.plugins.config: config must be an object")
			}
		}
		plugins = append(plugins, plugin)
	}
	return &GatewayTargetConfig{
		ProviderKind: m.ProviderKind,
		Plugins:      plugins,
		Remote:       m.Remote,
	}, nil
}

func yamlNodeJSON(node yaml.Node) (json.RawMessage, error) {
	if node.Kind == 0 {
		return nil, nil
	}
	var value any
	if err := node.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

type manifestRuntime struct {
	Type       agentruntime.RuntimeType   `yaml:"type"`
	Provider   string                     `yaml:"provider"`
	Mode       agentruntime.RuntimeMode   `yaml:"mode"`
	Preferred  agentruntime.RuntimeType   `yaml:"preferred"`
	Candidates []agentruntime.RuntimeType `yaml:"candidates"`
	RuntimeRef string                     `yaml:"runtime_ref"`
	TimeoutMS  int64                      `yaml:"timeout_ms"`
	MaxTurns   int                        `yaml:"max_turns"`
	Checkpoint bool                       `yaml:"checkpoint"`
	Stream     bool                       `yaml:"stream"`
}

type manifestOrchestration struct {
	DefaultMode  ExecutionMode               `yaml:"default_mode"`
	WorkflowRef  string                      `yaml:"workflow_ref"`
	GraphRef     string                      `yaml:"graph_ref"`
	DeepAgentRef string                      `yaml:"deep_agent_ref"`
	Workflow     *manifestWorkflowDefinition `yaml:"workflow"`
	Graph        *manifestGraphDefinition    `yaml:"graph"`
	SubAgents    []string                    `yaml:"sub_agents"`
	A2ATargets   []string                    `yaml:"a2a_targets"`
	Fallback     []string                    `yaml:"fallback"`
}

type manifestWorkflowDefinition struct {
	WorkflowID string                 `yaml:"workflow_id"`
	EntryNode  string                 `yaml:"entry_node"`
	Nodes      []manifestWorkflowNode `yaml:"nodes"`
	Edges      []manifestWorkflowEdge `yaml:"edges"`
}

type manifestGraphDefinition struct {
	GraphID        string                 `yaml:"graph_id"`
	EntryNode      string                 `yaml:"entry_node"`
	Nodes          []manifestWorkflowNode `yaml:"nodes"`
	Edges          []manifestWorkflowEdge `yaml:"edges"`
	StateSchemaRef string                 `yaml:"state_schema_ref"`
}

type manifestWorkflowNode struct {
	NodeID     string            `yaml:"node_id"`
	NodeType   string            `yaml:"node_type"`
	AgentID    string            `yaml:"agent_id"`
	ToolRef    string            `yaml:"tool_ref"`
	InputKeys  []string          `yaml:"input_keys"`
	OutputKeys []string          `yaml:"output_keys"`
	Config     map[string]string `yaml:"config"`
}

type manifestWorkflowEdge struct {
	FromNodeID string `yaml:"from_node_id"`
	ToNodeID   string `yaml:"to_node_id"`
	Condition  string `yaml:"condition"`
}

func (m manifestOrchestration) toConfig() OrchestrationConfig {
	return OrchestrationConfig{
		DefaultMode:  m.DefaultMode,
		WorkflowRef:  m.WorkflowRef,
		GraphRef:     m.GraphRef,
		DeepAgentRef: m.DeepAgentRef,
		Workflow:     m.Workflow.toRuntime(),
		Graph:        m.Graph.toRuntime(),
		SubAgents:    append([]string(nil), m.SubAgents...),
		A2ATargets:   append([]string(nil), m.A2ATargets...),
		Fallback:     append([]string(nil), m.Fallback...),
	}
}

func (m *manifestWorkflowDefinition) toRuntime() *agentruntime.WorkflowDefinition {
	if m == nil {
		return nil
	}
	return &agentruntime.WorkflowDefinition{
		WorkflowID: m.WorkflowID,
		EntryNode:  m.EntryNode,
		Nodes:      manifestNodesToRuntime(m.Nodes),
		Edges:      manifestEdgesToRuntime(m.Edges),
	}
}

func (m *manifestGraphDefinition) toRuntime() *agentruntime.GraphDefinition {
	if m == nil {
		return nil
	}
	return &agentruntime.GraphDefinition{
		GraphID:        m.GraphID,
		EntryNode:      m.EntryNode,
		Nodes:          manifestNodesToRuntime(m.Nodes),
		Edges:          manifestEdgesToRuntime(m.Edges),
		StateSchemaRef: m.StateSchemaRef,
	}
}

func manifestNodesToRuntime(nodes []manifestWorkflowNode) []agentruntime.WorkflowNode {
	out := make([]agentruntime.WorkflowNode, len(nodes))
	for index, node := range nodes {
		out[index] = agentruntime.WorkflowNode{
			NodeID: node.NodeID, NodeType: node.NodeType, AgentID: node.AgentID, ToolRef: node.ToolRef,
			InputKeys: append([]string(nil), node.InputKeys...), OutputKeys: append([]string(nil), node.OutputKeys...),
			Config: cloneStringMap(node.Config),
		}
	}
	return out
}

func manifestEdgesToRuntime(edges []manifestWorkflowEdge) []agentruntime.WorkflowEdge {
	out := make([]agentruntime.WorkflowEdge, len(edges))
	for index, edge := range edges {
		out[index] = agentruntime.WorkflowEdge{FromNodeID: edge.FromNodeID, ToNodeID: edge.ToNodeID, Condition: edge.Condition}
	}
	return out
}

type manifestPrompt struct {
	PromptRef     string `yaml:"prompt_ref"`
	PromptVersion string `yaml:"prompt_version"`
}

type manifestPolicyRef struct {
	PolicyRef string `yaml:"policy_ref"`
}

type manifestModel struct {
	// model_route_policy 是正式设计字段；router_policy 仅兼容早期样例。
	ModelRoutePolicy string                          `yaml:"model_route_policy"`
	RouterPolicy     string                          `yaml:"router_policy"`
	Primary          string                          `yaml:"primary"`
	Fallback         []string                        `yaml:"fallback"`
	Temperature      *float64                        `yaml:"temperature"`
	TopP             *float64                        `yaml:"top_p"`
	MaxTokens        *int                            `yaml:"max_tokens"`
	Stop             []string                        `yaml:"stop"`
	ToolChoice       string                          `yaml:"tool_choice"`
	Retry            int                             `yaml:"retry"`
	ReasoningMode    agentruntime.ModelReasoningMode `yaml:"reasoning_mode"`
	ReasoningBudget  int                             `yaml:"reasoning_budget_tokens"`
	ReasoningEffort  string                          `yaml:"reasoning_effort"`
}

func (m manifestModel) toConfig() ModelConfig {
	return ModelConfig{
		RouterPolicy:    firstNonEmpty(m.ModelRoutePolicy, m.RouterPolicy),
		Primary:         m.Primary,
		Fallback:        append([]string(nil), m.Fallback...),
		Temperature:     cloneFloat64(m.Temperature),
		TopP:            cloneFloat64(m.TopP),
		MaxTokens:       cloneInt(m.MaxTokens),
		Stop:            append([]string(nil), m.Stop...),
		ToolChoice:      m.ToolChoice,
		Retry:           m.Retry,
		ReasoningMode:   m.ReasoningMode,
		ReasoningBudget: m.ReasoningBudget,
		ReasoningEffort: m.ReasoningEffort,
	}
}

func cloneFloat64(value *float64) *float64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

type manifestObservabilityPolicy struct {
	TraceSampleRate float64  `yaml:"trace_sample_rate"`
	Redaction       []string `yaml:"redaction"`
}

func loadYAMLFile(path string) ([]AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, wrapError(CodeLoadFailed, path, "read agent config file", err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, wrapError(CodeLoadFailed, path, "parse yaml agent config", err)
	}
	if len(document.Content) != 1 {
		return nil, newError(CodeLoadFailed, path, "yaml document is empty")
	}
	root := document.Content[0]
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var manifests []manifestDTO
	switch root.Kind {
	case yaml.MappingNode:
		var manifest manifestDTO
		if err := decoder.Decode(&manifest); err != nil {
			return nil, wrapError(CodeLoadFailed, path, "decode agent manifest", err)
		}
		manifests = append(manifests, manifest)
	case yaml.SequenceNode:
		if err := decoder.Decode(&manifests); err != nil {
			return nil, wrapError(CodeLoadFailed, path, "decode agent manifests", err)
		}
	default:
		return nil, newError(CodeLoadFailed, path, "yaml root must be a mapping or sequence")
	}
	configs := make([]AgentConfig, 0, len(manifests))
	for index, manifest := range manifests {
		cfg, err := manifest.toAgentConfig()
		if err != nil {
			return nil, wrapError(CodeLoadFailed, path, fmt.Sprintf("convert agent manifest %d", index), err)
		}
		configs = append(configs, cfg)
	}
	return configs, nil
}

func (m manifestDTO) toAgentConfig() (AgentConfig, error) {
	tools, toolPolicy, err := decodeTools(m.Tools, m.ToolPolicy)
	if err != nil {
		return AgentConfig{}, err
	}
	subAgents, err := decodeSubAgents(m.SubAgents)
	if err != nil {
		return AgentConfig{}, err
	}
	runtimeType := m.Runtime.Type
	if runtimeType == "" {
		runtimeType = agentruntime.RuntimeType(m.Runtime.Provider)
	}
	orchestration := m.Orchestration.toConfig()
	runtimeMode := m.Runtime.Mode
	if runtimeMode == "" && orchestration.DefaultMode != "" {
		runtimeMode, err = executionmode.ToRuntimeMode(orchestration.DefaultMode)
		if err != nil {
			return AgentConfig{}, wrapError(CodeInvalidConfig, "orchestration.default_mode", "unsupported execution mode", err)
		}
	}
	if len(subAgents) == 0 {
		subAgents = append([]string(nil), orchestration.SubAgents...)
	}
	model := m.Model.toConfig()
	gateway, err := m.Gateway.toConfig()
	if err != nil {
		return AgentConfig{}, err
	}
	cfg := AgentConfig{
		AgentID: m.AgentID, AgentType: m.AgentType, Version: m.Version, Status: m.Status,
		Runtime:   agentruntime.RuntimeSpec{Type: runtimeType, Mode: runtimeMode, Preferred: m.Runtime.Preferred, Candidates: m.Runtime.Candidates},
		PromptRef: firstNonEmpty(m.PromptRef, m.Prompt.PromptRef), PromptVersion: firstNonEmpty(m.PromptVersion, m.Prompt.PromptVersion),
		Tools: tools, SubAgents: subAgents,
		DataPassing:        cloneDataPassingPolicy(m.DataPassing),
		ContextPolicyRef:   firstNonEmpty(m.ContextPolicyRef, m.ContextPolicy.PolicyRef),
		ContextCompaction:  m.ContextCompaction,
		GuardrailPolicyRef: firstNonEmpty(m.GuardrailPolicyRef, m.GuardrailPolicy.PolicyRef),
		ProtocolPolicy:     m.ProtocolPolicy, FallbackPolicy: m.FallbackPolicy, Metadata: m.Metadata,
		ProcessPresentation: m.ProcessPresentation,
		Name:                m.Name, Description: m.Description, Owner: m.Owner, Tags: m.Tags, Capability: m.Capability,
		RuntimePolicy: RuntimeConfig{Provider: m.Runtime.Provider, RuntimeRef: m.Runtime.RuntimeRef, TimeoutMS: m.Runtime.TimeoutMS, MaxTurns: m.Runtime.MaxTurns, Checkpoint: m.Runtime.Checkpoint, Stream: m.Runtime.Stream},
		Orchestration: orchestration, Model: model, ToolPolicy: toolPolicy, Skills: m.Skills,
		Context: m.Context, Memory: m.Memory, Guardrails: m.Guardrails, Output: m.Output,
		Observability: m.Observability, Release: m.Release, Gateway: gateway,
		Extensions: m.Extensions,
	}
	if cfg.Observability.SamplingRate == 0 {
		cfg.Observability.SamplingRate = m.ObservabilityPolicy.TraceSampleRate
	}
	if len(cfg.Observability.Redaction) == 0 {
		cfg.Observability.Redaction = append([]string(nil), m.ObservabilityPolicy.Redaction...)
	}
	return normalizeConfig(cfg), nil
}

func decodeTools(node yaml.Node, policy ToolsConfig) ([]VersionedRef, ToolsConfig, error) {
	if node.Kind == 0 {
		return nil, policy, nil
	}
	switch node.Kind {
	case yaml.SequenceNode:
		for _, item := range node.Content {
			if item.Kind != yaml.MappingNode {
				return nil, policy, fmt.Errorf("tools entries must be mappings")
			}
			if err := validateMappingKeys(item, "tools", "name", "version"); err != nil {
				return nil, policy, err
			}
		}
		var tools []VersionedRef
		if err := node.Decode(&tools); err != nil {
			return nil, policy, err
		}
		return tools, policy, nil
	case yaml.MappingNode:
		if err := validateMappingKeys(&node, "tools", "allowlist", "risk_level", "mcp_servers", "hitl_required_tools"); err != nil {
			return nil, policy, err
		}
		var legacy struct {
			Allowlist         []string `yaml:"allowlist"`
			RiskLevel         string   `yaml:"risk_level"`
			MCPServers        []string `yaml:"mcp_servers"`
			HITLRequiredTools []string `yaml:"hitl_required_tools"`
		}
		if err := node.Decode(&legacy); err != nil {
			return nil, policy, err
		}
		policy.Allowlist = append(policy.Allowlist, legacy.Allowlist...)
		policy.RiskLevel = firstNonEmpty(policy.RiskLevel, legacy.RiskLevel)
		policy.MCPServers = append(policy.MCPServers, legacy.MCPServers...)
		policy.HITLRequiredTools = append(policy.HITLRequiredTools, legacy.HITLRequiredTools...)
		return nil, policy, nil
	default:
		return nil, policy, fmt.Errorf("tools must be a sequence or mapping")
	}
}

func decodeSubAgents(node yaml.Node) ([]string, error) {
	if node.Kind == 0 {
		return nil, nil
	}
	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("sub_agents must be a sequence")
	}
	out := make([]string, 0, len(node.Content))
	for _, item := range node.Content {
		switch item.Kind {
		case yaml.ScalarNode:
			out = append(out, item.Value)
		case yaml.MappingNode:
			if err := validateMappingKeys(item, "sub_agents", "agent_id", "alias"); err != nil {
				return nil, err
			}
			var ref struct {
				AgentID string `yaml:"agent_id"`
			}
			if err := item.Decode(&ref); err != nil {
				return nil, err
			}
			if ref.AgentID == "" {
				return nil, fmt.Errorf("sub_agents.agent_id is required")
			}
			out = append(out, ref.AgentID)
		default:
			return nil, fmt.Errorf("unsupported sub_agent node kind %d", item.Kind)
		}
	}
	return out, nil
}

func validateMappingKeys(node *yaml.Node, field string, allowed ...string) error {
	known := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		known[key] = struct{}{}
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		key := node.Content[index].Value
		if _, ok := known[key]; !ok {
			return fmt.Errorf("unknown %s field %q", field, key)
		}
	}
	return nil
}
