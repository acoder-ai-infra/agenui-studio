package protocol

import (
	"context"
	"sync"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// EventBroker is the in-process fan-out of post-persist events (P3-D2 live
// subscribe). Publishers only publish already-persisted events (source =
// dispatcher.Result.Events, after Bridge.AppendEvent), so the persist-before-push
// invariant holds. P0 is an in-memory implementation; distributed (scheduled /
// multi-process) delivery is a P1 target (Redis / MQ pub/sub).
type EventBroker interface {
	Publish(ctx context.Context, ev observability.AgentEvent) error
	// Subscribe returns a stream of events for runID plus a cancel func. The
	// stream is closed when cancel is called.
	Subscribe(ctx context.Context, runID string) (<-chan observability.AgentEvent, func(), error)
}

// SessionSubscriber is an optional broker capability: a stream of every event in
// a session (across all its runs), used to fan a parent run's descendants
// (sub-agent child runs share the parent's SessionID) into one run-tree stream.
// A broker that does not implement it degrades to per-run Subscribe.
type SessionSubscriber interface {
	// SubscribeSession returns a stream of every event whose SessionID == sessionID
	// plus a cancel func. The stream is closed when cancel is called.
	SubscribeSession(ctx context.Context, sessionID string) (<-chan observability.AgentEvent, func(), error)
}

// MemoryBroker is the P0 in-process EventBroker. It fans out to all subscribers
// of a run. Delivery is best-effort per subscriber (a lagging subscriber may
// drop frames past its buffer) because the EventStore is the source of truth and
// clients recover via replay.
type MemoryBroker struct {
	mu       sync.Mutex
	subs     map[string]map[int]chan observability.AgentEvent
	sessSubs map[string]map[int]chan observability.AgentEvent
	next     int
	buffer   int
}

// NewMemoryBroker builds a MemoryBroker with a default per-subscriber buffer.
func NewMemoryBroker() *MemoryBroker {
	return &MemoryBroker{
		subs:     make(map[string]map[int]chan observability.AgentEvent),
		sessSubs: make(map[string]map[int]chan observability.AgentEvent),
		buffer:   256,
	}
}

// Publish fans ev out to every live subscriber of ev.RunID and every session
// subscriber of ev.SessionID (the latter powers run-tree fan-in).
func (b *MemoryBroker) Publish(_ context.Context, ev observability.AgentEvent) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs[ev.RunID] {
		select {
		case ch <- ev:
		default:
			// Subscriber lagging; event remains durable in the EventStore.
		}
	}
	if ev.SessionID != "" {
		for _, ch := range b.sessSubs[ev.SessionID] {
			select {
			case ch <- ev:
			default:
			}
		}
	}
	return nil
}

// Subscribe registers a new subscriber for runID. The returned cancel func
// unregisters and closes the stream; it is safe to call more than once.
func (b *MemoryBroker) Subscribe(_ context.Context, runID string) (<-chan observability.AgentEvent, func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.registerLocked(b.subs, runID)
}

// SubscribeSession registers a new subscriber for every event in sessionID
// (across all its runs). It powers run-tree fan-in: descendant child runs share
// the parent's SessionID, so one session subscription catches dynamically
// created children the caller filters down to the run tree it cares about.
func (b *MemoryBroker) SubscribeSession(_ context.Context, sessionID string) (<-chan observability.AgentEvent, func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.registerLocked(b.sessSubs, sessionID)
}

// registerLocked registers a subscriber under index[key]; caller holds b.mu.
func (b *MemoryBroker) registerLocked(index map[string]map[int]chan observability.AgentEvent, key string) (<-chan observability.AgentEvent, func(), error) {
	if index[key] == nil {
		index[key] = make(map[int]chan observability.AgentEvent)
	}
	id := b.next
	b.next++
	ch := make(chan observability.AgentEvent, b.buffer)
	index[key][id] = ch

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if m, ok := index[key]; ok {
				if c, ok := m[id]; ok {
					delete(m, id)
					close(c)
				}
				if len(m) == 0 {
					delete(index, key)
				}
			}
		})
	}
	return ch, cancel, nil
}

var (
	_ EventBroker       = (*MemoryBroker)(nil)
	_ SessionSubscriber = (*MemoryBroker)(nil)
)
