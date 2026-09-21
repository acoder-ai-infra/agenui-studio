package executionmode

import (
	"errors"
	"fmt"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
)

var ErrUnsupported = errors.New("execution mode unsupported")

// Mode 是 Agent Binding、Registry、Orchestrator 与 Dispatcher 共享的执行范式。
// RuntimeMode 描述具体执行器的数据传递方式，两者必须通过显式映射衔接。
type Mode string

const (
	DirectAction Mode = "direct_action"
	SingleAgent  Mode = "single_agent"
	DeepAgent    Mode = "deep_agent"
	Workflow     Mode = "workflow"
	Graph        Mode = "graph"
)

func (m Mode) Validate() error {
	switch m {
	case DirectAction, SingleAgent, DeepAgent, Workflow, Graph:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrUnsupported, m)
	}
}

func Validate(mode Mode) error {
	return mode.Validate()
}

func ToRuntimeMode(mode Mode) (agentruntime.RuntimeMode, error) {
	if err := mode.Validate(); err != nil {
		return "", err
	}
	switch mode {
	case DirectAction:
		return agentruntime.RuntimeModeDirect, nil
	case SingleAgent:
		return agentruntime.RuntimeModeReact, nil
	case DeepAgent:
		return agentruntime.RuntimeModeDeepAgent, nil
	case Workflow:
		return agentruntime.RuntimeModeWorkflow, nil
	case Graph:
		return agentruntime.RuntimeModeGraph, nil
	default:
		panic("validated execution mode is not mapped")
	}
}

func FromRuntimeMode(mode agentruntime.RuntimeMode) (Mode, error) {
	switch mode {
	case agentruntime.RuntimeModeDirect:
		return DirectAction, nil
	case agentruntime.RuntimeModeReact:
		return SingleAgent, nil
	case agentruntime.RuntimeModeDeepAgent:
		return DeepAgent, nil
	case agentruntime.RuntimeModeWorkflow:
		return Workflow, nil
	case agentruntime.RuntimeModeGraph:
		return Graph, nil
	default:
		return "", fmt.Errorf("%w: runtime mode %q", ErrUnsupported, mode)
	}
}
