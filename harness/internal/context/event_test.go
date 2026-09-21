package context

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestInMemoryEventBusPublishAndSubscribe(t *testing.T) {
	bus := NewInMemoryEventBus()
	ctx := context.Background()

	var received []MutationEvent
	var mu sync.Mutex
	bus.Subscribe("task.*", func(e MutationEvent) {
		mu.Lock()
		received = append(received, e)
		mu.Unlock()
	})

	_ = bus.Publish(ctx, MutationEvent{
		SessionID: "s1", Path: "task.status",
		NewValue: "running", Writer: Writer{Role: "rule", Name: "r"},
	})

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 event, got %d", len(received))
	}
	if received[0].NewValue != "running" {
		t.Fatalf("unexpected event value: %v", received[0].NewValue)
	}
}

func TestInMemoryEventBusPatternMatching(t *testing.T) {
	bus := NewInMemoryEventBus()
	ctx := context.Background()

	var taskCount, allCount int64
	bus.Subscribe("task.*", func(e MutationEvent) {
		atomic.AddInt64(&taskCount, 1)
	})
	bus.Subscribe("*", func(e MutationEvent) {
		atomic.AddInt64(&allCount, 1)
	})

	_ = bus.Publish(ctx, MutationEvent{Path: "task.status", NewValue: "x"})
	_ = bus.Publish(ctx, MutationEvent{Path: "agent.plan", NewValue: "y"})

	if atomic.LoadInt64(&taskCount) != 1 {
		t.Fatalf("task.* should match 1 event, got %d", taskCount)
	}
	if atomic.LoadInt64(&allCount) != 2 {
		t.Fatalf("* should match 2 events, got %d", allCount)
	}
}

func TestInMemoryEventBusUnsubscribe(t *testing.T) {
	bus := NewInMemoryEventBus()
	ctx := context.Background()

	var count int64
	sub := bus.Subscribe("task.*", func(e MutationEvent) {
		atomic.AddInt64(&count, 1)
	})

	_ = bus.Publish(ctx, MutationEvent{Path: "task.status", NewValue: "a"})
	sub.Unsubscribe()
	_ = bus.Publish(ctx, MutationEvent{Path: "task.status", NewValue: "b"})

	if atomic.LoadInt64(&count) != 1 {
		t.Fatalf("expected 1 event before unsubscribe, got %d", count)
	}
}

func TestInMemoryEventBusHistory(t *testing.T) {
	bus := NewInMemoryEventBus()
	ctx := context.Background()

	_ = bus.Publish(ctx, MutationEvent{SessionID: "s1", Path: "a", NewValue: 1})
	_ = bus.Publish(ctx, MutationEvent{SessionID: "s1", Path: "b", NewValue: 2})
	_ = bus.Publish(ctx, MutationEvent{SessionID: "s2", Path: "c", NewValue: 3})

	h1 := bus.History("s1")
	if len(h1) != 2 {
		t.Fatalf("expected 2 events for s1, got %d", len(h1))
	}
	h2 := bus.History("s2")
	if len(h2) != 1 {
		t.Fatalf("expected 1 event for s2, got %d", len(h2))
	}
	h3 := bus.History("nonexistent")
	if len(h3) != 0 {
		t.Fatalf("expected 0 events for nonexistent, got %d", len(h3))
	}
}

func TestInMemoryEventBusTimestampAutoSet(t *testing.T) {
	bus := NewInMemoryEventBus()
	ctx := context.Background()

	before := time.Now()
	_ = bus.Publish(ctx, MutationEvent{SessionID: "s1", Path: "x", NewValue: 1})
	after := time.Now()

	h := bus.History("s1")
	if len(h) != 1 {
		t.Fatalf("expected 1 event, got %d", len(h))
	}
	if h[0].Timestamp.Before(before) || h[0].Timestamp.After(after) {
		t.Fatalf("timestamp out of range: %v", h[0].Timestamp)
	}
}

func TestInMemoryEventBusConcurrency(t *testing.T) {
	bus := NewInMemoryEventBus()
	ctx := context.Background()

	var count int64
	bus.Subscribe("*", func(e MutationEvent) {
		atomic.AddInt64(&count, 1)
	})

	done := make(chan struct{})
	for i := 0; i < 100; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			_ = bus.Publish(ctx, MutationEvent{
				SessionID: "s1",
				Path:      "counter",
				NewValue:  n,
			})
		}(i)
	}
	for i := 0; i < 100; i++ {
		<-done
	}

	if atomic.LoadInt64(&count) != 100 {
		t.Fatalf("expected 100 events, got %d", count)
	}
}

func TestMatchEventPattern(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"task.*", "task.status", true},
		{"task.*", "task", true},
		{"task.*", "other", false},
		{"*", "anything", true},
		{"exact", "exact", true},
		{"exact", "other", false},
	}
	for _, tt := range tests {
		got := matchEventPattern(tt.pattern, tt.path)
		if got != tt.want {
			t.Errorf("matchEventPattern(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}
