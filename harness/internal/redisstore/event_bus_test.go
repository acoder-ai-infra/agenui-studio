package redisstore

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	ctxpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
)

func newTestEventBus(t *testing.T) (*EventBus, Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	t.Cleanup(func() { _ = client.Close() })
	return NewEventBus(client, Config{}), client
}

// TestEventBusPublishHistory: published events appear in history.
func TestEventBusPublishHistory(t *testing.T) {
	ctx := context.Background()
	bus, _ := newTestEventBus(t)

	ev := ctxpkg.MutationEvent{
		SessionID: "sess-1", Path: "task.status", NewValue: "done",
		Writer: ctxpkg.Writer{Role: "agent"},
	}
	if err := bus.Publish(ctx, ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	hist := bus.History("sess-1")
	if len(hist) != 1 {
		t.Fatalf("History len: got %d, want 1", len(hist))
	}
	if hist[0].Path != "task.status" {
		t.Fatalf("History path: got %q, want %q", hist[0].Path, "task.status")
	}
}

// TestEventBusHistoryEmpty: empty session returns nil.
func TestEventBusHistoryEmpty(t *testing.T) {
	bus, _ := newTestEventBus(t)
	hist := bus.History("nonexistent")
	if len(hist) != 0 {
		t.Fatalf("History empty: got %d, want 0", len(hist))
	}
}

// TestEventBusSubscribeWildcard: subscriber with "*" receives all events.
func TestEventBusSubscribeWildcard(t *testing.T) {
	ctx := context.Background()
	bus, _ := newTestEventBus(t)

	var received atomic.Int64
	sub := bus.Subscribe("*", func(ev ctxpkg.MutationEvent) {
		received.Add(1)
	})
	defer sub.Unsubscribe()

	// Give subscriber goroutine time to connect.
	time.Sleep(50 * time.Millisecond)

	for i := 0; i < 3; i++ {
		if err := bus.Publish(ctx, ctxpkg.MutationEvent{
			SessionID: "sess-1", Path: "any.path", NewValue: i,
		}); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if got := received.Load(); got != 3 {
		t.Fatalf("wildcard received: got %d, want 3", got)
	}
}

// TestEventBusSubscribePattern: subscriber with "task.*" only receives matching events.
func TestEventBusSubscribePattern(t *testing.T) {
	ctx := context.Background()
	bus, _ := newTestEventBus(t)

	var taskCount, otherCount atomic.Int64
	sub := bus.Subscribe("task.*", func(ev ctxpkg.MutationEvent) {
		taskCount.Add(1)
	})
	defer sub.Unsubscribe()
	time.Sleep(50 * time.Millisecond)

	// Publish matching and non-matching events.
	if err := bus.Publish(ctx, ctxpkg.MutationEvent{SessionID: "s", Path: "task.status"}); err != nil {
		t.Fatalf("Publish task: %v", err)
	}
	if err := bus.Publish(ctx, ctxpkg.MutationEvent{SessionID: "s", Path: "user.name"}); err != nil {
		t.Fatalf("Publish user: %v", err)
	}
	if err := bus.Publish(ctx, ctxpkg.MutationEvent{SessionID: "s", Path: "task.priority"}); err != nil {
		t.Fatalf("Publish task.priority: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	if got := taskCount.Load(); got != 2 {
		t.Fatalf("task matches: got %d, want 2", got)
	}
	if got := otherCount.Load(); got != 0 {
		t.Fatalf("other matches: got %d, want 0", got)
	}
}

// TestEventBusSubscribeExactMatch: subscriber with exact path only matches that path.
func TestEventBusSubscribeExactMatch(t *testing.T) {
	ctx := context.Background()
	bus, _ := newTestEventBus(t)

	var count atomic.Int64
	sub := bus.Subscribe("task.status", func(ev ctxpkg.MutationEvent) {
		count.Add(1)
	})
	defer sub.Unsubscribe()
	time.Sleep(50 * time.Millisecond)

	_ = bus.Publish(ctx, ctxpkg.MutationEvent{SessionID: "s", Path: "task.status"})
	_ = bus.Publish(ctx, ctxpkg.MutationEvent{SessionID: "s", Path: "task.priority"})
	time.Sleep(100 * time.Millisecond)

	if got := count.Load(); got != 1 {
		t.Fatalf("exact match: got %d, want 1", got)
	}
}

// TestEventBusUnsubscribe: after Unsubscribe, handler stops receiving.
func TestEventBusUnsubscribe(t *testing.T) {
	ctx := context.Background()
	bus, _ := newTestEventBus(t)

	var count atomic.Int64
	sub := bus.Subscribe("*", func(ev ctxpkg.MutationEvent) {
		count.Add(1)
	})
	time.Sleep(50 * time.Millisecond)

	_ = bus.Publish(ctx, ctxpkg.MutationEvent{SessionID: "s", Path: "a"})
	time.Sleep(50 * time.Millisecond)
	sub.Unsubscribe()
	time.Sleep(50 * time.Millisecond)

	_ = bus.Publish(ctx, ctxpkg.MutationEvent{SessionID: "s", Path: "b"})
	time.Sleep(100 * time.Millisecond)

	if got := count.Load(); got != 1 {
		t.Fatalf("after unsubscribe: got %d, want 1", got)
	}
}

// TestEventBusHistoryBounded: history respects the configured limit.
func TestEventBusHistoryBounded(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	t.Cleanup(func() { _ = client.Close() })
	bus := NewEventBus(client, Config{EventHistoryLimit: 3})

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_ = bus.Publish(ctx, ctxpkg.MutationEvent{SessionID: "s", Path: "p", NewValue: i})
	}
	hist := bus.History("s")
	if len(hist) != 3 {
		t.Fatalf("bounded history: got %d, want 3", len(hist))
	}
	// Should contain the 3 newest (oldest first after reverse).
	if hist[0].NewValue != float64(2) {
		t.Fatalf("oldest kept: got %v, want 2", hist[0].NewValue)
	}
}

// TestEventBusCrossSessionPublish: events from different sessions are independent.
func TestEventBusCrossSessionPublish(t *testing.T) {
	ctx := context.Background()
	bus, _ := newTestEventBus(t)

	_ = bus.Publish(ctx, ctxpkg.MutationEvent{SessionID: "s1", Path: "a"})
	_ = bus.Publish(ctx, ctxpkg.MutationEvent{SessionID: "s2", Path: "b"})

	h1 := bus.History("s1")
	h2 := bus.History("s2")
	if len(h1) != 1 || h1[0].Path != "a" {
		t.Fatalf("s1 history: got %+v", h1)
	}
	if len(h2) != 1 || h2[0].Path != "b" {
		t.Fatalf("s2 history: got %+v", h2)
	}
}

// TestEventBusConcurrentPublish: concurrent publishes don't lose events.
func TestEventBusConcurrentPublish(t *testing.T) {
	ctx := context.Background()
	bus, _ := newTestEventBus(t)

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			_ = bus.Publish(ctx, ctxpkg.MutationEvent{
				SessionID: "race", Path: "p", NewValue: idx,
			})
		}(i)
	}
	wg.Wait()

	hist := bus.History("race")
	if len(hist) != N {
		t.Fatalf("concurrent history: got %d, want %d", len(hist), N)
	}
}
