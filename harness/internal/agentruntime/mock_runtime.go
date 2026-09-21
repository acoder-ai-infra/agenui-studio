package agentruntime

import (
	"context"
	"errors"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type MockRuntime struct {
	NameValue     string
	Script        []observability.AgentEvent
	RunError      error
	Unhealthy     bool
	HealthReason  string
	DelayPerEvent time.Duration
}

func NewMockRuntime(events ...observability.AgentEvent) *MockRuntime {
	return &MockRuntime{NameValue: string(RuntimeTypeMock), Script: events}
}

func (r *MockRuntime) Name() string {
	if r.NameValue != "" {
		return r.NameValue
	}
	return string(RuntimeTypeMock)
}

func (r *MockRuntime) Descriptor(ctx context.Context) RuntimeDescriptor {
	return RuntimeDescriptor{
		Name:           RuntimeType(r.Name()),
		RuntimeVersion: "test",
		AdapterVersion: "test-v1",
		Governance:     RuntimeGovernanceManaged,
		Capabilities:   r.Capabilities(ctx),
	}
}

func (r *MockRuntime) Capabilities(context.Context) RuntimeCapabilities {
	return RuntimeCapabilities{
		Streaming:        true,
		Resume:           true,
		ToolCall:         true,
		ParallelToolCall: true,
		SubAgent:         true,
		Checkpoint:       true,
		ControlRequest:   true,
		Workflow:         true,
		DeepAgent:        true,
		Artifact:         true,
		Memory:           true,
		Cancellation:     true,
		A2A:              true,
		MCP:              true,
	}
}

func (r *MockRuntime) ValidateConfig(_ context.Context, def AgentDefinition) error {
	if def.AgentID == "" {
		return errors.New("agent_id required")
	}
	if def.Version == "" {
		return errors.New("agent version required")
	}
	switch def.Runtime.Mode {
	case "", RuntimeModeDirect, RuntimeModeReact, RuntimeModeDeepAgent, RuntimeModePlanExecute:
		return nil
	case RuntimeModeWorkflow:
		if def.Workflow == nil {
			return errors.New("workflow definition required")
		}
		if def.Workflow.EntryNode == "" {
			return errors.New("workflow entry_node required")
		}
		if len(def.Workflow.Nodes) == 0 {
			return errors.New("workflow nodes required")
		}
		return nil
	case RuntimeModeGraph:
		if def.Graph == nil {
			return errors.New("graph definition required")
		}
		if def.Graph.EntryNode == "" {
			return errors.New("graph entry_node required")
		}
		if len(def.Graph.Nodes) == 0 {
			return errors.New("graph nodes required")
		}
		return nil
	default:
		return errors.New("unsupported runtime mode")
	}
}

func (r *MockRuntime) Build(ctx context.Context, def AgentDefinition) (AgentHandle, error) {
	if err := r.ValidateConfig(ctx, def); err != nil {
		return AgentHandle{}, err
	}
	binding, err := newRuntimeBinding(ctx, r, def, runtimeBindingFacts{})
	if err != nil {
		return AgentHandle{}, err
	}
	return AgentHandle{Definition: def, Runtime: RuntimeType(r.Name()), Binding: binding, BuiltAt: time.Now()}, nil
}

func (r *MockRuntime) Run(ctx context.Context, _ RunRequest) (<-chan observability.AgentEvent, error) {
	if r.RunError != nil {
		return nil, r.RunError
	}
	out := make(chan observability.AgentEvent, len(r.Script))
	go func() {
		defer close(out)
		for _, event := range r.Script {
			if r.DelayPerEvent > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(r.DelayPerEvent):
				}
			}
			select {
			case <-ctx.Done():
				return
			case out <- event:
			}
		}
	}()
	return out, nil
}

func (r *MockRuntime) Resume(ctx context.Context, req ResumeRequest) (<-chan observability.AgentEvent, error) {
	return r.Run(ctx, RunRequest{SessionID: req.SessionID, RunID: req.RunID, Definition: req.Definition, Trace: req.Trace})
}

func (r *MockRuntime) Cancel(context.Context, CancelRequest) error {
	return nil
}

func (r *MockRuntime) Health(context.Context) RuntimeHealth {
	return RuntimeHealth{Available: !r.Unhealthy, Reason: r.HealthReason, CheckedAt: time.Now()}
}

func (r *MockRuntime) Shutdown(context.Context) error {
	return nil
}
