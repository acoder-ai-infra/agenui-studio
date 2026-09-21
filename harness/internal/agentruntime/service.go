package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storagewrite"
)

var (
	ErrRuntimeMissing          = errors.New("agent runtime missing")
	ErrRuntimeUnavailable      = errors.New("agent runtime unavailable")
	ErrStateMissing            = errors.New("runtime state manager missing")
	ErrRunIDMissing            = errors.New("run_id required")
	ErrSessionMissing          = errors.New("session_id required")
	ErrCheckpointIDMissing     = errors.New("checkpoint_id required")
	ErrControlRequestIDMissing = errors.New("control_request_id required")
	ErrResumeTokenMissing      = errors.New("resume_token required")
	ErrInterruptBindingMissing = errors.New("runtime interrupt binding missing")
	ErrRunInputRoleInvalid     = errors.New("run input role must be user")
	ErrChildResumeUnsupported  = errors.New("platform child run resume is unsupported")
)

const runtimeCleanupTimeout = 5 * time.Second

func (s *RuntimeService) Run(ctx context.Context, req RunRequest) (<-chan observability.AgentEvent, error) {
	if !s.hasRuntime() {
		return nil, ErrRuntimeMissing
	}
	if s.State == nil {
		return nil, ErrStateMissing
	}
	if s.Writer == nil {
		return nil, ErrStorageWriterMissing
	}
	if s.Assembler == nil {
		return nil, ErrModelContextPackageMissing
	}
	if err := validateRunRequest(req); err != nil {
		return nil, err
	}
	if err := s.Finalization.Validate(); err != nil {
		return nil, err
	}
	ctx = s.prepareContext(ctx, req)
	ctx = WithRuntimeHookSnapshot(ctx, s.Hooks.Snapshot())
	runtimeName := string(req.Definition.Runtime.Type)
	if (runtimeName == "" || req.Definition.Runtime.Type == RuntimeTypeAuto) && s.Runtime != nil {
		runtimeName = s.Runtime.Name()
	}
	ctx, span := s.Tracer.Start(ctx, "runtime.run",
		observability.String("run_id", req.RunID),
		observability.String("agent_id", req.Definition.AgentID),
		observability.String("runtime", runtimeName),
	)

	if err := s.startRun(ctx, req); err != nil {
		runtimeErr := s.classifyError(err, ErrorStageState)
		span.RecordError(err)
		span.End()
		return nil, runtimeErr
	}
	var preEvents []observability.AgentEvent
	ctx, req, hookEvents, err := s.executeHooks(ctx, req, HookBeforeBuild, nil, nil, nil)
	preEvents = append(preEvents, hookEvents...)
	if err != nil {
		span.RecordError(err)
		if writeErr := s.failBeforeStream(ctx, req, err); writeErr != nil {
			span.RecordError(writeErr)
		}
		span.End()
		return nil, err
	}
	runtime, handle, fallbackEvent, fallbackDecision, err := s.buildRuntime(ctx, req)
	if err != nil {
		runtimeErr := s.classifyError(err, ErrorStageRuntimeSelection)
		span.RecordError(err)
		if writeErr := s.failBeforeStream(ctx, req, runtimeErr); writeErr != nil {
			span.RecordError(writeErr)
		}
		span.End()
		return nil, runtimeErr
	}
	req.Definition = handle.Definition
	if err := s.bindRuntime(ctx, req, handle.Binding); err != nil {
		runtimeErr := s.classifyError(err, ErrorStageState)
		span.RecordError(err)
		if writeErr := s.failBeforeStream(ctx, req, runtimeErr); writeErr != nil {
			span.RecordError(writeErr)
		}
		span.End()
		return nil, runtimeErr
	}
	persistedBinding, err := s.State.RuntimeBinding(ctx, req.RunID)
	if err != nil {
		runtimeErr := s.classifyError(err, ErrorStageState)
		span.RecordError(err)
		if writeErr := s.failBeforeStream(ctx, req, runtimeErr); writeErr != nil {
			span.RecordError(writeErr)
		}
		span.End()
		return nil, runtimeErr
	}
	handle.Binding = persistedBinding
	ctx = s.withRuntimeContext(ctx, req, runtime)
	if fallbackEvent.EventType != "" {
		preEvents = append(preEvents, fallbackEvent)
	}
	ctx, req, hookEvents, err = s.executeHooks(ctx, req, HookAfterBuild, &handle, nil, nil)
	preEvents = append(preEvents, hookEvents...)
	if err == nil && fallbackDecision != nil {
		ctx, req, hookEvents, err = s.executeHooks(ctx, req, HookOnFallback, &handle, fallbackDecision, nil)
		preEvents = append(preEvents, hookEvents...)
	}
	if err == nil {
		ctx, req, hookEvents, err = s.executeHooks(ctx, req, HookBeforeRun, &handle, fallbackDecision, nil)
		preEvents = append(preEvents, hookEvents...)
	}
	if err != nil {
		span.RecordError(err)
		if writeErr := s.failBeforeStream(ctx, req, err); writeErr != nil {
			span.RecordError(writeErr)
		}
		span.End()
		return nil, err
	}

	ctx, req, contextEvents, err := s.buildModelContext(ctx, req, runtime, handle)
	if err != nil {
		runtimeErr := s.classifyError(err, ErrorStageModelContext)
		span.RecordError(err)
		if writeErr := s.failContextBeforeStream(ctx, req, runtimeErr); writeErr != nil {
			span.RecordError(writeErr)
		}
		span.End()
		return nil, runtimeErr
	}
	preEvents = append(preEvents, contextEvents...)
	pkg, ok := ModelContextPackageFrom(ctx)
	if !ok {
		runtimeErr := s.classifyError(ErrModelContextPackageMissing, ErrorStageModelContext)
		span.RecordError(runtimeErr)
		if writeErr := s.failBeforeStream(ctx, req, runtimeErr); writeErr != nil {
			span.RecordError(writeErr)
		}
		span.End()
		return nil, runtimeErr
	}
	capabilityBinding, err := newRuntimeCapabilityBinding(pkg.Capabilities)
	if err != nil {
		runtimeErr := s.classifyError(err, ErrorStageModelContext)
		span.RecordError(err)
		if writeErr := s.failBeforeStream(ctx, req, runtimeErr); writeErr != nil {
			span.RecordError(writeErr)
		}
		span.End()
		return nil, runtimeErr
	}
	if err := s.bindRuntimeCapabilities(ctx, req, *capabilityBinding); err != nil {
		runtimeErr := s.classifyError(err, ErrorStageState)
		span.RecordError(err)
		if writeErr := s.failBeforeStream(ctx, req, runtimeErr); writeErr != nil {
			span.RecordError(writeErr)
		}
		span.End()
		return nil, runtimeErr
	}
	handle.Binding.Capabilities = capabilityBinding

	ctx, req, adapterStep, adapterStepEvents, err := s.startStepWithHooks(ctx, req, StepStart{Kind: StepKindRuntimeAdapter, Name: "runtime_adapter"}, &handle)
	if err != nil {
		runtimeErr := s.classifyError(err, ErrorStageState)
		span.RecordError(err)
		if writeErr := s.failBeforeStream(ctx, req, runtimeErr); writeErr != nil {
			span.RecordError(writeErr)
		}
		span.End()
		return nil, runtimeErr
	}
	preEvents = append(preEvents, adapterStepEvents...)
	executionCtx, cancel := executionContext(ctx, req.Definition.Timeout)
	executionCtx = WithRuntimeParentStepID(executionCtx, adapterStep.StepID)
	executionCtx = withRuntimeEventCommitBarrier(executionCtx, newRuntimeEventCommitBarrier(s.IDs))
	active := s.active.register(req.RunID, cancel)
	active.setRuntime(runtime)
	adapterEvents, err := runtime.Run(executionCtx, req)
	if err != nil {
		runtimeErr := s.classifyError(err, ErrorStageRuntimeAdapter)
		s.active.remove(req.RunID, active)
		cancel()
		span.RecordError(err)
		if _, _, _, writeErr := s.failStepWithHooks(ctx, req, adapterStep, StepKindRuntimeAdapter, "runtime_adapter", &handle, runtimeErr); writeErr != nil {
			span.RecordError(writeErr)
		}
		if writeErr := s.failBeforeStream(ctx, req, runtimeErr); writeErr != nil {
			span.RecordError(writeErr)
		}
		span.End()
		return nil, runtimeErr
	}

	out := make(chan observability.AgentEvent, 16)
	go s.pipeEvents(executionCtx, span, req, adapterEvents, out, adapterStep, active, handle, true, preEvents...)
	return out, nil
}

