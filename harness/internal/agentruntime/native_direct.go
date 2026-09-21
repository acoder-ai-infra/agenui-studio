package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

var (
	ErrDirectModelInvokerMissing   = errors.New("direct runtime model invoker missing")
	ErrDirectToolInvokerMissing    = errors.New("direct runtime tool invoker missing")
	ErrDirectContextRebuildMissing = errors.New("direct runtime context rebuilder missing")
	ErrDirectModeUnsupported       = errors.New("direct runtime mode unsupported")
	ErrDirectResumeUnsupported     = errors.New("direct runtime resume unsupported")
	ErrDirectInvalidModelEvent     = errors.New("direct runtime invalid model event")
	ErrDirectInvalidToolEvent      = errors.New("direct runtime invalid tool event")
	ErrDirectModelStreamIncomplete = errors.New("direct runtime model stream incomplete")
	ErrDirectPackageMismatch       = errors.New("direct runtime model context package mismatch")
	ErrDirectBoundaryExceeded      = errors.New("direct runtime boundary exceeded")
	ErrDirectToolNotAuthorized     = errors.New("direct runtime tool not authorized")
	ErrDirectToolRouteAmbiguous    = errors.New("direct runtime tool route ambiguous")
	ErrDirectToolSchemaMissing     = errors.New("direct runtime authorized tool schema missing")
)

// NativeDirectRuntime executes one model round with at most one governed tool round.
type NativeDirectRuntime struct {
	Models ModelInvoker
	// ModelInputTransform 在 direct 循环每轮模型调用前改写请求（方案 3.3
	// native 路径；BeforeModelHook 扩展由 Composition Root 投影至此）。
	// 为 nil 时跳过；返回错误 fail closed。
	ModelInputTransform NativeModelInvokeTransform
	ModelInputs         ModelInputGovernor
	PreModel            RuntimePreModelCompactor
	Tools               ToolInvoker
	Contexts            ModelContextRebuilder
	Clock               func() time.Time
}

// NewNativeDirectRuntime creates the bounded native direct execution loop.
func NewNativeDirectRuntime(models ModelInvoker, tools ToolInvoker, contexts ModelContextRebuilder) *NativeDirectRuntime {
	compactor, _ := NewDefaultContextCompactorProvider(nil).Resolve(context.Background(), DefaultContextCompactionPolicy(), RuntimeDescriptor{Name: RuntimeTypeNative})
	return &NativeDirectRuntime{
		Models: models, ModelInputs: NewDefaultModelInputGovernor(nil), PreModel: compactor,
		Tools: tools, Contexts: contexts, Clock: time.Now,
	}
}

func NewNativeDirectRuntimeWithCompaction(models ModelInvoker, tools ToolInvoker, contexts ModelContextRebuilder, compactor RuntimePreModelCompactor) *NativeDirectRuntime {
	runtime := NewNativeDirectRuntime(models, tools, contexts)
	if compactor != nil {
		runtime.PreModel = compactor
	}
	return runtime
}

func (*NativeDirectRuntime) Name() string {
	return string(RuntimeTypeNative)
}

func (r *NativeDirectRuntime) Descriptor(ctx context.Context) RuntimeDescriptor {
	return RuntimeDescriptor{
		Name:           RuntimeTypeNative,
		RuntimeVersion: "builtin-v1",
		AdapterVersion: "v1",
		Governance:     RuntimeGovernanceManaged,
		Capabilities:   r.Capabilities(ctx),
	}
}

func (r *NativeDirectRuntime) Capabilities(context.Context) RuntimeCapabilities {
	toolCall := r != nil && r.Tools != nil
	// MCP is materialized as a frozen tool route by the Harness capability
	// layer; the direct loop executes it through the same governed ToolInvoker.
	return RuntimeCapabilities{Streaming: true, ToolCall: toolCall, MCP: toolCall, Cancellation: true}
}

