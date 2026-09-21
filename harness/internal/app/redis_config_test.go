package app

import (
	"context"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/redisstore"
)

func TestBuildRedisMapsSnapshotTTLAndQuotaPolicy(t *testing.T) {
	deps := defaultBuildDependencies()
	var constructed redisstore.Config
	deps.newRedisClient = func(cfg redisstore.Config) redisstore.Client {
		constructed = cfg
		return &observingRedisClient{}
	}
	deps.pingRedis = func(context.Context, redisstore.Client) error { return nil }

	client, got, err := buildRedis(RedisConfig{
		Enabled: true, Addrs: []string{"redis.test:6379"}, SnapshotTTLs: 7200,
		QuotaCommitTTLs: 300, QuotaFailOpen: true,
	}, observability.NoopLogger{}, true, deps)
	if err != nil {
		t.Fatalf("buildRedis: %v", err)
	}
	defer client.Close()
	if got.SnapshotTTL != 2*time.Hour || constructed.SnapshotTTL != 2*time.Hour {
		t.Fatalf("SnapshotTTL = (%v, %v), want 2h", got.SnapshotTTL, constructed.SnapshotTTL)
	}
	if got.QuotaCommitTTL != 5*time.Minute || constructed.QuotaCommitTTL != 5*time.Minute {
		t.Fatalf("QuotaCommitTTL = (%v, %v), want 5m", got.QuotaCommitTTL, constructed.QuotaCommitTTL)
	}
	if !got.QuotaFailOpen || !constructed.QuotaFailOpen {
		t.Fatalf("QuotaFailOpen was not propagated: returned=%t constructed=%t", got.QuotaFailOpen, constructed.QuotaFailOpen)
	}
}