func (s *RuntimeService) Resume(ctx context.Context, req ResumeRequest) (<-chan observability.AgentEvent, error) {
	if !s.hasRuntime() {
		return nil, ErrRuntimeMissing
	}
	if s.State == nil {
		return nil, ErrStateMissing
	}
	if s.Writer == nil {
		return nil, ErrStorageWriterMissing
	}
	if s.Assembler == nil {
		return nil, ErrModelContextPackageMissing
	}
	if err := validateResumeRequest(req); err != nil {
		return nil, err
	}
	if err := s.Finalization.Validate(); err != nil {
		return nil, err
	}
	productionAssembler := productionAssemblerReady(s.Assembler)
	if productionAssembler && req.ContextSnapshotRef == "" {
		return nil, ErrProductionContextSnapshot
	}
	ctx = observability.WithTraceContext(ctx, req.Trace)
	ctx = observability.WithRun(ctx, req.SessionID, req.RunID)
	ctx = observability.WithAgent(ctx, req.Definition.AgentID, req.Definition.AgentType, req.Definition.Version)
	ctx = observability.WithLogger(ctx, s.Logger)
	ctx = WithScopedData(ctx, req.ScopedData)
	ctx = WithRuntimeHookSnapshot(ctx, s.Hooks.Snapshot())
	ctx, span := s.Tracer.Start(ctx, "runtime.resume",
		observability.String("run_id", req.RunID),
		observability.String("checkpoint_id", req.CheckpointID),
	)
	binding, err := s.State.RuntimeBinding(ctx, req.RunID)
	if err != nil {
		runtimeErr := s.classifyError(err, ErrorStageResume)
		span.RecordError(err)
		span.End()
		return nil, runtimeErr
	}
	if err := validateResumeRuntimeBinding(binding, req); err != nil {
		runtimeErr := s.classifyError(err, ErrorStageResume)
		span.RecordError(err)
		span.End()
		return nil, runtimeErr
	}
	if productionAssembler && binding.Capabilities == nil {
		runtimeErr := s.classifyError(ErrRuntimeBindingInvalid, ErrorStageResume)
		span.RecordError(runtimeErr)
		span.End()
		return nil, runtimeErr
	}
	req.Definition.Runtime.Type = binding.Runtime
	req.ConfigSnapshotRef = binding.ConfigSnapshotRef
	req.ConfigHash = binding.ConfigHash
	req.AgentBindingID = binding.AgentBindingID
	// Resume 的身份以首次 Run 固化的绑定为准；调用方不能借恢复请求切换租户或用户。
	req.TenantID = binding.TenantID
	req.UserID = binding.UserID
	tc := observability.MustTraceContext(ctx)
	tc.TenantID = binding.TenantID
	tc.UserID = binding.UserID
	tc.ParentRunID = req.ParentRunID
	tc.RootRunID = req.RunID
	if req.ParentRunID != "" {
		tc.RootRunID = req.ParentRunID
	}
	req.Trace = tc
	ctx = observability.WithTraceContext(ctx, tc)
	runReq := runRequestFromResume(req)
	resumeAttemptID := "resume_" + s.IDs.NewRequestID()
	resumeAccepted, err := s.beginResume(ctx, runReq, req, resumeAttemptID)
	if err != nil {
		runtimeErr := s.classifyError(err, ErrorStageResume)
		if errors.Is(err, ErrResumeAlreadyClaimed) {
			s.Logger.Warn(ctx, "resume claim is already held; inspect the run before manual retry",
				observability.String("run_id", req.RunID),
				observability.String("resume_attempt_id", resumeAttemptID),
			)
		}
		span.RecordError(err)
		span.End()
		return nil, runtimeErr
	}
	preEvents := []observability.AgentEvent{resumeAccepted}
	ctx, runReq, hookEvents, err := s.executeHooks(ctx, runReq, HookBeforeBuild, nil, nil, nil)
	preEvents = append(preEvents, hookEvents...)
	if err != nil {
		runtimeErr := s.classifyResumePreparationError(err, ErrorStageHook)
		span.RecordError(err)
		if writeErr := s.failResumeAttempt(ctx, runReq, resumeAttemptID, runtimeErr); writeErr != nil {
			span.RecordError(writeErr)
		}
		span.End()
		return nil, runtimeErr
	}
	runtime, handle, err := s.buildBoundRuntime(ctx, runReq, binding)
	if err != nil {
		runtimeErr := s.classifyResumePreparationError(err, ErrorStageRuntimeSelection)
		span.RecordError(err)
		if writeErr := s.failResumeAttempt(ctx, runReq, resumeAttemptID, runtimeErr); writeErr != nil {
			span.RecordError(writeErr)
		}
		span.End()
		return nil, runtimeErr
	}
	runReq.Definition = handle.Definition
	req.Definition = handle.Definition
	ctx = s.withRuntimeContext(ctx, runReq, runtime)
	ctx, runReq, hookEvents, err = s.executeHooks(ctx, runReq, HookAfterBuild, &handle, nil, nil)
	preEvents = append(preEvents, hookEvents...)
	if err == nil {
		ctx, runReq, hookEvents, err = s.executeHooks(ctx, runReq, HookBeforeRun, &handle, nil, nil)
		preEvents = append(preEvents, hookEvents...)
	}
	if err != nil {
		runtimeErr := s.classifyResumePreparationError(err, ErrorStageHook)
		span.RecordError(err)
		if writeErr := s.failResumeAttempt(ctx, runReq, resumeAttemptID, runtimeErr); writeErr != nil {
			span.RecordError(writeErr)
		}
		span.End()
		return nil, runtimeErr
	}
	req.ScopedData = runReq.ScopedData
	ctx, runReq, contextEvents, err := s.buildModelContext(ctx, runReq, runtime, handle)
	if err != nil {
		runtimeErr := s.classifyResumePreparationError(err, ErrorStageModelContext)
		span.RecordError(err)
		if writeErr := s.recordModelContextBuildFailed(ctx, runReq, runtimeErr); writeErr != nil {
			span.RecordError(writeErr)
		}
		if writeErr := s.failResumeAttempt(ctx, runReq, resumeAttemptID, runtimeErr); writeErr != nil {
			span.RecordError(writeErr)
		}
		span.End()
		return nil, runtimeErr
	}
	preEvents = append(preEvents, contextEvents...)
	if err := s.activateResume(ctx, runReq, resumeAttemptID); err != nil {
		runtimeErr := s.classifyError(err, ErrorStageState)
		s.Logger.Warn(ctx, "resume activation result requires state verification before retry",
			observability.String("run_id", req.RunID),
			observability.String("resume_attempt_id", resumeAttemptID),
		)
		span.RecordError(err)
		// Activate 可能已经提交、只是客户端没有收到结果。此处只能通过
		// attempt-fenced cleanup 收口，绝不能用通用 FailRun 越过 owner 校验；
		// 极端的不确定状态交给日志告警和外部一次性修复处理。
		if writeErr := s.failResumeAttempt(ctx, runReq, resumeAttemptID, runtimeErr); writeErr != nil && !errors.Is(writeErr, ErrResumeClaimLost) {
			span.RecordError(writeErr)
		}
		span.End()
		return nil, runtimeErr
	}
	ctx, runReq, adapterStep, adapterStepEvents, err := s.startStepWithHooks(ctx, runReq, StepStart{Kind: StepKindRuntimeAdapter, Name: "runtime_adapter"}, &handle)
	if err != nil {
		runtimeErr := s.classifyError(err, ErrorStageState)
		span.RecordError(err)
		if writeErr := s.failBeforeStream(ctx, runReq, runtimeErr); writeErr != nil {
			span.RecordError(writeErr)
		}
		span.End()
		return nil, runtimeErr
	}
	preEvents = append(preEvents, adapterStepEvents...)
	req.ScopedData = runReq.ScopedData
	executionCtx, cancel := executionContext(ctx, req.Definition.Timeout)
	executionCtx = WithRuntimeParentStepID(executionCtx, adapterStep.StepID)
	executionCtx = withRuntimeEventCommitBarrier(executionCtx, newRuntimeEventCommitBarrier(s.IDs))
	active := s.active.register(req.RunID, cancel)
	active.setRuntime(runtime)
	adapterEvents, err := runtime.Resume(executionCtx, req)
	if err != nil {
		runtimeErr := s.classifyError(err, ErrorStageRuntimeAdapter)
		s.active.remove(req.RunID, active)
		cancel()
		span.RecordError(err)
		if _, _, _, writeErr := s.failStepWithHooks(ctx, runReq, adapterStep, StepKindRuntimeAdapter, "runtime_adapter", &handle, runtimeErr); writeErr != nil {
			span.RecordError(writeErr)
		}
		if writeErr := s.failBeforeStream(ctx, runReq, runtimeErr); writeErr != nil {
			span.RecordError(writeErr)
		}
		span.End()
		return nil, runtimeErr
	}
	out := make(chan observability.AgentEvent, 16)
	go s.pipeEvents(executionCtx, span, runReq, adapterEvents, out, adapterStep, active, handle, false, preEvents...)
	return out, nil
}

