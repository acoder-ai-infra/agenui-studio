package redisstore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

// ModelCache is a distributed modelgateway.ModelCache backed by Redis. Entries
// are JSON-encoded ModelResponse values under a namespaced key with a TTL, so
// multiple gateway processes share one cache and stale entries self-expire.
type ModelCache struct {
	client Client
	prefix string
	ttl    time.Duration
}

// NewModelCache builds a Redis-backed ModelCache. A zero CacheTTL defaults to 10m.
func NewModelCache(client Client, cfg Config) *ModelCache {
	ttl := cfg.CacheTTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &ModelCache{client: client, prefix: prefix(cfg) + "mcache:", ttl: ttl}
}

func (c *ModelCache) key(k string) string { return c.prefix + k }

func (c *ModelCache) Get(ctx context.Context, key string) (modelgateway.ModelResponse, bool) {
	raw, err := c.client.Get(ctx, c.key(key)).Bytes()
	if err != nil {
		return modelgateway.ModelResponse{}, false // miss (redis.Nil) or transient error → treat as miss
	}
	var resp modelgateway.ModelResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return modelgateway.ModelResponse{}, false
	}
	return resp, true
}

func (c *ModelCache) Put(ctx context.Context, key string, resp modelgateway.ModelResponse) {
	raw, err := json.Marshal(resp)
	if err != nil {
		return
	}
	_ = c.client.Set(ctx, c.key(key), raw, c.ttl).Err()
}

var _ modelgateway.ModelCache = (*ModelCache)(nil)
