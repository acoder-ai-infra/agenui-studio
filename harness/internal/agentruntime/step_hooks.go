package agentruntime

import (
	"context"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func (s *RuntimeService) startStepWithHooks(ctx context.Context, req RunRequest, start StepStart, handle *AgentHandle) (context.Context, RunRequest, *StepSnapshot, []observability.AgentEvent, error) {
	if start.StepID == "" {
		start.StepID = string(start.Kind) + "_" + s.IDs.NewRequestID()
	}
	stepContext := &RuntimeHookStep{
		StepID:       start.StepID,
		Kind:         start.Kind,
		Name:         start.Name,
		ParentStepID: start.ParentStepID,
		Status:       StepStatusCreated,
	}
	ctx, req, events, err := s.executeHooksWithContext(ctx, req, HookBeforeStep, handle, nil, nil, stepContext, nil, nil)
	if err != nil {
		return ctx, req, nil, events, err
	}
	step, started, err := s.startStep(ctx, req, start)
	if err != nil {
		return ctx, req, nil, events, err
	}
	events = append(events, started)
	return ctx, req, step, events, nil
}

func (s *RuntimeService) completeStepWithHooks(ctx context.Context, req RunRequest, step *StepSnapshot, kind StepKind, name string, handle *AgentHandle) (context.Context, RunRequest, []observability.AgentEvent, error) {
	completed, err := s.completeStep(ctx, req, step, kind)
	if err != nil {
		return ctx, req, nil, err
	}
	events := []observability.AgentEvent{completed}
	stepContext := &RuntimeHookStep{StepID: step.StepID, Kind: kind, Name: name, Status: StepStatusCompleted}
	ctx, req, hookEvents, err := s.executeHooksWithContext(ctx, req, HookAfterStep, handle, nil, nil, stepContext, nil, nil)
	events = append(events, hookEvents...)
	return ctx, req, events, err
}

func (s *RuntimeService) failStepWithHooks(ctx context.Context, req RunRequest, step *StepSnapshot, kind StepKind, name string, handle *AgentHandle, stepErr error) (context.Context, RunRequest, []observability.AgentEvent, error) {
	failed, err := s.failStep(ctx, req, step, kind, stepErr)
	if err != nil {
		return ctx, req, nil, err
	}
	runtimeErr := s.classifyError(stepErr, errorStageForStep(kind))
	events := []observability.AgentEvent{failed}
	stepContext := &RuntimeHookStep{StepID: step.StepID, Kind: kind, Name: name, Status: StepStatusFailed, Error: runtimeErr}
	ctx, req, hookEvents, hookErr := s.executeHooksWithContext(ctx, req, HookAfterStep, handle, nil, runtimeErr, stepContext, nil, nil)
	events = append(events, hookEvents...)
	if hookErr != nil {
		return ctx, req, events, hookErr
	}
	return ctx, req, events, nil
}

func (s *RuntimeService) cancelStepWithHooks(ctx context.Context, req RunRequest, step *StepSnapshot, kind StepKind, name string, handle *AgentHandle) (context.Context, RunRequest, []observability.AgentEvent, error) {
	cancelled, err := s.cancelStep(ctx, req, step, kind)
	if err != nil {
		return ctx, req, nil, err
	}
	events := []observability.AgentEvent{cancelled}
	stepContext := &RuntimeHookStep{StepID: step.StepID, Kind: kind, Name: name, Status: StepStatusCancelled}
	ctx, req, hookEvents, hookErr := s.executeHooksWithContext(ctx, req, HookAfterStep, handle, nil, nil, stepContext, nil, nil)
	events = append(events, hookEvents...)
	if hookErr != nil {
		return ctx, req, events, hookErr
	}
	return ctx, req, events, nil
}

func (s *RuntimeService) executeErrorHooks(ctx context.Context, req RunRequest, handle *AgentHandle, err error) (context.Context, RunRequest, []observability.AgentEvent, error) {
	runtimeErr := s.classifyError(err, ErrorStageRuntimeAdapter)
	return s.executeHooksWithContext(ctx, req, HookOnError, handle, nil, runtimeErr, nil, nil, nil)
}

func (s *RuntimeService) executeInterruptHooks(ctx context.Context, req RunRequest, handle *AgentHandle, interrupt RuntimeHookInterrupt) (context.Context, RunRequest, []observability.AgentEvent, error) {
	return s.executeHooksWithContext(ctx, req, HookOnInterrupt, handle, nil, nil, nil, &interrupt, nil)
}