func (r *NativeDirectRuntime) ValidateConfig(_ context.Context, def AgentDefinition) error {
	if r == nil || r.Models == nil {
		return ErrDirectModelInvokerMissing
	}
	if def.AgentID == "" {
		return errors.New("agent_id required")
	}
	if def.Version == "" {
		return errors.New("agent version required")
	}
	if def.Runtime.Mode != RuntimeModeDirect {
		return fmt.Errorf("%w: %s", ErrDirectModeUnsupported, def.Runtime.Mode)
	}
	policy, err := NormalizeContextCompactionPolicy(def.ContextCompaction)
	if err != nil {
		return err
	}
	if r.PreModel == nil && policy.SemanticSummary == SemanticSummaryRequired {
		return ErrRuntimePreModelCompactorMissing
	}
	if readiness, ok := r.PreModel.(RuntimePreModelCompactorReadiness); ok {
		if err := readiness.ValidatePolicy(policy); err != nil {
			return err
		}
	}
	if len(def.ToolRefs) > 0 && r.Tools == nil {
		return ErrDirectToolInvokerMissing
	}
	if len(def.ToolRefs) > 0 && r.Contexts == nil {
		return ErrDirectContextRebuildMissing
	}
	return nil
}

func (r *NativeDirectRuntime) Build(ctx context.Context, def AgentDefinition) (AgentHandle, error) {
	if err := r.ValidateConfig(ctx, def); err != nil {
		return AgentHandle{}, err
	}
	clock := r.Clock
	if clock == nil {
		clock = time.Now
	}
	binding, err := newRuntimeBinding(ctx, r, def, runtimeBindingFacts{})
	if err != nil {
		return AgentHandle{}, err
	}
	return AgentHandle{Definition: def, Runtime: RuntimeTypeNative, Binding: binding, BuiltAt: clock()}, nil
}

func (r *NativeDirectRuntime) Run(ctx context.Context, req RunRequest) (<-chan observability.AgentEvent, error) {
	if r == nil || r.Models == nil {
		return nil, ErrDirectModelInvokerMissing
	}
	pkg, ok := ModelContextPackageFrom(ctx)
	if !ok {
		return nil, ErrModelContextPackageMissing
	}
	if err := validateDirectPackage(req, pkg); err != nil {
		return nil, err
	}
	if hasDirectTools(pkg) {
		if r.Tools == nil {
			return nil, ErrDirectToolInvokerMissing
		}
		if r.Contexts == nil {
			return nil, ErrDirectContextRebuildMissing
		}
		if err := validateDirectToolSchemas(pkg); err != nil {
			return nil, err
		}
	}
	out := make(chan observability.AgentEvent, 16)
	go r.execute(ctx, req, pkg, out)
	return out, nil
}

