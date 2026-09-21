package context

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestHotContextStoreCASAndIsolation(t *testing.T) {
	store := NewInMemoryHotContextStore()
	view, err := store.CompareAndSwap(context.Background(), "session", 0, HotContextView{
		Messages:  []Message{{ID: "m1", Content: "original"}},
		Fragments: []ContextFragment{{Content: "summary"}},
	})
	if err != nil || view.Version != 1 {
		t.Fatalf("initial CAS failed: %#v %v", view, err)
	}
	view.Messages[0].Content = "mutated"
	loaded, err := store.Load(context.Background(), "session", 10)
	if err != nil || loaded.Messages[0].Content != "original" {
		t.Fatalf("cache value leaked mutable state: %#v %v", loaded, err)
	}
}

func TestHotContextStoreDoesNotPersistCurrentInputMarker(t *testing.T) {
	store := NewInMemoryHotContextStore()
	view, err := store.CompareAndSwap(context.Background(), "session", 0, HotContextView{
		Messages: []Message{{ID: "m1", Role: RoleUser, Content: "current", CurrentInput: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if view.Messages[0].CurrentInput {
		t.Fatal("transient current-input marker was returned from persisted hot context")
	}
	loaded, err := store.Load(context.Background(), "session", 0)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Messages[0].CurrentInput {
		t.Fatal("transient current-input marker leaked from hot context")
	}
}

func TestHotContextStoreConcurrentCASHasOneWinner(t *testing.T) {
	store := NewInMemoryHotContextStore()
	if _, err := store.CompareAndSwap(context.Background(), "session", 0, HotContextView{}); err != nil {
		t.Fatal(err)
	}
	const writers = 64
	var successes atomic.Int64
	var conflicts atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.CompareAndSwap(context.Background(), "session", 1, HotContextView{})
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, ErrHotContextConflict):
				conflicts.Add(1)
			default:
				t.Errorf("unexpected CAS error: %v", err)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 || conflicts.Load() != writers-1 {
		t.Fatalf("successes=%d conflicts=%d", successes.Load(), conflicts.Load())
	}
}
