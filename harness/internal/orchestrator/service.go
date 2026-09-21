package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentbinding"
	"github.com/AGenUI/agenui-studio/harness/internal/agentregistry"
	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/dispatcher"
	"github.com/AGenUI/agenui-studio/harness/internal/executionmode"
	"github.com/AGenUI/agenui-studio/harness/internal/identifiercontract"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/scheduler"
	"github.com/AGenUI/agenui-studio/harness/internal/storagewrite"
)

const (
	bindingReason       = "orchestrator.agent_binding"
	bindingFailedReason = "orchestrator.agent_binding_failed"
)

type Options struct {
	BindingResolver BindingResolver
	// Registry 只保留给旧装配代码；Service 主链只依赖 BindingResolver。
	Registry   agentregistry.Registry
	Dispatcher Dispatcher
	Runtime    RuntimeController
	Writer     StorageWriteExecutor
	Tracer     observability.TraceProvider
	Logger     observability.StructuredLogger
	IDs        observability.IDGenerator
	Clock      func() time.Time
}

type Service struct {
	binder     BindingResolver
	dispatcher Dispatcher
	runtime    RuntimeController
	writer     StorageWriteExecutor
	tracer     observability.TraceProvider
	logger     observability.StructuredLogger
	ids        observability.IDGenerator
	clock      func() time.Time
}

