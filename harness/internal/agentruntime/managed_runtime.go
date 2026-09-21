package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type RuntimeGovernanceMode string

const (
	RuntimeGovernanceManaged RuntimeGovernanceMode = "managed"
	RuntimeGovernanceBridged RuntimeGovernanceMode = "bridged"
	RuntimeGovernanceOpaque  RuntimeGovernanceMode = "opaque"
)

type RuntimeDependency string

type SubAgentExecutionScope string

const (
	RuntimeDependencyModel      RuntimeDependency = "model_gateway"
	RuntimeDependencyTool       RuntimeDependency = "tool_gateway"
	RuntimeDependencySubAgent   RuntimeDependency = "agent_gateway"
	RuntimeDependencyCheckpoint RuntimeDependency = "checkpoint_store"

	// SubAgentScopePlatformChildRun identifies a registered Harness Agent. It
	// must be invoked through Agent Gateway and owns an independent child Run.
	SubAgentScopePlatformChildRun SubAgentExecutionScope = "platform_child_run"
	// SubAgentScopeRuntimeInternal identifies a native runtime implementation
	// detail such as a planner or replanner. It stays inside the parent Run and
	// may not own an independent session, permission identity, or A2A endpoint.
	SubAgentScopeRuntimeInternal SubAgentExecutionScope = "runtime_internal"
)

var ErrRuntimeEnvironmentDependencyMissing = errors.New("runtime environment dependency missing")

var ErrRuntimeCapabilityMissing = errors.New("runtime required capability missing")

// ErrNestedPlatformSubAgentForbidden protects the platform contract that a
// child Run is an execution leaf. Runtime-internal helpers remain allowed
// inside that child Run, but another platform child Run is not.
var ErrNestedPlatformSubAgentForbidden = errors.New("nested platform sub-agent delegation forbidden")

type RuntimeDescriptor struct {
	Name             RuntimeType           `json:"name"`
	RuntimeVersion   string                `json:"runtime_version,omitempty"`
	AdapterVersion   string                `json:"adapter_version"`
	Governance       RuntimeGovernanceMode `json:"governance"`
	CheckpointFormat string                `json:"checkpoint_format,omitempty"`
	Capabilities     RuntimeCapabilities   `json:"capabilities"`
}

func (c RuntimeCapabilities) Supports(capability string) bool {
	switch capability {
	case "streaming":
		return c.Streaming
	case "resume":
		return c.Resume
	case "checkpoint":
		return c.Checkpoint
	case "control_request":
		return c.ControlRequest
	case "tool_call":
		return c.ToolCall
	case "parallel_tool_call":
		return c.ParallelToolCall
	case "sub_agent":
		return c.SubAgent
	case "workflow":
		return c.Workflow
	case "deep_agent":
		return c.DeepAgent
	case "artifact":
		return c.Artifact
	case "memory":
		return c.Memory
	case "cancellation":
		return c.Cancellation
	case "a2a":
		return c.A2A
	case "mcp":
		return c.MCP
	default:
		return false
	}
}

func validateRequiredRuntimeCapabilities(required []string, capabilities RuntimeCapabilities) error {
	for _, capability := range required {
		if !capabilities.Supports(capability) {
			return fmt.Errorf("%w: capability=%s", ErrRuntimeCapabilityMissing, capability)
		}
	}
	return nil
}

// RuntimeEnvironment contains the only outbound execution dependencies a
// managed runtime adapter may use. MCP and Skill capabilities are materialized
// as governed tools and therefore execute through Tools as well.
type RuntimeEnvironment struct {
	// Models is the raw Model Gateway port. Runtime adapters must obtain the
	// callable dependency through GovernedModelInvoker so final-input governance
	// cannot be skipped when a new adapter is introduced.
	Models      ModelInvoker
	ModelInputs ModelInputGovernor
	// PreModelCompactor is the portable Stage-B implementation supplied by the
	// Context composition root. Native adapters may compile the same policy to
	// native middleware instead of calling this port directly.
	PreModelCompactor RuntimePreModelCompactor
	// ContextCompactors is the process-level provider used by Runtime factories
	// to resolve PreModelCompactor from the frozen package policy.
	ContextCompactors ContextCompactorProvider
	Tools             ToolInvoker
	SubAgents         SubAgentInvoker
	Checkpoints       RuntimeCheckpointStoreResolver
}