func (r *NativeDirectRuntime) execute(ctx context.Context, req RunRequest, pkg ModelContextPackage, out chan<- observability.AgentEvent) {
	defer close(out)
	// A child Run may be started from a parent Runtime context. Always install
	// the emitter owned by this execution so governance and tool events cannot
	// be attributed to the parent Run's bridge.
	ctx = WithRuntimeEventEmitter(ctx, directRuntimeEventEmitter{out: out})
	toolCall, runtimeErr := r.runModelRound(ctx, req, pkg, 1, hasDirectTools(pkg), out)
	if runtimeErr != nil {
		r.sendFailure(ctx, out, runtimeErr)
		return
	}
	if toolCall == nil {
		return
	}
	route, err := directToolRoute(pkg, *toolCall)
	if err != nil {
		code := "DIRECT_TOOL_NOT_AUTHORIZED"
		message := "model requested an unauthorized tool"
		if errors.Is(err, ErrDirectToolRouteAmbiguous) {
			code = "DIRECT_TOOL_ROUTE_AMBIGUOUS"
			message = "model requested a tool with multiple authorized routes"
		}
		r.sendFailure(ctx, out, NewRuntimeError(ErrorPermissionDenied, code, message).WithCause(err))
		return
	}
	if r.Tools == nil {
		r.sendFailure(ctx, out, NewRuntimeError(ErrorDependencyUnavailable, "DIRECT_TOOL_GATEWAY_UNAVAILABLE", "tool gateway unavailable").WithRetryable(true).WithDependency("tool_gateway").WithCause(ErrDirectToolInvokerMissing))
		return
	}
	toolCtx, cancelTool := context.WithCancel(ctx)
	eventSink := newDirectToolEventSink(toolCtx, out)
	result, err := r.Tools.Invoke(toolCtx, ToolInvocationRequest{
		Trace:        observability.MustTraceContext(ctx),
		SessionID:    req.SessionID,
		RunID:        req.RunID,
		AgentID:      req.Definition.AgentID,
		ToolCallID:   toolCall.ToolCallID,
		ToolName:     route.ToolName,
		ToolVersion:  route.ToolVersion,
		Source:       route.Source,
		SourceRef:    route.SourceRef,
		SnapshotRef:  route.SnapshotRef,
		ProcessStage: req.Definition.ProcessPresentation.Stage,
		Arguments:    append([]byte(nil), toolCall.Arguments...),
	}, eventSink)
	eventSink.Close()
	cancelTool()
	if err != nil {
		cause := errors.Unwrap(err)
		causeText := ""
		if cause != nil {
			causeText = cause.Error()
		}
		observability.LoggerFrom(ctx, observability.NoopLogger{}).Error(ctx, "governed tool invocation failed", err,
			observability.String("run_id", req.RunID),
			observability.String("agent_id", req.Definition.AgentID),
			observability.String("tool_name", toolCall.Name),
			observability.String("tool_call_id", toolCall.ToolCallID),
			observability.String("tool_source", string(route.Source)),
			observability.String("tool_source_ref", route.SourceRef),
			observability.String("tool_snapshot_ref", route.SnapshotRef),
			observability.String("tool_version", route.ToolVersion),
			observability.String("error_cause", causeText),
		)
		runtimeErr := DefaultRuntimeErrorClassifier{}.Classify(err, ErrorStageRuntimeAdapter)
		if runtimeErr.Type == ErrorCancelled {
			r.sendEvent(ctx, out, observability.AgentEvent{EventType: EventRunCancelled, Visibility: observability.VisibilityDebug})
		} else {
			r.sendFailure(ctx, out, runtimeErr)
		}
		return
	}
	if err := eventSink.ValidateComplete(result.IsError); err != nil {
		r.sendFailure(ctx, out, NewRuntimeError(ErrorSchemaValidation, "DIRECT_TOOL_EVENT_INVALID", "tool gateway returned an invalid lifecycle").WithCause(err))
		return
	}
	if r.Contexts == nil {
		r.sendFailure(ctx, out, NewRuntimeError(ErrorDependencyUnavailable, "DIRECT_CONTEXT_REBUILDER_UNAVAILABLE", "context rebuilder unavailable").WithRetryable(true).WithDependency("context_engine").WithCause(ErrDirectContextRebuildMissing))
		return
	}
	rebuilt, err := r.Contexts.Rebuild(ctx, ModelContextRebuildRequest{Run: req, InitialPackage: pkg, ToolCall: *toolCall, Result: result})
	if err != nil {
		r.sendFailure(ctx, out, NewRuntimeError(ErrorRuntime, "DIRECT_CONTEXT_REBUILD_FAILED", "model context rebuild failed").WithRetryable(true).WithDependency("context_engine").WithCause(err))
		return
	}
	if err := validateDirectPackage(req, rebuilt); err != nil {
		r.sendFailure(ctx, out, NewRuntimeError(ErrorSchemaValidation, "DIRECT_CONTEXT_PACKAGE_INVALID", "rebuilt model context package is invalid").WithCause(err))
		return
	}
	if rebuilt.PackageID == pkg.PackageID {
		r.sendFailure(ctx, out, NewRuntimeError(ErrorSchemaValidation, "DIRECT_CONTEXT_PACKAGE_STALE", "context rebuild returned the previous package").WithCause(ErrDirectPackageMismatch))
		return
	}
	secondToolCall, runtimeErr := r.runModelRound(ctx, req, rebuilt, 2, false, out)
	if runtimeErr != nil {
		r.sendFailure(ctx, out, runtimeErr)
		return
	}
	if secondToolCall != nil {
		r.sendFailure(ctx, out, NewRuntimeError(ErrorRuntime, "DIRECT_RUNTIME_BOUNDARY_EXCEEDED", "direct runtime supports only one tool round").WithRetryable(true).WithDegraded(true).WithCause(ErrDirectBoundaryExceeded))
	}
}

