package dispatcher

import (
	"context"
	"errors"
	"fmt"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/scheduler"
)

var (
	ErrRuntimeRunnerMissing        = errors.New("runtime runner missing")
	ErrDispatchRequestMismatch     = errors.New("dispatch and frozen run request mismatch")
	ErrRuntimeEventStreamMissing   = errors.New("runtime event stream missing")
	ErrRuntimeEventProtocolInvalid = errors.New("runtime event protocol invalid")
	ErrRuntimeTerminalEventMissing = errors.New("runtime terminal event missing")
)

type RuntimeRunner interface {
	Run(ctx context.Context, req agentruntime.RunRequest) (<-chan observability.AgentEvent, error)
}

// NewRuntimeDispatchHandler adapts a leased scheduler dispatch back into the
// one canonical Runtime.Run entrypoint.
func NewRuntimeDispatchHandler(store RunRequestStore, runtime RuntimeRunner) scheduler.DispatchHandler {
	return func(ctx context.Context, dispatch scheduler.RunDispatch) error {
		if store == nil {
			return scheduler.HandlerError{Type: scheduler.ErrorTypeUnknown, Err: ErrRunRequestStoreMissing}
		}
		if runtime == nil {
			return scheduler.HandlerError{Type: scheduler.ErrorTypeUnknown, Err: ErrRuntimeRunnerMissing}
		}
		frozen, err := store.Get(ctx, dispatch.RequestRef)
		if err != nil {
			return scheduler.HandlerError{Type: scheduler.ErrorTypeUnknown, Err: err}
		}
		if !dispatchMatchesFrozenRequest(dispatch, frozen) {
			return scheduler.HandlerError{Type: scheduler.ErrorTypePermissionDenied, Err: ErrDispatchRequestMismatch}
		}

		// durable store 的返回值也不能直接交给 Runtime；执行副本允许 Runtime 内部修改，
		// 但冻结事实必须保持不可变，供重试与审计继续使用。
		req, err := cloneRunRequest(frozen)
		if err != nil {
			return scheduler.HandlerError{Type: scheduler.ErrorTypeGuardrailBlocked, Err: err}
		}
		if delivery, ok := observability.TraceContextFrom(ctx); ok && delivery.TraceID == req.Trace.TraceID {
			req.Trace = mergeDeliveryTrace(req.Trace, delivery)
		}

		events, err := runtime.Run(ctx, req)
		if err != nil {
			return scheduler.HandlerError{Type: scheduler.ErrorTypeUnknown, Err: err}
		}
		if events == nil {
			return scheduler.HandlerError{Type: scheduler.ErrorTypeUnknown, Err: ErrRuntimeEventStreamMissing}
		}
		return observeRuntimeEvents(events, req)
	}
}

func dispatchMatchesFrozenRequest(dispatch scheduler.RunDispatch, req agentruntime.RunRequest) bool {
	if req.RunID != dispatch.RunID || req.SessionID != dispatch.SessionID || req.Definition.AgentID != dispatch.AgentID {
		return false
	}
	expected := stableRunTrace(req)
	actual := dispatch.Trace
	return actual.TraceID == expected.TraceID &&
		actual.SessionID == expected.SessionID && actual.RunID == expected.RunID &&
		actual.UserID == expected.UserID && actual.TenantID == expected.TenantID &&
		actual.AgentID == expected.AgentID && actual.AgentType == expected.AgentType &&
		actual.AgentVersion == expected.AgentVersion && actual.Runtime == expected.Runtime
}

func observeRuntimeEvents(events <-chan observability.AgentEvent, req agentruntime.RunRequest) error {
	var (
		terminal     *observability.AgentEvent
		controlCount int
		protocolErr  error
	)
	for event := range events {
		if err := validateRuntimeEvent(event, req); err != nil {
			protocolErr = errors.Join(protocolErr, err)
			continue
		}
		switch event.EventType {
		case observability.EventControlRequestCreated:
			controlCount++
			if controlCount > 1 {
				protocolErr = errors.Join(protocolErr, fmt.Errorf("%w: multiple control requests", ErrRuntimeEventProtocolInvalid))
			}
		case observability.EventRunCompleted, observability.EventRunFailed,
			observability.EventRunCancelled, observability.EventRunExpired:
			if terminal != nil {
				protocolErr = errors.Join(protocolErr, fmt.Errorf("%w: multiple terminal events", ErrRuntimeEventProtocolInvalid))
				continue
			}
			current := event
			terminal = &current
		}
	}
	if controlCount > 0 && terminal != nil {
		protocolErr = errors.Join(protocolErr, fmt.Errorf("%w: control and terminal events are mutually exclusive", ErrRuntimeEventProtocolInvalid))
	}
	if protocolErr != nil {
		return scheduler.HandlerError{Type: scheduler.ErrorTypeGuardrailBlocked, Err: protocolErr}
	}
	if controlCount == 1 {
		return nil
	}
	if terminal == nil {
		return scheduler.HandlerError{Type: scheduler.ErrorTypeUnknown, Err: ErrRuntimeTerminalEventMissing}
	}

	switch terminal.EventType {
	case observability.EventRunCompleted:
		return nil
	case observability.EventRunCancelled:
		return scheduler.HandlerError{Type: scheduler.ErrorTypeCancelled, Message: "runtime run cancelled"}
	case observability.EventRunExpired:
		return scheduler.HandlerError{Type: scheduler.ErrorTypeTimeout, Message: "runtime run expired"}
	case observability.EventRunFailed:
		return scheduler.HandlerError{Type: schedulerErrorType(terminal.Error), Message: safeRuntimeFailureMessage(terminal.Error)}
	default:
		return scheduler.HandlerError{Type: scheduler.ErrorTypeUnknown, Err: ErrRuntimeTerminalEventMissing}
	}
}

