package redisstore

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	ctxpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
)

func newTestHotContextStore(t *testing.T) (*HotContextStore, Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	t.Cleanup(func() { _ = client.Close() })
	return NewHotContextStore(client, Config{}), client
}

// TestHotContextLoadMiss: loading a non-existent session returns ErrHotContextMiss.
func TestHotContextLoadMiss(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestHotContextStore(t)
	_, err := store.Load(ctx, "nonexistent", 10)
	if !errors.Is(err, ctxpkg.ErrHotContextMiss) {
		t.Fatalf("Load miss: got %v, want ErrHotContextMiss", err)
	}
}

// TestHotContextLoadEmptySessionID: empty session ID returns ErrSessionIDMissing.
func TestHotContextLoadEmptySessionID(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestHotContextStore(t)
	_, err := store.Load(ctx, "", 10)
	if !errors.Is(err, ctxpkg.ErrSessionIDMissing) {
		t.Fatalf("Load empty: got %v, want ErrSessionIDMissing", err)
	}
}

// TestHotContextCASFirstWrite: first CAS (no prior state) succeeds with version 1.
func TestHotContextCASFirstWrite(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestHotContextStore(t)
	view := ctxpkg.HotContextView{
		Messages: []ctxpkg.Message{{ID: "m1", Content: "hello"}},
		Version:  0,
	}
	result, err := store.CompareAndSwap(ctx, "sess-1", 0, view)
	if err != nil {
		t.Fatalf("CAS first write: %v", err)
	}
	if result.Version != 1 {
		t.Fatalf("Version: got %d, want 1", result.Version)
	}
	loaded, err := store.Load(ctx, "sess-1", 0)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Version != 1 {
		t.Fatalf("loaded Version: got %d, want 1", loaded.Version)
	}
	if len(loaded.Messages) != 1 || loaded.Messages[0].Content != "hello" {
		t.Fatalf("loaded Messages: got %+v", loaded.Messages)
	}
}

// TestHotContextCASSequential: two sequential CAS operations increment version.
func TestHotContextCASSequential(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestHotContextStore(t)

	// First CAS: version 0 → 1
	v1 := ctxpkg.HotContextView{
		Messages: []ctxpkg.Message{{ID: "m1", Content: "first"}},
	}
	result1, err := store.CompareAndSwap(ctx, "sess-2", 0, v1)
	if err != nil {
		t.Fatalf("CAS 1: %v", err)
	}
	if result1.Version != 1 {
		t.Fatalf("Version after CAS 1: got %d, want 1", result1.Version)
	}

	// Second CAS: version 1 → 2
	v2 := ctxpkg.HotContextView{
		Messages: []ctxpkg.Message{{ID: "m1", Content: "first"}, {ID: "m2", Content: "second"}},
	}
	result2, err := store.CompareAndSwap(ctx, "sess-2", 1, v2)
	if err != nil {
		t.Fatalf("CAS 2: %v", err)
	}
	if result2.Version != 2 {
		t.Fatalf("Version after CAS 2: got %d, want 2", result2.Version)
	}
}

// TestHotContextCASConflict: CAS with wrong expected version returns conflict.
func TestHotContextCASConflict(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestHotContextStore(t)

	// Write version 1
	v1 := ctxpkg.HotContextView{Messages: []ctxpkg.Message{{ID: "m1"}}}
	if _, err := store.CompareAndSwap(ctx, "sess-3", 0, v1); err != nil {
		t.Fatalf("CAS setup: %v", err)
	}

	// Try CAS with expected=0 (stale), should conflict.
	v2 := ctxpkg.HotContextView{Messages: []ctxpkg.Message{{ID: "m2"}}}
	_, err := store.CompareAndSwap(ctx, "sess-3", 0, v2)
	if !errors.Is(err, ctxpkg.ErrHotContextConflict) {
		t.Fatalf("CAS conflict: got %v, want ErrHotContextConflict", err)
	}
}

// TestHotContextMaxMessages: Load with maxMessages truncates the message list.
func TestHotContextMaxMessages(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestHotContextStore(t)

	msgs := make([]ctxpkg.Message, 5)
	for i := range msgs {
		msgs[i] = ctxpkg.Message{ID: string(rune('a' + i)), Content: "msg"}
	}
	v := ctxpkg.HotContextView{Messages: msgs}
	if _, err := store.CompareAndSwap(ctx, "sess-4", 0, v); err != nil {
		t.Fatalf("CAS: %v", err)
	}

	loaded, err := store.Load(ctx, "sess-4", 3)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Messages) != 3 {
		t.Fatalf("maxMessages: got %d messages, want 3", len(loaded.Messages))
	}
}

// TestHotContextCASCurrentInputCleared: CurrentInput flag is cleared during CAS.
func TestHotContextCASCurrentInputCleared(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestHotContextStore(t)

	v := ctxpkg.HotContextView{
		Messages: []ctxpkg.Message{{ID: "m1", CurrentInput: true}},
	}
	if _, err := store.CompareAndSwap(ctx, "sess-5", 0, v); err != nil {
		t.Fatalf("CAS: %v", err)
	}
	loaded, err := store.Load(ctx, "sess-5", 0)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Messages[0].CurrentInput {
		t.Fatal("CurrentInput should be cleared after CAS")
	}
}

// TestHotContextCASConcurrent: concurrent CAS operations — exactly one succeeds
// per version increment. With N goroutines racing on the same version, exactly
// one wins.
func TestHotContextCASConcurrent(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestHotContextStore(t)

	// Set up initial version 1.
	v0 := ctxpkg.HotContextView{Messages: []ctxpkg.Message{{ID: "init"}}}
	if _, err := store.CompareAndSwap(ctx, "sess-race", 0, v0); err != nil {
		t.Fatalf("setup CAS: %v", err)
	}

	const goroutines = 20
	var successes atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			v := ctxpkg.HotContextView{
				Messages: []ctxpkg.Message{{ID: "winner"}},
			}
			_, err := store.CompareAndSwap(ctx, "sess-race", 1, v)
			if err == nil {
				successes.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if got := successes.Load(); got != 1 {
		t.Fatalf("concurrent CAS successes = %d, want exactly 1", got)
	}
	loaded, err := store.Load(ctx, "sess-race", 0)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Version != 2 {
		t.Fatalf("Version after race: got %d, want 2", loaded.Version)
	}
}