func (r *NativeDirectRuntime) runModelRound(ctx context.Context, req RunRequest, pkg ModelContextPackage, round int, allowTools bool, out chan<- observability.AgentEvent) (*ModelToolCall, *RuntimeError) {
	modelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	invokeReq := ModelInvokeRequest{Trace: observability.MustTraceContext(ctx), Package: pkg, Round: round, AllowTools: allowTools, Options: cloneModelCallOptions(pkg.RuntimeConstraints.ModelOptions)}
	// 本 Run 冻结的 scoped-data 快照随模型调用下传，供 BeforeModelHook 只读消费。
	invokeReq.ScopedData = cloneScopedData(req.ScopedData)
	if r.ModelInputTransform != nil {
		transformed, transformErr := r.ModelInputTransform(modelCtx, req.Definition, invokeReq)
		if transformErr != nil {
			return nil, NewRuntimeError(ErrorSchemaValidation, "MODEL_INPUT_TRANSFORM_FAILED", "model input transformer failed").WithCause(transformErr)
		}
		invokeReq = transformed
	}
	stream, err := NewContextManagedModelInvoker(r.Models, r.PreModel, r.ModelInputs).Invoke(modelCtx, invokeReq)
	if err != nil {
		if errors.Is(err, ErrModelInputBudgetExceeded) {
			return nil, NewRuntimeError(ErrorSchemaValidation, "MODEL_INPUT_BUDGET_EXCEEDED", "final model input exceeds its budget").WithCause(err)
		}
		r.sendEvent(ctx, out, observability.AgentEvent{
			EventType:  observability.EventModelCallFailed,
			Visibility: observability.VisibilityDebug,
			Error:      NewRuntimeError(ErrorModel, "MODEL_INVOKE_FAILED", "model invocation failed").WithRetryable(true).EventError(),
		})
		return nil, NewRuntimeError(ErrorModel, "MODEL_INVOKE_FAILED", "model invocation failed").WithRetryable(true).WithCause(err)
	}
	if stream == nil {
		return nil, NewRuntimeError(ErrorSchemaValidation, "DIRECT_MODEL_STREAM_INVALID", "model gateway returned an invalid stream").WithCause(ErrDirectInvalidModelEvent)
	}
	var toolCall *ModelToolCall
	started := false
	completed := false
	for {
		select {
		case <-ctx.Done():
			return nil, nil
		case item, ok := <-stream:
			if !ok {
				if !completed {
					return nil, NewRuntimeError(ErrorModel, "DIRECT_MODEL_STREAM_INCOMPLETE", "model stream ended without completion").WithRetryable(true).WithCause(ErrDirectModelStreamIncomplete)
				}
				return toolCall, nil
			}
			if !isDirectModelEvent(item.Event.EventType) {
				return nil, NewRuntimeError(ErrorSchemaValidation, "DIRECT_MODEL_EVENT_INVALID", "model gateway returned an invalid event").WithCause(ErrDirectInvalidModelEvent)
			}
			if item.Event.EventType == observability.EventModelCallStarted {
				if started || completed {
					return nil, NewRuntimeError(ErrorSchemaValidation, "DIRECT_MODEL_EVENT_INVALID", "model gateway returned an invalid lifecycle").WithCause(ErrDirectInvalidModelEvent)
				}
				started = true
			} else if item.Event.EventType == observability.EventModelCallFailed && !started && !completed {
				if !r.sendEvent(ctx, out, item.Event) {
					return nil, nil
				}
				return nil, runtimeErrorFromEvent(item.Event.Error)
			} else if !started || completed {
				return nil, NewRuntimeError(ErrorSchemaValidation, "DIRECT_MODEL_EVENT_INVALID", "model gateway returned an invalid lifecycle").WithCause(ErrDirectInvalidModelEvent)
			}
			text := ""
			failed := false
			switch item.Event.EventType {
			case observability.EventModelTokenDelta:
				payloadText := finalTextFromPayload(item.Event.Payload)
				text = item.TextDelta
				if text == "" {
					text = payloadText
				}
				if payloadText != "" && text != payloadText {
					return nil, NewRuntimeError(ErrorSchemaValidation, "DIRECT_MODEL_EVENT_INVALID", "model text payload does not match stream item").WithCause(ErrDirectInvalidModelEvent)
				}
			case observability.EventModelToolCallDelta:
				if item.ToolCall == nil || item.ToolCall.ToolCallID == "" || item.ToolCall.Name == "" {
					return nil, NewRuntimeError(ErrorSchemaValidation, "DIRECT_MODEL_TOOL_CALL_INVALID", "model gateway returned an invalid tool call").WithCause(ErrDirectInvalidModelEvent)
				}
				if toolCall != nil {
					return nil, NewRuntimeError(ErrorRuntime, "DIRECT_RUNTIME_BOUNDARY_EXCEEDED", "direct runtime supports one tool call").WithRetryable(true).WithDegraded(true).WithCause(ErrDirectBoundaryExceeded)
				}
				copy := *item.ToolCall
				copy.Arguments = append([]byte(nil), item.ToolCall.Arguments...)
				toolCall = &copy
				item.Event.Payload = JSONPayload(copy)
			case observability.EventModelCallCompleted:
				completed = true
			case observability.EventModelCallFailed:
				failed = true
			}
			if !r.sendEvent(ctx, out, item.Event) {
				return nil, nil
			}
			textPayload := JSONPayload(map[string]string{"text": text})
			if text != "" && !r.sendEvent(ctx, out, observability.AgentEvent{
				EventType: EventAgentTextDelta, Visibility: observability.VisibilityUserVisible,
				Payload: textPayload, PayloadPreview: textPayload,
			}) {
				return nil, nil
			}
			if failed {
				return nil, runtimeErrorFromEvent(item.Event.Error)
			}
		}
	}
}

