package context

import (
	"context"
	"sync"
	"time"
)

// Session represents a conversation session.
type Session struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	TenantID    string    `json:"tenant_id,omitempty"`
	Status      string    `json:"status"`
	CurrentTurn int       `json:"current_turn"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// SessionStore manages session lifecycle.
type SessionStore interface {
	Create(ctx context.Context, s *Session) error
	Get(ctx context.Context, id string) (*Session, error)
	Update(ctx context.Context, s *Session) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, userID string) ([]*Session, error)
}

// InMemorySessionStore is a mutex-protected in-memory SessionStore.
type InMemorySessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewInMemorySessionStore() *InMemorySessionStore {
	return &InMemorySessionStore{sessions: make(map[string]*Session)}
}

func (s *InMemorySessionStore) Create(_ context.Context, sess *Session) error {
	if sess.ID == "" {
		return ErrSessionIDMissing
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := *sess
	clone.CreatedAt = time.Now()
	clone.UpdatedAt = clone.CreatedAt
	s.sessions[sess.ID] = &clone
	return nil
}

func (s *InMemorySessionStore) Get(_ context.Context, id string) (*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, ErrSessionNotFound
	}
	clone := *sess
	return &clone, nil
}

func (s *InMemorySessionStore) Update(_ context.Context, sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[sess.ID]; !ok {
		return ErrSessionNotFound
	}
	clone := *sess
	clone.UpdatedAt = time.Now()
	s.sessions[sess.ID] = &clone
	return nil
}

func (s *InMemorySessionStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	return nil
}

func (s *InMemorySessionStore) List(_ context.Context, userID string) ([]*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*Session
	for _, sess := range s.sessions {
		if userID == "" || sess.UserID == userID {
			clone := *sess
			result = append(result, &clone)
		}
	}
	return result, nil
}
