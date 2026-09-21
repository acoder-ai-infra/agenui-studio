package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/cloudwego/eino/adk"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

func TestEinoToolProxyDelegatesToHarnessToolInvoker(t *testing.T) {
	invoker := &managedToolInvoker{result: ToolInvocationResult{Content: "gateway result"}}
	tool, err := NewEinoToolProxy(
		&schema.ToolInfo{Name: "search", Desc: "search"},
		invoker,
		ToolInvocationRequest{SessionID: "session_1", RunID: "run_1", AgentID: "agent_1"},
		managedToolIDGenerator{requestID: "tool_call_1"},
	)
	if err != nil {
		t.Fatalf("build managed tool: %v", err)
	}
	emitter := &capturingRuntimeEmitter{}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TraceID: "trace_1"})
	ctx = WithRuntimeEventEmitter(ctx, emitter)
	result, err := tool.InvokableRun(ctx, `{"query":"maps"}`)
	if err != nil {
		t.Fatalf("invoke managed tool: %v", err)
	}
	if result != "gateway result" || invoker.request.ToolName != "search" || invoker.request.ToolCallID != "tool_call_1" || string(invoker.request.Arguments) != `{"query":"maps"}` {
		t.Fatalf("unexpected managed invocation: result=%q request=%#v", result, invoker.request)
	}
	if len(emitter.events) != 2 || emitter.events[0].EventType != observability.EventToolCallStarted || emitter.events[1].EventType != observability.EventToolCallCompleted {
		t.Fatalf("gateway event was not bridged: %#v", emitter.events)
	}
}

func TestEinoToolProxyPreservesEinoToolCallID(t *testing.T) {
	invoker := &managedToolInvoker{result: ToolInvocationResult{Content: "gateway result"}}
	managed, err := NewEinoToolProxy(
		&schema.ToolInfo{Name: "search", Desc: "search"},
		invoker,
		ToolInvocationRequest{SessionID: "session_1", RunID: "run_1", AgentID: "agent_1"},
		managedToolIDGenerator{requestID: "generated_fallback"},
	)
	if err != nil {
		t.Fatalf("build managed tool: %v", err)
	}
	node, err := compose.NewToolNode(context.Background(), &compose.ToolsNodeConfig{Tools: []einotool.BaseTool{managed}})
	if err != nil {
		t.Fatalf("build eino tool node: %v", err)
	}
	emitter := &capturingRuntimeEmitter{}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TraceID: "trace_1"})
	ctx = WithRuntimeEventEmitter(ctx, emitter)
	_, err = node.Invoke(ctx, schema.AssistantMessage("", []schema.ToolCall{{
		ID:   "model_tool_call_42",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "search",
			Arguments: `{"query":"maps"}`,
		},
	}}))
	if err != nil {
		t.Fatalf("invoke eino tool node: %v", err)
	}
	if invoker.request.ToolCallID != "model_tool_call_42" {
		t.Fatalf("tool_call_id was replaced: got %q", invoker.request.ToolCallID)
	}
}

func TestEinoToolProxyFailsClosedWithoutRuntimeBridge(t *testing.T) {
	tool, err := NewEinoToolProxy(&schema.ToolInfo{Name: "search"}, &managedToolInvoker{}, ToolInvocationRequest{}, nil)
	if err != nil {
		t.Fatalf("build managed tool: %v", err)
	}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TraceID: "trace_1"})
	if _, err := tool.InvokableRun(ctx, `{}`); !errors.Is(err, ErrRuntimeEventEmitterMissing) {
		t.Fatalf("expected missing bridge rejection, got %v", err)
	}
}

