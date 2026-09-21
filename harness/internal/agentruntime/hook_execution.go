package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func (s *RuntimeService) executeHooks(ctx context.Context, req RunRequest, point RuntimeHookPoint, handle *AgentHandle, fallback *FallbackDecision, runErr *RuntimeError) (context.Context, RunRequest, []observability.AgentEvent, error) {
	return s.executeHooksWithContext(ctx, req, point, handle, fallback, runErr, nil, nil, nil)
}

// executeResponseHooks 在 before_response 切点携带累积响应文本执行 hooks。
func (s *RuntimeService) executeResponseHooks(ctx context.Context, req RunRequest, handle *AgentHandle, response *RuntimeHookResponse) (context.Context, RunRequest, []observability.AgentEvent, error) {
	return s.executeHooksWithContext(ctx, req, HookBeforeResponse, handle, nil, nil, nil, nil, response)
}

func (s *RuntimeService) executeHooksWithContext(ctx context.Context, req RunRequest, point RuntimeHookPoint, handle *AgentHandle, fallback *FallbackDecision, runErr *RuntimeError, step *RuntimeHookStep, interrupt *RuntimeHookInterrupt, response *RuntimeHookResponse) (context.Context, RunRequest, []observability.AgentEvent, error) {
	snapshot, ok := RuntimeHookSnapshotFrom(ctx)
	registrations := snapshot.Registrations(point)
	if !ok {
		registrations = s.Hooks.Registrations(point)
	}
	if len(registrations) == 0 {
		return ctx, req, nil, nil
	}
	var events []observability.AgentEvent
	for _, registration := range registrations {
		hookCtx, span := s.Tracer.Start(ctx, "runtime.hook",
			observability.String("run_id", req.RunID),
			observability.String("hook_id", registration.ID),
			observability.String("hook_point", string(point)),
		)
		hookStep, started, err := s.startStep(hookCtx, req, StepStart{
			Kind: StepKindProcessorHook,
			Name: registration.ID,
			Metadata: map[string]string{
				"hook_id":        registration.ID,
				"hook_point":     string(point),
				"failure_policy": string(registration.FailurePolicy),
			},
		})
		if err != nil {
			span.RecordError(err)
			span.End()
			return ctx, req, events, s.classifyError(err, ErrorStageState)
		}
		events = append(events, started)
		clonedRun, cloneErr := cloneRunRequest(req)
		clonedHandle, handleErr := cloneAgentHandle(handle)
		if cloneErr == nil {
			cloneErr = handleErr
		}
		input := RuntimeHookInput{
			Point:     point,
			Run:       clonedRun,
			Handle:    clonedHandle,
			Fallback:  cloneFallbackDecision(fallback),
			Error:     cloneRuntimeError(runErr),
			Step:      cloneRuntimeHookStep(step),
			Interrupt: cloneRuntimeHookInterrupt(interrupt),
			Response:  cloneRuntimeHookResponse(response),
		}
		var output RuntimeHookOutput
		hookErr := cloneErr
		if hookErr == nil {
			executionCtx, cancel := context.WithTimeout(hookCtx, registration.Timeout)
			output, hookErr = executeRuntimeHook(executionCtx, registration.Hook, input)
			cancel()
		}
		if errors.Is(hookErr, context.DeadlineExceeded) {
			hookErr = NewRuntimeError(ErrorTimeout, "RUNTIME_HOOK_TIMEOUT", "runtime hook timed out").WithRetryable(true).WithCause(hookErr)
		}
		if hookErr == nil {
			merged, mergeErr := mergeScopedData(req.ScopedData, output.ScopedData, registration.AllowOverwrite)
			if mergeErr != nil {
				hookErr = mergeErr
			} else {
				req.ScopedData = merged
			}
		}
		if hookErr != nil {
			runtimeErr := s.classifyError(hookErr, ErrorStageHook)
			span.RecordError(hookErr)
			failed, writeErr := s.failStep(hookCtx, req, hookStep, StepKindProcessorHook, runtimeErr)
			if writeErr != nil {
				span.RecordError(writeErr)
				span.End()
				return ctx, req, events, s.classifyError(writeErr, ErrorStageState)
			}
			events = append(events, failed)
			span.End()
			if registration.FailurePolicy == HookFailClosed {
				return ctx, req, events, runtimeErr
			}
			continue
		}
		ctx = WithScopedData(ctx, req.ScopedData)
		completed, err := s.completeStep(hookCtx, req, hookStep, StepKindProcessorHook)
		if err != nil {
			span.RecordError(err)
			span.End()
			return ctx, req, events, s.classifyError(err, ErrorStageState)
		}
		events = append(events, completed)
		span.End()
	}
	return ctx, req, events, nil
}

func cloneRuntimeHookStep(step *RuntimeHookStep) *RuntimeHookStep {
	if step == nil {
		return nil
	}
	copy := *step
	copy.Error = cloneRuntimeError(step.Error)
	return &copy
}

func cloneRuntimeHookInterrupt(interrupt *RuntimeHookInterrupt) *RuntimeHookInterrupt {
	if interrupt == nil {
		return nil
	}
	copy := *interrupt
	return &copy
}

// cloneRuntimeHookResponse 隔离响应视图，避免 hook 篡改共享状态。
func cloneRuntimeHookResponse(response *RuntimeHookResponse) *RuntimeHookResponse {
	if response == nil {
		return nil
	}
	copy := *response
	return &copy
}