func validateRunRequest(req RunRequest) error {
	if req.RunID == "" {
		return ErrRunIDMissing
	}
	if req.SessionID == "" {
		return ErrSessionMissing
	}
	for _, message := range req.Input {
		if message.Role != "user" {
			return ErrRunInputRoleInvalid
		}
	}
	return nil
}

func validateResumeRequest(req ResumeRequest) error {
	if req.RunID == "" {
		return ErrRunIDMissing
	}
	if req.SessionID == "" {
		return ErrSessionMissing
	}
	if req.CheckpointID == "" {
		return ErrCheckpointIDMissing
	}
	if req.ControlRequestID == "" {
		return ErrControlRequestIDMissing
	}
	if req.ResumeToken == "" {
		return ErrResumeTokenMissing
	}
	// platform child run 的 resume 由 Agent Gateway 凭 child control 绑定发起，
	// 与父 Run 走同一条 claim 校验链；此处不再按 ParentRunID fail-closed。
	return nil
}

func validateCancelRequest(req CancelRequest) error {
	if req.RunID == "" {
		return ErrRunIDMissing
	}
	if req.SessionID == "" {
		return ErrSessionMissing
	}
	return nil
}

func runRequestFromResume(req ResumeRequest) RunRequest {
	return RunRequest{
		SessionID:          req.SessionID,
		RunID:              req.RunID,
		ParentRunID:        req.ParentRunID,
		Definition:         req.Definition,
		ContextSnapshotRef: req.ContextSnapshotRef,
		ConfigSnapshotRef:  req.ConfigSnapshotRef,
		ConfigHash:         req.ConfigHash,
		AgentBindingID:     req.AgentBindingID,
		ScopedData:         req.ScopedData,
		UserID:             req.UserID,
		TenantID:           req.TenantID,
		Trace:              req.Trace,
		ResultVisibility:   req.ResultVisibility,
	}
}

func (s *RuntimeService) Cancel(ctx context.Context, req CancelRequest) error {
	if !s.hasRuntime() {
		return ErrRuntimeMissing
	}
	if err := validateCancelRequest(req); err != nil {
		return err
	}
	if active := s.active.get(req.RunID); active != nil {
		runtime, accepted := active.requestCancel(req)
		if !accepted {
			return nil
		}
		var runtimeErr error
		if runtime != nil {
			runtimeErr = runtime.Cancel(ctx, req)
		}
		active.cancel()
		return runtimeErr
	}
	if s.Writer != nil {
		runReq := RunRequest{SessionID: req.SessionID, RunID: req.RunID}
		if _, err := s.cancelRunWithEvent(ctx, runReq, req); err != nil {
			return err
		}
	} else if s.State != nil {
		if err := s.State.CancelRun(ctx, req); err != nil {
			return err
		}
	}
	// An inactive Run has no process-local runtime execution to cancel. Durable
	// state is the source of truth; distributed cancellation must be routed to
	// the lease owner instead of invoking the service's default runtime.
	return nil
}

func (s *RuntimeService) prepareContext(ctx context.Context, req RunRequest) context.Context {
	tc := req.Trace
	if tc.TraceID == "" {
		tc.TraceID = s.IDs.NewTraceID()
	}
	tc.SessionID = req.SessionID
	tc.RunID = req.RunID
	tc.ParentRunID = req.ParentRunID
	tc.RootRunID = req.RunID
	if req.ParentRunID != "" {
		tc.RootRunID = req.ParentRunID
	}
	tc.UserID = req.UserID
	tc.TenantID = req.TenantID
	tc.AgentID = req.Definition.AgentID
	tc.AgentType = req.Definition.AgentType
	tc.AgentVersion = req.Definition.Version
	if s.Runtime != nil {
		tc.Runtime = s.Runtime.Name()
	}
	ctx = observability.WithTraceContext(ctx, tc)
	ctx = observability.WithLogger(ctx, s.Logger)
	ctx = WithScopedData(ctx, req.ScopedData)
	return ctx
}

func (s *RuntimeService) pipeEvents(ctx context.Context, span observability.Span, req RunRequest, adapterEvents <-chan observability.AgentEvent, out chan<- observability.AgentEvent, adapterStep *StepSnapshot, active *activeRun, handle AgentHandle, emitRunStarted bool, preEvents ...observability.AgentEvent) {
	commitBarrier, _ := runtimeEventCommitBarrierFrom(ctx)
	defer close(out)
	defer span.End()
	defer active.cancel()
	defer s.active.remove(req.RunID, active)
	defer commitBarrier.failAll(ErrRuntimeEventCommitBarrierClosed)
	nestedSpans := make(map[string]observability.Span)
	defer endRuntimeNestedSpans(nestedSpans)

	for _, event := range preEvents {
		if !sendRuntimeEvent(ctx, out, event) {
			s.finishInterrupted(ctx, req, out, adapterStep, active, &handle)
			return
		}
	}

	if emitRunStarted {
		if err := s.appendAndSend(ctx, req, out, observability.AgentEvent{
			EventType:  EventRunStarted,
			Visibility: observability.VisibilityDebug,
		}); err != nil {
			span.RecordError(err)
			if ctx.Err() != nil {
				s.finishInterrupted(ctx, req, out, adapterStep, active, &handle)
				return
			}
			if adapterStep != nil {
				if _, _, failedEvents, stepErr := s.failStepWithHooks(ctx, req, adapterStep, StepKindRuntimeAdapter, "runtime_adapter", &handle, err); stepErr == nil {
					sendRuntimeEvents(ctx, out, failedEvents)
				}
			}
			if failErr := s.failRun(ctx, req, err); failErr != nil {
				span.RecordError(failErr)
			}
			return
		}
	}

	var pendingRunCompleted *observability.AgentEvent
	accumulator := newFinalResponseAccumulator(s.Finalization.MaxContentBytes)
	for {
		select {
		case <-ctx.Done():
			s.finishInterrupted(ctx, req, out, adapterStep, active, &handle)
			return
		case event, ok := <-adapterEvents:
			if !ok {
				goto adapterDone
			}
			event = s.observeRuntimeNestedStep(ctx, event, nestedSpans)
			switch event.EventType {
			case EventFinalResponse:
				accumulator.Add(event)
				continue
			case EventRunCompleted:
				eventCopy := event
				pendingRunCompleted = &eventCopy
				continue
			case EventRunFailed:
				runtimeErr := runtimeErrorFromEvent(event.Error)
				event.Visibility = observability.VisibilityDebug
				event.Error = runtimeErr.EventError()
				event.Payload = JSONPayload(runtimeErr.Payload())
				span.RecordError(runtimeErr)
				if adapterStep != nil {
					updatedCtx, updatedReq, failedEvents, stepErr := s.failStepWithHooks(ctx, req, adapterStep, StepKindRuntimeAdapter, "runtime_adapter", &handle, runtimeErr)
					ctx, req = updatedCtx, updatedReq
					if stepErr != nil {
						span.RecordError(stepErr)
					} else {
						sendRuntimeEvents(ctx, out, failedEvents)
					}
				}
				ctx, req, hookEvents, hookErr := s.executeErrorHooks(ctx, req, &handle, runtimeErr)
				sendRuntimeEvents(ctx, out, hookEvents)
				if hookErr != nil {
					span.RecordError(hookErr)
				}
				if err := s.appendAndSend(ctx, req, out, event); err != nil {
					span.RecordError(err)
				}
				if failErr := s.failRun(ctx, req, runtimeErr); failErr != nil {
					span.RecordError(failErr)
				}
				return
			case EventRunCancelled:
				runtimeErr := NewRuntimeError(ErrorCancelled, "RUN_CANCELLED", "runtime execution cancelled")
				event.Visibility = observability.VisibilityDebug
				event.Error = runtimeErr.EventError()
				event.Payload = JSONPayload(runtimeErr.Payload())
				if adapterStep != nil {
					updatedCtx, updatedReq, failedEvents, stepErr := s.failStepWithHooks(ctx, req, adapterStep, StepKindRuntimeAdapter, "runtime_adapter", &handle, runtimeErr)
					ctx, req = updatedCtx, updatedReq
					if stepErr != nil {
						span.RecordError(stepErr)
					} else {
						sendRuntimeEvents(ctx, out, failedEvents)
					}
				}
				if err := s.appendAndSend(ctx, req, out, event); err != nil {
					span.RecordError(err)
				}
				if cancelErr := s.cancelRun(ctx, CancelRequest{SessionID: req.SessionID, RunID: req.RunID}); cancelErr != nil {
					span.RecordError(cancelErr)
				}
				return
			case observability.EventControlRequestCreated:
				if err := s.handleInterrupt(ctx, req, out, event, adapterStep, active, &handle); err != nil {
					span.RecordError(err)
					s.failDuringStream(ctx, span, req, out, err, ErrorStageResume)
				}
				return
			}
			accumulator.Add(event)
			event = s.normalizeEvent(ctx, req, event)
			if observability.IsEphemeralDelta(event.EventType) {
				if !sendRuntimeEvent(ctx, out, event) {
					s.finishInterrupted(ctx, req, out, adapterStep, active, &handle)
					return
				}
				continue
			}
			var persistErr error
			if isRuntimeStepLifecycleEvent(event.EventType) {
				event.Sequence, persistErr = s.persistAdapterEvent(ctx, req, event)
			} else {
				event.Sequence, persistErr = s.appendEventWithSequence(ctx, req, event)
			}
			if persistErr != nil {
				commitBarrier.acknowledge(event.EventID, persistErr)
				span.RecordError(persistErr)
				if adapterStep != nil {
					if updatedCtx, updatedReq, failedEvents, stepErr := s.failStepWithHooks(ctx, req, adapterStep, StepKindRuntimeAdapter, "runtime_adapter", &handle, persistErr); stepErr == nil {
						ctx, req = updatedCtx, updatedReq
						sendRuntimeEvents(ctx, out, failedEvents)
					}
				}
				if failErr := s.failRun(ctx, req, persistErr); failErr != nil {
					span.RecordError(failErr)
				}
				return
			}
			commitBarrier.acknowledge(event.EventID, nil)
			if !sendRuntimeEvent(ctx, out, event) {
				s.finishInterrupted(ctx, req, out, adapterStep, active, &handle)
				return
			}
		}
	}

adapterDone:
	if err := ctx.Err(); err != nil {
		s.finishInterrupted(ctx, req, out, adapterStep, active, &handle)
		return
	}
	var err error
	if adapterStep != nil {
		var stepEvents []observability.AgentEvent
		ctx, req, stepEvents, err = s.completeStepWithHooks(ctx, req, adapterStep, StepKindRuntimeAdapter, "runtime_adapter", &handle)
		if err != nil {
			span.RecordError(err)
			s.failDuringStream(ctx, span, req, out, err, ErrorStageHook)
			return
		}
		if !sendRuntimeEvents(ctx, out, stepEvents) {
			s.finishInterrupted(ctx, req, out, adapterStep, active, &handle)
			return
		}
	}
	var hookEvents []observability.AgentEvent
	ctx, req, hookEvents, err = s.executeHooks(ctx, req, HookAfterRun, &handle, nil, nil)
	if !sendRuntimeEvents(ctx, out, hookEvents) {
		s.finishInterrupted(ctx, req, out, adapterStep, active, &handle)
		return
	}
	if err != nil {
		s.failDuringStream(ctx, span, req, out, err, ErrorStageHook)
		return
	}
	// before_response 切点携带累积响应文本，供输出校验类 hook（OutputValidator
	// 桥接器等）在 final_response 落盘前实施 fail-closed 拦截。累积内容超限
	// 时不阻断 hook 流程，finalizeRun 会统一报错。
	var hookResponse *RuntimeHookResponse
	if responseText, contentErr := accumulator.Content(); contentErr == nil {
		hookResponse = &RuntimeHookResponse{Text: responseText}
	}
	ctx, req, hookEvents, err = s.executeResponseHooks(ctx, req, &handle, hookResponse)
	if !sendRuntimeEvents(ctx, out, hookEvents) {
		s.finishInterrupted(ctx, req, out, adapterStep, active, &handle)
		return
	}
	if err != nil {
		s.failDuringStream(ctx, span, req, out, err, ErrorStageHook)
		return
	}
	finalEvent, completedEvent, err := s.finalizeRun(ctx, req, accumulator, pendingRunCompleted)
	if err != nil {
		s.failDuringStream(ctx, span, req, out, err, ErrorStageFinalization)
		return
	}
	if !sendRuntimeEvent(ctx, out, finalEvent) {
		return
	}
	sendRuntimeEvent(ctx, out, completedEvent)
}

