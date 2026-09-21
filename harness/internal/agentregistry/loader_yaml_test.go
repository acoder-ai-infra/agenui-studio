package agentregistry

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
)

func TestYAMLLoaderRejectsLegacyGatewayFields(t *testing.T) {
	base := `
agent_id: gateway_agent
agent_type: assistant
version: v1
runtime: {type: mock, mode: react}
prompt_ref: prompt://gateway/system
gateway:
  provider_kind: local_agent
`
	tests := map[string]string{
		"revision":      base + "  revision: rev-1\n",
		"task_contract": base + "  task_contract: {ref: task://legacy}\n",
		"string_plugin": base + "  plugins: [basic_validator]\n",
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := loadYAMLSource(t, source); err == nil {
				t.Fatal("legacy Gateway syntax must fail closed")
			}
		})
	}
}

func TestYAMLLoaderDecodesGatewayPluginsInWrittenOrder(t *testing.T) {
	const source = `
agent_id: gateway_agent
agent_type: assistant
version: v1
runtime: {type: mock, mode: react}
prompt_ref: prompt://gateway/system
gateway:
  provider_kind: local_agent
  plugins:
    - plugin_id: first_plugin
      config:
        threshold: 2
        enabled: true
    - plugin_id: second_plugin
      config: {}
`
	cfg := loadSingleYAMLConfig(t, source)
	if len(cfg.Gateway.Plugins) != 2 || cfg.Gateway.Plugins[0].PluginID != "first_plugin" || cfg.Gateway.Plugins[1].PluginID != "second_plugin" {
		t.Fatalf("gateway plugin order changed: %#v", cfg.Gateway.Plugins)
	}
	if got := string(cfg.Gateway.Plugins[0].Config); got != `{"enabled":true,"threshold":2}` {
		t.Fatalf("plugin config is not canonical JSON: %s", got)
	}
	if err := ValidateAgentConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestYAMLLoaderRejectsNonObjectGatewayPluginConfig(t *testing.T) {
	for name, config := range map[string]string{
		"array":  "[]",
		"scalar": "strict",
		"null":   "null",
	} {
		t.Run(name, func(t *testing.T) {
			source := `
agent_id: gateway_agent
agent_type: assistant
version: v1
runtime: {type: mock, mode: react}
prompt_ref: prompt://gateway/system
gateway:
  provider_kind: local_agent
  plugins:
    - plugin_id: basic_validator
      config: ` + config + "\n"
			if _, err := loadYAMLSource(t, source); err == nil {
				t.Fatal("non-object gateway plugin config must fail closed")
			}
		})
	}
}

func TestYAMLLoaderRejectsUnknownFields(t *testing.T) {
	tests := map[string]string{
		"top_level": `
agent_id: strict_agent
agent_type: assistant
agent_typo: rejected
version: v1
runtime: {type: mock}
prompt_ref: prompt://strict/system
`,
		"nested": `
agent_id: strict_agent
agent_type: assistant
version: v1
runtime: {type: mock}
prompt_ref: prompt://strict/system
model:
  model_route_polciy: typo-must-not-fallback
  primary: model-a
`,
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(source), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			if _, err := NewYAMLLoader(dir).Load(context.Background()); err == nil {
				t.Fatal("unknown YAML field must fail closed")
			}
		})
	}
}

