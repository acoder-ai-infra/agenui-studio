package agentregistry

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/executionmode"
	"github.com/AGenUI/agenui-studio/harness/internal/processpresentation"
)

const maxEinoDeepAgentIterations = 1000

func validateExecutionConfig(cfg AgentConfig) error {
	if err := processpresentation.ValidateStage(cfg.ProcessPresentation.Stage); err != nil {
		return wrapError(CodeInvalidConfig, "process_presentation.stage", "invalid process presentation", err)
	}
	if err := validateRuntimePolicy(cfg); err != nil {
		return err
	}
	if err := executionmode.Validate(cfg.Orchestration.DefaultMode); err != nil {
		return wrapError(CodeInvalidConfig, "orchestration.default_mode", "unsupported execution mode", err)
	}
	seen := make(map[ExecutionMode]struct{}, len(cfg.Capability.ExecutionModes))
	for _, mode := range cfg.Capability.ExecutionModes {
		if err := executionmode.Validate(mode); err != nil {
			return wrapError(CodeInvalidConfig, "capability.execution_modes", "unsupported execution mode", err)
		}
		if _, duplicate := seen[mode]; duplicate {
			return newError(CodeInvalidConfig, "capability.execution_modes", "duplicate execution mode: "+string(mode))
		}
		seen[mode] = struct{}{}
	}
	if _, ok := seen[cfg.Orchestration.DefaultMode]; !ok {
		return newError(CodeInvalidConfig, "orchestration.default_mode", "default mode must be listed in capability.execution_modes")
	}
	wantRuntimeMode, err := executionmode.ToRuntimeMode(cfg.Orchestration.DefaultMode)
	if err != nil {
		return wrapError(CodeInvalidConfig, "orchestration.default_mode", "map execution mode", err)
	}
	if cfg.Runtime.Mode != wantRuntimeMode {
		return newError(CodeInvalidConfig, "runtime.mode", fmt.Sprintf("runtime mode %q does not match default execution mode %q", cfg.Runtime.Mode, cfg.Orchestration.DefaultMode))
	}
	if _, direct := seen[executionmode.DirectAction]; direct {
		if len(seen) != 1 {
			return newError(CodeInvalidConfig, "capability.execution_modes", "direct_action requires a dedicated native/direct definition")
		}
		if err := validateDirectRuntime(cfg.Runtime); err != nil {
			return err
		}
	}
	if _, workflow := seen[executionmode.Workflow]; workflow {
		if cfg.Orchestration.WorkflowRef == "" {
			return newError(CodeInvalidConfig, "orchestration.workflow_ref", "workflow_ref is required for workflow mode")
		}
		if err := validateWorkflowDefinition(cfg.Orchestration.Workflow); err != nil {
			return err
		}
		if cfg.Orchestration.Workflow.WorkflowID != cfg.Orchestration.WorkflowRef {
			return newError(CodeInvalidConfig, "orchestration.workflow.workflow_id", "workflow_id must equal workflow_ref")
		}
		if err := validateNodeReferences(cfg, "orchestration.workflow", cfg.Orchestration.Workflow.Nodes); err != nil {
			return err
		}
	}
	if _, graph := seen[executionmode.Graph]; graph {
		if cfg.Orchestration.GraphRef == "" {
			return newError(CodeInvalidConfig, "orchestration.graph_ref", "graph_ref is required for graph mode")
		}
		if err := validateGraphDefinition(cfg.Orchestration.Graph); err != nil {
			return err
		}
		if cfg.Orchestration.Graph.GraphID != cfg.Orchestration.GraphRef {
			return newError(CodeInvalidConfig, "orchestration.graph.graph_id", "graph_id must equal graph_ref")
		}
		if err := validateNodeReferences(cfg, "orchestration.graph", cfg.Orchestration.Graph.Nodes); err != nil {
			return err
		}
	}
	return nil
}

func validateRuntimePolicy(cfg AgentConfig) error {
	maxTurns := cfg.RuntimePolicy.MaxTurns
	if maxTurns < 0 {
		return newError(CodeInvalidConfig, "runtime.max_turns", "max_turns must be greater than or equal to 0")
	}
	if maxTurns <= maxEinoDeepAgentIterations || !supportsExecutionMode(cfg, executionmode.DeepAgent) || !mayUseEino(cfg.Runtime) {
		return nil
	}
	// Registry 负责把作者态 max_turns 编译为 Eino max_iterations，已知的硬上限应在发布前拒绝。
	return newError(CodeInvalidConfig, "runtime.max_turns", fmt.Sprintf("Eino deep_agent max_turns must not exceed %d", maxEinoDeepAgentIterations))
}

