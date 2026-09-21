package redisstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	ctxpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
)

func newTestMemoryStore(t *testing.T) (*MemoryStore, Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	t.Cleanup(func() { _ = client.Close() })
	return NewMemoryStore(client, Config{}), client
}

func testMemoryItem(key string, importance float64) *ctxpkg.MemoryItem {
	return &ctxpkg.MemoryItem{
		Key:        key,
		Value:      map[string]any{"data": key},
		Namespace:  []string{"ns1"},
		Category:   "fact",
		Importance: importance,
	}
}

// TestMemoryPutGet: basic round-trip.
func TestMemoryPutGet(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestMemoryStore(t)

	item := testMemoryItem("k1", 0.8)
	if err := store.Put(ctx, item); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(ctx, []string{"ns1"}, "k1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Key != "k1" || got.Importance != 0.8 {
		t.Fatalf("Get mismatch: got %+v", got)
	}
	if got.Category != "fact" {
		t.Fatalf("Category: got %q, want %q", got.Category, "fact")
	}
}

// TestMemoryGetNotFound: returns ErrMemoryNotFound.
func TestMemoryGetNotFound(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestMemoryStore(t)
	_, err := store.Get(ctx, []string{"ns1"}, "nonexistent")
	if !errors.Is(err, ctxpkg.ErrMemoryNotFound) {
		t.Fatalf("Get missing: got %v, want ErrMemoryNotFound", err)
	}
}

// TestMemoryDelete: removes an item.
func TestMemoryDelete(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestMemoryStore(t)

	item := testMemoryItem("k2", 0.5)
	_ = store.Put(ctx, item)
	if err := store.Delete(ctx, []string{"ns1"}, "k2"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := store.Get(ctx, []string{"ns1"}, "k2")
	if !errors.Is(err, ctxpkg.ErrMemoryNotFound) {
		t.Fatalf("after Delete: got %v, want ErrMemoryNotFound", err)
	}
}

// TestMemorySearch: finds items matching namespace prefix with filters.
func TestMemorySearch(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestMemoryStore(t)

	items := []*ctxpkg.MemoryItem{
		{Key: "a", Value: map[string]any{}, Namespace: []string{"ns1"}, Category: "fact", Importance: 0.9},
		{Key: "b", Value: map[string]any{}, Namespace: []string{"ns1"}, Category: "summary", Importance: 0.5},
		{Key: "c", Value: map[string]any{}, Namespace: []string{"ns1"}, Category: "fact", Importance: 0.3},
		{Key: "d", Value: map[string]any{}, Namespace: []string{"ns2"}, Category: "fact", Importance: 0.7},
	}
	for _, item := range items {
		if err := store.Put(ctx, item); err != nil {
			t.Fatalf("Put %s: %v", item.Key, err)
		}
	}

	// Search ns1 with category=fact.
	result, err := store.Search(ctx, []string{"ns1"}, ctxpkg.MemorySearchOptions{
		Categories: []string{"fact"},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("Search ns1 fact: got %d, want 2", len(result))
	}
	// Should be sorted by importance desc (from ZSET), but then re-sorted by UpdatedAt.
	// Since all items were just created, UpdatedAt is similar.

	// Search with MinScore.
	result, err = store.Search(ctx, []string{"ns1"}, ctxpkg.MemorySearchOptions{
		MinScore: 0.6,
	})
	if err != nil {
		t.Fatalf("Search min: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("Search min=0.6: got %d, want 1", len(result))
	}
	if result[0].Key != "a" {
		t.Fatalf("Search min=0.6: got key %q, want %q", result[0].Key, "a")
	}

	// Search with Limit.
	result, err = store.Search(ctx, []string{"ns1"}, ctxpkg.MemorySearchOptions{Limit: 1})
	if err != nil {
		t.Fatalf("Search limit: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("Search limit=1: got %d, want 1", len(result))
	}
}

// TestMemorySearchEmpty: returns empty slice for no matches.
func TestMemorySearchEmpty(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestMemoryStore(t)
	result, err := store.Search(ctx, []string{"nonexistent"}, ctxpkg.MemorySearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("Search empty: got %d results, want 0", len(result))
	}
}

// TestMemoryExpiration: items with ExpiresAt expire after TTL.
func TestMemoryExpiration(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	t.Cleanup(func() { _ = client.Close() })
	store := NewMemoryStore(client, Config{})

	ctx := context.Background()
	exp := time.Now().Add(2 * time.Second)
	item := &ctxpkg.MemoryItem{
		Key:       "expiring",
		Value:     map[string]any{},
		Namespace: []string{"ns1"},
		ExpiresAt: &exp,
	}
	if err := store.Put(ctx, item); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Should exist now.
	if _, err := store.Get(ctx, []string{"ns1"}, "expiring"); err != nil {
		t.Fatalf("Get before expiry: %v", err)
	}
	// Fast-forward past TTL.
	mr.FastForward(3 * time.Second)
	_, err := store.Get(ctx, []string{"ns1"}, "expiring")
	if !errors.Is(err, ctxpkg.ErrMemoryNotFound) {
		t.Fatalf("after expiry: got %v, want ErrMemoryNotFound", err)
	}
}

// TestMemoryPutUpdatesTimestamps: Put sets CreatedAt, UpdatedAt, LastAccessedAt.
func TestMemoryPutUpdatesTimestamps(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestMemoryStore(t)

	item := testMemoryItem("ts", 0.5)
	if err := store.Put(ctx, item); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(ctx, []string{"ns1"}, "ts")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("CreatedAt should be set")
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt should be set")
	}
	if got.LastAccessedAt.IsZero() {
		t.Fatal("LastAccessedAt should be set")
	}
}