func TestYAMLLoaderRejectsLegacyExecutionMode(t *testing.T) {
	const source = `
agent_id: legacy_agent
agent_type: assistant
version: v1
runtime: {type: eino}
prompt_ref: prompt://legacy/system
capability:
  execution_modes: [workflow_graph]
orchestration:
  default_mode: workflow_graph
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(source), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := NewYAMLLoader(dir).Load(context.Background()); err == nil {
		t.Fatal("legacy execution mode must fail closed")
	}
}

func TestYAMLLoaderDecodesCanonicalWorkflowDefinition(t *testing.T) {
	const source = `
agent_id: workflow_agent
agent_type: assistant
version: v1
runtime: {type: mock}
prompt_ref: prompt://workflow/system
capability:
  execution_modes: [workflow]
tools:
  - name: search
    version: v1
sub_agents: [planner]
orchestration:
  default_mode: workflow
  workflow_ref: workflow://trip/v1
  workflow:
    workflow_id: workflow://trip/v1
    entry_node: plan
    nodes:
      - node_id: plan
        node_type: agent
        agent_id: planner
      - node_id: search
        node_type: tool
        tool_ref: search@v1
    edges:
      - from_node_id: plan
        to_node_id: search
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(source), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	configs, err := NewYAMLLoader(dir).Load(context.Background())
	if err != nil {
		t.Fatalf("load canonical workflow: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("config count = %d, want 1", len(configs))
	}
	if err := ValidateAgentConfig(configs[0]); err != nil {
		t.Fatalf("validate canonical workflow: %v", err)
	}
	definition := configs[0].ToAgentDefinition()
	if definition.Workflow == nil || definition.Workflow.WorkflowID != "workflow://trip/v1" || definition.Runtime.Mode != "workflow" {
		t.Fatalf("workflow definition was not compiled: %#v", definition)
	}
}

func TestYAMLLoaderMigratesLegacyContextCompression(t *testing.T) {
	const source = `
agent_id: legacy_context
agent_type: assistant
version: v1
runtime: {type: mock, mode: react}
prompt_ref: prompt://legacy/system
context:
  compression: summary_v1
`
	cfg := loadSingleYAMLConfig(t, source)
	if cfg.Context.Compression != "" || cfg.ContextCompaction.PolicyHash == "" {
		t.Fatalf("legacy compression was not normalized: %#v", cfg)
	}
	if err := ValidateAgentConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestYAMLLoaderDecodesTypedContextCompaction(t *testing.T) {
	const source = `
agent_id: typed_context
agent_type: assistant
version: v1
runtime: {type: mock, mode: react}
prompt_ref: prompt://typed/system
context_compaction:
  policy_id: tenant-travel
  version: v3
  semantic_summary: required
  soft_trigger_ratio: 0.70
  compact_trigger_ratio: 0.85
  target_ratio: 0.60
  preserve_recent_turns: 3
  max_summary_tokens: 512
`
	cfg := loadSingleYAMLConfig(t, source)
	policy := cfg.ContextCompaction
	if policy.PolicyID != "tenant-travel" || policy.Version != "v3" || policy.SemanticSummary != agentruntime.SemanticSummaryRequired ||
		policy.PreserveRecentTurns != 3 || policy.MaxSummaryTokens != 512 || policy.PolicyHash == "" {
		t.Fatalf("typed compaction policy = %#v", policy)
	}
	if err := ValidateAgentConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestYAMLLoaderPreservesExplicitZeroTemperatureAndModelOptions(t *testing.T) {
	const source = `
agent_id: model_options
agent_type: assistant
version: v1
runtime: {type: mock, mode: react}
prompt_ref: prompt://model/system
model:
  temperature: 0
  top_p: 0.8
  max_tokens: 4096
  stop: [END]
  tool_choice: auto
  reasoning_mode: enabled
  reasoning_budget_tokens: 2048
  reasoning_effort: low
`
	cfg := loadSingleYAMLConfig(t, source)
	if err := ValidateAgentConfig(cfg); err != nil {
		t.Fatal(err)
	}
	options := cfg.ToAgentDefinition().ModelOptions
	if options.Temperature == nil || *options.Temperature != 0 || options.TopP == nil || *options.TopP != 0.8 || options.MaxTokens == nil || *options.MaxTokens != 4096 || options.ToolChoice != "auto" || len(options.Stop) != 1 || options.Stop[0] != "END" || options.ReasoningBudget != 2048 || options.ReasoningEffort != "low" {
		t.Fatalf("model options changed across YAML registration: %#v", options)
	}
}

func loadSingleYAMLConfig(t *testing.T, source string) AgentConfig {
	t.Helper()
	configs, err := loadYAMLSource(t, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 1 {
		t.Fatalf("config count = %d", len(configs))
	}
	return configs[0]
}

func loadYAMLSource(t *testing.T, source string) ([]AgentConfig, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return NewYAMLLoader(dir).Load(context.Background())
}
