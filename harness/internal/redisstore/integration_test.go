//go:build integration
// +build integration

package redisstore_test

import (
	"context"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/redis/go-redis/v9"

	ctxpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/redisstore"
)

// Integration tests against real Redis.
//
// Required environment variables:
//   HARNESS_TEST_REDIS_ADDR     — e.g. "host:6379"
//   HARNESS_TEST_REDIS_USER     — e.g. "default"
//   HARNESS_TEST_REDIS_PASSWORD — password (never logged)
//
// Run with:
//   HARNESS_TEST_REDIS_ADDR=... HARNESS_TEST_REDIS_USER=... HARNESS_TEST_REDIS_PASSWORD=... \
//     go test -tags=integration ./internal/redisstore/...
//
// If any variable is missing the suite is skipped, never falls back to built-in credentials.

// newTestRedisClient creates a client from env vars with a unique key prefix per invocation.
// It calls t.Skip if credentials are not configured or Redis is unreachable.
func newTestRedisClient(t *testing.T) (redisstore.Client, redisstore.Config) {
	t.Helper()

	addr := os.Getenv("HARNESS_TEST_REDIS_ADDR")
	user := os.Getenv("HARNESS_TEST_REDIS_USER")
	pass := os.Getenv("HARNESS_TEST_REDIS_PASSWORD")
	if addr == "" || pass == "" {
		t.Skip("HARNESS_TEST_REDIS_ADDR and HARNESS_TEST_REDIS_PASSWORD must be set for integration tests")
	}

	// Unique prefix per run — prevents concurrent suites from colliding.
	keyPrefix := fmt.Sprintf("harness-it-%s:", ulid.Make().String())

	client := redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs:    []string{addr},
		Username: user,
		Password: pass,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		// Log only the address, never the password or full URI.
		t.Skipf("Redis not available at %s: %v", addr, err)
	}

	t.Cleanup(func() {
		// Clean up test keys under this run's unique prefix.
		keys, _ := client.Keys(context.Background(), keyPrefix+"*").Result()
		if len(keys) > 0 {
			client.Del(context.Background(), keys...)
		}
		client.Close()
	})

	cfg := redisstore.Config{
		Addrs:       []string{addr},
		Password:    pass,
		KeyPrefix:   keyPrefix,
		SnapshotTTL: 1 * time.Hour,
		StateTTL:    1 * time.Hour,
		HotTTL:      1 * time.Hour,
		CacheTTL:    1 * time.Hour,
	}
	return client, cfg
}

// TestIntegrationContextSnapshotStore verifies ContextSnapshotStore against real Redis.
func TestIntegrationContextSnapshotStore(t *testing.T) {
	client, cfg := newTestRedisClient(t)
	store := redisstore.NewContextSnapshotStore(client, cfg)
	ctx := context.Background()

	snap := ctxpkg.Snapshot{
		ID:           "test-snap-1",
		SessionID:    "sess-1",
		RunID:        "run-1",
		MessageIDs:   []string{"m1", "m2"},
		LastSequence: 2,
		Source:       "test",
		ContentHash:  "sha256:test-hash",
		CreatedAt:    time.Now().Truncate(time.Millisecond),
	}

	// Save
	if _, err := store.Save(ctx, snap); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Load
	loaded, err := store.Load(ctx, "test-snap-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ID != snap.ID || loaded.SessionID != snap.SessionID {
		t.Fatalf("Load mismatch: got %+v, want %+v", loaded, snap)
	}

	// Idempotent save (same identity)
	if _, err := store.Save(ctx, snap); err != nil {
		t.Fatalf("Idempotent Save: %v", err)
	}

	// Identity conflict
	snap2 := snap
	snap2.SessionID = "different-session"
	if _, err := store.Save(ctx, snap2); err == nil {
		t.Fatal("Save with different SessionID should fail")
	}
}

