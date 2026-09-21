package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

var (
	ErrEinoToolProxyInfoMissing    = errors.New("eino tool proxy info missing")
	ErrEinoToolProxyInvokerMissing = errors.New("eino tool proxy invoker missing")
	ErrEinoToolProxyArguments      = errors.New("eino tool proxy arguments invalid")
	ErrEinoToolProxyLifecycle      = errors.New("eino tool proxy event lifecycle invalid")
	ErrEinoToolProxyResumePayload  = errors.New("eino tool proxy resume payload invalid")
	ErrRuntimeEventEmitterMissing  = errors.New("runtime event emitter missing")

	// Deprecated: use the EinoToolProxy errors above.
	ErrEinoManagedToolInfoMissing    = ErrEinoToolProxyInfoMissing
	ErrEinoManagedToolInvokerMissing = ErrEinoToolProxyInvokerMissing
	ErrEinoManagedToolArguments      = ErrEinoToolProxyArguments
	ErrEinoManagedToolLifecycle      = ErrEinoToolProxyLifecycle
	ErrEinoManagedToolResumePayload  = ErrEinoToolProxyResumePayload
)

const EinoToolProxyInterruptStateSchemaVersion = "harness.eino_tool_interrupt.v1"

func init() {
	// Keep this name stable: persisted Eino checkpoints refer to it across
	// process restarts and adapter upgrades.
	schema.RegisterName[EinoToolProxyInterruptState]("harness_eino_tool_proxy_interrupt_state_v1")
	schema.RegisterName[EinoToolProxyInterruptInfo]("harness_eino_tool_proxy_interrupt_info_v1")
	schema.RegisterName[map[string]any]("harness_json_object_v1")
	schema.RegisterName[[]any]("harness_json_array_v1")
	schema.RegisterName[GatewayProxyInterruptInfo]("harness_gateway_proxy_interrupt_info_v1")
}

// Deprecated: use EinoToolProxyInterruptStateSchemaVersion.
const EinoManagedToolInterruptStateSchemaVersion = EinoToolProxyInterruptStateSchemaVersion

// EinoToolProxy exposes Eino's native tool interface while delegating every
// side effect to the Harness ToolInvoker port.
type EinoToolProxy struct {
	info    *schema.ToolInfo
	invoker ToolInvoker
	request ToolInvocationRequest
	ids     observability.IDGenerator
}

// Deprecated: use EinoToolProxy.
type EinoManagedTool = EinoToolProxy

// EinoToolProxyInterruptState is persisted inside Eino's native checkpoint.
// GatewayState remains runtime-neutral and is returned to ToolInvoker on resume.
type EinoToolProxyInterruptState struct {
	SchemaVersion string
	ToolCallID    string
	Info          json.RawMessage
	GatewayState  json.RawMessage
}

// EinoToolProxyInterruptInfo keeps arbitrary gateway info checkpoint-safe. A
// dynamic map cannot be persisted through Eino's interface-valued gob state.
type EinoToolProxyInterruptInfo struct {
	Payload json.RawMessage
}

func (i EinoToolProxyInterruptInfo) MarshalJSON() ([]byte, error) {
	if len(i.Payload) == 0 {
		return []byte("null"), nil
	}
	return i.Payload, nil
}

// Deprecated: use EinoToolProxyInterruptState.
type EinoManagedToolInterruptState = EinoToolProxyInterruptState

var _ einotool.InvokableTool = (*EinoToolProxy)(nil)

func NewEinoToolProxy(info *schema.ToolInfo, invoker ToolInvoker, request ToolInvocationRequest, ids observability.IDGenerator) (*EinoToolProxy, error) {
	if info == nil || info.Name == "" {
		return nil, ErrEinoToolProxyInfoMissing
	}
	if invoker == nil {
		return nil, ErrEinoToolProxyInvokerMissing
	}
	if ids == nil {
		ids = observability.NewULIDGenerator("eino_tool")
	}
	clonedInfo := *info
	request.ToolName = info.Name
	return &EinoToolProxy{info: &clonedInfo, invoker: invoker, request: request, ids: ids}, nil
}

// Deprecated: use NewEinoToolProxy.
func NewEinoManagedTool(info *schema.ToolInfo, invoker ToolInvoker, request ToolInvocationRequest, ids observability.IDGenerator) (*EinoToolProxy, error) {
	return NewEinoToolProxy(info, invoker, request, ids)
}

