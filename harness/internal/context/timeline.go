package context

import (
	"context"
	"sync"
)

// Timeline is a mutable hot conversation view. It is not the durable fact
// ledger and may be truncated or rebuilt at any time.
type Timeline interface {
	Append(ctx context.Context, sessionID string, msgs ...Message) error
	GetAll(ctx context.Context, sessionID string) ([]Message, error)
	Truncate(ctx context.Context, sessionID string, keepLast int) error
	Len(ctx context.Context, sessionID string) (int, error)
}

type InMemoryTimeline struct {
	mu       sync.RWMutex
	messages map[string][]Message
}

func NewInMemoryTimeline() *InMemoryTimeline {
	return &InMemoryTimeline{messages: make(map[string][]Message)}
}

func (t *InMemoryTimeline) Append(_ context.Context, sessionID string, msgs ...Message) error {
	if sessionID == "" {
		return ErrSessionIDMissing
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, msg := range msgs {
		msg.SessionID = sessionID
		t.messages[sessionID] = append(t.messages[sessionID], cloneMessageValue(msg))
	}
	return nil
}

func (t *InMemoryTimeline) GetAll(_ context.Context, sessionID string) ([]Message, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	src := t.messages[sessionID]
	if len(src) == 0 {
		return nil, nil
	}
	out := make([]Message, len(src))
	for i := range src {
		out[i] = cloneMessageValue(src[i])
	}
	return out, nil
}

func (t *InMemoryTimeline) Truncate(_ context.Context, sessionID string, keepLast int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	msgs := t.messages[sessionID]
	if keepLast <= 0 || keepLast >= len(msgs) {
		return nil
	}
	kept := make([]Message, keepLast)
	copy(kept, msgs[len(msgs)-keepLast:])
	t.messages[sessionID] = kept
	return nil
}

func (t *InMemoryTimeline) Len(_ context.Context, sessionID string) (int, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.messages[sessionID]), nil
}
