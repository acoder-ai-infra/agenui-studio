package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storagewrite"
)

type runtimeAttempt struct {
	runtimeType RuntimeType
	err         error
}

func (s *RuntimeService) hasRuntime() bool {
	return s.Runtime != nil || len(s.Runtimes) > 0
}

func (s *RuntimeService) buildRuntime(ctx context.Context, req RunRequest) (AgentRuntime, AgentHandle, observability.AgentEvent, *FallbackDecision, error) {
	candidates := s.runtimeCandidates(req.Definition.Runtime)
	if len(candidates) == 0 {
		return nil, AgentHandle{}, observability.AgentEvent{}, nil, ErrRuntimeMissing
	}
	var attempts []runtimeAttempt
	for _, candidate := range candidates {
		runtime := s.runtimeFor(candidate)
		if runtime == nil {
			attempts = append(attempts, runtimeAttempt{runtimeType: candidate, err: fmt.Errorf("%w: %s not registered", ErrRuntimeUnavailable, candidate)})
			continue
		}
		health := runtime.Health(ctx)
		if !health.Available {
			reason := health.Reason
			if reason == "" {
				reason = "health unavailable"
			}
			attempts = append(attempts, runtimeAttempt{runtimeType: candidate, err: fmt.Errorf("%w: %s %s", ErrRuntimeUnavailable, candidate, reason)})
			continue
		}
		if err := validateRequiredRuntimeCapabilities(req.Definition.RequiredCapabilities, runtime.Capabilities(ctx)); err != nil {
			attempts = append(attempts, runtimeAttempt{runtimeType: candidate, err: err})
			continue
		}
		def := req.Definition
		def.Runtime.Type = candidate
		if err := runtime.ValidateConfig(ctx, def); err != nil {
			attempts = append(attempts, runtimeAttempt{runtimeType: candidate, err: err})
			continue
		}
		handle, err := runtime.Build(ctx, def)
		if err != nil {
			attempts = append(attempts, runtimeAttempt{runtimeType: candidate, err: err})
			continue
		}
		if handle.Runtime != "" && handle.Runtime != RuntimeTypeAuto && handle.Runtime != candidate {
			attempts = append(attempts, runtimeAttempt{runtimeType: candidate, err: runtimeBindingMismatch("handle.runtime", string(candidate), string(handle.Runtime))})
			continue
		}
		handle.Runtime = candidate
		handle.Definition = def
		binding, err := newRuntimeBinding(ctx, runtime, def, runtimeBindingFactsFromRun(req))
		if err != nil {
			attempts = append(attempts, runtimeAttempt{runtimeType: candidate, err: err})
			continue
		}
		if binding.Runtime != candidate {
			attempts = append(attempts, runtimeAttempt{runtimeType: candidate, err: runtimeBindingMismatch("descriptor.runtime", string(candidate), string(binding.Runtime))})
			continue
		}
		handle.Binding = binding
		if len(attempts) == 0 {
			return runtime, handle, observability.AgentEvent{}, nil, nil
		}
		decision := FallbackDecision{
			RunID:       req.RunID,
			FromRuntime: attempts[0].runtimeType,
			ToRuntime:   handle.Runtime,
			Reason:      fallbackReason(attempts),
			CreatedAt:   time.Now(),
		}
		event, err := s.markFallback(ctx, req, decision)
		if err != nil {
			return nil, AgentHandle{}, observability.AgentEvent{}, nil, err
		}
		return runtime, handle, event, &decision, nil
	}
	return nil, AgentHandle{}, observability.AgentEvent{}, nil, fallbackError(attempts)
}

// buildBoundRuntime resolves the exact runtime identity persisted by the Run.
// It intentionally has no fallback path: native checkpoints are not portable
// across runtime or adapter versions unless an explicit migration exists.
func (s *RuntimeService) buildBoundRuntime(ctx context.Context, req RunRequest, expected RuntimeBinding) (AgentRuntime, AgentHandle, error) {
	if err := expected.Validate(); err != nil {
		return nil, AgentHandle{}, err
	}
	runtime := s.runtimeFor(expected.Runtime)
	if runtime == nil {
		return nil, AgentHandle{}, fmt.Errorf("%w: bound runtime %s not registered", ErrRuntimeUnavailable, expected.Runtime)
	}
	health := runtime.Health(ctx)
	if !health.Available {
		reason := health.Reason
		if reason == "" {
			reason = "health unavailable"
		}
		return nil, AgentHandle{}, fmt.Errorf("%w: bound runtime %s %s", ErrRuntimeUnavailable, expected.Runtime, reason)
	}
	def := req.Definition
	def.Runtime.Type = expected.Runtime
	if err := validateRequiredRuntimeCapabilities(def.RequiredCapabilities, runtime.Capabilities(ctx)); err != nil {
		return nil, AgentHandle{}, err
	}
	if err := runtime.ValidateConfig(ctx, def); err != nil {
		return nil, AgentHandle{}, err
	}
	actual, err := newRuntimeBinding(ctx, runtime, def, runtimeBindingFactsFromRun(req))
	if err != nil {
		return nil, AgentHandle{}, err
	}
	if err := assertRuntimeBinding(expected, actual); err != nil {
		return nil, AgentHandle{}, err
	}
	handle, err := runtime.Build(ctx, def)
	if err != nil {
		return nil, AgentHandle{}, err
	}
	if handle.Runtime != "" && handle.Runtime != RuntimeTypeAuto && handle.Runtime != expected.Runtime {
		return nil, AgentHandle{}, runtimeBindingMismatch("handle.runtime", string(expected.Runtime), string(handle.Runtime))
	}
	handle.Runtime = expected.Runtime
	handle.Definition = def
	handle.Binding = expected
	return runtime, handle, nil
}

