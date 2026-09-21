package context

import (
	"context"
	"strings"
	"sync"
	"time"
)

// MutationEvent records a state change.
type MutationEvent struct {
	SessionID string    `json:"session_id"`
	Path      string    `json:"path"`
	OldValue  any       `json:"old_value,omitempty"`
	NewValue  any       `json:"new_value"`
	Writer    Writer    `json:"writer"`
	Timestamp time.Time `json:"timestamp"`
	TraceID   string    `json:"trace_id,omitempty"`
}

// EventHandler processes a mutation event.
type EventHandler func(MutationEvent)

// Subscription represents an active event subscription.
type Subscription interface {
	Unsubscribe()
}

// EventBus publishes and subscribes to state mutation events.
type EventBus interface {
	Publish(ctx context.Context, event MutationEvent) error
	Subscribe(pattern string, handler EventHandler) Subscription
	History(sessionID string) []MutationEvent
}

type subscription struct {
	bus     *InMemoryEventBus
	pattern string
	id      int
}

func (s *subscription) Unsubscribe() {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	handlers := s.bus.handlers[s.pattern]
	for i, h := range handlers {
		if h.id == s.id {
			s.bus.handlers[s.pattern] = append(handlers[:i], handlers[i+1:]...)
			break
		}
	}
}

type registeredHandler struct {
	id      int
	handler EventHandler
}

// InMemoryEventBus is a mutex-protected in-memory EventBus.
type InMemoryEventBus struct {
	mu       sync.RWMutex
	handlers map[string][]registeredHandler
	history  map[string][]MutationEvent
	nextID   int
}

func NewInMemoryEventBus() *InMemoryEventBus {
	return &InMemoryEventBus{
		handlers: make(map[string][]registeredHandler),
		history:  make(map[string][]MutationEvent),
	}
}

func (b *InMemoryEventBus) Publish(_ context.Context, event MutationEvent) error {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	b.mu.Lock()
	b.history[event.SessionID] = append(b.history[event.SessionID], event)

	// Collect matching handlers under lock, invoke outside.
	var matched []EventHandler
	for pattern, handlers := range b.handlers {
		if matchEventPattern(pattern, event.Path) {
			for _, h := range handlers {
				matched = append(matched, h.handler)
			}
		}
	}
	b.mu.Unlock()

	for _, h := range matched {
		h(event)
	}
	return nil
}

func (b *InMemoryEventBus) Subscribe(pattern string, handler EventHandler) Subscription {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	id := b.nextID
	b.handlers[pattern] = append(b.handlers[pattern], registeredHandler{id: id, handler: handler})
	return &subscription{bus: b, pattern: pattern, id: id}
}

func (b *InMemoryEventBus) History(sessionID string) []MutationEvent {
	b.mu.RLock()
	defer b.mu.RUnlock()
	src := b.history[sessionID]
	out := make([]MutationEvent, len(src))
	copy(out, src)
	return out
}

// matchEventPattern matches event path patterns.
// Supports exact match and "task.*" wildcard prefix matching.
func matchEventPattern(pattern, path string) bool {
	if pattern == path {
		return true
	}
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		prefix := strings.TrimSuffix(pattern, ".*")
		return strings.HasPrefix(path, prefix+".") || path == prefix
	}
	return false
}