type directRuntimeEventEmitter struct {
	out chan<- observability.AgentEvent
}

func (e directRuntimeEventEmitter) Emit(ctx context.Context, event observability.AgentEvent) error {
	return emitRuntimeEventWithCommitBarrier(ctx, event, func(event observability.AgentEvent) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e.out <- event:
			return nil
		}
	})
}

func (r *NativeDirectRuntime) Resume(context.Context, ResumeRequest) (<-chan observability.AgentEvent, error) {
	return nil, ErrDirectResumeUnsupported
}

func (*NativeDirectRuntime) Cancel(context.Context, CancelRequest) error {
	return nil
}

func (r *NativeDirectRuntime) Health(context.Context) RuntimeHealth {
	available := r != nil && r.Models != nil
	reason := ""
	if !available {
		reason = ErrDirectModelInvokerMissing.Error()
	}
	return RuntimeHealth{Available: available, Reason: reason, CheckedAt: time.Now()}
}

func (*NativeDirectRuntime) Shutdown(context.Context) error {
	return nil
}

func (r *NativeDirectRuntime) sendFailure(ctx context.Context, out chan<- observability.AgentEvent, runtimeErr *RuntimeError) {
	if runtimeErr == nil || ctx.Err() != nil {
		return
	}
	r.sendEvent(ctx, out, observability.AgentEvent{
		EventType:  EventRunFailed,
		Visibility: observability.VisibilityDebug,
		Payload:    JSONPayload(runtimeErr.Payload()),
		Error:      runtimeErr.EventError(),
	})
}

