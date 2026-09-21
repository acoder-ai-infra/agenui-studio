package testkit

import (
	"sync"
	"sync/atomic"
	"time"
)

// Clock is a deterministic wall clock for testkit fixtures. Tests use it to
// stamp Scenario event CreatedAt values that are stable across runs.
//
// Concurrency: Clock is safe for concurrent use. Advance is atomic with
// respect to Now.
type Clock struct {
	mu     sync.Mutex
	nowUnixNano atomic.Int64
}

// NewClock builds a Clock anchored at start.
func NewClock(start time.Time) *Clock {
	c := &Clock{}
	c.nowUnixNano.Store(start.UnixNano())
	return c
}

// Now returns the current clock value.
func (c *Clock) Now() time.Time {
	return time.Unix(0, c.nowUnixNano.Load())
}

// Advance moves the clock forward by d. Negative durations are rejected as a
// no-op; time-travel backwards would make Sequence assertions ambiguous.
func (c *Clock) Advance(d time.Duration) time.Time {
	if d < 0 {
		return c.Now()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.nowUnixNano.Load() + d.Nanoseconds()
	c.nowUnixNano.Store(next)
	return time.Unix(0, next)
}

// Set jumps the clock to an absolute time. Rarely needed; primarily useful in
// bring-up tests to align mock traces with real timestamps.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nowUnixNano.Store(t.UnixNano())
}
