package redisstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ctxpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
)

// EventBus is a distributed context.EventBus backed by Redis Pub/Sub and Lists.
// Events are published to a single channel and fanned out to all subscribed
// processes; each subscriber filters locally by path pattern. A per-session
// bounded list preserves recent history for late-joining consumers.
type EventBus struct {
	client       Client
	prefix       string
	historyLimit int64
}

// NewEventBus builds a Redis-backed EventBus.
func NewEventBus(client Client, cfg Config) *EventBus {
	limit := cfg.EventHistoryLimit
	if limit <= 0 {
		limit = 100
	}
	return &EventBus{
		client:       client,
		prefix:       prefix(cfg) + "events:",
		historyLimit: int64(limit),
	}
}

func (b *EventBus) channel() string              { return b.prefix + "channel" }
func (b *EventBus) historyKey(sessionID string) string { return b.prefix + "history:{" + sessionID + "}" }

// Publish records the event in the session history list and broadcasts it to
// all subscribed processes via Pub/Sub.
func (b *EventBus) Publish(ctx context.Context, event ctxpkg.MutationEvent) error {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	// Append to bounded history (LPUSH + LTRIM keeps the newest N entries).
	hKey := b.historyKey(event.SessionID)
	pipe := b.client.TxPipeline()
	pipe.LPush(ctx, hKey, data)
	pipe.LTrim(ctx, hKey, 0, b.historyLimit-1)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis history: %w", err)
	}
	// Fan-out to all subscribers.
	return b.client.Publish(ctx, b.channel(), data).Err()
}

// Subscribe registers a handler for events whose path matches the pattern.
// The handler runs in a dedicated goroutine that exits on Unsubscribe or context
// cancellation. Pattern matching uses the same semantics as InMemoryEventBus:
// exact match, "*" wildcard, and "prefix.*" namespace wildcard.
func (b *EventBus) Subscribe(pattern string, handler ctxpkg.EventHandler) ctxpkg.Subscription {
	ctx, cancel := context.WithCancel(context.Background())
	pubsub := b.client.Subscribe(ctx, b.channel())
	// Block until the subscription is confirmed so no events are missed.
	if _, err := pubsub.Receive(ctx); err != nil {
		cancel()
		_ = pubsub.Close()
		return &redisEventSubscription{cancel: func() {}}
	}
	var closed atomic.Bool
	go func() {
		defer func() {
			_ = pubsub.Close()
			cancel()
		}()
		ch := pubsub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				var ev ctxpkg.MutationEvent
				if err := json.Unmarshal([]byte(msg.Payload), &ev); err != nil {
					continue
				}
				if matchEventPattern(pattern, ev.Path) {
					handler(ev)
				}
			}
		}
	}()
	return &redisEventSubscription{
		cancel: func() {
			if closed.CompareAndSwap(false, true) {
				cancel()
			}
		},
	}
}

// History returns the bounded event history for a session, newest first (matches
// the LPUSH ordering). The InMemoryEventBus returns oldest-first; we reverse
// here for semantic equivalence.
func (b *EventBus) History(sessionID string) []ctxpkg.MutationEvent {
	items, err := b.client.LRange(context.Background(), b.historyKey(sessionID), 0, -1).Result()
	if err != nil {
		return nil
	}
	out := make([]ctxpkg.MutationEvent, 0, len(items))
	for _, raw := range items {
		var ev ctxpkg.MutationEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			continue
		}
		out = append(out, ev)
	}
	// Reverse to match InMemoryEventBus (oldest first).
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// redisEventSubscription implements context.Subscription with a sync.Once-like
// cancel guard to prevent double-close on the underlying Pub/Sub.
type redisEventSubscription struct {
	cancel func()
	once   sync.Once
}

func (s *redisEventSubscription) Unsubscribe() {
	s.once.Do(s.cancel)
}

// matchEventPattern mirrors the unexported context.matchEventPattern for use in
// the Redis adapter. Supports exact match, "*" catch-all, and "prefix.*"
// namespace wildcard.
func matchEventPattern(pattern, path string) bool {
	if pattern == path {
		return true
	}
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		pfx := strings.TrimSuffix(pattern, ".*")
		return strings.HasPrefix(path, pfx+".") || path == pfx
	}
	return false
}

var _ ctxpkg.EventBus = (*EventBus)(nil)