func (t *EinoToolProxy) Info(context.Context) (*schema.ToolInfo, error) {
	if t == nil || t.info == nil {
		return nil, ErrEinoToolProxyInfoMissing
	}
	clonedInfo := *t.info
	return &clonedInfo, nil
}

func (t *EinoToolProxy) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...einotool.Option) (string, error) {
	if t == nil || t.invoker == nil {
		return "", ErrEinoToolProxyInvokerMissing
	}
	arguments := []byte(argumentsInJSON)
	if len(arguments) == 0 || !json.Valid(arguments) {
		return "", NewRuntimeError(ErrorSchemaValidation, "TOOL_SCHEMA_VALIDATION_FAILED", "tool arguments failed schema validation").WithCause(ErrEinoToolProxyArguments)
	}
	emitter, ok := RuntimeEventEmitterFrom(ctx)
	if !ok {
		return "", ErrRuntimeEventEmitterMissing
	}
	req := t.request
	req.Trace = observability.MustTraceContext(ctx)
	resume, savedState, err := einoToolProxyResume(ctx)
	if err != nil {
		return "", err
	}
	req.ToolCallID = compose.GetToolCallID(ctx)
	if req.ToolCallID == "" && savedState.ToolCallID != "" {
		req.ToolCallID = savedState.ToolCallID
	}
	if req.ToolCallID == "" {
		req.ToolCallID = t.ids.NewRequestID()
	}
	req.Arguments = append([]byte(nil), arguments...)
	req.Resume = resume
	if resume != nil && resume.WasInterrupted && einoResumeIsTargeted(ctx) && !resume.IsResumeTarget {
		return "", einotool.StatefulInterrupt(ctx, decodeEinoToolProxyInterruptInfo(savedState.Info), savedState)
	}
	sink := newRuntimeToolEventSink(emitter, resume != nil && resume.WasInterrupted)
	result, err := t.invoker.Invoke(ctx, req, sink)
	if err != nil {
		var interrupted *ToolInvocationInterruptedError
		if errors.As(err, &interrupted) {
			if lifecycleErr := sink.ValidateInterrupted(); lifecycleErr != nil {
				return "", lifecycleErr
			}
			info, marshalErr := encodeEinoToolProxyInterruptInfo(interrupted.Info)
			if marshalErr != nil {
				return "", marshalErr
			}
			state := EinoToolProxyInterruptState{
				SchemaVersion: EinoToolProxyInterruptStateSchemaVersion,
				ToolCallID:    req.ToolCallID,
				Info:          info,
				GatewayState:  append(json.RawMessage(nil), interrupted.State...),
			}
			return "", einotool.StatefulInterrupt(ctx, decodeEinoToolProxyInterruptInfo(info), state)
		}
		return "", err
	}
	if err := sink.ValidateComplete(result.IsError); err != nil {
		return "", err
	}
	if result.Content != "" {
		return result.Content, nil
	}
	if result.ContentRef != "" {
		payload, marshalErr := json.Marshal(map[string]any{"content_ref": result.ContentRef, "is_error": result.IsError})
		if marshalErr != nil {
			return "", fmt.Errorf("marshal managed tool result: %w", marshalErr)
		}
		return string(payload), nil
	}
	return "", nil
}

type runtimeToolEventSink struct {
	emitter  RuntimeEventEmitter
	mu       sync.Mutex
	started  bool
	rejected bool
	terminal observability.EventType
	err      error
}

func newRuntimeToolEventSink(emitter RuntimeEventEmitter, alreadyStarted bool) *runtimeToolEventSink {
	return &runtimeToolEventSink{emitter: emitter, started: alreadyStarted}
}

