package agentruntime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

var (
	ErrRuntimeEventBridgeClosed = errors.New("runtime event bridge closed")
	ErrRuntimeEventBridgeFull   = errors.New("runtime event bridge full")
)

type RuntimeEventEmitter interface {
	Emit(ctx context.Context, event observability.AgentEvent) error
}

type RuntimeEventTryEmitter interface {
	TryEmit(ctx context.Context, event observability.AgentEvent) error
}

type runtimeEventEmitterContextKey struct{}
type runtimeParentStepContextKey struct{}

func WithRuntimeEventEmitter(ctx context.Context, emitter RuntimeEventEmitter) context.Context {
	return context.WithValue(ctx, runtimeEventEmitterContextKey{}, emitter)
}

func RuntimeEventEmitterFrom(ctx context.Context) (RuntimeEventEmitter, bool) {
	emitter, ok := ctx.Value(runtimeEventEmitterContextKey{}).(RuntimeEventEmitter)
	return emitter, ok && emitter != nil
}

func WithRuntimeParentStepID(ctx context.Context, stepID string) context.Context {
	return context.WithValue(ctx, runtimeParentStepContextKey{}, stepID)
}

func RuntimeParentStepIDFrom(ctx context.Context) string {
	stepID, _ := ctx.Value(runtimeParentStepContextKey{}).(string)
	return stepID
}

type runtimeEventBridge struct {
	events chan observability.AgentEvent
	done   chan struct{}
	once   sync.Once
	closed atomic.Bool
}

func newRuntimeEventBridge(buffer int) *runtimeEventBridge {
	if buffer <= 0 {
		buffer = 32
	}
	return &runtimeEventBridge{events: make(chan observability.AgentEvent, buffer), done: make(chan struct{})}
}

func (b *runtimeEventBridge) Emit(ctx context.Context, event observability.AgentEvent) error {
	return emitRuntimeEventWithCommitBarrier(ctx, event, func(event observability.AgentEvent) error {
		return b.enqueue(ctx, event)
	})
}

func (b *runtimeEventBridge) enqueue(ctx context.Context, event observability.AgentEvent) error {
	if b.closed.Load() {
		return ErrRuntimeEventBridgeClosed
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.done:
		return ErrRuntimeEventBridgeClosed
	case b.events <- event:
		return nil
	}
}

func (b *runtimeEventBridge) TryEmit(ctx context.Context, event observability.AgentEvent) error {
	if b.closed.Load() {
		return ErrRuntimeEventBridgeClosed
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.done:
		return ErrRuntimeEventBridgeClosed
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.done:
		return ErrRuntimeEventBridgeClosed
	case b.events <- event:
		return nil
	default:
		return ErrRuntimeEventBridgeFull
	}
}

func (b *runtimeEventBridge) Close() {
	b.once.Do(func() {
		b.closed.Store(true)
		close(b.done)
	})
}