func (s *RuntimeService) observeRuntimeNestedStep(ctx context.Context, event observability.AgentEvent, spans map[string]observability.Span) observability.AgentEvent {
	if event.StepID == "" || !isRuntimeStepLifecycleEvent(event.EventType) || s.Tracer == nil {
		return event
	}
	switch event.EventType {
	case EventRuntimeStepStarted:
		span := spans[event.StepID]
		if span == nil {
			var payload struct {
				Kind StepKind `json:"kind"`
				Name string   `json:"name"`
			}
			_ = json.Unmarshal(event.Payload, &payload)
			_, span = s.Tracer.Start(ctx, "runtime.step",
				observability.String("step_id", event.StepID),
				observability.String("step_kind", string(payload.Kind)),
				observability.String("step_name", payload.Name),
			)
			spans[event.StepID] = span
		}
		return attachRuntimeSpanContext(event, span)
	case EventRuntimeStepCompleted, EventRuntimeStepFailed, EventRuntimeStepCancelled:
		span := spans[event.StepID]
		if span == nil {
			return event
		}
		event = attachRuntimeSpanContext(event, span)
		if event.EventType == EventRuntimeStepFailed && event.Error != nil {
			span.RecordError(errors.New(event.Error.Message))
		}
		span.End()
		delete(spans, event.StepID)
		return event
	default:
		return event
	}
}

func attachRuntimeSpanContext(event observability.AgentEvent, span observability.Span) observability.AgentEvent {
	if span == nil {
		return event
	}
	tc := span.TraceContext()
	event.TraceID = tc.TraceID
	event.SpanID = tc.SpanID
	event.ParentSpanID = tc.ParentSpanID
	return event
}

func endRuntimeNestedSpans(spans map[string]observability.Span) {
	for stepID, span := range spans {
		if span != nil {
			span.End()
		}
		delete(spans, stepID)
	}
}

func (s *RuntimeService) failDuringStream(ctx context.Context, span observability.Span, req RunRequest, out chan<- observability.AgentEvent, err error, stage RuntimeErrorStage) {
	span.RecordError(err)
	runtimeErr := s.classifyError(err, stage)
	ctx, req, hookEvents, hookErr := s.executeErrorHooks(ctx, req, nil, runtimeErr)
	sendRuntimeEvents(ctx, out, hookEvents)
	if hookErr != nil {
		span.RecordError(hookErr)
	}
	if appendErr := s.appendAndSend(ctx, req, out, observability.AgentEvent{
		EventType:  EventRunFailed,
		Visibility: observability.VisibilityDebug,
		Payload:    JSONPayload(runtimeErr.Payload()),
		Error:      runtimeErr.EventError(),
	}); appendErr != nil {
		span.RecordError(appendErr)
	}
	if failErr := s.failRun(ctx, req, runtimeErr); failErr != nil {
		span.RecordError(failErr)
	}
}