func executeRuntimeHook(ctx context.Context, hook RuntimeHook, input RuntimeHookInput) (output RuntimeHookOutput, err error) {
	// In-process code cannot be forcefully stopped safely. Hooks execute in the
	// caller's lifecycle and must cooperate with ctx cancellation. Hooks that
	// cannot provide that guarantee belong in a separately managed process.
	return executeRuntimeHookDirect(ctx, hook, input)
}

func executeRuntimeHookDirect(ctx context.Context, hook RuntimeHook, input RuntimeHookInput) (output RuntimeHookOutput, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: %v", ErrHookPanic, recovered)
		}
	}()
	return hook.Execute(ctx, input)
}

func mergeScopedData(base, patch ScopedData, allowOverwrite bool) (ScopedData, error) {
	merged := cloneScopedData(base)
	if merged.Run == nil && len(patch.Run) > 0 {
		merged.Run = make(map[string]ScopedDataItem, len(patch.Run))
	}
	for key, item := range patch.Run {
		if err := validateScopedDataItem("run", key, item); err != nil {
			return ScopedData{}, err
		}
		if _, exists := merged.Run[key]; exists && !allowOverwrite {
			return ScopedData{}, fmt.Errorf("%w: run.%s", ErrHookDataConflict, key)
		}
		merged.Run[key] = cloneScopedDataItem(item)
	}
	if merged.Agents == nil && len(patch.Agents) > 0 {
		merged.Agents = make(map[string]map[string]ScopedDataItem, len(patch.Agents))
	}
	for agentID, values := range patch.Agents {
		if agentID == "" {
			return ScopedData{}, fmt.Errorf("%w: agent_id required", ErrHookDataInvalid)
		}
		if merged.Agents[agentID] == nil {
			merged.Agents[agentID] = make(map[string]ScopedDataItem, len(values))
		}
		for key, item := range values {
			if err := validateScopedDataItem("agent."+agentID, key, item); err != nil {
				return ScopedData{}, err
			}
			if _, exists := merged.Agents[agentID][key]; exists && !allowOverwrite {
				return ScopedData{}, fmt.Errorf("%w: agent.%s.%s", ErrHookDataConflict, agentID, key)
			}
			merged.Agents[agentID][key] = cloneScopedDataItem(item)
		}
	}
	return merged, nil
}

func validateScopedDataItem(scope, key string, item ScopedDataItem) error {
	if key == "" {
		return fmt.Errorf("%w: %s key required", ErrHookDataInvalid, scope)
	}
	if item.Source == "" {
		return fmt.Errorf("%w: %s.%s source required", ErrHookDataInvalid, scope, key)
	}
	if len(item.Value) == 0 && item.Ref == "" {
		return fmt.Errorf("%w: %s.%s value or ref required", ErrHookDataInvalid, scope, key)
	}
	if len(item.Value) > 0 && !json.Valid(item.Value) {
		return fmt.Errorf("%w: %s.%s value is not valid json", ErrHookDataInvalid, scope, key)
	}
	visibility := observability.EventVisibility(item.Visibility)
	if visibility != "" && visibility != observability.VisibilityUserVisible && visibility != observability.VisibilityDebug && visibility != observability.VisibilityInternal && visibility != observability.VisibilityRestricted {
		return fmt.Errorf("%w: %s.%s visibility=%s", ErrHookDataInvalid, scope, key, item.Visibility)
	}
	return nil
}

func cloneRunRequest(req RunRequest) (RunRequest, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return RunRequest{}, fmt.Errorf("%w: clone run request: %v", ErrHookDataInvalid, err)
	}
	var cloned RunRequest
	if err := json.Unmarshal(data, &cloned); err != nil {
		return RunRequest{}, fmt.Errorf("%w: clone run request: %v", ErrHookDataInvalid, err)
	}
	return cloned, nil
}

func cloneScopedData(data ScopedData) ScopedData {
	out := ScopedData{}
	if data.Run != nil {
		out.Run = make(map[string]ScopedDataItem, len(data.Run))
		for key, item := range data.Run {
			out.Run[key] = cloneScopedDataItem(item)
		}
	}
	if data.Agents != nil {
		out.Agents = make(map[string]map[string]ScopedDataItem, len(data.Agents))
		for agentID, values := range data.Agents {
			out.Agents[agentID] = make(map[string]ScopedDataItem, len(values))
			for key, item := range values {
				out.Agents[agentID][key] = cloneScopedDataItem(item)
			}
		}
	}
	return out
}

func cloneScopedDataItem(item ScopedDataItem) ScopedDataItem {
	item.Value = append([]byte(nil), item.Value...)
	return item
}

func cloneAgentHandle(handle *AgentHandle) (*AgentHandle, error) {
	if handle == nil {
		return nil, nil
	}
	clonedRun, err := cloneRunRequest(RunRequest{Definition: handle.Definition})
	if err != nil {
		return nil, err
	}
	copy := *handle
	copy.Definition = clonedRun.Definition
	return &copy, nil
}

func cloneFallbackDecision(decision *FallbackDecision) *FallbackDecision {
	if decision == nil {
		return nil
	}
	copy := *decision
	return &copy
}

func cloneRuntimeError(runtimeErr *RuntimeError) *RuntimeError {
	if runtimeErr == nil {
		return nil
	}
	copy := *runtimeErr
	return &copy
}