func NewService(opts Options) (*Service, error) {
	binder := opts.BindingResolver
	if binder == nil && opts.Registry != nil {
		binder = agentbinding.NewResolver(registryConfigResolver{registry: opts.Registry})
	}
	if binder == nil || opts.Dispatcher == nil || opts.Runtime == nil || opts.Writer == nil {
		return nil, fmt.Errorf("%w: binding resolver, dispatcher, runtime and storage writer are required", ErrMissingDependency)
	}
	if opts.Tracer == nil {
		opts.Tracer = observability.NewNoopTracer("orchestrator")
	}
	if opts.Logger == nil {
		opts.Logger = observability.NoopLogger{}
	}
	if opts.IDs == nil {
		opts.IDs = observability.NewULIDGenerator("")
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	return &Service{
		binder:     binder,
		dispatcher: opts.Dispatcher,
		runtime:    opts.Runtime,
		writer:     opts.Writer,
		tracer:     opts.Tracer,
		logger:     opts.Logger,
		ids:        opts.IDs,
		clock:      opts.Clock,
	}, nil
}

func (s *Service) Run(ctx context.Context, req RunRequest) (*dispatcher.Result, error) {
	if err := validateRunEnvelope(req); err != nil {
		return nil, err
	}
	ctx, trace := s.prepareContext(ctx, req)
	ctx, span := s.tracer.Start(ctx, "orchestrator.bind_and_run",
		observability.String("run_id", req.BindingRequest.RunID),
		observability.String("binding_id", req.BindingRequest.BindingID),
	)
	defer span.End()
	trace = observability.MustTraceContext(ctx)

	resolved, err := s.binder.Resolve(ctx, req.BindingRequest)
	if err != nil {
		err = normalizeBindingError(err)
	}
	if err == nil {
		err = validateBindingResult(req, resolved)
	}
	if err != nil {
		err = s.failBinding(ctx, req, trace, err)
		span.RecordError(err)
		return nil, err
	}

	binding := resolved.Binding
	definition := resolved.Definition
	trace.AgentID = definition.AgentID
	trace.AgentType = definition.AgentType
	trace.AgentVersion = definition.Version
	trace.Runtime = string(definition.Runtime.Type)
	ctx = observability.WithTraceContext(ctx, trace)

	_, err = s.persistBinding(ctx, req, trace, resolved)
	if err != nil {
		err = safeWrap(ErrBindingPersistenceFailed, err)
		span.RecordError(err)
		return nil, err
	}

	runtimeReq := agentruntime.RunRequest{
		SessionID:          binding.SessionID,
		RunID:              binding.RunID,
		ParentRunID:        req.ParentRunID,
		Definition:         definition,
		Input:              append([]agentruntime.Message(nil), req.Input...),
		ContextSnapshotRef: req.ContextSnapshotRef,
		ConfigSnapshotRef:  binding.ConfigSnapshotRef,
		ConfigHash:         binding.ConfigHash,
		AgentBindingID:     binding.BindingID,
		ScopedData:         req.ScopedData,
		UserID:             req.UserID,
		TenantID:           req.TenantID,
		Trace:              trace,
		Metadata:           runtimeMetadata(req.Metadata, binding),
		ExternalFragments:  req.ExternalFragments,
		ResultVisibility:   req.ResultVisibility,
	}
	policy := trustedDispatchPolicy(req.Policy, binding.ExecutionMode, definition)
	result, err := s.dispatcher.Dispatch(ctx, dispatcher.DispatchRunRequest{
		Run:      runtimeReq,
		Policy:   policy,
		Priority: req.Priority,
	})
	if err != nil {
		if errors.Is(err, dispatcher.ErrExecutorNotReady) {
			if writeErr := s.persistPreRuntimeFailure(ctx, req, trace, binding); writeErr != nil {
				err = errors.Join(err, writeErr)
			}
		}
		err = safeWrap(ErrDispatchFailed, err)
		span.RecordError(err)
		return nil, err
	}
	s.logger.Info(ctx, "agent binding persisted; delegated to runtime",
		observability.String("binding_id", binding.BindingID),
		observability.String("binding_hash", binding.BindingHash),
		observability.String("config_hash", binding.ConfigHash),
	)
	// Dispatcher / RuntimeService 已经通过 StorageWritePlan 管理执行事件，禁止在这里二次写入。
	return result, nil
}

func (s *Service) persistPreRuntimeFailure(ctx context.Context, req RunRequest, trace observability.TraceContext, binding agentbinding.EffectiveBinding) error {
	eventError := observability.EventError{
		Code: "EXECUTOR_NOT_READY", Type: observability.EventErrorUpstream,
		Message: "execution mode executor is not ready", Retryable: false,
	}
	payload := PreRuntimeFailure{
		SchemaVersion: PreRuntimeFailureSchemaVersion,
		RunID:         binding.RunID, BindingID: binding.BindingID, Stage: "dispatch", Error: eventError,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return safeWrap(ErrPreRuntimeFailureWrite, err)
	}
	key := identifiercontract.ComposeIdempotencyKey(binding.RunID, "pre_runtime_failed", binding.BindingID)
	event := observability.AgentEvent{
		EventID: s.ids.NewEventID(), SchemaVersion: observability.AgentEventSchemaVersion,
		IdempotencyKey: identifiercontract.ComposeIdempotencyKey(key, "event"), TraceID: trace.TraceID, SpanID: trace.SpanID, ParentSpanID: trace.ParentSpanID,
		SessionID: binding.SessionID, RunID: binding.RunID, AgentID: binding.AgentID,
		EventType: observability.EventRunFailed, Visibility: observability.VisibilityDebug,
		Payload: raw, Error: &eventError, CreatedAt: s.clock(),
	}
	_, err = s.writer.Execute(ctx, storagewrite.Plan{
		TraceID: trace.TraceID, TenantID: req.TenantID, SessionID: binding.SessionID, RunID: binding.RunID,
		IdempotencyKey: key, Reason: "orchestrator.pre_runtime_failure", CommitPolicy: storagewrite.CommitPolicyRequiredFirst,
		RequiredWrites: []storagewrite.Write{
			{Store: storagewrite.StoreRun, Operation: storagewrite.OperationUpdate, Ref: "run:" + binding.RunID + ":pre_runtime_failed", Payload: payload},
			{Store: storagewrite.StoreEvent, Operation: storagewrite.OperationAppend, Ref: "event:" + binding.RunID + ":run_failed:pre_runtime", Payload: event},
		},
	})
	if err != nil {
		return safeWrap(ErrPreRuntimeFailureWrite, err)
	}
	return nil
}

func trustedDispatchPolicy(requested dispatcher.DispatchPolicy, mode executionmode.Mode, definition agentruntime.AgentDefinition) dispatcher.DispatchPolicy {
	policy := requested
	policy.ReasonCodes = append([]string(nil), requested.ReasonCodes...)
	policy.ExecutionMode = mode
	risk := definition.Metadata["tool_risk_level"]
	if risk == "medium" || risk == "high" || definition.Metadata["hitl_required_tools"] != "" {
		policy.HasSideEffect = true
		policy.ResumeRequired = true
		policy.ReasonCodes = appendUnique(policy.ReasonCodes, "registry_tool_risk")
	}
	return policy
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func (s *Service) Resume(ctx context.Context, req agentruntime.ResumeRequest) (<-chan observability.AgentEvent, error) {
	events, err := s.runtime.Resume(ctx, req)
	return events, safeWrap(ErrResumeFailed, err)
}

func (s *Service) Cancel(ctx context.Context, req agentruntime.CancelRequest) error {
	dispatchErr := s.dispatcher.Cancel(ctx, scheduler.CancelDispatchRequest{RunID: req.RunID, Reason: req.Reason})
	runtimeErr := s.runtime.Cancel(ctx, req)
	if errors.Is(dispatchErr, scheduler.ErrDispatchNotFound) {
		dispatchErr = nil
	}
	if errors.Is(runtimeErr, agentruntime.ErrRunNotFound) {
		// 尚未启动的 scheduled run 只有队列事实，没有 Runtime state。
		runtimeErr = nil
	}
	return safeWrap(ErrCancelFailed, errors.Join(dispatchErr, runtimeErr))
}

func (s *Service) persistBinding(ctx context.Context, req RunRequest, trace observability.TraceContext, result agentbinding.Result) (storagewrite.Result, error) {
	success := agentbinding.NewSuccessPayload(result)
	payload, err := json.Marshal(success)
	if err != nil {
		return storagewrite.Result{}, fmt.Errorf("marshal agent binding: %w", err)
	}
	binding := result.Binding
	key := bindingIdempotencyKey(binding)
	event := observability.AgentEvent{
		EventID:        s.ids.NewEventID(),
		SchemaVersion:  observability.AgentEventSchemaVersion,
		IdempotencyKey: key,
		TraceID:        trace.TraceID,
		SpanID:         trace.SpanID,
		ParentSpanID:   trace.ParentSpanID,
		SessionID:      binding.SessionID,
		RunID:          binding.RunID,
		AgentID:        result.Definition.AgentID,
		AgentType:      result.Definition.AgentType,
		Runtime:        string(result.Definition.Runtime.Type),
		EventType:      observability.EventAgentBinding,
		Visibility:     observability.VisibilityInternal,
		Payload:        payload,
		CreatedAt:      s.clock(),
	}
	return s.writer.Execute(ctx, storagewrite.Plan{
		TraceID:        trace.TraceID,
		TenantID:       req.TenantID,
		SessionID:      binding.SessionID,
		RunID:          binding.RunID,
		IdempotencyKey: key,
		Reason:         bindingReason,
		CommitPolicy:   storagewrite.CommitPolicyRequiredFirst,
		RequiredWrites: []storagewrite.Write{
			{
				Store:     storagewrite.StoreRun,
				Operation: storagewrite.OperationInsert,
				Ref:       bindingRunRef(binding),
				Payload:   success,
			},
			{
				Store:     storagewrite.StoreEvent,
				Operation: storagewrite.OperationAppend,
				Ref:       bindingEventRef(binding, observability.EventAgentBinding),
				Payload:   event,
			},
		},
	})
}

func (s *Service) persistBindingFailure(ctx context.Context, req RunRequest, trace observability.TraceContext, cause error) error {
	failure := agentbinding.NewFailurePayload(req.BindingRequest, cause)
	payload, err := json.Marshal(failure)
	if err != nil {
		return fmt.Errorf("marshal agent_binding_failed: %w", err)
	}
	key := bindingFailureIdempotencyKey(req.BindingRequest, failure.Code)
	failedKey := identifiercontract.ComposeIdempotencyKey(key, "failed")
	agentID := requestedAgentID(req.BindingRequest)
	event := observability.AgentEvent{
		EventID:        s.ids.NewEventID(),
		SchemaVersion:  observability.AgentEventSchemaVersion,
		IdempotencyKey: failedKey,
		TraceID:        trace.TraceID,
		SpanID:         trace.SpanID,
		ParentSpanID:   trace.ParentSpanID,
		SessionID:      req.BindingRequest.SessionID,
		RunID:          req.BindingRequest.RunID,
		AgentID:        agentID,
		EventType:      observability.EventAgentBindingFailed,
		Visibility:     observability.VisibilityInternal,
		Payload:        payload,
		Error:          bindingFailureError(failure),
		CreatedAt:      s.clock(),
	}
	writes := []storagewrite.Write{{
		Store:          storagewrite.StoreEvent,
		Operation:      storagewrite.OperationAppend,
		Ref:            bindingFailureEventRef(req.BindingRequest, failure.Code),
		IdempotencyKey: failedKey,
		Payload:        event,
	}}
	// AskUser remains a binding failure draft here. Only the Control Service may
	// publish control_request_created after the request and resumable checkpoint
	// have been committed atomically.
	var askUser *agentbinding.AskUserError
	if !errors.As(cause, &askUser) {
		writes = append([]storagewrite.Write{{
			Store: storagewrite.StoreRun, Operation: storagewrite.OperationUpdate,
			Ref: "run:" + req.BindingRequest.RunID + ":binding_failed", Payload: failure,
		}}, writes...)
	}
	_, err = s.writer.Execute(ctx, storagewrite.Plan{
		TraceID:        trace.TraceID,
		TenantID:       req.TenantID,
		SessionID:      req.BindingRequest.SessionID,
		RunID:          req.BindingRequest.RunID,
		IdempotencyKey: key,
		Reason:         bindingFailedReason,
		CommitPolicy:   storagewrite.CommitPolicyRequiredFirst,
		RequiredWrites: writes,
	})
	if err != nil {
		return safeWrap(ErrBindingFailureWriteFailed, err)
	}
	return nil
}

func (s *Service) failBinding(ctx context.Context, req RunRequest, trace observability.TraceContext, cause error) error {
	if writeErr := s.persistBindingFailure(ctx, req, trace, cause); writeErr != nil {
		return errors.Join(cause, writeErr)
	}
	return cause
}

func bindingFailureError(failure agentbinding.FailurePayload) *observability.EventError {
	errorType := observability.EventErrorInternal
	switch failure.Code {
	case agentbinding.CodeControlDenied, agentbinding.CodeAgentDisabled:
		errorType = observability.EventErrorPermissionDenied
	case agentbinding.CodeConfigResolveFailed, agentbinding.CodeCapabilitySnapshotFailed:
		errorType = observability.EventErrorUpstream
	case agentbinding.CodeInvalidRequest,
		agentbinding.CodeSourceInvalid,
		agentbinding.CodeControlRuleInvalid,
		agentbinding.CodeAskUserRequired,
		agentbinding.CodeConfigInvalid,
		agentbinding.CodeExecutionModeUnsupported,
		agentbinding.CodeExecutionModeNotAllowed,
		agentbinding.CodeExecutionModeMismatch,
		agentbinding.CodeTargetInvalid,
		agentbinding.CodeDefinitionInvalid,
		agentbinding.CodeConfigSnapshotMissing,
		agentbinding.CodeConfigHashMissing,
		agentbinding.CodeBindingHashMismatch:
		errorType = observability.EventErrorSchemaValidation
	}
	return &observability.EventError{
		Code:      string(failure.Code),
		Type:      errorType,
		Message:   failure.SafeMessage,
		Retryable: failure.Retryable,
	}
}

func normalizeBindingError(err error) error {
	if err == nil {
		return nil
	}
	var askUser *agentbinding.AskUserError
	if errors.As(err, &askUser) {
		return askUser
	}
	var bindingErr *agentbinding.Error
	if errors.As(err, &bindingErr) {
		return bindingErr
	}
	var coded interface {
		BindingErrorCode() agentbinding.ErrorCode
	}
	if errors.As(err, &coded) {
		// 外部 Resolver 只提供 code 时也必须重建安全错误，禁止透传其 Error()。
		return agentbinding.NewError(agentbinding.StageConfigResolve, coded.BindingErrorCode(), false, err)
	}
	return agentbinding.NewError(agentbinding.StageConfigResolve, agentbinding.CodeConfigResolveFailed, true, err)
}

func (s *Service) prepareContext(ctx context.Context, req RunRequest) (context.Context, observability.TraceContext) {
	trace := req.Trace
	if trace.TraceID == "" {
		trace.TraceID = s.ids.NewTraceID()
	}
	trace.SessionID = req.BindingRequest.SessionID
	trace.RunID = req.BindingRequest.RunID
	trace.UserID = req.UserID
	trace.TenantID = req.TenantID
	trace.AgentID = requestedAgentID(req.BindingRequest)
	ctx = observability.WithTraceContext(ctx, trace)
	ctx = observability.WithLogger(ctx, s.logger)
	return ctx, trace
}

func validateRunEnvelope(req RunRequest) error {
	if req.TenantID == "" {
		return fmt.Errorf("%w: tenant_id required", ErrInvalidRequest)
	}
	if req.BindingRequest.BindingID == "" || req.BindingRequest.SessionID == "" || req.BindingRequest.RunID == "" {
		return fmt.Errorf("%w: binding_id, session_id and run_id required", ErrInvalidRequest)
	}
	for i, message := range req.Input {
		// Orchestrator 的外部输入只允许用户消息；system 指令必须来自已冻结的 Prompt snapshot。
		if message.Role != "user" {
			cause := fmt.Errorf("%w: input[%d].role must be user", ErrInvalidRequest, i)
			return agentbinding.NewError(agentbinding.StageSourceSelection, agentbinding.CodeInvalidRequest, false, cause)
		}
	}
	return nil
}

func validateBindingResult(req RunRequest, result agentbinding.Result) error {
	binding := result.Binding
	if err := binding.Validate(); err != nil {
		return err
	}
	if binding.BindingID != req.BindingRequest.BindingID || binding.SessionID != req.BindingRequest.SessionID || binding.RunID != req.BindingRequest.RunID {
		return agentbinding.NewError(agentbinding.StageStaticValidate, agentbinding.CodeInvalidRequest, false, nil)
	}
	definition := result.Definition
	if definition.AgentID != binding.AgentID || definition.Version != binding.AgentVersion || definition.Runtime.Type == "" {
		return agentbinding.NewError(agentbinding.StageStaticValidate, agentbinding.CodeDefinitionInvalid, false, nil)
	}
	if err := validateToolPolicyMetadata(definition); err != nil {
		return agentbinding.NewError(agentbinding.StageStaticValidate, agentbinding.CodeDefinitionInvalid, false, err)
	}
	if definition.PromptRef == "" || definition.Metadata["prompt_version"] == "" ||
		definition.Metadata["prompt_hash"] == "" || definition.Metadata["prompt_snapshot_ref"] == "" ||
		definition.Metadata["prompt_content_hash"] == "" {
		return agentbinding.NewError(agentbinding.StageStaticValidate, agentbinding.CodeDefinitionInvalid, false, nil)
	}
	wantRuntimeMode, err := executionmode.ToRuntimeMode(binding.ExecutionMode)
	if err != nil {
		return agentbinding.NewError(agentbinding.StageStaticValidate, agentbinding.CodeExecutionModeUnsupported, false, err)
	}
	if definition.Runtime.Mode != wantRuntimeMode {
		return agentbinding.NewError(agentbinding.StageStaticValidate, agentbinding.CodeExecutionModeMismatch, false, nil)
	}
	if req.Policy.ExecutionMode != "" && req.Policy.ExecutionMode != binding.ExecutionMode {
		return agentbinding.NewError(agentbinding.StageStaticValidate, agentbinding.CodeExecutionModeMismatch, false, nil)
	}
	switch binding.ExecutionMode {
	case executionmode.DirectAction:
		// Direct Action 必须由专用 native runtime 执行，外部 Binder 也不能绕过该约束。
		if definition.Runtime.Type != agentruntime.RuntimeTypeNative ||
			definition.Runtime.Mode != agentruntime.RuntimeModeDirect ||
			definition.Runtime.Preferred != "" && definition.Runtime.Preferred != agentruntime.RuntimeTypeNative {
			return agentbinding.NewError(agentbinding.StageStaticValidate, agentbinding.CodeDefinitionInvalid, false, nil)
		}
		for _, candidate := range definition.Runtime.Candidates {
			if candidate != agentruntime.RuntimeTypeNative {
				return agentbinding.NewError(agentbinding.StageStaticValidate, agentbinding.CodeDefinitionInvalid, false, nil)
			}
		}
	case executionmode.Workflow:
		if definition.Workflow == nil || definition.Workflow.WorkflowID == "" || binding.Target.Ref != definition.Workflow.WorkflowID || binding.Target.Version != definition.Version || binding.Target.Hash != contentHash(definition.Workflow) {
			return agentbinding.NewError(agentbinding.StageStaticValidate, agentbinding.CodeTargetInvalid, false, nil)
		}
	case executionmode.Graph:
		if definition.Graph == nil || definition.Graph.GraphID == "" || binding.Target.Ref != definition.Graph.GraphID || binding.Target.Version != definition.Version || binding.Target.Hash != contentHash(definition.Graph) || binding.Target.StateSchemaRef != definition.Graph.StateSchemaRef {
			return agentbinding.NewError(agentbinding.StageStaticValidate, agentbinding.CodeTargetInvalid, false, nil)
		}
	}
	return nil
}

func validateToolPolicyMetadata(definition agentruntime.AgentDefinition) error {
	for _, ref := range definition.ToolRefs {
		if ref == "" || ref != strings.TrimSpace(ref) || strings.Count(ref, "@") != 1 {
			return fmt.Errorf("tool ref must use exact name@version")
		}
		name, version, ok := strings.Cut(ref, "@")
		if !ok || name == "" || version == "" {
			return fmt.Errorf("tool ref must use exact name@version")
		}
	}
	risk := definition.Metadata["tool_risk_level"]
	switch risk {
	case "low", "medium", "high":
		return nil
	case "":
		if len(definition.ToolRefs) == 0 {
			return nil
		}
		return fmt.Errorf("tool risk level is required")
	default:
		return fmt.Errorf("tool risk level is not canonical")
	}
}

func runtimeMetadata(source map[string]string, binding agentbinding.EffectiveBinding) map[string]string {
	metadata := cloneMetadata(source)
	metadata["binding_id"] = binding.BindingID
	metadata["binding_hash"] = binding.BindingHash
	metadata["execution_mode"] = string(binding.ExecutionMode)
	metadata["binding_source"] = string(binding.Source)
	metadata["config_hash"] = binding.ConfigHash
	metadata["config_snapshot_ref"] = binding.ConfigSnapshotRef
	metadata["target_kind"] = string(binding.Target.Kind)
	metadata["target_ref"] = binding.Target.Ref
	if len(binding.CapabilitySnapshotRefs) > 0 {
		metadata["capability_snapshot_refs"] = strings.Join(binding.CapabilitySnapshotRefs, ",")
	}
	return metadata
}

func cloneMetadata(source map[string]string) map[string]string {
	result := make(map[string]string, len(source)+4)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func requestedAgentID(req agentbinding.BindingRequest) string {
	if req.Request != nil {
		return req.Request.AgentID
	}
	return ""
}

func bindingIdempotencyKey(binding agentbinding.EffectiveBinding) string {
	return identifiercontract.ComposeIdempotencyKey(binding.RunID, "agent_binding", bindingPersistenceFingerprint(binding))
}

func bindingFailureIdempotencyKey(req agentbinding.BindingRequest, code agentbinding.ErrorCode) string {
	return identifiercontract.ComposeIdempotencyKey(req.RunID, "agent_binding_failed", req.BindingID, string(code))
}

func bindingRunRef(binding agentbinding.EffectiveBinding) string {
	return fmt.Sprintf("run:%s:agent_binding", binding.RunID)
}

func bindingEventRef(binding agentbinding.EffectiveBinding, eventType observability.EventType) string {
	return fmt.Sprintf("event:%s:%s:%s", binding.RunID, eventType, bindingPersistenceFingerprint(binding))
}

// persistence fingerprint 同时冻结事实 ID、选择语义和配置快照。
// binding_hash 故意不含配置字段，不能单独承担 Run 槽位的幂等身份。
func bindingPersistenceFingerprint(binding agentbinding.EffectiveBinding) string {
	capabilityRefs := append([]string(nil), binding.CapabilitySnapshotRefs...)
	sort.Strings(capabilityRefs)
	parts := append([]string{
		binding.BindingID,
		binding.BindingHash,
		binding.ConfigHash,
		binding.ConfigSnapshotRef,
	}, capabilityRefs...)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return fmt.Sprintf("sha256:%x", sum[:])
}

func bindingFailureEventRef(req agentbinding.BindingRequest, code agentbinding.ErrorCode) string {
	return fmt.Sprintf("event:%s:%s:%s:%s", req.RunID, observability.EventAgentBindingFailed, req.BindingID, code)
}

func contentHash(value any) string {
	payload, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("sha256:%x", sum[:])
}

var _ Orchestrator = (*Service)(nil)