func (s *RuntimeService) finishInterrupted(ctx context.Context, req RunRequest, out chan<- observability.AgentEvent, adapterStep *StepSnapshot, active *activeRun, handle *AgentHandle) {
	err := ctx.Err()
	if err == nil {
		return
	}
	persistCtx := context.WithoutCancel(ctx)
	interruptType := "cancel"
	if errors.Is(err, context.DeadlineExceeded) {
		interruptType = "timeout"
	}
	persistCtx, req, hookEvents, hookErr := s.executeInterruptHooks(persistCtx, req, handle, RuntimeHookInterrupt{ControlType: interruptType})
	trySendRuntimeEvents(out, hookEvents)
	if hookErr != nil {
		_ = s.failRun(persistCtx, req, hookErr)
		return
	}
	if adapterStep != nil {
		var stepEvents []observability.AgentEvent
		var stepErr error
		if errors.Is(err, context.DeadlineExceeded) {
			persistCtx, req, stepEvents, stepErr = s.failStepWithHooks(persistCtx, req, adapterStep, StepKindRuntimeAdapter, "runtime_adapter", handle, err)
		} else {
			persistCtx, req, stepEvents, stepErr = s.cancelStepWithHooks(persistCtx, req, adapterStep, StepKindRuntimeAdapter, "runtime_adapter", handle)
		}
		if stepErr == nil {
			trySendRuntimeEvents(out, stepEvents)
		} else {
			s.Logger.Error(persistCtx, "runtime interrupted step persistence failed", stepErr,
				observability.String("run_id", req.RunID), observability.String("step_id", adapterStep.StepID))
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		event, expireErr := s.expireRunWithEvent(persistCtx, req, err)
		if expireErr == nil {
			trySendRuntimeEvent(out, event)
		} else {
			s.Logger.Error(persistCtx, "runtime expiration persistence failed", expireErr, observability.String("run_id", req.RunID))
		}
		return
	}
	cancel := CancelRequest{SessionID: req.SessionID, RunID: req.RunID, Reason: "execution context cancelled"}
	if requested := active.cancellation(); requested != nil {
		cancel = *requested
	}
	event, cancelErr := s.cancelRunWithEvent(persistCtx, req, cancel)
	if cancelErr == nil {
		trySendRuntimeEvent(out, event)
	} else {
		s.Logger.Error(persistCtx, "runtime cancellation persistence failed", cancelErr, observability.String("run_id", req.RunID))
	}
}

func (s *RuntimeService) handleInterrupt(ctx context.Context, req RunRequest, out chan<- observability.AgentEvent, event observability.AgentEvent, adapterStep *StepSnapshot, active *activeRun, handle *AgentHandle) error {
	// platform child run 与父 Run 走同一条 canonical control 链路：child 的
	// control 事实建在 child run 上，但只被 Agent Gateway 内部消费——gateway
	// 把 proposal 上交给父 Run，由父创建用户可见的 ControlRequest（方案 §6.5）。
	event = s.normalizeEvent(ctx, req, event)
	var err error
	event.Payload, err = sanitizeControlRequestPayload(event.Payload)
	if err != nil {
		return err
	}
	provider, ok := active.runtimeValue().(RuntimeInterruptBindingProvider)
	if !ok {
		return ErrInterruptBindingMissing
	}
	binding, err := provider.TakeInterruptBinding(ctx, event)
	if err != nil {
		return err
	}
	interrupt := RuntimeHookInterrupt{
		CheckpointID:     binding.CheckpointID,
		ControlRequestID: binding.ControlRequestID,
		ControlType:      controlRequestType(event.Payload),
	}
	binding.TenantID = req.TenantID
	binding.Type = controlRequestType(event.Payload)
	binding.Event = event
	ctx, req, hookEvents, err := s.executeInterruptHooks(ctx, req, handle, interrupt)
	if !sendRuntimeEvents(ctx, out, hookEvents) {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	if s.ControlRequests == nil {
		return ErrControlRequestCreatorMissing
	}
	publicEvent, err := s.ControlRequests.CreateControlRequest(ctx, ControlRequestCreateRequest{
		TenantID:      req.TenantID,
		UserID:        req.UserID,
		SessionID:     req.SessionID,
		RunID:         req.RunID,
		Type:          binding.Type,
		CheckpointID:  binding.CheckpointID,
		RequestID:     binding.ControlRequestID,
		ResumeToken:   binding.ResumeToken,
		PromptPreview: controlPromptPreview(event.Payload),
		Event:         event,
	})
	if err != nil {
		return err
	}
	event = publicEvent
	writes := make([]storagewrite.Write, 0, 1)
	if adapterStep != nil {
		writes = append(writes, storagewrite.Write{Store: storagewrite.StoreStep, Operation: storagewrite.OperationUpdate, Ref: "step:" + req.RunID + ":" + adapterStep.StepID + ":waiting_control", Payload: StepStoreWrite{Action: StepStoreActionWait, StepID: adapterStep.StepID}})
	}
	if len(writes) > 0 {
		if _, err := s.Writer.Execute(ctx, s.plan(ctx, req, "runtime.step_waiting_control", writes...)); err != nil {
			return err
		}
	}
	if !sendRuntimeEvent(ctx, out, event) {
		return ctx.Err()
	}
	return nil
}

func sendRuntimeEvent(ctx context.Context, out chan<- observability.AgentEvent, event observability.AgentEvent) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- event:
		return true
	}
}

// trySendRuntimeEvent is only for a terminal event after ctx cancellation.
// Durable state is authoritative; transport delivery must not keep a Run alive
// when its consumer has already disconnected.
func trySendRuntimeEvent(out chan<- observability.AgentEvent, event observability.AgentEvent) bool {
	select {
	case out <- event:
		return true
	default:
		return false
	}
}

func trySendRuntimeEvents(out chan<- observability.AgentEvent, events []observability.AgentEvent) {
	for _, event := range events {
		if !trySendRuntimeEvent(out, event) {
			return
		}
	}
}

func sendRuntimeEvents(ctx context.Context, out chan<- observability.AgentEvent, events []observability.AgentEvent) bool {
	for _, event := range events {
		if !sendRuntimeEvent(ctx, out, event) {
			return false
		}
	}
	return true
}

func controlRequestType(payload json.RawMessage) string {
	var value struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(payload, &value)
	return value.Type
}

func controlPromptPreview(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var value any
	if err := json.Unmarshal(payload, &value); err != nil {
		return ""
	}
	best := findControlPromptString(value)
	if len(best) > 8000 {
		return best[:8000]
	}
	return best
}

func findControlPromptString(value any) string {
	var best string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			if controlPromptScore(x) > controlPromptScore(best) {
				best = x
			}
		case []any:
			for _, item := range x {
				walk(item)
			}
		case map[string]any:
			for _, key := range []string{"prompt", "question", "message", "content", "text", "markdown", "title", "description", "info"} {
				if child, ok := x[key]; ok {
					walk(child)
				}
			}
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(value)
	return best
}

func controlPromptScore(text string) int {
	score := len(text)
	if strings.Contains(text, "**") {
		score += 5000
	}
	if strings.Contains(text, "- ✅") || strings.Contains(text, "- ❓") || strings.Contains(text, "- ❌") {
		score += 10000
	}
	if strings.Contains(text, "请确认") || strings.Contains(text, "是否") {
		score += 2000
	}
	return score
}

func sanitizeControlRequestPayload(payload json.RawMessage) (json.RawMessage, error) {
	if len(payload) == 0 {
		return payload, nil
	}
	var value map[string]any
	if err := json.Unmarshal(payload, &value); err != nil {
		return nil, fmt.Errorf("invalid control request payload: %w", err)
	}
	delete(value, "resume_token")
	return JSONPayload(value), nil
}

func (s *RuntimeService) appendAndSend(ctx context.Context, req RunRequest, out chan<- observability.AgentEvent, event observability.AgentEvent) error {
	event = s.normalizeEvent(ctx, req, event)
	sequence, err := s.appendEventWithSequence(ctx, req, event)
	if err != nil {
		return err
	}
	event.Sequence = sequence
	if !sendRuntimeEvent(ctx, out, event) {
		return ctx.Err()
	}
	return nil
}

func (s *RuntimeService) persistAdapterEvent(ctx context.Context, req RunRequest, event observability.AgentEvent) (int64, error) {
	writes := make([]storagewrite.Write, 0, 2)
	switch event.EventType {
	case EventRuntimeStepStarted:
		if event.StepID == "" {
			return 0, ErrInvalidStorePayload
		}
		start := StepStart{StepID: event.StepID}
		if err := json.Unmarshal(event.Payload, &start); err != nil || start.Kind == "" {
			return 0, ErrInvalidStorePayload
		}
		start.StepID = event.StepID
		writes = append(writes, storagewrite.Write{
			Store:     storagewrite.StoreStep,
			Operation: storagewrite.OperationInsert,
			Ref:       "step:" + req.RunID + ":" + event.StepID + ":start",
			Payload:   StepStoreWrite{Action: StepStoreActionStart, StepID: event.StepID, Step: start},
		})
	case EventRuntimeStepCompleted:
		if event.StepID == "" {
			return 0, ErrInvalidStorePayload
		}
		writes = append(writes, storagewrite.Write{
			Store:     storagewrite.StoreStep,
			Operation: storagewrite.OperationUpdate,
			Ref:       "step:" + req.RunID + ":" + event.StepID + ":completed",
			Payload:   StepStoreWrite{Action: StepStoreActionComplete, StepID: event.StepID},
		})
	case EventRuntimeStepFailed:
		if event.StepID == "" {
			return 0, ErrInvalidStorePayload
		}
		message := "runtime nested step failed"
		if event.Error != nil && event.Error.Message != "" {
			message = event.Error.Message
		}
		writes = append(writes, storagewrite.Write{
			Store:     storagewrite.StoreStep,
			Operation: storagewrite.OperationUpdate,
			Ref:       "step:" + req.RunID + ":" + event.StepID + ":failed",
			Payload:   StepStoreWrite{Action: StepStoreActionFail, StepID: event.StepID, Error: message},
		})
	case EventRuntimeStepCancelled:
		if event.StepID == "" {
			return 0, ErrInvalidStorePayload
		}
		writes = append(writes, storagewrite.Write{
			Store:     storagewrite.StoreStep,
			Operation: storagewrite.OperationUpdate,
			Ref:       "step:" + req.RunID + ":" + event.StepID + ":cancelled",
			Payload:   StepStoreWrite{Action: StepStoreActionCancel, StepID: event.StepID},
		})
	}
	writes = append(writes, storagewrite.Write{
		Store:     storagewrite.StoreEvent,
		Operation: storagewrite.OperationAppend,
		Ref:       event.EventID,
		Payload:   event,
	})
	reason := "runtime.append_adapter_event." + string(event.EventType)
	if event.StepID != "" {
		reason += "." + event.StepID
	}
	result, err := s.Writer.Execute(ctx, s.plan(ctx, req, reason, writes...))
	if err != nil {
		return 0, err
	}
	for _, write := range result.Required {
		if write.Write.Store == storagewrite.StoreEvent {
			return write.Receipt.Sequence, nil
		}
	}
	return 0, nil
}