func (e RuntimeEnvironment) GovernedModelInvoker() ModelInvoker {
	return NewGovernedModelInvoker(e.Models, e.ModelInputs)
}

func (e RuntimeEnvironment) ContextManagedModelInvoker() ModelInvoker {
	return NewContextManagedModelInvoker(e.Models, e.PreModelCompactor, e.ModelInputs)
}

func (e RuntimeEnvironment) ResolvePreModelCompactor(ctx context.Context, policy ContextCompactionPolicy, descriptor RuntimeDescriptor) (RuntimePreModelCompactor, error) {
	normalized, err := NormalizeContextCompactionPolicy(policy)
	if err != nil {
		return nil, err
	}
	if e.ContextCompactors != nil {
		compactor, resolveErr := e.ContextCompactors.Resolve(ctx, normalized, descriptor)
		if resolveErr != nil {
			return nil, resolveErr
		}
		return validateResolvedPreModelCompactor(compactor, normalized)
	}
	if e.PreModelCompactor != nil {
		return validateResolvedPreModelCompactor(e.PreModelCompactor, normalized)
	}
	return NewDefaultContextCompactorProvider(nil).Resolve(ctx, normalized, descriptor)
}

func validateResolvedPreModelCompactor(compactor RuntimePreModelCompactor, policy ContextCompactionPolicy) (RuntimePreModelCompactor, error) {
	if compactor == nil {
		if policy.SemanticSummary == SemanticSummaryRequired {
			return nil, ErrRuntimePreModelCompactorMissing
		}
		return nil, nil
	}
	if readiness, ok := compactor.(RuntimePreModelCompactorReadiness); ok {
		if err := readiness.ValidatePolicy(policy); err != nil {
			return nil, err
		}
	}
	return compactor, nil
}

type RuntimeEnvironmentRequirements struct {
	Dependencies []RuntimeDependency `json:"dependencies"`
}

func (e RuntimeEnvironment) Validate(requirements RuntimeEnvironmentRequirements) error {
	for _, dependency := range uniqueRuntimeDependencies(requirements.Dependencies) {
		var available bool
		switch dependency {
		case RuntimeDependencyModel:
			available = IsGovernedModelInvoker(e.Models)
		case RuntimeDependencyTool:
			available = e.Tools != nil
		case RuntimeDependencySubAgent:
			available = e.SubAgents != nil
		case RuntimeDependencyCheckpoint:
			available = e.Checkpoints != nil
		default:
			return fmt.Errorf("%w: unsupported dependency=%s", ErrRuntimeEnvironmentDependencyMissing, dependency)
		}
		if !available {
			return fmt.Errorf("%w: dependency=%s", ErrRuntimeEnvironmentDependencyMissing, dependency)
		}
	}
	return nil
}

// SubAgentInvoker is the Harness Agent Gateway port for platform_child_run
// agents. Runtime-internal agents are assembled and executed natively by the
// runtime adapter and must not be sent through this port.
type SubAgentInvoker interface {
	Invoke(ctx context.Context, req SubAgentInvocationRequest, events SubAgentEventSink) (SubAgentInvocationResult, error)
}

type SubAgentEventSink interface {
	Emit(ctx context.Context, event observability.AgentEvent) error
}

type SubAgentInvocationRequest struct {
	Trace                   observability.TraceContext `json:"trace"`
	Scope                   SubAgentExecutionScope     `json:"scope"`
	TenantID                string                     `json:"tenant_id,omitempty"`
	SessionID               string                     `json:"session_id"`
	ParentRunID             string                     `json:"parent_run_id"`
	ParentAgentID           string                     `json:"parent_agent_id"`
	ParentAgentVersion      string                     `json:"parent_agent_version"`
	ParentConfigSnapshotRef string                     `json:"parent_config_snapshot_ref"`
	ParentConfigHash        string                     `json:"parent_config_hash"`
	SubAgentRef             string                     `json:"sub_agent_ref"`
	TaskID                  string                     `json:"task_id"`
	Description             string                     `json:"description"`
	// InputParts are explicitly inherited from the current parent turn.
	// Artifact bytes stay in Artifact Store; child Runs receive references only.
	InputParts []contextpkg.ContentPart `json:"input_parts,omitempty"`
	ScopedData ScopedData               `json:"scoped_data,omitempty"`
	Depth      int                      `json:"depth,omitempty"`
}