func validateRuntimeEvent(event observability.AgentEvent, req agentruntime.RunRequest) error {
	if event.SchemaVersion != observability.AgentEventSchemaVersion || event.EventID == "" ||
		event.TraceID != req.Trace.TraceID || event.SessionID != req.SessionID || event.RunID != req.RunID ||
		event.AgentID != req.Definition.AgentID || event.AgentType != req.Definition.AgentType ||
		!isAllowedRuntime(req.Definition.Runtime, event.Runtime) {
		return fmt.Errorf("%w: event identity mismatch", ErrRuntimeEventProtocolInvalid)
	}
	return nil
}

func mergeDeliveryTrace(stable, delivery observability.TraceContext) observability.TraceContext {
	stable.RootSpanID = delivery.RootSpanID
	stable.SpanID = delivery.SpanID
	stable.ParentSpanID = delivery.ParentSpanID
	stable.ConversationID = delivery.ConversationID
	stable.RequestID = delivery.RequestID
	stable.Channel = delivery.Channel
	stable.Protocol = delivery.Protocol
	stable.Source = delivery.Source
	stable.Sampled = delivery.Sampled
	stable.DebugEnabled = delivery.DebugEnabled
	stable.Baggage = cloneTraceBaggage(delivery.Baggage)
	return stable
}

func cloneTraceBaggage(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func isAllowedRuntime(spec agentruntime.RuntimeSpec, runtime string) bool {
	actual := agentruntime.RuntimeType(runtime)
	if actual == "" || actual == agentruntime.RuntimeTypeAuto {
		return false
	}
	declaredConcrete := false
	if spec.Type != "" && spec.Type != agentruntime.RuntimeTypeAuto {
		declaredConcrete = true
		if actual == spec.Type {
			return true
		}
	}
	if spec.Preferred != "" && spec.Preferred != agentruntime.RuntimeTypeAuto {
		declaredConcrete = true
		if actual == spec.Preferred {
			return true
		}
	}
	for _, candidate := range spec.Candidates {
		if candidate == "" || candidate == agentruntime.RuntimeTypeAuto {
			continue
		}
		declaredConcrete = true
		if actual == candidate {
			return true
		}
	}
	// unconstrained auto 由 RuntimeService 选择受信任的默认实现；一旦声明了
	// concrete runtime，事件只能来自该白名单。
	return spec.Type == agentruntime.RuntimeTypeAuto && !declaredConcrete
}

func schedulerErrorType(eventErr *observability.EventError) scheduler.ErrorType {
	if eventErr == nil {
		return scheduler.ErrorTypeUnknown
	}
	switch eventErr.Type {
	case observability.EventErrorTimeout:
		return scheduler.ErrorTypeTimeout
	case observability.EventErrorCancelled:
		return scheduler.ErrorTypeCancelled
	case observability.EventErrorPermissionDenied:
		return scheduler.ErrorTypePermissionDenied
	case observability.EventErrorGuardrailBlocked:
		return scheduler.ErrorTypeGuardrailBlocked
	case observability.EventErrorRateLimited, observability.EventErrorUpstream:
		return scheduler.ErrorTypeTransient
	case observability.EventErrorResourceExhausted:
		return scheduler.ErrorTypeUnknown
	default:
		return scheduler.ErrorTypeUnknown
	}
}

func safeRuntimeFailureMessage(eventErr *observability.EventError) string {
	if eventErr == nil || eventErr.Code == "" {
		return "runtime run failed"
	}
	return fmt.Sprintf("runtime run failed: %s", eventErr.Code)
}