func (s *RuntimeService) runtimeCandidates(spec RuntimeSpec) []RuntimeType {
	var candidates []RuntimeType
	add := func(runtimeType RuntimeType) {
		if runtimeType == "" || runtimeType == RuntimeTypeAuto {
			return
		}
		for _, existing := range candidates {
			if existing == runtimeType {
				return
			}
		}
		candidates = append(candidates, runtimeType)
	}
	add(spec.Type)
	add(spec.Preferred)
	for _, candidate := range spec.Candidates {
		add(candidate)
	}
	if len(candidates) == 0 && s.Runtime != nil {
		add(inferRuntimeType(s.Runtime))
	}
	return candidates
}

func (s *RuntimeService) runtimeFor(runtimeType RuntimeType) AgentRuntime {
	if runtimeType == "" || runtimeType == RuntimeTypeAuto {
		return s.Runtime
	}
	if runtime := s.Runtimes[runtimeType]; runtime != nil {
		return runtime
	}
	if s.Runtime != nil && inferRuntimeType(s.Runtime) == runtimeType {
		return s.Runtime
	}
	return nil
}

func (s *RuntimeService) markFallback(ctx context.Context, req RunRequest, decision FallbackDecision) (observability.AgentEvent, error) {
	event := s.normalizeEvent(ctx, req, observability.AgentEvent{
		EventType:  EventFallback,
		Runtime:    string(decision.ToRuntime),
		Visibility: observability.VisibilityDebug,
		Payload: JSONPayload(map[string]any{
			"run_id":       decision.RunID,
			"from_runtime": decision.FromRuntime,
			"to_runtime":   decision.ToRuntime,
			"reason":       decision.Reason,
		}),
	})
	_, err := s.Writer.Execute(ctx, s.plan(ctx, req, "runtime.fallback."+string(decision.FromRuntime)+"."+string(decision.ToRuntime),
		storagewrite.Write{Store: storagewrite.StoreFallback, Operation: storagewrite.OperationAppend, Ref: "fallback:" + req.RunID + ":" + event.EventID, Payload: FallbackStoreWrite{Decision: decision}},
		storagewrite.Write{Store: storagewrite.StoreEvent, Operation: storagewrite.OperationAppend, Ref: event.EventID, Payload: event},
	))
	return event, err
}

func (s *RuntimeService) withRuntimeContext(ctx context.Context, req RunRequest, runtime AgentRuntime) context.Context {
	tc := observability.MustTraceContext(ctx)
	tc.Runtime = runtime.Name()
	tc.AgentID = req.Definition.AgentID
	tc.AgentType = req.Definition.AgentType
	tc.AgentVersion = req.Definition.Version
	return observability.WithTraceContext(ctx, tc)
}

func fallbackReason(attempts []runtimeAttempt) string {
	if len(attempts) == 0 || attempts[0].err == nil {
		return "runtime fallback"
	}
	return attempts[0].err.Error()
}

func fallbackError(attempts []runtimeAttempt) error {
	if len(attempts) == 0 {
		return ErrRuntimeMissing
	}
	return fmt.Errorf("%w: %w", ErrRuntimeUnavailable, errors.Join(attemptErrors(attempts)...))
}

func attemptErrors(attempts []runtimeAttempt) []error {
	errs := make([]error, 0, len(attempts))
	for _, attempt := range attempts {
		if attempt.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", attempt.runtimeType, attempt.err))
		}
	}
	return errs
}

func inferRuntimeType(runtime AgentRuntime) RuntimeType {
	if runtime == nil {
		return ""
	}
	switch RuntimeType(runtime.Name()) {
	case RuntimeTypeEino:
		return RuntimeTypeEino
	case RuntimeTypeGoogleADK:
		return RuntimeTypeGoogleADK
	case RuntimeTypeNative:
		return RuntimeTypeNative
	case RuntimeTypeMock:
		return RuntimeTypeMock
	default:
		return RuntimeType(runtime.Name())
	}
}
