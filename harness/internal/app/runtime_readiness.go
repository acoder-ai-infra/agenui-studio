package app

import (
	"context"
	"fmt"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/dispatcher"
)

type formalExecutorReadiness struct{ runtime *agentruntime.RuntimeService }

func (g formalExecutorReadiness) Check(ctx context.Context, req dispatcher.DispatchRunRequest) error {
	switch req.Policy.ExecutionMode {
	case dispatcher.ExecutionModeDirectAction:
		if req.Policy.HasSideEffect || req.Policy.ResumeRequired {
			return fmt.Errorf("%w: direct execution requires checkpoint/resume", dispatcher.ErrExecutorNotReady)
		}
		return nil
	case dispatcher.ExecutionModeSingleAgent, dispatcher.ExecutionModeDeepAgent:
		if g.runtime == nil || req.Run.Definition.Runtime.Type != agentruntime.RuntimeTypeEino {
			return fmt.Errorf("%w: %s requires the installed Eino runtime", dispatcher.ErrExecutorNotReady, req.Policy.ExecutionMode)
		}
		return nil
	case dispatcher.ExecutionModeWorkflow, dispatcher.ExecutionModeGraph:
		return fmt.Errorf("%w: %s", dispatcher.ErrExecutorNotReady, req.Policy.ExecutionMode)
	default:
		return req.Policy.ExecutionMode.Validate()
	}
}

var _ dispatcher.ExecutorReadinessGate = formalExecutorReadiness{}
