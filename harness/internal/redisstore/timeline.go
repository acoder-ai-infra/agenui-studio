package redisstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	ctxpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
)

// Timeline is a distributed context.Timeline backed by Redis Lists. Each session
// gets a single RPUSH-ordered list. It is a hot conversation view, not the
// durable fact ledger: it may be truncated or rebuilt at any time.
type Timeline struct {
	client Client
	prefix string
	ttl    time.Duration
}

// NewTimeline builds a Redis-backed Timeline.
func NewTimeline(client Client, cfg Config) *Timeline {
	ttl := cfg.StateTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Timeline{
		client: client,
		prefix: prefix(cfg) + "timeline:",
		ttl:    ttl,
	}
}

func (t *Timeline) key(sessionID string) string {
	return t.prefix + "{" + sessionID + "}"
}

// Append adds messages to the end of a session's timeline using a transactional
// pipeline (RPUSH + EXPIRE) so the TTL is refreshed on every write.
func (t *Timeline) Append(ctx context.Context, sessionID string, msgs ...ctxpkg.Message) error {
	if sessionID == "" {
		return ctxpkg.ErrSessionIDMissing
	}
	key := t.key(sessionID)
	pipe := t.client.TxPipeline()
	for _, msg := range msgs {
		msg.SessionID = sessionID
		data, err := json.Marshal(msg)
		if err != nil {
			return fmt.Errorf("marshal message: %w", err)
		}
		pipe.RPush(ctx, key, data)
	}
	pipe.Expire(ctx, key, t.ttl)
	_, err := pipe.Exec(ctx)
	return err
}

// GetAll returns every message in the timeline, preserving insertion order.
func (t *Timeline) GetAll(ctx context.Context, sessionID string) ([]ctxpkg.Message, error) {
	items, err := t.client.LRange(ctx, t.key(sessionID), 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("redis lrange: %w", err)
	}
	if len(items) == 0 {
		return nil, nil
	}
	out := make([]ctxpkg.Message, 0, len(items))
	for _, raw := range items {
		var msg ctxpkg.Message
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			continue // skip corrupt entries
		}
		out = append(out, msg)
	}
	return out, nil
}

// Truncate keeps only the last keepLast messages. If keepLast is 0 or exceeds
// the current length, it is a no-op (matches InMemoryTimeline behavior).
func (t *Timeline) Truncate(ctx context.Context, sessionID string, keepLast int) error {
	if keepLast <= 0 {
		return nil
	}
	n, err := t.client.LLen(ctx, t.key(sessionID)).Result()
	if err != nil {
		return fmt.Errorf("redis llen: %w", err)
	}
	if int64(keepLast) >= n {
		return nil
	}
	// LTRIM keeps elements in [start, stop]; negative indices count from the end.
	return t.client.LTrim(ctx, t.key(sessionID), -int64(keepLast), -1).Err()
}

// Len returns the number of messages in the timeline.
func (t *Timeline) Len(ctx context.Context, sessionID string) (int, error) {
	n, err := t.client.LLen(ctx, t.key(sessionID)).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("redis llen: %w", err)
	}
	return int(n), nil
}

var _ ctxpkg.Timeline = (*Timeline)(nil)
