package redisstore

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestSemaphore(t *testing.T) (*Semaphore, Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	t.Cleanup(func() { _ = client.Close() })
	return NewSemaphore(client, Config{}), client
}

// TestSemaphoreAcquireRelease: single acquire/release cycle.
func TestSemaphoreAcquireRelease(t *testing.T) {
	ctx := context.Background()
	sem, _ := newTestSemaphore(t)

	if err := sem.Acquire(ctx, "srv", 3); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	count, err := sem.Count(ctx, "srv")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 1 {
		t.Fatalf("Count after Acquire: got %d, want 1", count)
	}

	if err := sem.Release(ctx, "srv"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	count, err = sem.Count(ctx, "srv")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 0 {
		t.Fatalf("Count after Release: got %d, want 0", count)
	}
}

// TestSemaphoreLimit: exactly N acquires succeed, N+1 blocks.
func TestSemaphoreLimit(t *testing.T) {
	ctx := context.Background()
	sem, _ := newTestSemaphore(t)

	const limit = 3
	for i := 0; i < limit; i++ {
		if err := sem.Acquire(ctx, "limited", limit); err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
	}
	count, _ := sem.Count(ctx, "limited")
	if count != limit {
		t.Fatalf("Count at limit: got %d, want %d", count, limit)
	}

	// N+1 should block; use short timeout to verify.
	tCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	err := sem.Acquire(tCtx, "limited", limit)
	if err == nil {
		t.Fatal("Acquire past limit should fail with context timeout")
	}
}

// TestSemaphoreReleaseNoOp: releasing when count is 0 doesn't go negative.
func TestSemaphoreReleaseNoOp(t *testing.T) {
	ctx := context.Background()
	sem, _ := newTestSemaphore(t)

	if err := sem.Release(ctx, "empty"); err != nil {
		t.Fatalf("Release empty: %v", err)
	}
	count, _ := sem.Count(ctx, "empty")
	if count != 0 {
		t.Fatalf("Count after empty Release: got %d, want 0", count)
	}
}

// TestSemaphoreContextCancellation: acquire respects context cancellation.
func TestSemaphoreContextCancellation(t *testing.T) {
	ctx := context.Background()
	sem, _ := newTestSemaphore(t)

	// Fill the semaphore.
	for i := 0; i < 2; i++ {
		if err := sem.Acquire(ctx, "full", 2); err != nil {
			t.Fatalf("setup Acquire: %v", err)
		}
	}

	ctx2, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	err := sem.Acquire(ctx2, "full", 2)
	if err == nil {
		t.Fatal("Acquire should fail on context timeout")
	}
}

// TestSemaphoreLeaseExpiry: slots are released when the lease expires.
func TestSemaphoreLeaseExpiry(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	t.Cleanup(func() { _ = client.Close() })
	sem := NewSemaphore(client, Config{SemaphoreLeaseTTL: 2 * time.Second})

	ctx := context.Background()
	if err := sem.Acquire(ctx, "expiring", 1); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Fast-forward past the lease TTL.
	mr.FastForward(3 * time.Second)

	// The lease should have expired, so a new acquire should succeed.
	if err := sem.Acquire(ctx, "expiring", 1); err != nil {
		t.Fatalf("Acquire after lease expiry: %v", err)
	}
}

// TestSemaphoreConcurrent: concurrent acquires — exactly `limit` succeed.
func TestSemaphoreConcurrent(t *testing.T) {
	ctx := context.Background()
	sem, _ := newTestSemaphore(t)

	const limit = 5
	const goroutines = 30
	var (
		mu      sync.Mutex
		held    []int
		wg      sync.WaitGroup
		release sync.WaitGroup
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			tCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			defer cancel()
			if err := sem.Acquire(tCtx, "race", limit); err != nil {
				return
			}
			mu.Lock()
			held = append(held, idx)
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if len(held) != limit {
		t.Fatalf("concurrent acquires: got %d, want %d", len(held), limit)
	}
	count, _ := sem.Count(ctx, "race")
	if count != limit {
		t.Fatalf("count after burst: got %d, want %d", count, limit)
	}

	// Release all.
	release.Add(len(held))
	var released atomic.Int64
	for range held {
		go func() {
			defer release.Done()
			_ = sem.Release(ctx, "race")
			released.Add(1)
		}()
	}
	release.Wait()

	count, _ = sem.Count(ctx, "race")
	if count != 0 {
		t.Fatalf("count after release all: got %d, want 0", count)
	}
}

// TestSemaphoreEmptyName: empty name returns error.
func TestSemaphoreEmptyName(t *testing.T) {
	ctx := context.Background()
	sem, _ := newTestSemaphore(t)
	err := sem.Acquire(ctx, "", 10)
	if err == nil {
		t.Fatal("empty name should fail")
	}
}
