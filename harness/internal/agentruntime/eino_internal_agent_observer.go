package agentruntime

import (
	"context"
	"errors"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/compose"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

var ErrEinoInternalAgentMissing = errors.New("eino runtime-internal agent missing")

// EinoInternalAgentDecoratorPort is the only supported assembly path for
// runtime-internal Eino agents that need nested Step observability.
type EinoInternalAgentDecoratorPort interface {
	Wrap(agent adk.ResumableAgent) (adk.ResumableAgent, error)
}

// EinoInternalAgentDecorator is stateless and safe to share across concurrent
// agent builds.
type EinoInternalAgentDecorator struct{}

var _ EinoInternalAgentDecoratorPort = EinoInternalAgentDecorator{}

func (EinoInternalAgentDecorator) Wrap(agent adk.ResumableAgent) (adk.ResumableAgent, error) {
	if agent == nil {
		return nil, ErrEinoInternalAgentMissing
	}
	if _, ok := agent.(*EinoInternalAgentObserver); ok {
		return agent, nil
	}
	return NewEinoInternalAgentObserver(agent)
}

// EinoInternalAgentObserver decorates a runtime-internal Eino agent with
// canonical nested Step lifecycle events. It does not turn the native helper
// into a registered platform Agent or route it through Agent Gateway.
type EinoInternalAgentObserver struct {
	agent adk.ResumableAgent
}

var _ adk.ResumableAgent = (*EinoInternalAgentObserver)(nil)

func NewEinoInternalAgentObserver(agent adk.ResumableAgent) (*EinoInternalAgentObserver, error) {
	if agent == nil {
		return nil, ErrEinoInternalAgentMissing
	}
	return &EinoInternalAgentObserver{agent: agent}, nil
}

func (a *EinoInternalAgentObserver) Name(ctx context.Context) string {
	if a == nil || a.agent == nil {
		return ""
	}
	return a.agent.Name(ctx)
}

func (a *EinoInternalAgentObserver) Description(ctx context.Context) string {
	if a == nil || a.agent == nil {
		return ""
	}
	return a.agent.Description(ctx)
}

func (a *EinoInternalAgentObserver) Run(ctx context.Context, input *adk.AgentInput, options ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	return a.observe(ctx, func() *adk.AsyncIterator[*adk.AgentEvent] {
		return a.agent.Run(ctx, input, options...)
	})
}

func (a *EinoInternalAgentObserver) Resume(ctx context.Context, info *adk.ResumeInfo, options ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	return a.observe(ctx, func() *adk.AsyncIterator[*adk.AgentEvent] {
		return a.agent.Resume(ctx, info, options...)
	})
}

func (a *EinoInternalAgentObserver) observe(ctx context.Context, start func() *adk.AsyncIterator[*adk.AgentEvent]) *adk.AsyncIterator[*adk.AgentEvent] {
	iterator, generator := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	emitter, ok := RuntimeEventEmitterFrom(ctx)
	if !ok {
		generator.Send(&adk.AgentEvent{Err: ErrRuntimeEventEmitterMissing})
		generator.Close()
		return iterator
	}
	name := a.Name(ctx)
	stepID := einoInternalAgentStepID(ctx, name)
	parentStepID := RuntimeParentStepIDFrom(ctx)
	metadata := map[string]string{
		"scope": string(SubAgentScopeRuntimeInternal),
	}
	if address := compose.GetCurrentAddress(ctx).String(); address != "" {
		metadata["runtime_address"] = address
	}
	if toolCallID := compose.GetToolCallID(ctx); toolCallID != "" {
		metadata["tool_call_id"] = toolCallID
	}
	if err := emitter.Emit(ctx, runtimeInternalAgentStepEvent(EventRuntimeStepStarted, stepID, parentStepID, name, metadata, nil)); err != nil {
		generator.Send(&adk.AgentEvent{Err: err})
		generator.Close()
		return iterator
	}
	source := start()
	if source == nil {
		err := errors.New("eino runtime-internal agent returned nil iterator")
		_ = emitter.Emit(ctx, runtimeInternalAgentStepEvent(EventRuntimeStepFailed, stepID, parentStepID, name, metadata, err))
		generator.Send(&adk.AgentEvent{Err: err})
		generator.Close()
		return iterator
	}
	go func() {
		defer generator.Close()
		interrupted := false
		var executionErr error
		for {
			event, exists := source.Next()
			if !exists {
				break
			}
			if event != nil {
				if event.Err != nil && executionErr == nil {
					executionErr = event.Err
				}
				if event.Action != nil && event.Action.Interrupted != nil {
					interrupted = true
				}
			}
			generator.Send(event)
		}
		if interrupted {
			return
		}
		if executionErr == nil && ctx.Err() != nil {
			executionErr = ctx.Err()
		}
		terminal := EventRuntimeStepCompleted
		if errors.Is(executionErr, context.Canceled) {
			terminal = EventRuntimeStepCancelled
		} else if executionErr != nil {
			terminal = EventRuntimeStepFailed
		}
		if err := emitter.Emit(context.WithoutCancel(ctx), runtimeInternalAgentStepEvent(terminal, stepID, parentStepID, name, metadata, executionErr)); err != nil {
			generator.Send(&adk.AgentEvent{Err: err})
		}
	}()
	return iterator
}

func einoInternalAgentStepID(ctx context.Context, name string) string {
	tc := observability.MustTraceContext(ctx)
	invocation := compose.GetToolCallID(ctx)
	if invocation == "" {
		invocation = compose.GetCurrentAddress(ctx).String()
	}
	if invocation == "" {
		invocation = tc.RequestID
	}
	if invocation == "" {
		invocation = "root"
	}
	return "runtime_internal_agent_" + stableRuntimeFactID(tc.RunID, name+"\x00"+invocation)
}

func runtimeInternalAgentStepEvent(eventType observability.EventType, stepID, parentStepID, name string, metadata map[string]string, stepErr error) observability.AgentEvent {
	status := StepStatusRunning
	switch eventType {
	case EventRuntimeStepCompleted:
		status = StepStatusCompleted
	case EventRuntimeStepFailed:
		status = StepStatusFailed
	case EventRuntimeStepCancelled:
		status = StepStatusCancelled
	}
	event := observability.AgentEvent{
		StepID:     stepID,
		EventType:  eventType,
		Visibility: observability.VisibilityDebug,
		Payload: JSONPayload(map[string]any{
			"step_id":        stepID,
			"kind":           StepKindRuntimeInternalAgent,
			"name":           name,
			"parent_step_id": parentStepID,
			"metadata":       metadata,
			"status":         status,
		}),
	}
	if stepErr != nil {
		runtimeErr := NewRuntimeError(ErrorRuntime, "EINO_INTERNAL_AGENT_FAILED", "runtime-internal agent failed").WithCause(stepErr)
		event.Error = runtimeErr.EventError()
		event.Payload = JSONPayload(map[string]any{
			"step_id":        stepID,
			"kind":           StepKindRuntimeInternalAgent,
			"name":           name,
			"parent_step_id": parentStepID,
			"metadata":       metadata,
			"status":         status,
			"error":          runtimeErr.Payload(),
		})
	}
	return event
}
