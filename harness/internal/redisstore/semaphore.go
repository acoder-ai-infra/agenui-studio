package redisstore

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Semaphore is a distributed counting semaphore backed by Redis. It uses Lua
// scripts for atomic acquire/release so the count stays exact across processes.
// A per-slot lease TTL prevents crashed holders from permanently blocking slots.
//
// EXPERIMENTAL: This implementation does NOT track slot ownership. Any process
// can release any slot, which means a crashed holder's slot may be prematurely
// freed by an unrelated Release call. The lease TTL provides only coarse-grained
// crash recovery. Do not use this in production until ownership tracking is
// implemented (each Acquire should return a lease token that only that caller
// can use to Release).
//
// The semaphore is identified by a name (e.g. an MCP server ID). Multiple
// processes share the same counter under that name.
type Semaphore struct {
	client   Client
	prefix   string
	leaseTTL time.Duration
}

// NewSemaphore builds a Redis-backed distributed semaphore.
func NewSemaphore(client Client, cfg Config) *Semaphore {
	lease := cfg.SemaphoreLeaseTTL
	if lease <= 0 {
		lease = 30 * time.Second
	}
	return &Semaphore{
		client:   client,
		prefix:   prefix(cfg) + "sem:",
		leaseTTL: lease,
	}
}

func (s *Semaphore) key(name string) string { return s.prefix + name }

// acquireScript atomically checks the current count against the limit and
// increments if below. The lease TTL ensures crashed holders release slots.
// KEYS[1] = semaphore counter key
// ARGV[1] = limit
// ARGV[2] = lease TTL seconds
// Returns: 1 = acquired, 0 = at limit
var acquireScript = redis.NewScript(`
local current = tonumber(redis.call('GET', KEYS[1]) or '0')
local limit = tonumber(ARGV[1])
if current >= limit then
  return 0
end
redis.call('INCR', KEYS[1])
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[2]))
return 1
`)

// releaseScript atomically decrements the counter, clamping at 0.
// KEYS[1] = semaphore counter key
// Returns: the new count
var releaseScript = redis.NewScript(`
local current = tonumber(redis.call('GET', KEYS[1]) or '0')
if current > 0 then
  return redis.call('DECR', KEYS[1])
end
return 0
`)

// Acquire blocks until a slot is available or the context is cancelled. It
// polls with exponential back-off (capped at 50ms) on contention.
func (s *Semaphore) Acquire(ctx context.Context, name string, limit int) error {
	if name == "" {
		return fmt.Errorf("semaphore: name required")
	}
	if limit <= 0 {
		limit = 32
	}
	leaseSec := int64((s.leaseTTL + time.Second - 1) / time.Second)
	backoff := 5 * time.Millisecond
	for {
		ok, err := acquireScript.Run(ctx, s.client,
			[]string{s.key(name)}, limit, leaseSec,
		).Int()
		if err != nil {
			return fmt.Errorf("redis acquire: %w", err)
		}
		if ok == 1 {
			return nil
		}
		// Contention: back off, respecting context cancellation.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 50*time.Millisecond {
			backoff *= 2
		}
	}
}

// Release frees one slot. Safe to call even if no slot is held (clamps at 0).
func (s *Semaphore) Release(ctx context.Context, name string) error {
	_, err := releaseScript.Run(ctx, s.client, []string{s.key(name)}).Int()
	if err != nil {
		return fmt.Errorf("redis release: %w", err)
	}
	return nil
}

// Count returns the current number of acquired slots (for testing/monitoring).
func (s *Semaphore) Count(ctx context.Context, name string) (int, error) {
	n, err := s.client.Get(ctx, s.key(name)).Int()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("redis get: %w", err)
	}
	return n, nil
}
