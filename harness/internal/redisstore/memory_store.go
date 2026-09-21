package redisstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	ctxpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
)

// MemoryStore is a distributed context.MemoryStore backed by Redis. Items are
// stored as individual JSON strings with per-item TTL (driven by ExpiresAt), and
// a global sorted set (score = importance) provides an efficient Search index.
//
// Key layout:
//
//	{prefix}mem:item:{ns}/{key}  → JSON *MemoryItem  (TTL from ExpiresAt or default)
//	{prefix}mem:index            → ZSET  member="{ns}/{key}"  score=importance
type MemoryStore struct {
	client Client
	prefix string
	ttl    time.Duration
}

// NewMemoryStore builds a Redis-backed MemoryStore.
func NewMemoryStore(client Client, cfg Config) *MemoryStore {
	return &MemoryStore{
		client: client,
		prefix: prefix(cfg) + "mem:",
		ttl:    cfg.MemoryTTL, // 0 = no default TTL; per-item ExpiresAt takes precedence
	}
}

func (s *MemoryStore) itemKey(namespace []string, key string) string {
	return s.prefix + "item:" + strings.Join(namespace, "/") + "/" + key
}

func (s *MemoryStore) compositeKey(namespace []string, key string) string {
	return strings.Join(namespace, "/") + "/" + key
}

func (s *MemoryStore) indexKey() string {
	return s.prefix + "index"
}

// itemTTL returns the TTL for a memory item. ExpiresAt takes precedence; if
// absent, falls back to the store-level default (which may be 0 = no expiry).
func (s *MemoryStore) itemTTL(item *ctxpkg.MemoryItem) time.Duration {
	if item.ExpiresAt != nil {
		remaining := time.Until(*item.ExpiresAt)
		if remaining <= 0 {
			return time.Second // minimum positive TTL
		}
		return remaining
	}
	return s.ttl
}

// Get retrieves a memory item by namespace+key. Updates LastAccessedAt on read.
func (s *MemoryStore) Get(ctx context.Context, namespace []string, key string) (*ctxpkg.MemoryItem, error) {
	data, err := s.client.Get(ctx, s.itemKey(namespace, key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ctxpkg.ErrMemoryNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("redis get: %w", err)
	}
	var item ctxpkg.MemoryItem
	if err := json.Unmarshal(data, &item); err != nil {
		return nil, fmt.Errorf("unmarshal memory item: %w", err)
	}
	item.LastAccessedAt = time.Now()
	return &item, nil
}

// Put writes a memory item, updating both the value key and the importance
// index. Timestamps are set automatically (CreatedAt preserved if non-zero).
func (s *MemoryStore) Put(ctx context.Context, item *ctxpkg.MemoryItem) error {
	now := time.Now()
	clone := *item
	if clone.CreatedAt.IsZero() {
		clone.CreatedAt = now
	}
	clone.UpdatedAt = now
	if clone.LastAccessedAt.IsZero() {
		clone.LastAccessedAt = now
	}
	data, err := json.Marshal(&clone)
	if err != nil {
		return fmt.Errorf("marshal memory item: %w", err)
	}
	iKey := s.itemKey(item.Namespace, item.Key)
	ck := s.compositeKey(item.Namespace, item.Key)
	ttl := s.itemTTL(&clone)

	pipe := s.client.TxPipeline()
	pipe.Set(ctx, iKey, data, ttl)
	pipe.ZAdd(ctx, s.indexKey(), redis.Z{Score: clone.Importance, Member: ck})
	_, err = pipe.Exec(ctx)
	return err
}

// Delete removes a memory item and its index entry.
func (s *MemoryStore) Delete(ctx context.Context, namespace []string, key string) error {
	ck := s.compositeKey(namespace, key)
	pipe := s.client.TxPipeline()
	pipe.Del(ctx, s.itemKey(namespace, key))
	pipe.ZRem(ctx, s.indexKey(), ck)
	_, err := pipe.Exec(ctx)
	return err
}

// Search finds memory items matching a namespace prefix with optional category,
// importance, and limit filters. Uses the importance ZSET for efficient top-K
// retrieval, then filters in Go.
func (s *MemoryStore) Search(ctx context.Context, namespace []string, opts ctxpkg.MemorySearchOptions) ([]*ctxpkg.MemoryItem, error) {
	nsPrefix := strings.Join(namespace, "/")
	// Fetch candidates from the index sorted set, highest importance first.
	members, err := s.client.ZRevRangeWithScores(ctx, s.indexKey(), 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("redis zrevrange: %w", err)
	}
	now := time.Now()
	var result []*ctxpkg.MemoryItem
	for _, m := range members {
		ck, ok := m.Member.(string)
		if !ok {
			continue
		}
		// Namespace prefix match.
		if !strings.HasPrefix(ck, nsPrefix) {
			continue
		}
		// Importance floor.
		if m.Score < opts.MinScore {
			continue
		}
		// Fetch the full item.
		parts := strings.SplitN(ck, "/", len(namespace)+1)
		if len(parts) < 2 {
			continue
		}
		// Re-derive the item key from the composite key.
		itemKey := s.prefix + "item:" + ck
		data, err := s.client.Get(ctx, itemKey).Bytes()
		if errors.Is(err, redis.Nil) {
			// Stale index entry; skip.
			continue
		}
		if err != nil {
			continue
		}
		var item ctxpkg.MemoryItem
		if err := json.Unmarshal(data, &item); err != nil {
			continue
		}
		// Skip expired items.
		if item.ExpiresAt != nil && item.ExpiresAt.Before(now) {
			continue
		}
		// Category filter.
		if len(opts.Categories) > 0 {
			found := false
			for _, c := range opts.Categories {
				if item.Category == c {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		clone := item
		result = append(result, &clone)
	}
	// Sort by UpdatedAt descending (matches InMemoryMemoryStore).
	sort.Slice(result, func(i, j int) bool {
		return result[i].UpdatedAt.After(result[j].UpdatedAt)
	})
	if opts.Limit > 0 && len(result) > opts.Limit {
		result = result[:opts.Limit]
	}
	return result, nil
}

var _ ctxpkg.MemoryStore = (*MemoryStore)(nil)