func isRuntimeStepLifecycleEvent(eventType observability.EventType) bool {
	return eventType == EventRuntimeStepStarted || eventType == EventRuntimeStepCompleted || eventType == EventRuntimeStepFailed || eventType == EventRuntimeStepCancelled
}

func (s *RuntimeService) buildModelContext(ctx context.Context, req RunRequest, runtime AgentRuntime, handle AgentHandle) (context.Context, RunRequest, []observability.AgentEvent, error) {
	ctx = withRuntimeCapabilityBinding(ctx, handle.Binding.Capabilities)
	ctx, req, step, events, err := s.startStepWithHooks(ctx, req, StepStart{Kind: StepKindModelContext, Name: "model_context"}, &handle)
	if err != nil {
		s.Logger.Error(ctx, "model context step start failed", err,
			observability.String("run_id", req.RunID),
			observability.String("agent_id", req.Definition.AgentID),
		)
		return ctx, req, events, err
	}
	pkg, err := s.Assembler.Build(ctx, RuntimeContextAssemblyRequest{
		Run:          req,
		Handle:       handle,
		Capabilities: runtime.Capabilities(ctx),
		Trace:        observability.MustTraceContext(ctx),
	})
	if err != nil {
		s.Logger.Error(ctx, "model context assembly failed", err,
			observability.String("run_id", req.RunID),
			observability.String("agent_id", req.Definition.AgentID),
			observability.String("step_id", step.StepID),
		)
		updatedCtx, updatedReq, failedEvents, writeErr := s.failStepWithHooks(ctx, req, step, StepKindModelContext, "model_context", &handle, err)
		events = append(events, failedEvents...)
		if writeErr != nil {
			return updatedCtx, updatedReq, events, writeErr
		}
		return updatedCtx, updatedReq, events, err
	}
	if err := validateRuntimeCapabilityBinding(handle.Binding.Capabilities, pkg.Capabilities); err != nil {
		updatedCtx, updatedReq, failedEvents, writeErr := s.failStepWithHooks(ctx, req, step, StepKindModelContext, "model_context", &handle, err)
		events = append(events, failedEvents...)
		if writeErr != nil {
			return updatedCtx, updatedReq, events, writeErr
		}
		return updatedCtx, updatedReq, events, err
	}
	ctx = WithModelContextPackage(ctx, pkg)
	toolSnapshotID := ""
	if pkg.Capabilities.ToolSnapshot != nil {
		toolSnapshotID = pkg.Capabilities.ToolSnapshot.SnapshotID
	}
	event := s.normalizeEvent(ctx, req, observability.AgentEvent{
		EventType:  EventModelContextBuilt,
		Visibility: observability.VisibilityDebug,
		Payload: JSONPayload(map[string]any{
			"package_id":             pkg.PackageID,
			"context_hash":           pkg.ContextHash,
			"context_snapshot_ref":   pkg.Run.ContextSnapshotRef,
			"tool_snapshot_id":       toolSnapshotID,
			"token_count":            pkg.Messages.TokenCount,
			"trimmed_count":          pkg.Messages.TrimmedCount,
			"compaction_records":     pkg.Messages.CompactionRecords,
			"compaction_policy_hash": pkg.RuntimeConstraints.CompactionPolicy.PolicyHash,
			"phase":                  "runtime_assembly",
		}),
	})
	sequence, err := s.appendEventWithSequence(ctx, req, event)
	if err != nil {
		updatedCtx, updatedReq, failedEvents, writeErr := s.failStepWithHooks(ctx, req, step, StepKindModelContext, "model_context", &handle, err)
		events = append(events, failedEvents...)
		if writeErr != nil {
			return updatedCtx, updatedReq, events, writeErr
		}
		return updatedCtx, updatedReq, events, err
	}
	event.Sequence = sequence
	events = append(events, event)
	ctx, req, completedEvents, err := s.completeStepWithHooks(ctx, req, step, StepKindModelContext, "model_context", &handle)
	events = append(events, completedEvents...)
	if err != nil {
		return ctx, req, events, err
	}
	return ctx, req, events, nil
}

func (s *RuntimeService) failBeforeStream(ctx context.Context, req RunRequest, err error) error {
	runtimeErr := s.classifyError(err, ErrorStageRuntimeAdapter)
	_, _, _, _ = s.executeErrorHooks(ctx, req, nil, runtimeErr)
	event := s.normalizeEvent(ctx, req, observability.AgentEvent{
		EventType:  EventRunFailed,
		Visibility: observability.VisibilityDebug,
		Payload:    JSONPayload(runtimeErr.Payload()),
		Error:      runtimeErr.EventError(),
	})
	_, writeErr := s.Writer.Execute(ctx, s.plan(ctx, req, "runtime.fail_run_before_stream",
		storagewrite.Write{Store: storagewrite.StoreEvent, Operation: storagewrite.OperationAppend, Ref: event.EventID, Payload: event},
		storagewrite.Write{Store: storagewrite.StoreRun, Operation: storagewrite.OperationUpdate, Ref: "run:" + req.RunID + ":failed", Payload: RunStoreWrite{Action: RunStoreActionFail, RuntimeError: runtimeErr}},
	))
	return writeErr
}

func (s *RuntimeService) failContextBeforeStream(ctx context.Context, req RunRequest, err error) error {
	runtimeErr := s.classifyError(err, ErrorStageModelContext)
	_, _, _, _ = s.executeErrorHooks(ctx, req, nil, runtimeErr)
	contextEvent := s.normalizeEvent(ctx, req, observability.AgentEvent{
		EventType:  EventModelContextBuildFailed,
		Visibility: observability.VisibilityDebug,
		Payload:    JSONPayload(runtimeErr.Payload()),
		Error:      runtimeErr.EventError(),
	})
	runEvent := s.normalizeEvent(ctx, req, observability.AgentEvent{
		EventType:  EventRunFailed,
		Visibility: observability.VisibilityDebug,
		Payload:    JSONPayload(runtimeErr.Payload()),
		Error:      runtimeErr.EventError(),
	})
	_, writeErr := s.Writer.Execute(ctx, s.plan(ctx, req, "runtime.fail_model_context_before_stream",
		storagewrite.Write{Store: storagewrite.StoreEvent, Operation: storagewrite.OperationAppend, Ref: contextEvent.EventID, Payload: contextEvent},
		storagewrite.Write{Store: storagewrite.StoreEvent, Operation: storagewrite.OperationAppend, Ref: runEvent.EventID, Payload: runEvent},
		storagewrite.Write{Store: storagewrite.StoreRun, Operation: storagewrite.OperationUpdate, Ref: "run:" + req.RunID + ":failed", Payload: RunStoreWrite{Action: RunStoreActionFail, RuntimeError: runtimeErr}},
	))
	return writeErr
}

func (s *RuntimeService) startStep(ctx context.Context, req RunRequest, step StepStart) (*StepSnapshot, observability.AgentEvent, error) {
	if step.StepID == "" {
		step.StepID = string(step.Kind) + "_" + s.IDs.NewRequestID()
	}
	event := s.normalizeEvent(ctx, req, observability.AgentEvent{
		StepID:     step.StepID,
		EventType:  EventRuntimeStepStarted,
		Visibility: observability.VisibilityDebug,
		Payload: JSONPayload(map[string]any{
			"step_id":        step.StepID,
			"kind":           step.Kind,
			"name":           step.Name,
			"parent_step_id": step.ParentStepID,
			"metadata":       step.Metadata,
		}),
	})
	reason := "runtime.start_step." + string(step.Kind) + "." + step.StepID
	result, err := s.Writer.Execute(ctx, s.plan(ctx, req, reason,
		storagewrite.Write{Store: storagewrite.StoreStep, Operation: storagewrite.OperationInsert, Ref: "step:" + req.RunID + ":" + step.StepID + ":start", Payload: StepStoreWrite{Action: StepStoreActionStart, StepID: step.StepID, Step: step}},
		storagewrite.Write{Store: storagewrite.StoreEvent, Operation: storagewrite.OperationAppend, Ref: event.EventID, Payload: event},
	))
	if err != nil {
		return nil, observability.AgentEvent{}, err
	}
	event.Sequence = eventSequence(result)
	return &StepSnapshot{
		RunID:        req.RunID,
		StepID:       step.StepID,
		Kind:         step.Kind,
		Name:         step.Name,
		ParentStepID: step.ParentStepID,
		Metadata:     cloneStringMap(step.Metadata),
		Status:       StepStatusRunning,
		StartedAt:    event.CreatedAt,
	}, event, nil
}