func (*NativeDirectRuntime) sendEvent(ctx context.Context, out chan<- observability.AgentEvent, event observability.AgentEvent) bool {
	err := emitRuntimeEventWithCommitBarrier(ctx, event, func(event observability.AgentEvent) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- event:
			return nil
		}
	})
	return err == nil
}

func validateDirectPackage(req RunRequest, pkg ModelContextPackage) error {
	if pkg.SchemaVersion != ModelContextPackageSchemaVersion || pkg.PackageID == "" || validateModelContextIntegrity(pkg) != nil || pkg.Run.RunID != req.RunID || pkg.Run.SessionID != req.SessionID {
		return fmt.Errorf("%w: run_id=%s package_run_id=%s", ErrDirectPackageMismatch, req.RunID, pkg.Run.RunID)
	}
	if err := validateContextCompactionPolicyBinding(req.Definition, pkg); err != nil {
		return fmt.Errorf("%w: %v", ErrDirectPackageMismatch, err)
	}
	return nil
}

type directToolInvocationRoute struct {
	Source      ToolInvocationSource
	SourceRef   string
	SnapshotRef string
	ToolName    string
	ToolVersion string
}

func hasDirectTools(pkg ModelContextPackage) bool {
	if len(pkg.Capabilities.Tools) > 0 {
		return true
	}
	for _, snapshot := range pkg.Capabilities.MCPSnapshots {
		if len(snapshot.Tools) > 0 {
			return true
		}
	}
	return false
}

func validateDirectToolSchemas(pkg ModelContextPackage) error {
	defined := make(map[string]struct{}, len(pkg.Capabilities.ToolDefinitions))
	for _, definition := range pkg.Capabilities.ToolDefinitions {
		defined[definition.Name] = struct{}{}
	}
	for _, ref := range pkg.Capabilities.Tools {
		name := strings.SplitN(ref, "@", 2)[0]
		if _, ok := defined[name]; !ok {
			return fmt.Errorf("%w: tool=%s", ErrDirectToolSchemaMissing, ref)
		}
	}
	for _, snapshot := range pkg.Capabilities.MCPSnapshots {
		for _, tool := range snapshot.Tools {
			if _, ok := defined[tool.Name]; !ok {
				return fmt.Errorf("%w: mcp_server=%s tool=%s", ErrDirectToolSchemaMissing, snapshot.ServerID, tool.Name)
			}
		}
	}
	return nil
}

func directToolRoute(pkg ModelContextPackage, call ModelToolCall) (directToolInvocationRoute, error) {
	var routes []directToolInvocationRoute
	for _, ref := range pkg.Capabilities.Tools {
		if directToolRefMatches(ref, call) {
			name, version := splitDirectToolRef(ref)
			routes = append(routes, directToolInvocationRoute{
				Source: ToolSourceRegistry, SourceRef: ref, ToolName: name, ToolVersion: version,
			})
		}
	}
	for _, snapshot := range pkg.Capabilities.MCPSnapshots {
		for _, tool := range snapshot.Tools {
			if tool.Name == call.Name && call.Version == "" && !strings.Contains(call.Name, "@") {
				routes = append(routes, directToolInvocationRoute{
					Source: ToolSourceMCP, SourceRef: snapshot.ServerID, SnapshotRef: snapshot.ID, ToolName: tool.Name,
				})
			}
		}
	}
	for _, snapshot := range pkg.Capabilities.HTTPToolSnapshots {
		if snapshot.Name == call.Name && call.Version == "" && !strings.Contains(call.Name, "@") {
			routes = append(routes, directToolInvocationRoute{
				Source: ToolSourceHTTPTool, SourceRef: snapshot.Name, SnapshotRef: snapshot.DefinitionHash, ToolName: snapshot.Name,
			})
		}
	}
	if len(routes) == 0 {
		return directToolInvocationRoute{}, ErrDirectToolNotAuthorized
	}
	if len(routes) != 1 {
		return directToolInvocationRoute{}, ErrDirectToolRouteAmbiguous
	}
	return routes[0], nil
}

