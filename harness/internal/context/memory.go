package context

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemoryItem represents a single memory entry.
type MemoryItem struct {
	Key            string         `json:"key"`
	Value          map[string]any `json:"value"`
	Namespace      []string       `json:"namespace"`
	Category       string         `json:"category,omitempty"`
	Source         string         `json:"source,omitempty"`
	Importance     float64        `json:"importance"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	LastAccessedAt time.Time      `json:"last_accessed_at"`
	ExpiresAt      *time.Time     `json:"expires_at,omitempty"`
}

// MemorySearchOptions configures a memory search.
type MemorySearchOptions struct {
	Limit      int
	MinScore   float64
	Categories []string
}

// MemoryStore provides namespace-scoped KV storage for long-term memory.
type MemoryStore interface {
	Get(ctx context.Context, namespace []string, key string) (*MemoryItem, error)
	Put(ctx context.Context, item *MemoryItem) error
	Delete(ctx context.Context, namespace []string, key string) error
	Search(ctx context.Context, namespace []string, opts MemorySearchOptions) ([]*MemoryItem, error)
}

// InMemoryMemoryStore is a mutex-protected in-memory MemoryStore.
type InMemoryMemoryStore struct {
	mu    sync.RWMutex
	items map[string]*MemoryItem // compositeKey → item
}

func NewInMemoryMemoryStore() *InMemoryMemoryStore {
	return &InMemoryMemoryStore{items: make(map[string]*MemoryItem)}
}

func compositeKey(namespace []string, key string) string {
	return strings.Join(namespace, "/") + "/" + key
}

func (s *InMemoryMemoryStore) Get(_ context.Context, namespace []string, key string) (*MemoryItem, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, ok := s.items[compositeKey(namespace, key)]
	if !ok {
		return nil, ErrMemoryNotFound
	}
	clone := *item
	clone.LastAccessedAt = time.Now()
	return &clone, nil
}

func (s *InMemoryMemoryStore) Put(_ context.Context, item *MemoryItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	clone := *item
	if clone.CreatedAt.IsZero() {
		clone.CreatedAt = now
	}
	clone.UpdatedAt = now
	if clone.LastAccessedAt.IsZero() {
		clone.LastAccessedAt = now
	}
	s.items[compositeKey(item.Namespace, item.Key)] = &clone
	return nil
}

func (s *InMemoryMemoryStore) Delete(_ context.Context, namespace []string, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, compositeKey(namespace, key))
	return nil
}

func (s *InMemoryMemoryStore) Search(_ context.Context, namespace []string, opts MemorySearchOptions) ([]*MemoryItem, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	prefix := strings.Join(namespace, "/")
	now := time.Now()
	var result []*MemoryItem

	for _, item := range s.items {
		itemPrefix := strings.Join(item.Namespace, "/")
		if !strings.HasPrefix(itemPrefix, prefix) {
			continue
		}
		// Skip expired.
		if item.ExpiresAt != nil && item.ExpiresAt.Before(now) {
			continue
		}
		// Category filter.
		if len(opts.Categories) > 0 {
			found := false
			for _, c := range opts.Categories {
				if item.Category == c {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		// Min importance filter.
		if item.Importance < opts.MinScore {
			continue
		}
		clone := *item
		result = append(result, &clone)
	}

	// Sort by UpdatedAt descending.
	sort.Slice(result, func(i, j int) bool {
		return result[i].UpdatedAt.After(result[j].UpdatedAt)
	})

	if opts.Limit > 0 && len(result) > opts.Limit {
		result = result[:opts.Limit]
	}
	return result, nil
}
