package agentruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestToolEventSinksAcceptExplicitPreExecutionRejection(t *testing.T) {
	event := preExecutionFailureEvent(false)

	t.Run("direct", func(t *testing.T) {
		out := make(chan observability.AgentEvent, 1)
		sink := newDirectToolEventSink(context.Background(), out)
		if err := sink.Emit(context.Background(), event); err != nil {
			t.Fatalf("emit pre-execution rejection: %v", err)
		}
		if err := sink.ValidateComplete(true); err != nil {
			t.Fatalf("validate pre-execution rejection: %v", err)
		}
	})

	t.Run("eino", func(t *testing.T) {
		sink := newRuntimeToolEventSink(&capturingRuntimeEmitter{}, false)
		if err := sink.Emit(context.Background(), event); err != nil {
			t.Fatalf("emit pre-execution rejection: %v", err)
		}
		if err := sink.ValidateComplete(true); err != nil {
			t.Fatalf("validate pre-execution rejection: %v", err)
		}
	})
}

func TestToolEventSinksRejectAmbiguousOrPostExecutionFailureWithoutStart(t *testing.T) {
	for _, event := range []observability.AgentEvent{
		{EventType: observability.EventToolCallFailed},
		preExecutionFailureEvent(true),
	} {
		direct := newDirectToolEventSink(context.Background(), make(chan observability.AgentEvent, 1))
		if err := direct.Emit(context.Background(), event); !errors.Is(err, ErrDirectInvalidToolEvent) {
			t.Fatalf("direct accepted invalid failure: %v", err)
		}
		eino := newRuntimeToolEventSink(&capturingRuntimeEmitter{}, false)
		if err := eino.Emit(context.Background(), event); !errors.Is(err, ErrEinoToolProxyLifecycle) {
			t.Fatalf("eino accepted invalid failure: %v", err)
		}
	}
}

func preExecutionFailureEvent(executed bool) observability.AgentEvent {
	return observability.AgentEvent{
		EventType: observability.EventToolCallFailed,
		PayloadPreview: JSONPayload(map[string]any{
			"tool_failure": map[string]any{"executed": executed},
		}),
	}
}