// validateContextConfig 校验 context 装配策略：history 仅接受空 / session /
// none（ADR-019）；非法值在 Build/发布期 fail-closed。
func validateContextConfig(cfg AgentConfig) error {
	switch cfg.Context.History {
	case "", ContextHistorySession, ContextHistoryNone:
	default:
		return newError(CodeInvalidConfig, "context.history", "must be one of: session, none")
	}
	switch cfg.ToolPolicy.Execution {
	case "", ToolExecutionSequential, ToolExecutionParallel:
		return nil
	default:
		return newError(CodeInvalidConfig, "tool_policy.execution", "must be one of: sequential, parallel")
	}
}

// validateExtensionsConfig 校验 agent 级扩展绑定声明：ID 非空、无控制字符、
// 列表内不重复。存在性校验不在本层做（Registry 不感知扩展目录），由
// Composition Root Build 预检与运行期 fail-closed 兜底。
// tool_call_interceptors 仅 eino 路径有挂载点：永远不会落到 eino 的 agent
// 声明该字段直接拒绝，避免静默失效。
func validateExtensionsConfig(cfg AgentConfig) error {
	lists := []struct {
		field string
		ids   []string
	}{
		{field: "extensions.before_model_hooks", ids: cfg.Extensions.BeforeModelHooks},
		{field: "extensions.tool_call_interceptors", ids: cfg.Extensions.ToolCallInterceptors},
		{field: "extensions.output_validators", ids: cfg.Extensions.OutputValidators},
	}
	for _, list := range lists {
		seen := make(map[string]struct{}, len(list.ids))
		for _, id := range list.ids {
			if strings.TrimSpace(id) == "" {
				return newError(CodeInvalidConfig, list.field, "extension id must not be empty")
			}
			if strings.IndexFunc(id, unicode.IsControl) >= 0 {
				return newError(CodeInvalidConfig, list.field, "extension id must not contain control characters")
			}
			if _, duplicate := seen[id]; duplicate {
				return newError(CodeInvalidConfig, list.field, "duplicate extension id: "+id)
			}
			seen[id] = struct{}{}
		}
	}
	if len(cfg.Extensions.ToolCallInterceptors) > 0 && !mayUseEino(cfg.Runtime) {
		return newError(CodeInvalidConfig, "extensions.tool_call_interceptors", "tool_call_interceptors require an eino-capable runtime (native runtime has no tool interception mount point)")
	}
	return nil
}

func supportsExecutionMode(cfg AgentConfig, target ExecutionMode) bool {
	if cfg.Orchestration.DefaultMode == target {
		return true
	}
	for _, mode := range cfg.Capability.ExecutionModes {
		if mode == target {
			return true
		}
	}
	return false
}

func mayUseEino(spec agentruntime.RuntimeSpec) bool {
	if spec.Type == agentruntime.RuntimeTypeEino || spec.Type == agentruntime.RuntimeTypeAuto || spec.Preferred == agentruntime.RuntimeTypeEino {
		return true
	}
	for _, candidate := range spec.Candidates {
		if candidate == agentruntime.RuntimeTypeEino {
			return true
		}
	}
	return false
}

func validateDirectRuntime(spec agentruntime.RuntimeSpec) error {
	if spec.Type != agentruntime.RuntimeTypeNative || spec.Mode != agentruntime.RuntimeModeDirect {
		return newError(CodeInvalidConfig, "runtime", "direct_action requires runtime.type=native and runtime.mode=direct")
	}
	if spec.Preferred != "" && spec.Preferred != agentruntime.RuntimeTypeNative {
		return newError(CodeInvalidConfig, "runtime.preferred", "direct_action only supports the native runtime")
	}
	for _, candidate := range spec.Candidates {
		if candidate != agentruntime.RuntimeTypeNative {
			return newError(CodeInvalidConfig, "runtime.candidates", "direct_action only supports the native runtime")
		}
	}
	return nil
}

func validateWorkflowDefinition(definition *agentruntime.WorkflowDefinition) error {
	if definition == nil {
		return newError(CodeInvalidConfig, "orchestration.workflow", "workflow definition is required")
	}
	return validateNodesAndEdges("orchestration.workflow", definition.EntryNode, definition.Nodes, definition.Edges)
}