func (s *RuntimeService) completeStep(ctx context.Context, req RunRequest, step *StepSnapshot, kind StepKind) (observability.AgentEvent, error) {
	if step == nil || step.StepID == "" {
		return observability.AgentEvent{}, ErrInvalidStorePayload
	}
	event := s.normalizeEvent(ctx, req, observability.AgentEvent{
		StepID:     step.StepID,
		EventType:  EventRuntimeStepCompleted,
		Visibility: observability.VisibilityDebug,
		Payload: JSONPayload(map[string]any{
			"step_id": step.StepID,
			"kind":    kind,
			"status":  StepStatusCompleted,
		}),
	})
	reason := "runtime.complete_step." + string(kind) + "." + step.StepID
	result, err := s.Writer.Execute(ctx, s.plan(ctx, req, reason,
		storagewrite.Write{Store: storagewrite.StoreStep, Operation: storagewrite.OperationUpdate, Ref: "step:" + req.RunID + ":" + step.StepID + ":completed", Payload: StepStoreWrite{Action: StepStoreActionComplete, StepID: step.StepID}},
		storagewrite.Write{Store: storagewrite.StoreEvent, Operation: storagewrite.OperationAppend, Ref: event.EventID, Payload: event},
	))
	event.Sequence = eventSequence(result)
	return event, err
}

func (s *RuntimeService) failStep(ctx context.Context, req RunRequest, step *StepSnapshot, kind StepKind, stepErr error) (observability.AgentEvent, error) {
	if step == nil || step.StepID == "" {
		return observability.AgentEvent{}, ErrInvalidStorePayload
	}
	runtimeErr := s.classifyError(stepErr, errorStageForStep(kind))
	event := s.normalizeEvent(ctx, req, observability.AgentEvent{
		StepID:     step.StepID,
		EventType:  EventRuntimeStepFailed,
		Visibility: observability.VisibilityDebug,
		Payload: JSONPayload(map[string]any{
			"step_id": step.StepID,
			"kind":    kind,
			"status":  StepStatusFailed,
			"error":   runtimeErr.Payload(),
		}),
		Error: runtimeErr.EventError(),
	})
	reason := "runtime.fail_step." + string(kind) + "." + step.StepID
	result, err := s.Writer.Execute(ctx, s.plan(ctx, req, reason,
		storagewrite.Write{Store: storagewrite.StoreStep, Operation: storagewrite.OperationUpdate, Ref: "step:" + req.RunID + ":" + step.StepID + ":failed", Payload: StepStoreWrite{Action: StepStoreActionFail, StepID: step.StepID, Error: runtimeErr.Error()}},
		storagewrite.Write{Store: storagewrite.StoreEvent, Operation: storagewrite.OperationAppend, Ref: event.EventID, Payload: event},
	))
	event.Sequence = eventSequence(result)
	return event, err
}

func (s *RuntimeService) cancelStep(ctx context.Context, req RunRequest, step *StepSnapshot, kind StepKind) (observability.AgentEvent, error) {
	if step == nil || step.StepID == "" {
		return observability.AgentEvent{}, ErrInvalidStorePayload
	}
	event := s.normalizeEvent(ctx, req, observability.AgentEvent{
		StepID:     step.StepID,
		EventType:  EventRuntimeStepCancelled,
		Visibility: observability.VisibilityDebug,
		Payload: JSONPayload(map[string]any{
			"step_id": step.StepID,
			"kind":    kind,
			"status":  StepStatusCancelled,
		}),
	})
	reason := "runtime.cancel_step." + string(kind) + "." + step.StepID
	result, err := s.Writer.Execute(ctx, s.plan(ctx, req, reason,
		storagewrite.Write{Store: storagewrite.StoreStep, Operation: storagewrite.OperationUpdate, Ref: "step:" + req.RunID + ":" + step.StepID + ":cancelled", Payload: StepStoreWrite{Action: StepStoreActionCancel, StepID: step.StepID}},
		storagewrite.Write{Store: storagewrite.StoreEvent, Operation: storagewrite.OperationAppend, Ref: event.EventID, Payload: event},
	))
	event.Sequence = eventSequence(result)
	return event, err
}

func (s *RuntimeService) startRun(ctx context.Context, req RunRequest) error {
	_, err := s.Writer.Execute(ctx, s.plan(ctx, req, "runtime.start_run",
		storagewrite.Write{Store: storagewrite.StoreRun, Operation: storagewrite.OperationInsert, Ref: "run:" + req.RunID + ":start", Payload: RunStoreWrite{Action: RunStoreActionStart, Run: req}},
	))
	return err
}

func (s *RuntimeService) bindRuntime(ctx context.Context, req RunRequest, binding RuntimeBinding) error {
	_, err := s.Writer.Execute(ctx, s.plan(ctx, req, "runtime.bind_runtime",
		storagewrite.Write{
			Store:     storagewrite.StoreRun,
			Operation: storagewrite.OperationUpdate,
			Ref:       "run:" + req.RunID + ":runtime_binding",
			Payload:   RunStoreWrite{Action: RunStoreActionBindRuntime, Binding: binding},
		},
	))
	return err
}

func (s *RuntimeService) bindRuntimeCapabilities(ctx context.Context, req RunRequest, binding RuntimeCapabilityBinding) error {
	_, err := s.Writer.Execute(ctx, s.plan(ctx, req, "runtime.bind_capabilities",
		storagewrite.Write{
			Store:     storagewrite.StoreRun,
			Operation: storagewrite.OperationUpdate,
			Ref:       "run:" + req.RunID + ":capability_binding",
			Payload:   RunStoreWrite{Action: RunStoreActionBindCapabilities, Capabilities: binding},
		},
	))
	return err
}

func productionAssemblerReady(assembler RuntimeContextAssembler) bool {
	validator, ok := assembler.(productionValidator)
	return ok && validator.ValidateProduction() == nil
}

func (s *RuntimeService) beginResume(ctx context.Context, runReq RunRequest, req ResumeRequest, attemptID string) (observability.AgentEvent, error) {
	event := s.normalizeEvent(ctx, runReq, observability.AgentEvent{
		EventType:  EventResumeAccepted,
		Visibility: observability.VisibilityInternal,
		Payload: JSONPayload(map[string]string{
			"checkpoint_id":      req.CheckpointID,
			"control_request_id": req.ControlRequestID,
			"attempt_id":         attemptID,
		}),
	})
	_, err := s.Writer.Execute(ctx, s.plan(ctx, runReq, "runtime.begin_resume",
		storagewrite.Write{
			Store:          storagewrite.StoreRun,
			Operation:      storagewrite.OperationUpdate,
			Ref:            "run:" + req.RunID + ":resume:" + attemptID + ":claim",
			IdempotencyKey: req.RunID + ":resume:" + attemptID + ":claim",
			Payload: RunStoreWrite{
				Action:          RunStoreActionBeginResume,
				Resume:          req,
				ResumeAttemptID: attemptID,
				Event:           event,
			},
		},
	))
	return event, err
}

func (s *RuntimeService) failResumeAttempt(ctx context.Context, req RunRequest, attemptID string, runtimeErr *RuntimeError) error {
	persistCtx, cancel := runtimeCleanupContext(ctx)
	defer cancel()
	if runtimeErr == nil {
		runtimeErr = s.classifyError(errors.New("runtime resume attempt failed"), ErrorStageResume)
	}
	payload := runtimeErr.Payload()
	payload["attempt_id"] = attemptID
	event := s.normalizeEvent(persistCtx, req, observability.AgentEvent{
		EventType:  EventResumeFailed,
		Visibility: observability.VisibilityInternal,
		Payload:    JSONPayload(payload),
		Error:      runtimeErr.EventError(),
	})
	_, err := s.Writer.Execute(persistCtx, s.plan(persistCtx, req, "runtime.fail_resume_attempt",
		storagewrite.Write{
			Store:          storagewrite.StoreRun,
			Operation:      storagewrite.OperationUpdate,
			Ref:            "run:" + req.RunID + ":resume:" + attemptID + ":failed",
			IdempotencyKey: req.RunID + ":resume:" + attemptID + ":failed",
			Payload: RunStoreWrite{
				Action:          RunStoreActionFailResume,
				ResumeAttemptID: attemptID,
				Event:           event,
				RuntimeError:    runtimeErr,
			},
		},
	))
	return err
}

func (s *RuntimeService) classifyResumePreparationError(err error, stage RuntimeErrorStage) *RuntimeError {
	if errors.Is(err, context.Canceled) {
		return NewRuntimeError(ErrorCancelled, "RESUME_PREPARATION_CANCELLED", "resume preparation cancelled").
			WithRetryable(true).
			WithCause(err)
	}
	return s.classifyError(err, stage)
}