type SubAgentInvocationResult struct {
	ChildRunID string `json:"child_run_id"`
	Content    string `json:"content,omitempty"`
	ContentRef string `json:"content_ref,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
	// ResultType 区分结果形态：空/"content" 表示常规完成；"ask_user" 表示
	// 子 Agent 在等待用户输入，本次 attempt 以 InteractionProposal 上交结束。
	ResultType string `json:"result_type,omitempty"`
	// Proposal 在 ResultType 为 "ask_user" 时携带子 Agent 的交互请求
	//（prompt/kind 等业务字段的 JSON 原文，形状与 control payload 对齐）。
	Proposal json.RawMessage `json:"proposal,omitempty"`
	// ChildControl 在 ResultType 为 "ask_user" 时携带恢复子 Run 所需的
	// control 绑定（child 自己的 control 面只被 Agent Gateway 内部消费）。
	ChildControl *SubAgentChildControl `json:"child_control,omitempty"`
}

// SubAgentResultAskUserType 是 SubAgentInvocationResult.ResultType 的
// ask_user 取值。
const SubAgentResultAskUserType = "ask_user"

// SubAgentChildControl 是子 Run 的 control 恢复绑定。ControlTicket 是 SDK
// 同款 codec 密文；Agent Gateway 凭它走 canonical Resume 正门恢复子 Run。
type SubAgentChildControl struct {
	ChildRunID    string `json:"child_run_id"`
	RequestID     string `json:"request_id"`
	CheckpointID  string `json:"checkpoint_id"`
	ControlTicket string `json:"control_ticket"`
}

// SubAgentResumeRequest 是恢复处于 ask_user 等待中的子 Run 的入参。
type SubAgentResumeRequest struct {
	Trace           observability.TraceContext `json:"trace"`
	TenantID        string                     `json:"tenant_id,omitempty"`
	SessionID       string                     `json:"session_id"`
	ParentRunID     string                     `json:"parent_run_id"`
	SubAgentRef     string                     `json:"sub_agent_ref"`
	TaskID          string                     `json:"task_id"`
	Control         SubAgentChildControl       `json:"control"`
	ResponsePayload json.RawMessage            `json:"response_payload,omitempty"`
}

// SubAgentResumer 是 Agent Gateway 的可选恢复端口：把父 Run 收到的用户
// 答复路由回等待中的子 Run。实现方（agentgateway.Service）按 canonical
// Resume 链恢复子 Run 并等待其终态。
type SubAgentResumer interface {
	ResumeChild(ctx context.Context, req SubAgentResumeRequest, events SubAgentEventSink) (SubAgentInvocationResult, error)
}

func managedRuntimeRequirements(def AgentDefinition, pkg ModelContextPackage) RuntimeEnvironmentRequirements {
	requirements := RuntimeEnvironmentRequirements{Dependencies: []RuntimeDependency{RuntimeDependencyCheckpoint}}
	switch def.Runtime.Mode {
	case RuntimeModeReact, RuntimeModeDeepAgent, RuntimeModePlanExecute:
		requirements.Dependencies = append(requirements.Dependencies, RuntimeDependencyModel)
	}
	if len(pkg.Capabilities.Tools) > 0 || len(pkg.Capabilities.MCPServers) > 0 || len(pkg.Capabilities.Skills) > 0 {
		requirements.Dependencies = append(requirements.Dependencies, RuntimeDependencyTool)
	}
	if len(pkg.Capabilities.SubAgents) > 0 {
		requirements.Dependencies = append(requirements.Dependencies, RuntimeDependencySubAgent)
	}
	return requirements
}

func uniqueRuntimeDependencies(input []RuntimeDependency) []RuntimeDependency {
	seen := make(map[RuntimeDependency]struct{}, len(input))
	result := make([]RuntimeDependency, 0, len(input))
	for _, dependency := range input {
		if _, ok := seen[dependency]; ok {
			continue
		}
		seen[dependency] = struct{}{}
		result = append(result, dependency)
	}
	return result
}