func splitDirectToolRef(ref string) (string, string) {
	name, version, found := strings.Cut(ref, "@")
	if !found {
		return ref, ""
	}
	return name, version
}

func directToolAuthorized(refs []string, call ModelToolCall) bool {
	for _, ref := range refs {
		if directToolRefMatches(ref, call) {
			return true
		}
	}
	return false
}

func directToolRefMatches(ref string, call ModelToolCall) bool {
	if strings.Contains(call.Name, "@") {
		return call.Version == "" && ref == call.Name
	}
	if call.Version != "" {
		return ref == call.Name+"@"+call.Version
	}
	return ref == call.Name || strings.SplitN(ref, "@", 2)[0] == call.Name
}

func isDirectModelEvent(eventType observability.EventType) bool {
	switch eventType {
	case observability.EventModelCallStarted, observability.EventModelTokenDelta, observability.EventModelThoughtDelta,
		observability.EventModelToolCallDelta, observability.EventModelUsageDelta, observability.EventModelCallCompleted,
		observability.EventModelCallFailed, observability.EventModelFallbackApplied:
		return true
	default:
		return false
	}
}

func isCanonicalToolEvent(eventType observability.EventType) bool {
	switch eventType {
	case observability.EventToolCallStarted, observability.EventToolCallProgress, observability.EventToolCallCompleted,
		observability.EventToolCallFailed, observability.EventToolCallCancelled, observability.EventToolArtifactCreated:
		return true
	default:
		return false
	}
}

type directToolEventSink struct {
	mu       sync.Mutex
	ctx      context.Context
	out      chan<- observability.AgentEvent
	started  bool
	rejected bool
	terminal observability.EventType
	closed   bool
	err      error
}

func newDirectToolEventSink(ctx context.Context, out chan<- observability.AgentEvent) *directToolEventSink {
	return &directToolEventSink{ctx: ctx, out: out}
}

func (s *directToolEventSink) Emit(ctx context.Context, event observability.AgentEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if s.closed {
		return ErrDirectInvalidToolEvent
	}
	if !isCanonicalToolEvent(event.EventType) || s.terminal != "" {
		s.err = ErrDirectInvalidToolEvent
		return s.err
	}
	switch event.EventType {
	case observability.EventToolCallStarted:
		if s.started {
			s.err = ErrDirectInvalidToolEvent
			return s.err
		}
		s.started = true
	case observability.EventToolCallCompleted, observability.EventToolCallFailed, observability.EventToolCallCancelled:
		if !s.started && !isPreExecutionToolFailure(event) {
			s.err = ErrDirectInvalidToolEvent
			return s.err
		}
		s.rejected = !s.started
		s.terminal = event.EventType
	default:
		if !s.started {
			s.err = ErrDirectInvalidToolEvent
			return s.err
		}
	}
	select {
	case <-s.ctx.Done():
		s.err = s.ctx.Err()
		return s.err
	case <-ctx.Done():
		s.err = ctx.Err()
		return s.err
	case s.out <- event:
		return nil
	}
}

func (s *directToolEventSink) ValidateComplete(resultIsError bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if (!s.started && !s.rejected) || s.terminal == "" {
		return ErrDirectInvalidToolEvent
	}
	if resultIsError && s.terminal == observability.EventToolCallCompleted {
		return ErrDirectInvalidToolEvent
	}
	if !resultIsError && s.terminal != observability.EventToolCallCompleted {
		return ErrDirectInvalidToolEvent
	}
	return nil
}

func (s *directToolEventSink) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}
