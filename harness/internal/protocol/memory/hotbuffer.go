// Package memory holds the P0 in-memory implementations for the protocol layer
// (short-TTL HotStreamBuffer). It is non-durable and for dev/tests; the realtime
// channel is not a source of truth and is recoverable from the EventStore.
package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
)

// HotBuffer is the in-memory HotStreamBuffer with a per-run short TTL.
type HotBuffer struct {
	mu  sync.Mutex
	ttl time.Duration
	now func() time.Time

	runs map[string]*runBuffer
}

type runBuffer struct {
	deltas    []protocol.HotDelta
	lastSeq   int64
	updatedAt time.Time
}

// Option configures a HotBuffer.
type Option func(*HotBuffer)

// WithTTL sets the retention TTL (default 60s).
func WithTTL(ttl time.Duration) Option { return func(b *HotBuffer) { b.ttl = ttl } }

// WithClock injects a clock (for tests).
func WithClock(now func() time.Time) Option { return func(b *HotBuffer) { b.now = now } }

// NewHotBuffer builds an in-memory HotStreamBuffer.
func NewHotBuffer(opts ...Option) *HotBuffer {
	b := &HotBuffer{ttl: 60 * time.Second, now: time.Now, runs: make(map[string]*runBuffer)}
	for _, o := range opts {
		o(b)
	}
	if b.now == nil {
		b.now = time.Now
	}
	return b
}

func (b *HotBuffer) clock() time.Time { return b.now() }

// Push appends an ordered delta for runID. Out-of-order or duplicate DeltaSeq is
// tolerated but retained order is by DeltaSeq on Read.
func (b *HotBuffer) Push(_ context.Context, runID string, delta protocol.HotDelta) error {
	if runID == "" {
		return &protocol.ProtocolError{Code: "HOT_BUFFER_INVALID", Message: "run_id required"}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rb := b.runs[runID]
	now := b.clock()
	// Drop an expired buffer before appending so a stale run starts fresh.
	if rb != nil && b.ttl > 0 && now.Sub(rb.updatedAt) > b.ttl {
		rb = nil
	}
	if rb == nil {
		rb = &runBuffer{}
		b.runs[runID] = rb
	}
	if delta.CreatedAt.IsZero() {
		delta.CreatedAt = now
	}
	rb.deltas = append(rb.deltas, delta)
	if delta.DeltaSeq > rb.lastSeq {
		rb.lastSeq = delta.DeltaSeq
	}
	rb.updatedAt = now
	return nil
}

// Read returns deltas with DeltaSeq > afterDeltaSeq. expired is true when the
// run buffer has aged out (TTL).
func (b *HotBuffer) Read(_ context.Context, runID string, afterDeltaSeq int64, limit int) ([]protocol.HotDelta, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rb := b.runs[runID]
	if rb == nil {
		return nil, true, nil
	}
	if b.ttl > 0 && b.clock().Sub(rb.updatedAt) > b.ttl {
		delete(b.runs, runID)
		return nil, true, nil
	}
	src := append([]protocol.HotDelta(nil), rb.deltas...)
	sort.Slice(src, func(i, j int) bool { return src[i].DeltaSeq < src[j].DeltaSeq })
	var out []protocol.HotDelta
	for _, d := range src {
		if d.DeltaSeq <= afterDeltaSeq {
			continue
		}
		out = append(out, d)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, false, nil
}

var _ protocol.HotStreamBuffer = (*HotBuffer)(nil)