func (s *RuntimeService) activateResume(ctx context.Context, req RunRequest, resumeAttemptID string) error {
	_, err := s.Writer.Execute(ctx, s.plan(ctx, req, "runtime.activate_resume",
		storagewrite.Write{
			Store:          storagewrite.StoreRun,
			Operation:      storagewrite.OperationUpdate,
			Ref:            "run:" + req.RunID + ":resume:" + resumeAttemptID + ":activate",
			IdempotencyKey: req.RunID + ":resume:" + resumeAttemptID + ":activate",
			Payload: RunStoreWrite{
				Action:          RunStoreActionActivateResume,
				ResumeAttemptID: resumeAttemptID,
			},
		},
	))
	return err
}

func (s *RuntimeService) recordModelContextBuildFailed(ctx context.Context, req RunRequest, runtimeErr *RuntimeError) error {
	persistCtx, cancel := runtimeCleanupContext(ctx)
	defer cancel()
	event := s.normalizeEvent(persistCtx, req, observability.AgentEvent{
		EventType:  EventModelContextBuildFailed,
		Visibility: observability.VisibilityDebug,
		Payload:    JSONPayload(runtimeErr.Payload()),
		Error:      runtimeErr.EventError(),
	})
	return s.appendEvent(persistCtx, req, event)
}

func runtimeCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	// 恢复 claim 已经落库后，即使请求超时也必须有机会归还 token，避免 Run 永久卡在 resuming。
	return context.WithTimeout(context.WithoutCancel(ctx), runtimeCleanupTimeout)
}

func (s *RuntimeService) failRun(ctx context.Context, req RunRequest, runErr error) error {
	// 终态状态写入必须与调用方 context 解耦。run_failed 事件一旦发往下游，
	// 父 Run（Agent Gateway 的 inline collector）会立即结束并取消继承下来的
	// context；若沿用原 context，RunStore 更新会因取消而失败，Run 永久停留在
	// running，进而污染 stuck-run 监控、失败率统计与重启后的 replay 一致性。
	persistCtx, cancel := runtimeCleanupContext(ctx)
	defer cancel()
	runtimeErr := s.classifyError(runErr, ErrorStageRuntimeAdapter)
	_, err := s.Writer.Execute(persistCtx, s.plan(persistCtx, req, "runtime.fail_run",
		storagewrite.Write{Store: storagewrite.StoreRun, Operation: storagewrite.OperationUpdate, Ref: "run:" + req.RunID + ":failed", Payload: RunStoreWrite{Action: RunStoreActionFail, RuntimeError: runtimeErr}},
	))
	return err
}

func (s *RuntimeService) cancelRun(ctx context.Context, req CancelRequest) error {
	// 与 failRun 同理：取消终态落库不能受上游 context 取消影响。
	persistCtx, cleanup := runtimeCleanupContext(ctx)
	defer cleanup()
	runReq := RunRequest{SessionID: req.SessionID, RunID: req.RunID}
	_, err := s.Writer.Execute(persistCtx, s.plan(persistCtx, runReq, "runtime.cancel_run",
		storagewrite.Write{Store: storagewrite.StoreRun, Operation: storagewrite.OperationUpdate, Ref: "run:" + req.RunID + ":cancelled", Payload: RunStoreWrite{Action: RunStoreActionCancel, Cancel: req}},
	))
	return err
}

func (s *RuntimeService) cancelRunWithEvent(ctx context.Context, req RunRequest, cancel CancelRequest) (observability.AgentEvent, error) {
	runtimeErr := NewRuntimeError(ErrorCancelled, "RUN_CANCELLED", "runtime execution cancelled")
	event := s.normalizeEvent(ctx, req, observability.AgentEvent{
		EventID:        "evt_" + stableRuntimeFactID(req.RunID, "run_cancelled"),
		IdempotencyKey: req.RunID + ":run_cancelled",
		EventType:      EventRunCancelled,
		Visibility:     observability.VisibilityDebug,
		Payload:        JSONPayload(runtimeErr.Payload()),
		Error:          runtimeErr.EventError(),
	})
	_, err := s.Writer.Execute(ctx, s.plan(ctx, req, "runtime.cancel_run",
		storagewrite.Write{Store: storagewrite.StoreEvent, Operation: storagewrite.OperationAppend, Ref: event.EventID, Payload: event},
		storagewrite.Write{Store: storagewrite.StoreRun, Operation: storagewrite.OperationUpdate, Ref: "run:" + req.RunID + ":cancelled", Payload: RunStoreWrite{Action: RunStoreActionCancel, Cancel: cancel}},
	))
	return event, err
}

func (s *RuntimeService) expireRunWithEvent(ctx context.Context, req RunRequest, runErr error) (observability.AgentEvent, error) {
	runtimeErr := s.classifyError(runErr, ErrorStageRuntimeAdapter)
	event := s.normalizeEvent(ctx, req, observability.AgentEvent{
		EventType:  EventRunExpired,
		Visibility: observability.VisibilityDebug,
		Payload:    JSONPayload(runtimeErr.Payload()),
		Error:      runtimeErr.EventError(),
	})
	_, err := s.Writer.Execute(ctx, s.plan(ctx, req, "runtime.expire_run",
		storagewrite.Write{Store: storagewrite.StoreEvent, Operation: storagewrite.OperationAppend, Ref: event.EventID, Payload: event},
		storagewrite.Write{Store: storagewrite.StoreRun, Operation: storagewrite.OperationUpdate, Ref: "run:" + req.RunID + ":expired", Payload: RunStoreWrite{Action: RunStoreActionExpire, RuntimeError: runtimeErr}},
	))
	return event, err
}

func executionContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithCancel(ctx)
}

func (s *RuntimeService) classifyError(err error, stage RuntimeErrorStage) *RuntimeError {
	if err == nil {
		err = errors.New("runtime operation failed")
	}
	classifier := s.Errors
	if classifier == nil {
		classifier = DefaultRuntimeErrorClassifier{}
	}
	return classifier.Classify(err, stage)
}

func errorStageForStep(kind StepKind) RuntimeErrorStage {
	switch kind {
	case StepKindModelContext:
		return ErrorStageModelContext
	case StepKindControlRequest:
		return ErrorStageResume
	default:
		return ErrorStageRuntimeAdapter
	}
}

func (s *RuntimeService) appendEvent(ctx context.Context, req RunRequest, event observability.AgentEvent) error {
	_, err := s.appendEventWithSequence(ctx, req, event)
	return err
}

func (s *RuntimeService) appendEventWithSequence(ctx context.Context, req RunRequest, event observability.AgentEvent) (int64, error) {
	result, err := s.Writer.Execute(ctx, s.plan(ctx, req, "runtime.append_event."+string(event.EventType),
		storagewrite.Write{Store: storagewrite.StoreEvent, Operation: storagewrite.OperationAppend, Ref: event.EventID, Payload: event},
	))
	if err != nil {
		return 0, err
	}
	if len(result.Required) == 0 {
		return 0, nil
	}
	return result.Required[0].Receipt.Sequence, nil
}

func eventSequence(result storagewrite.Result) int64 {
	for _, write := range result.Required {
		if write.Write.Store == storagewrite.StoreEvent {
			return write.Receipt.Sequence
		}
	}
	return 0
}

func (s *RuntimeService) plan(ctx context.Context, req RunRequest, reason string, required ...storagewrite.Write) storagewrite.Plan {
	tc := observability.MustTraceContext(ctx)
	traceID := tc.TraceID
	if traceID == "" {
		traceID = req.Trace.TraceID
	}
	if traceID == "" {
		traceID = req.RunID
	}
	tenantID := tc.TenantID
	if tenantID == "" {
		tenantID = req.TenantID
	}
	if tenantID == "" {
		tenantID = "default"
	}
	return storagewrite.Plan{
		TraceID:        traceID,
		TenantID:       tenantID,
		SessionID:      req.SessionID,
		RunID:          req.RunID,
		IdempotencyKey: fmt.Sprintf("%s:%s", req.RunID, reason),
		Reason:         reason,
		RequiredWrites: required,
	}
}

func (s *RuntimeService) normalizeEvent(ctx context.Context, req RunRequest, event observability.AgentEvent) observability.AgentEvent {
	tc := observability.MustTraceContext(ctx)
	if event.EventID == "" {
		event.EventID = s.IDs.NewEventID()
	}
	if event.SchemaVersion == "" {
		event.SchemaVersion = observability.AgentEventSchemaVersion
	}
	if event.TraceID == "" {
		event.TraceID = tc.TraceID
	}
	if event.SpanID == "" {
		event.SpanID = tc.SpanID
	}
	if event.SessionID == "" {
		event.SessionID = req.SessionID
	}
	if event.RunID == "" {
		event.RunID = req.RunID
	}
	if event.AgentID == "" {
		event.AgentID = req.Definition.AgentID
	}
	if event.AgentType == "" {
		event.AgentType = req.Definition.AgentType
	}
	if event.Runtime == "" {
		event.Runtime = string(req.Definition.Runtime.Type)
	}
	if event.Visibility == "" {
		event.Visibility = observability.VisibilityDebug
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	return event
}

func JSONPayload(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return data
}
