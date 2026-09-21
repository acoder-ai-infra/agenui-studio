// Package redisstore provides distributed (multi-process) implementations of the
// harness's ephemeral coordination stores that ship in-memory by default:
// model response cache, per-tenant quota/rate-limit, the live event broker, and
// the realtime HotStreamBuffer. All four are non-durable by design — the
// EventStore (MySQL/SQLite) remains the single source of truth — so Redis here is
// a shared, horizontally-scalable cache/coordination tier, not a system of record.
//
// The package depends only on the interfaces declared by modelgateway/protocol,
// so wiring is a drop-in replacement at the composition root (internal/app).
package redisstore

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// Client is the subset of go-redis used here. redis.UniversalClient satisfies it,
// so callers may pass a standalone or cluster client unchanged.
// Note: Sentinel configuration is not tested or supported in this adapter.
type Client = redis.UniversalClient

// Config configures a Redis connection for the composition root.
type Config struct {
	Addrs             []string      `json:"addrs"` // one addr = standalone; many = cluster
	Username          string        `json:"username"`          // Redis ACL username (default "")
	Password          string        `json:"password"`
	DB                int           `json:"db"`
	KeyPrefix         string        `json:"key_prefix"`          // namespace, e.g. "harness:"
	CacheTTL          time.Duration `json:"cache_ttl"`           // model response cache TTL (default 10m)
	HotTTL            time.Duration `json:"hot_ttl"`             // HotStreamBuffer TTL (default 60s)
	QuotaLeaseTTL     time.Duration `json:"quota_lease_ttl"`     // crash-safe concurrency lease (default 2m)
	QuotaCommitTTL    time.Duration `json:"quota_commit_ttl"`    // reservation commit deduplication window (default 5m)
	SnapshotTTL       time.Duration `json:"snapshot_ttl"`        // context/MCP/hot-context snapshot TTL (default 24h; 0=no expiry; <0=use default)
	StateTTL          time.Duration `json:"state_ttl"`           // state store session TTL (default 24h)
	MemoryTTL         time.Duration `json:"memory_ttl"`          // memory item default TTL (default 0=no expiry)
	EventHistoryLimit int           `json:"event_history_limit"` // max events per session in history (default 100)
	SemaphoreLeaseTTL time.Duration `json:"semaphore_lease_ttl"` // crash-safe semaphore lease (default 30s)
	// QuotaFailOpen controls quota behavior when Redis is unavailable.
	// false (default): fail closed — reject requests when quota cannot be checked
	//   (safest for hard cost/security boundaries).
	// true: fail open — allow requests through with a warning log
	//   (acceptable for soft rate-limiting where availability > strict enforcement).
	QuotaFailOpen bool `json:"quota_fail_open"`
}

// NewClient builds a go-redis UniversalClient from Config.
func NewClient(cfg Config) Client {
	return redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs:    cfg.Addrs,
		Username: cfg.Username,
		Password: cfg.Password,
		DB:       cfg.DB,
	})
}

func prefix(cfg Config) string {
	if cfg.KeyPrefix == "" {
		return "harness:"
	}
	return cfg.KeyPrefix
}

// Ping verifies connectivity; the composition root uses it to fail fast (or fall
// back to in-memory) at startup.
func Ping(ctx context.Context, c Client) error {
	return c.Ping(ctx).Err()
}