func validateGraphDefinition(definition *agentruntime.GraphDefinition) error {
	if definition == nil {
		return newError(CodeInvalidConfig, "orchestration.graph", "graph definition is required")
	}
	if definition.StateSchemaRef == "" {
		return newError(CodeInvalidConfig, "orchestration.graph.state_schema_ref", "graph state schema is required")
	}
	return validateNodesAndEdges("orchestration.graph", definition.EntryNode, definition.Nodes, definition.Edges)
}

func validateNodesAndEdges(field, entryNode string, nodes []agentruntime.WorkflowNode, edges []agentruntime.WorkflowEdge) error {
	if entryNode == "" {
		return newError(CodeInvalidConfig, field+".entry_node", "entry node is required")
	}
	if len(nodes) == 0 {
		return newError(CodeInvalidConfig, field+".nodes", "at least one node is required")
	}
	known := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		if node.NodeID == "" {
			return newError(CodeInvalidConfig, field+".nodes.node_id", "node_id is required")
		}
		if node.NodeType == "" {
			return newError(CodeInvalidConfig, field+".nodes.node_type", "node_type is required")
		}
		if _, duplicate := known[node.NodeID]; duplicate {
			return newError(CodeInvalidConfig, field+".nodes.node_id", "duplicate node_id: "+node.NodeID)
		}
		known[node.NodeID] = struct{}{}
	}
	if _, ok := known[entryNode]; !ok {
		return newError(CodeInvalidConfig, field+".entry_node", "entry node does not exist: "+entryNode)
	}
	for _, edge := range edges {
		if _, ok := known[edge.FromNodeID]; !ok {
			return newError(CodeInvalidConfig, field+".edges.from_node_id", "edge source does not exist: "+edge.FromNodeID)
		}
		if _, ok := known[edge.ToNodeID]; !ok {
			return newError(CodeInvalidConfig, field+".edges.to_node_id", "edge target does not exist: "+edge.ToNodeID)
		}
	}
	return nil
}

func validateNodeReferences(cfg AgentConfig, field string, nodes []agentruntime.WorkflowNode) error {
	tools := make(map[string]struct{}, len(cfg.Tools))
	for _, tool := range cfg.Tools {
		tools[versionedDependency(tool.Name, tool.Version)] = struct{}{}
	}
	subAgents := make(map[string]struct{}, len(cfg.SubAgents))
	for _, agentID := range cfg.SubAgents {
		subAgents[agentID] = struct{}{}
	}
	for _, node := range nodes {
		if node.ToolRef != "" {
			if _, ok := tools[node.ToolRef]; !ok {
				return newError(CodeInvalidConfig, field+".nodes.tool_ref", "node "+node.NodeID+" references an undeclared tool: "+node.ToolRef)
			}
		}
		if node.AgentID != "" {
			if _, ok := subAgents[node.AgentID]; !ok {
				return newError(CodeInvalidConfig, field+".nodes.agent_id", "node "+node.NodeID+" references an undeclared sub-agent: "+node.AgentID)
			}
		}
	}
	return nil
}

func applyExecutionMode(cfg AgentConfig, definition agentruntime.AgentDefinition, mode ExecutionMode) (agentruntime.AgentDefinition, error) {
	if err := executionmode.Validate(mode); err != nil {
		return agentruntime.AgentDefinition{}, wrapError(CodeInvalidConfig, "execution_mode", "unsupported execution mode", err)
	}
	runtimeMode, err := executionmode.ToRuntimeMode(mode)
	if err != nil {
		return agentruntime.AgentDefinition{}, wrapError(CodeInvalidConfig, "execution_mode", "map execution mode", err)
	}
	if mode == executionmode.DirectAction {
		if err := validateDirectRuntime(cfg.Runtime); err != nil {
			return agentruntime.AgentDefinition{}, err
		}
		definition.Runtime = agentruntime.RuntimeSpec{Type: agentruntime.RuntimeTypeNative, Mode: agentruntime.RuntimeModeDirect}
	} else {
		definition.Runtime.Mode = runtimeMode
	}
	definition.Workflow = nil
	definition.Graph = nil
	switch mode {
	case executionmode.Workflow:
		definition.Workflow = cloneWorkflowDefinition(cfg.Orchestration.Workflow)
	case executionmode.Graph:
		definition.Graph = cloneGraphDefinition(cfg.Orchestration.Graph)
	}
	definition.RequiredCapabilities = requiredCapabilities(cfg, mode)
	return definition, nil
}
