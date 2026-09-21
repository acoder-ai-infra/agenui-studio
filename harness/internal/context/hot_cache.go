package context

import (
	"context"
	"fmt"
	"sync"
)

// HotContextStore is the replaceable Redis/Tair boundary. CompareAndSwap
// prevents concurrent turns from silently overwriting a newer hot projection.
type HotContextStore interface {
	HotContextReader
	CompareAndSwap(ctx context.Context, sessionID string, expectedVersion int64, next HotContextView) (HotContextView, error)
}

// InMemoryHotContextStore is deterministic test/development infrastructure.
// It uses shared synchronization only; it never allocates per-session workers.
type InMemoryHotContextStore struct {
	mu    sync.RWMutex
	views map[string]HotContextView
}

var _ HotContextStore = (*InMemoryHotContextStore)(nil)

func NewInMemoryHotContextStore() *InMemoryHotContextStore {
	return &InMemoryHotContextStore{views: make(map[string]HotContextView)}
}

func (s *InMemoryHotContextStore) Load(_ context.Context, sessionID string, maxMessages int) (HotContextView, error) {
	if sessionID == "" {
		return HotContextView{}, ErrSessionIDMissing
	}
	s.mu.RLock()
	view, ok := s.views[sessionID]
	s.mu.RUnlock()
	if !ok {
		return HotContextView{}, ErrHotContextMiss
	}
	view = cloneHotContextView(view)
	if maxMessages > 0 && len(view.Messages) > maxMessages {
		view.Messages = append([]Message(nil), view.Messages[len(view.Messages)-maxMessages:]...)
	}
	return view, nil
}

func (s *InMemoryHotContextStore) CompareAndSwap(_ context.Context, sessionID string, expectedVersion int64, next HotContextView) (HotContextView, error) {
	if sessionID == "" {
		return HotContextView{}, ErrSessionIDMissing
	}
	if expectedVersion < 0 {
		return HotContextView{}, fmt.Errorf("%w: negative expected version", ErrHotContextConflict)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.views[sessionID]
	if current.Version != expectedVersion {
		return HotContextView{}, fmt.Errorf("%w: expected=%d actual=%d", ErrHotContextConflict, expectedVersion, current.Version)
	}
	next = cloneHotContextView(next)
	for i := range next.Messages {
		next.Messages[i].CurrentInput = false
	}
	next.Version = current.Version + 1
	s.views[sessionID] = next
	return cloneHotContextView(next), nil
}

func cloneHotContextView(view HotContextView) HotContextView {
	view.Messages = cloneMessageSlice(view.Messages)
	view.Fragments = cloneFragments(view.Fragments)
	return view
}
