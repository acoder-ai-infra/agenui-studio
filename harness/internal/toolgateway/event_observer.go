package toolgateway

import (
	"context"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type persistedEventSinkContextKey struct{}

func withPersistedEventSink(ctx context.Context, sink PersistedEventSink) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, persistedEventSinkContextKey{}, sink)
}

func emitPersistedEvent(ctx context.Context, event observability.AgentEvent) error {
	sink, _ := ctx.Value(persistedEventSinkContextKey{}).(PersistedEventSink)
	if sink == nil {
		return nil
	}
	return sink.Emit(ctx, cloneAgentEvent(event))
}