func TestEinoToolProxyMapsHarnessInterruptToEinoStatefulInterrupt(t *testing.T) {
	invoker := &managedToolInvoker{interrupt: &ToolInvocationInterruptedError{
		Info:  map[string]string{"type": "ask_user", "question": "continue?"},
		State: json.RawMessage(`{"cursor":2}`),
	}}
	managed, err := NewEinoToolProxy(
		&schema.ToolInfo{Name: "approval"},
		invoker,
		ToolInvocationRequest{SessionID: "session_1", RunID: "run_1", AgentID: "agent_1"},
		managedToolIDGenerator{requestID: "tool_call_interrupt"},
	)
	if err != nil {
		t.Fatalf("build managed tool: %v", err)
	}
	emitter := &capturingRuntimeEmitter{}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TraceID: "trace_1"})
	ctx = WithRuntimeEventEmitter(ctx, emitter)
	_, err = managed.InvokableRun(ctx, `{}`)
	if err == nil {
		t.Fatal("expected eino interrupt")
	}
	stateField := reflect.Indirect(reflect.ValueOf(err)).FieldByName("State")
	if !stateField.IsValid() || !stateField.CanInterface() {
		t.Fatalf("harness interrupt was not mapped to eino interrupt: %T %v", err, err)
	}
	state, ok := stateField.Interface().(EinoToolProxyInterruptState)
	if !ok || state.SchemaVersion != EinoToolProxyInterruptStateSchemaVersion || state.ToolCallID != "tool_call_interrupt" || string(state.GatewayState) != `{"cursor":2}` {
		t.Fatalf("eino interrupt state was not preserved: %#v", stateField.Interface())
	}
	if len(emitter.events) != 1 || emitter.events[0].EventType != observability.EventToolCallStarted {
		t.Fatalf("interrupted tool lifecycle is invalid: %#v", emitter.events)
	}
}

func TestEinoToolProxyRejectsInvalidArgumentsBeforeGateway(t *testing.T) {
	invoker := &managedToolInvoker{}
	tool, err := NewEinoToolProxy(&schema.ToolInfo{Name: "search"}, invoker, ToolInvocationRequest{}, nil)
	if err != nil {
		t.Fatalf("build managed tool: %v", err)
	}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TraceID: "trace_1"})
	ctx = WithRuntimeEventEmitter(ctx, &capturingRuntimeEmitter{})
	if _, err := tool.InvokableRun(ctx, `{`); !errors.Is(err, ErrEinoToolProxyArguments) {
		t.Fatalf("expected invalid arguments rejection, got %v", err)
	}
	if invoker.calls != 0 {
		t.Fatalf("gateway called for invalid arguments: %d", invoker.calls)
	}
}

func TestEinoToolProxyRejectsIncompleteGatewayLifecycle(t *testing.T) {
	invoker := &managedToolInvoker{omitTerminal: true, result: ToolInvocationResult{Content: "unsafe"}}
	tool, err := NewEinoToolProxy(&schema.ToolInfo{Name: "search"}, invoker, ToolInvocationRequest{}, nil)
	if err != nil {
		t.Fatalf("build managed tool: %v", err)
	}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TraceID: "trace_1"})
	ctx = WithRuntimeEventEmitter(ctx, &capturingRuntimeEmitter{})
	if _, err := tool.InvokableRun(ctx, `{}`); !errors.Is(err, ErrEinoToolProxyLifecycle) {
		t.Fatalf("expected incomplete lifecycle rejection, got %v", err)
	}
}