func (s *runtimeToolEventSink) Emit(ctx context.Context, event observability.AgentEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.emitter == nil {
		return ErrRuntimeEventEmitterMissing
	}
	if s.err != nil {
		return s.err
	}
	if !isCanonicalToolEvent(event.EventType) || s.terminal != "" {
		s.err = ErrEinoToolProxyLifecycle
		return s.err
	}
	switch event.EventType {
	case observability.EventToolCallStarted:
		if s.started {
			s.err = ErrEinoToolProxyLifecycle
			return s.err
		}
		s.started = true
	case observability.EventToolCallCompleted, observability.EventToolCallFailed, observability.EventToolCallCancelled:
		if !s.started && !isPreExecutionToolFailure(event) {
			s.err = ErrEinoToolProxyLifecycle
			return s.err
		}
		s.rejected = !s.started
		s.terminal = event.EventType
	default:
		if !s.started {
			s.err = ErrEinoToolProxyLifecycle
			return s.err
		}
	}
	return s.emitter.Emit(ctx, event)
}

func (s *runtimeToolEventSink) ValidateComplete(resultIsError bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if (!s.started && !s.rejected) || s.terminal == "" {
		return ErrEinoToolProxyLifecycle
	}
	if resultIsError && s.terminal == observability.EventToolCallCompleted {
		return ErrEinoToolProxyLifecycle
	}
	if !resultIsError && s.terminal != observability.EventToolCallCompleted {
		return ErrEinoToolProxyLifecycle
	}
	return nil
}

func (s *runtimeToolEventSink) ValidateInterrupted() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if !s.started || s.terminal != "" {
		return ErrEinoToolProxyLifecycle
	}
	return nil
}

func einoToolProxyResume(ctx context.Context) (*ToolInvocationResume, EinoToolProxyInterruptState, error) {
	wasInterrupted, hasState, state := einotool.GetInterruptState[EinoToolProxyInterruptState](ctx)
	if !wasInterrupted {
		return nil, EinoToolProxyInterruptState{}, nil
	}
	if !hasState {
		return nil, EinoToolProxyInterruptState{}, ErrEinoToolProxyResumePayload
	}
	if state.SchemaVersion != EinoToolProxyInterruptStateSchemaVersion || state.ToolCallID == "" {
		return nil, EinoToolProxyInterruptState{}, ErrEinoToolProxyResumePayload
	}
	isTarget, hasPayload, payload := einotool.GetResumeContext[any](ctx)
	resume := &ToolInvocationResume{
		WasInterrupted: true,
		IsResumeTarget: isTarget || !einoResumeIsTargeted(ctx),
		State:          append(json.RawMessage(nil), state.GatewayState...),
	}
	if hasPayload {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, EinoToolProxyInterruptState{}, fmt.Errorf("%w: %v", ErrEinoToolProxyResumePayload, err)
		}
		resume.Payload = data
	}
	return resume, state, nil
}

func encodeEinoToolProxyInterruptInfo(info any) (json.RawMessage, error) {
	if raw, ok := info.(json.RawMessage); ok {
		if !json.Valid(raw) {
			return nil, ErrEinoToolProxyResumePayload
		}
		return append(json.RawMessage(nil), raw...), nil
	}
	data, err := json.Marshal(info)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEinoToolProxyResumePayload, err)
	}
	return data, nil
}

func decodeEinoToolProxyInterruptInfo(info json.RawMessage) any {
	if !json.Valid(info) {
		info = json.RawMessage(`{"type":"tool_control_request"}`)
	}
	return EinoToolProxyInterruptInfo{Payload: append(json.RawMessage(nil), info...)}
}

type einoTargetedResumeKey struct{}

func withEinoTargetedResume(ctx context.Context, targeted bool) context.Context {
	return context.WithValue(ctx, einoTargetedResumeKey{}, targeted)
}

func einoResumeIsTargeted(ctx context.Context) bool {
	targeted, _ := ctx.Value(einoTargetedResumeKey{}).(bool)
	return targeted
}

// einoGlobalResumePayloadKey 承载非 targeted resume 时的 ControlPayload 原文，
// 供 GatewayProxyAgent 等可恢复 agent 在 ResumeData 为空时兜底读取。
type einoGlobalResumePayloadKey struct{}

func withEinoGlobalResumePayload(ctx context.Context, payload json.RawMessage) context.Context {
	if len(payload) == 0 {
		return ctx
	}
	return context.WithValue(ctx, einoGlobalResumePayloadKey{}, append(json.RawMessage(nil), payload...))
}

func einoGlobalResumePayload(ctx context.Context) json.RawMessage {
	payload, _ := ctx.Value(einoGlobalResumePayloadKey{}).(json.RawMessage)
	return payload
}