// TestIntegrationHotContextStore verifies HotContextStore CAS against real Redis.
func TestIntegrationHotContextStore(t *testing.T) {
	client, cfg := newTestRedisClient(t)
	store := redisstore.NewHotContextStore(client, cfg)
	ctx := context.Background()

	// First CAS (version 0 → 1)
	view1 := ctxpkg.HotContextView{
		Messages: []ctxpkg.Message{{ID: "m1", Content: "hello"}},
	}
	result1, err := store.CompareAndSwap(ctx, "test-session-hot", 0, view1)
	if err != nil {
		t.Fatalf("CAS 1: %v", err)
	}
	if result1.Version != 1 {
		t.Fatalf("Version after CAS 1: got %d, want 1", result1.Version)
	}

	// Second CAS (version 1 → 2)
	view2 := ctxpkg.HotContextView{
		Messages: []ctxpkg.Message{{ID: "m1"}, {ID: "m2", Content: "world"}},
	}
	result2, err := store.CompareAndSwap(ctx, "test-session-hot", 1, view2)
	if err != nil {
		t.Fatalf("CAS 2: %v", err)
	}
	if result2.Version != 2 {
		t.Fatalf("Version after CAS 2: got %d, want 2", result2.Version)
	}

	// Load
	loaded, err := store.Load(ctx, "test-session-hot", 10)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Version != 2 {
		t.Fatalf("Loaded version: got %d, want 2", loaded.Version)
	}
	if len(loaded.Messages) != 2 {
		t.Fatalf("Loaded messages: got %d, want 2", len(loaded.Messages))
	}

	// Conflict (wrong expected version)
	_, err = store.CompareAndSwap(ctx, "test-session-hot", 0, view2)
	if err == nil {
		t.Fatal("CAS with wrong expected version should fail")
	}
}

// TestIntegrationMCPSnapshotStore verifies MCPSnapshotStore against real Redis.
func TestIntegrationMCPSnapshotStore(t *testing.T) {
	client, cfg := newTestRedisClient(t)
	store := redisstore.NewMCPSnapshotStore(client, cfg)
	ctx := context.Background()

	snap := mcp.CapabilitySnapshot{
		ID:             "test-mcp-snap-1",
		ServerID:       "srv-1",
		ServerVersion:  "1.0",
		PrincipalHash:  "ph-1",
		PolicyHash:     "pol-1",
		CapabilityHash: "cap-1",
		Tools:          []mcp.Tool{{Name: "search"}},
		CreatedAt:      time.Now().Truncate(time.Millisecond),
	}

	if err := store.Save(ctx, snap); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := store.Load(ctx, "test-mcp-snap-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ID != snap.ID || loaded.ServerID != snap.ServerID {
		t.Fatalf("Load mismatch: got %+v, want %+v", loaded, snap)
	}
}

// TestIntegrationTimeline verifies Timeline against real Redis.
func TestIntegrationTimeline(t *testing.T) {
	client, cfg := newTestRedisClient(t)
	tl := redisstore.NewTimeline(client, cfg)
	ctx := context.Background()

	msgs := []ctxpkg.Message{
		{ID: "m1", Content: "hello"},
		{ID: "m2", Content: "world"},
	}
	if err := tl.Append(ctx, "test-session-tl", msgs...); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := tl.GetAll(ctx, "test-session-tl")
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("GetAll len: got %d, want 2", len(got))
	}

	n, err := tl.Len(ctx, "test-session-tl")
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if n != 2 {
		t.Fatalf("Len: got %d, want 2", n)
	}
}