func TestEinoToolProxyToRuntimePreservesRuntimeErrorInCanonicalEvent(t *testing.T) {
	tests := []struct {
		name       string
		runtimeErr *RuntimeError
	}{
		{
			name:       "permission is permanent",
			runtimeErr: NewRuntimeError(ErrorPermissionDenied, "TOOL_PERMISSION_DENIED", "tool invocation is not permitted"),
		},
		{
			name: "upstream may retry",
			runtimeErr: NewRuntimeError(ErrorDependencyUnavailable, "TOOL_UPSTREAM_ERROR", "tool upstream dependency failed").
				WithRetryable(true).
				WithDependency("tool_gateway"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool, err := NewEinoToolProxy(
				&schema.ToolInfo{Name: "search"},
				&managedToolInvoker{err: tt.runtimeErr},
				ToolInvocationRequest{SessionID: "session_1", RunID: "run_1", AgentID: "agent_1"},
				managedToolIDGenerator{requestID: "tool_call_1"},
			)
			if err != nil {
				t.Fatal(err)
			}
			logger := observability.NewRingLogger(observability.NoopLogger{}, 4)
			ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TraceID: "trace_1", RunID: "run_1"})
			ctx = observability.WithLogger(ctx, logger)
			ctx = WithRuntimeEventEmitter(ctx, &capturingRuntimeEmitter{})
			_, proxyErr := tool.InvokableRun(ctx, `{}`)
			var preserved *RuntimeError
			if !errors.As(proxyErr, &preserved) || preserved.Code != tt.runtimeErr.Code || preserved.Retryable != tt.runtimeErr.Retryable {
				t.Fatalf("proxy error=%#v, want=%#v", preserved, tt.runtimeErr)
			}

			out := make(chan observability.AgentEvent, 1)
			runtime := &EinoRuntime{}
			if terminal := runtime.handleEinoEvent(ctx, RunRequest{}, "", &adk.AgentEvent{Err: fmt.Errorf("eino tool node: %w", proxyErr)}, nil, out); !terminal {
				t.Fatal("typed tool error did not terminate Eino run")
			}
			event := <-out
			if event.EventType != observability.EventRunFailed || event.Error == nil ||
				event.Error.Code != tt.runtimeErr.Code || event.Error.Retryable != tt.runtimeErr.Retryable {
				t.Fatalf("canonical event=%#v, want=%#v", event, tt.runtimeErr.EventError())
			}
			logs := logger.QueryLogs(observability.LogQuery{RunID: "run_1", Level: "error"})
			if len(logs) != 1 || logs[0].Message != "eino runtime event failed" ||
				logs[0].Error == "" || logs[0].Fields["runtime_error_code"] != tt.runtimeErr.Code {
				t.Fatalf("runtime error log=%#v", logs)
			}
		})
	}
}

type managedToolInvoker struct {
	calls        int
	request      ToolInvocationRequest
	result       ToolInvocationResult
	interrupt    *ToolInvocationInterruptedError
	err          error
	omitTerminal bool
}

func (i *managedToolInvoker) Invoke(ctx context.Context, req ToolInvocationRequest, sink ToolEventSink) (ToolInvocationResult, error) {
	i.calls++
	i.request = req
	if err := sink.Emit(ctx, observability.AgentEvent{EventType: observability.EventToolCallStarted, Visibility: observability.VisibilityDebug}); err != nil {
		return ToolInvocationResult{}, err
	}
	if i.interrupt != nil {
		return ToolInvocationResult{}, i.interrupt
	}
	if i.err != nil {
		return ToolInvocationResult{}, i.err
	}
	if !i.omitTerminal {
		terminal := observability.EventToolCallCompleted
		if i.result.IsError {
			terminal = observability.EventToolCallFailed
		}
		if err := sink.Emit(ctx, observability.AgentEvent{EventType: terminal, Visibility: observability.VisibilityDebug}); err != nil {
			return ToolInvocationResult{}, err
		}
	}
	return i.result, nil
}

type capturingRuntimeEmitter struct {
	events []observability.AgentEvent
}

func (e *capturingRuntimeEmitter) Emit(_ context.Context, event observability.AgentEvent) error {
	e.events = append(e.events, event)
	return nil
}

type managedToolIDGenerator struct {
	requestID string
}

func (g managedToolIDGenerator) NewTraceID() string   { return "trace" }
func (g managedToolIDGenerator) NewSpanID() string    { return "span" }
func (g managedToolIDGenerator) NewRunID() string     { return "run" }
func (g managedToolIDGenerator) NewEventID() string   { return "event" }
func (g managedToolIDGenerator) NewRequestID() string { return g.requestID }
