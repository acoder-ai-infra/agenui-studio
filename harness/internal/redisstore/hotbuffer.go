package redisstore

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
)

// HotBuffer is a distributed protocol.HotStreamBuffer backed by a Redis sorted
// set per run (score = DeltaSeq). It is short-TTL and NOT a source of truth: once
// the key expires, the EventStore's agent_text_delta/final_response back it up,
// so a reader sees expired=true and falls back to after_sequence replay.
//
// Sharing the buffer in Redis lets any gateway process serve a client's
// stream-buffer poll regardless of which instance produced the deltas.
type HotBuffer struct {
	client Client
	prefix string
	ttl    time.Duration
}

// NewHotBuffer builds a Redis-backed HotStreamBuffer. A zero HotTTL defaults to 60s.
func NewHotBuffer(client Client, cfg Config) *HotBuffer {
	ttl := cfg.HotTTL
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &HotBuffer{client: client, prefix: prefix(cfg) + "hot:", ttl: ttl}
}

func (b *HotBuffer) key(runID string) string { return b.prefix + runID }

func (b *HotBuffer) Push(ctx context.Context, runID string, delta protocol.HotDelta) error {
	if runID == "" {
		return &protocol.ProtocolError{Code: "HOT_BUFFER_INVALID", Message: "run_id required"}
	}
	if delta.CreatedAt.IsZero() {
		delta.CreatedAt = time.Now()
	}
	raw, err := json.Marshal(delta)
	if err != nil {
		return err
	}
	key := b.key(runID)
	pipe := b.client.TxPipeline()
	// Score = DeltaSeq gives ordered retrieval; the member is the JSON delta so
	// identical-seq retries overwrite rather than duplicate.
	pipe.ZAdd(ctx, key, redis.Z{Score: float64(delta.DeltaSeq), Member: raw})
	pipe.Expire(ctx, key, b.ttl)
	_, err = pipe.Exec(ctx)
	return err
}

func (b *HotBuffer) Read(ctx context.Context, runID string, afterDeltaSeq int64, limit int) ([]protocol.HotDelta, bool, error) {
	key := b.key(runID)
	// A missing key means either "never existed" or "aged out"; both map to
	// expired=true so the caller falls back to the durable EventStore.
	n, err := b.client.Exists(ctx, key).Result()
	if err != nil {
		return nil, true, err
	}
	if n == 0 {
		return nil, true, nil
	}

	rng := &redis.ZRangeBy{
		Min: "(" + strconv.FormatInt(afterDeltaSeq, 10), // exclusive lower bound
		Max: "+inf",
	}
	if limit > 0 {
		rng.Count = int64(limit)
	}
	members, err := b.client.ZRangeByScore(ctx, key, rng).Result()
	if err != nil {
		return nil, false, err
	}
	out := make([]protocol.HotDelta, 0, len(members))
	for _, m := range members {
		var d protocol.HotDelta
		if err := json.Unmarshal([]byte(m), &d); err != nil {
			continue
		}
		out = append(out, d)
	}
	return out, false, nil
}

var _ protocol.HotStreamBuffer = (*HotBuffer)(nil)
