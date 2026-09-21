package agentruntime

import (
	"context"
	"errors"
	"sync"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

var (
	ErrRuntimeEventCommitBarrierClosed  = errors.New("runtime event commit barrier closed")
	ErrRuntimeEventCommitAlreadyPending = errors.New("runtime event commit already pending")
)

// runtimeEventCommitBarrier joins an adapter-side causal boundary to the
// RuntimeService-owned durable append. It never writes storage itself: the
// adapter registers an event before forwarding it, while RuntimeService
// acknowledges the same event_id only after Event Store has assigned the
// canonical sequence.
type runtimeEventCommitBarrier struct {
	ids observability.IDGenerator

	mu      sync.Mutex
	pending map[string]chan error
	closed  bool
	err     error
}

type runtimeEventCommitBarrierContextKey struct{}

func newRuntimeEventCommitBarrier(ids observability.IDGenerator) *runtimeEventCommitBarrier {
	return &runtimeEventCommitBarrier{ids: ids, pending: make(map[string]chan error)}
}

func withRuntimeEventCommitBarrier(ctx context.Context, barrier *runtimeEventCommitBarrier) context.Context {
	if barrier == nil {
		return ctx
	}
	return context.WithValue(ctx, runtimeEventCommitBarrierContextKey{}, barrier)
}

func runtimeEventCommitBarrierFrom(ctx context.Context) (*runtimeEventCommitBarrier, bool) {
	barrier, ok := ctx.Value(runtimeEventCommitBarrierContextKey{}).(*runtimeEventCommitBarrier)
	return barrier, ok && barrier != nil
}

func (b *runtimeEventCommitBarrier) register(event observability.AgentEvent) (observability.AgentEvent, <-chan error, error) {
	if b == nil {
		return event, nil, ErrRuntimeEventCommitBarrierClosed
	}
	if event.EventID == "" {
		if b.ids == nil {
			return event, nil, ErrRuntimeEventCommitBarrierClosed
		}
		event.EventID = b.ids.NewEventID()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		err := b.err
		if err == nil {
			err = ErrRuntimeEventCommitBarrierClosed
		}
		return event, nil, err
	}
	if _, exists := b.pending[event.EventID]; exists {
		return event, nil, ErrRuntimeEventCommitAlreadyPending
	}
	ack := make(chan error, 1)
	b.pending[event.EventID] = ack
	return event, ack, nil
}

func (b *runtimeEventCommitBarrier) acknowledge(eventID string, err error) {
	if b == nil || eventID == "" {
		return
	}
	b.mu.Lock()
	ack := b.pending[eventID]
	delete(b.pending, eventID)
	b.mu.Unlock()
	if ack != nil {
		ack <- err
		close(ack)
	}
}

func (b *runtimeEventCommitBarrier) failAll(err error) {
	if b == nil {
		return
	}
	if err == nil {
		err = ErrRuntimeEventCommitBarrierClosed
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	b.err = err
	pending := b.pending
	b.pending = make(map[string]chan error)
	b.mu.Unlock()
	for _, ack := range pending {
		ack <- err
		close(ack)
	}
}

func emitRuntimeEventWithCommitBarrier(
	ctx context.Context,
	event observability.AgentEvent,
	send func(observability.AgentEvent) error,
) error {
	// Sequence is owned by Event Store. A runtime/model adapter may accidentally
	// carry a positive upstream sequence, but that value is not proof that this
	// process has durably appended the event. Every model completion must cross
	// the RuntimeService commit barrier before a tool side effect may start.
	if event.EventType != observability.EventModelCallCompleted {
		return send(event)
	}
	barrier, ok := runtimeEventCommitBarrierFrom(ctx)
	if !ok {
		return send(event)
	}
	prepared, ack, err := barrier.register(event)
	if err != nil {
		return err
	}
	if err = send(prepared); err != nil {
		barrier.acknowledge(prepared.EventID, err)
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case commitErr := <-ack:
		return commitErr
	}
}