// TestIntegrationStateStore verifies StateStore against real Redis.
func TestIntegrationStateStore(t *testing.T) {
	client, cfg := newTestRedisClient(t)
	store := redisstore.NewStateStore(client, cfg, nil, nil)
	ctx := context.Background()

	if err := store.Set(ctx, ctxpkg.WriteRequest{
		SessionID: "test-session-state", Path: "shared.key", Value: "hello",
		Writer: ctxpkg.Writer{Role: "harness"},
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	val, err := store.Get(ctx, "test-session-state", "shared.key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "hello" {
		t.Fatalf("Get: got %v, want %q", val, "hello")
	}
}

// TestIntegrationSemaphore verifies Semaphore against real Redis.
func TestIntegrationSemaphore(t *testing.T) {
	client, cfg := newTestRedisClient(t)
	sem := redisstore.NewSemaphore(client, cfg)
	ctx := context.Background()

	if err := sem.Acquire(ctx, "test-sem", 3); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	count, err := sem.Count(ctx, "test-sem")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 1 {
		t.Fatalf("Count: got %d, want 1", count)
	}

	if err := sem.Release(ctx, "test-sem"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	count, _ = sem.Count(ctx, "test-sem")
	if count != 0 {
		t.Fatalf("Count after release: got %d, want 0", count)
	}
}

// TestIntegrationTenantQuotaManager exercises the Lua scripts against a real
// Redis server: mixed-duration leases, idempotent accounting and non-finite
// input rejection cannot be proven by compiling the integration build tag.
func TestIntegrationTenantQuotaManager(t *testing.T) {
	client, cfg := newTestRedisClient(t)
	cfg.QuotaCommitTTL = 45 * time.Second
	manager := redisstore.NewTenantQuotaManager(client, cfg, map[string]modelgateway.TenantQuota{
		"known": {MaxConcurrent: 2},
	}, nil)
	ctx := context.Background()
	if _, err := manager.Reserve(ctx, modelgateway.ModelRequest{Trace: observability.TraceContext{TenantID: "unknown"}}); err == nil {
		t.Fatal("unknown tenant reused another tenant's quota policy")
	}
	req := modelgateway.ModelRequest{Trace: observability.TraceContext{TenantID: "known"}}
	req.TimeoutMS = int((10 * time.Minute) / time.Millisecond)
	longReservation, err := manager.Reserve(ctx, req)
	if err != nil {
		t.Fatalf("reserve long request: %v", err)
	}
	req.TimeoutMS = int((2 * time.Minute) / time.Millisecond)
	shortReservation, err := manager.Reserve(ctx, req)
	if err != nil {
		t.Fatalf("reserve short request: %v", err)
	}
	inflightKey := cfg.KeyPrefix + "quota:{known}:inflight"
	if ttl := client.PTTL(ctx, inflightKey).Val(); ttl < 9*time.Minute {
		t.Fatalf("short request shortened long lease key TTL to %v", ttl)
	}

	usage := modelgateway.ModelUsage{PromptTokens: 7, CompletionTokens: 3}
	cost := modelgateway.ModelCost{Estimated: 0.5}
	manager.Commit(ctx, longReservation, usage, cost)
	manager.Commit(ctx, longReservation, usage, cost)
	if got := client.Get(ctx, cfg.KeyPrefix+"quota:{known}:tokens").Val(); got != "10" {
		t.Fatalf("idempotent token total=%q, want 10", got)
	}
	if got := client.Get(ctx, cfg.KeyPrefix+"quota:{known}:cost").Val(); got != "0.5" {
		t.Fatalf("idempotent cost total=%q, want 0.5", got)
	}
	markerKey := cfg.KeyPrefix + "quota:{known}:commit:" + longReservation.ReservationID
	if ttl := client.TTL(ctx, markerKey).Val(); ttl <= 0 || ttl > cfg.QuotaCommitTTL {
		t.Fatalf("commit marker TTL=%v, want (0,%v]", ttl, cfg.QuotaCommitTTL)
	}

	manager.Commit(ctx, shortReservation, usage, modelgateway.ModelCost{Estimated: math.NaN()})
	if got := client.Get(ctx, cfg.KeyPrefix+"quota:{known}:tokens").Val(); got != "10" {
		t.Fatalf("non-finite commit changed token total to %q", got)
	}
	manager.Release(ctx, longReservation)
	manager.Release(ctx, shortReservation)
}
