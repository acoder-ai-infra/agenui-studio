package context

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestInMemoryMemoryStorePutAndGet(t *testing.T) {
	store := NewInMemoryMemoryStore()
	ctx := context.Background()

	item := &MemoryItem{
		Key:        "pref_lang",
		Value:      map[string]any{"content": "Go"},
		Namespace:  []string{"user", "alice"},
		Category:   "preference",
		Importance: 0.8,
	}
	if err := store.Put(ctx, item); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := store.Get(ctx, []string{"user", "alice"}, "pref_lang")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Key != "pref_lang" || got.Category != "preference" {
		t.Fatalf("unexpected item: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatal("timestamps should be auto-set")
	}
}

func TestInMemoryMemoryStoreGetNotFound(t *testing.T) {
	store := NewInMemoryMemoryStore()
	_, err := store.Get(context.Background(), []string{"user", "alice"}, "nonexistent")
	if !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("expected ErrMemoryNotFound, got: %v", err)
	}
}

func TestInMemoryMemoryStoreDelete(t *testing.T) {
	store := NewInMemoryMemoryStore()
	ctx := context.Background()

	_ = store.Put(ctx, &MemoryItem{Key: "k1", Value: map[string]any{}, Namespace: []string{"user", "alice"}})
	_ = store.Delete(ctx, []string{"user", "alice"}, "k1")

	_, err := store.Get(ctx, []string{"user", "alice"}, "k1")
	if !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("expected ErrMemoryNotFound after delete, got: %v", err)
	}
}

func TestInMemoryMemoryStoreSearchByNamespace(t *testing.T) {
	store := NewInMemoryMemoryStore()
	ctx := context.Background()

	_ = store.Put(ctx, &MemoryItem{Key: "k1", Value: map[string]any{}, Namespace: []string{"user", "alice"}, Importance: 0.9})
	_ = store.Put(ctx, &MemoryItem{Key: "k2", Value: map[string]any{}, Namespace: []string{"user", "alice"}, Importance: 0.5})
	_ = store.Put(ctx, &MemoryItem{Key: "k3", Value: map[string]any{}, Namespace: []string{"user", "bob"}, Importance: 0.7})

	// Search alice's namespace.
	items, err := store.Search(ctx, []string{"user", "alice"}, MemorySearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items for alice, got %d", len(items))
	}

	// Search broader prefix.
	all, _ := store.Search(ctx, []string{"user"}, MemorySearchOptions{})
	if len(all) != 3 {
		t.Fatalf("expected 3 items under user prefix, got %d", len(all))
	}
}

func TestInMemoryMemoryStoreSearchCategoryFilter(t *testing.T) {
	store := NewInMemoryMemoryStore()
	ctx := context.Background()

	_ = store.Put(ctx, &MemoryItem{Key: "k1", Value: map[string]any{}, Namespace: []string{"user", "alice"}, Category: "preference", Importance: 0.9})
	_ = store.Put(ctx, &MemoryItem{Key: "k2", Value: map[string]any{}, Namespace: []string{"user", "alice"}, Category: "mistake", Importance: 0.8})
	_ = store.Put(ctx, &MemoryItem{Key: "k3", Value: map[string]any{}, Namespace: []string{"user", "alice"}, Category: "preference", Importance: 0.7})

	items, _ := store.Search(ctx, []string{"user", "alice"}, MemorySearchOptions{
		Categories: []string{"preference"},
	})
	if len(items) != 2 {
		t.Fatalf("expected 2 preference items, got %d", len(items))
	}
}

func TestInMemoryMemoryStoreSearchMinScore(t *testing.T) {
	store := NewInMemoryMemoryStore()
	ctx := context.Background()

	_ = store.Put(ctx, &MemoryItem{Key: "k1", Value: map[string]any{}, Namespace: []string{"u"}, Importance: 0.9})
	_ = store.Put(ctx, &MemoryItem{Key: "k2", Value: map[string]any{}, Namespace: []string{"u"}, Importance: 0.3})

	items, _ := store.Search(ctx, []string{"u"}, MemorySearchOptions{MinScore: 0.5})
	if len(items) != 1 {
		t.Fatalf("expected 1 item above 0.5, got %d", len(items))
	}
}

func TestInMemoryMemoryStoreSearchLimit(t *testing.T) {
	store := NewInMemoryMemoryStore()
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		_ = store.Put(ctx, &MemoryItem{
			Key:       fmt.Sprintf("k%d", i),
			Value:     map[string]any{"i": i},
			Namespace: []string{"u"},
		})
	}

	items, _ := store.Search(ctx, []string{"u"}, MemorySearchOptions{Limit: 3})
	if len(items) != 3 {
		t.Fatalf("expected 3 items with limit, got %d", len(items))
	}
}

func TestInMemoryMemoryStoreSearchExpired(t *testing.T) {
	store := NewInMemoryMemoryStore()
	ctx := context.Background()

	past := time.Now().Add(-1 * time.Hour)
	_ = store.Put(ctx, &MemoryItem{Key: "expired", Value: map[string]any{}, Namespace: []string{"u"}, ExpiresAt: &past})
	_ = store.Put(ctx, &MemoryItem{Key: "alive", Value: map[string]any{}, Namespace: []string{"u"}})

	items, _ := store.Search(ctx, []string{"u"}, MemorySearchOptions{})
	if len(items) != 1 {
		t.Fatalf("expected 1 non-expired item, got %d", len(items))
	}
	if items[0].Key != "alive" {
		t.Fatalf("expected 'alive', got: %s", items[0].Key)
	}
}

func TestInMemoryMemoryStoreConcurrency(t *testing.T) {
	store := NewInMemoryMemoryStore()
	ctx := context.Background()

	done := make(chan struct{})
	for i := 0; i < 50; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			_ = store.Put(ctx, &MemoryItem{
				Key: "k", Value: map[string]any{"n": n}, Namespace: []string{"u"},
			})
			_, _ = store.Get(ctx, []string{"u"}, "k")
			_, _ = store.Search(ctx, []string{"u"}, MemorySearchOptions{})
		}(i)
	}
	for i := 0; i < 50; i++ {
		<-done
	}
}
