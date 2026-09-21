package redisstore

import (
	"context"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	ctxpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
)

func newTestStateStore(t *testing.T) (*StateStore, Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	t.Cleanup(func() { _ = client.Close() })
	registry := ctxpkg.NewStaticPathRegistry(
		ctxpkg.PathSpec{Pattern: "rule.*", Ownership: ctxpkg.PathRuleOwned},
		ctxpkg.PathSpec{Pattern: "agent.*", Ownership: ctxpkg.PathAgentOwned},
		ctxpkg.PathSpec{Pattern: "shared.*", Ownership: "shared"},
	)
	return NewStateStore(client, Config{}, registry, ctxpkg.NoopValidateGate{}), client
}

// TestStateSetGet: basic round-trip.
func TestStateSetGet(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStateStore(t)

	if err := store.Set(ctx, ctxpkg.WriteRequest{
		SessionID: "sess-1", Path: "shared.key", Value: "hello",
		Writer: ctxpkg.Writer{Role: "harness"},
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	val, err := store.Get(ctx, "sess-1", "shared.key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "hello" {
		t.Fatalf("Get: got %v, want %q", val, "hello")
	}
}

// TestStateGetMissingPath: returns ErrStatePathNotFound when session exists but path doesn't.
func TestStateGetMissingPath(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStateStore(t)

	// Write one path so the session hash exists.
	_ = store.Set(ctx, ctxpkg.WriteRequest{
		SessionID: "sess-1", Path: "shared.k", Value: "v",
		Writer: ctxpkg.Writer{Role: "harness"},
	})
	_, err := store.Get(ctx, "sess-1", "nonexistent")
	if !errors.Is(err, ctxpkg.ErrStatePathNotFound) {
		t.Fatalf("Get missing path: got %v, want ErrStatePathNotFound", err)
	}
}

// TestStateGetMissingSession: returns nil (session hash doesn't exist).
func TestStateGetMissingSession(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStateStore(t)
	val, err := store.Get(ctx, "nonexistent", "any")
	if err != nil {
		t.Fatalf("Get missing session: %v", err)
	}
	if val != nil {
		t.Fatalf("Get missing session: got %v, want nil", val)
	}
}

// TestStateDelete: removes a path, subsequent Get returns ErrStatePathNotFound.
func TestStateDelete(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStateStore(t)

	// Write two paths so the hash survives deletion of one.
	_ = store.Set(ctx, ctxpkg.WriteRequest{
		SessionID: "sess-2", Path: "shared.k", Value: 42,
		Writer: ctxpkg.Writer{Role: "harness"},
	})
	_ = store.Set(ctx, ctxpkg.WriteRequest{
		SessionID: "sess-2", Path: "shared.k2", Value: 99,
		Writer: ctxpkg.Writer{Role: "harness"},
	})
	if err := store.Delete(ctx, "sess-2", "shared.k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := store.Get(ctx, "sess-2", "shared.k")
	if !errors.Is(err, ctxpkg.ErrStatePathNotFound) {
		t.Fatalf("after Delete: got %v, want ErrStatePathNotFound", err)
	}
	// Other path still accessible.
	val, err := store.Get(ctx, "sess-2", "shared.k2")
	if err != nil {
		t.Fatalf("Get k2: %v", err)
	}
	if val != float64(99) {
		t.Fatalf("Get k2: got %v, want 99", val)
	}
}

// TestStateSnapshot: returns all paths for a session.
func TestStateSnapshot(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStateStore(t)

	_ = store.Set(ctx, ctxpkg.WriteRequest{SessionID: "sess-3", Path: "shared.a", Value: 1, Writer: ctxpkg.Writer{Role: "harness"}})
	_ = store.Set(ctx, ctxpkg.WriteRequest{SessionID: "sess-3", Path: "shared.b", Value: "two", Writer: ctxpkg.Writer{Role: "harness"}})

	snap, err := store.Snapshot(ctx, "sess-3")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap) != 2 {
		t.Fatalf("Snapshot len: got %d, want 2", len(snap))
	}
	// JSON numbers unmarshal as float64.
	if v, ok := snap["shared.a"].(float64); !ok || v != 1 {
		t.Fatalf("Snapshot a: got %v (%T)", snap["shared.a"], snap["shared.a"])
	}
	if snap["shared.b"] != "two" {
		t.Fatalf("Snapshot b: got %v", snap["shared.b"])
	}
}

// TestStateSnapshotEmpty: returns nil for empty session.
func TestStateSnapshotEmpty(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStateStore(t)
	snap, err := store.Snapshot(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap != nil {
		t.Fatalf("Snapshot empty: got %v, want nil", snap)
	}
}

// TestStateSetEmptySessionID: returns ErrSessionIDMissing.
func TestStateSetEmptySessionID(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStateStore(t)
	err := store.Set(ctx, ctxpkg.WriteRequest{Path: "shared.k", Value: "v"})
	if !errors.Is(err, ctxpkg.ErrSessionIDMissing) {
		t.Fatalf("Set empty: got %v, want ErrSessionIDMissing", err)
	}
}

// TestStateRBACRuleOwned: only rule/harness writers can write to rule-owned paths.
func TestStateRBACRuleOwned(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStateStore(t)

	// Agent writer should fail on rule-owned path.
	err := store.Set(ctx, ctxpkg.WriteRequest{
		SessionID: "sess-4", Path: "rule.config", Value: "x",
		Writer: ctxpkg.Writer{Role: "agent"},
	})
	if !errors.Is(err, ctxpkg.ErrStatePathForbidden) {
		t.Fatalf("RBAC rule-owned: got %v, want ErrStatePathForbidden", err)
	}
	// Rule writer should succeed.
	err = store.Set(ctx, ctxpkg.WriteRequest{
		SessionID: "sess-4", Path: "rule.config", Value: "x",
		Writer: ctxpkg.Writer{Role: "rule"},
	})
	if err != nil {
		t.Fatalf("RBAC rule writer: %v", err)
	}
}

// TestStateRBACAgentOwned: only agent/harness writers can write to agent-owned paths.
func TestStateRBACAgentOwned(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStateStore(t)

	// Rule writer should fail on agent-owned path.
	err := store.Set(ctx, ctxpkg.WriteRequest{
		SessionID: "sess-5", Path: "agent.state", Value: "x",
		Writer: ctxpkg.Writer{Role: "rule"},
	})
	if !errors.Is(err, ctxpkg.ErrStatePathForbidden) {
		t.Fatalf("RBAC agent-owned: got %v, want ErrStatePathForbidden", err)
	}
	// Agent writer should succeed.
	err = store.Set(ctx, ctxpkg.WriteRequest{
		SessionID: "sess-5", Path: "agent.state", Value: "x",
		Writer: ctxpkg.Writer{Role: "agent"},
	})
	if err != nil {
		t.Fatalf("RBAC agent writer: %v", err)
	}
}

// TestStateComplexValue: stores and retrieves complex JSON values.
func TestStateComplexValue(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStateStore(t)

	complex := map[string]any{"nested": map[string]any{"key": "val"}, "list": []any{1, 2, 3}}
	if err := store.Set(ctx, ctxpkg.WriteRequest{
		SessionID: "sess-6", Path: "shared.complex", Value: complex,
		Writer: ctxpkg.Writer{Role: "harness"},
	}); err != nil {
		t.Fatalf("Set complex: %v", err)
	}
	val, err := store.Get(ctx, "sess-6", "shared.complex")
	if err != nil {
		t.Fatalf("Get complex: %v", err)
	}
	m, ok := val.(map[string]any)
	if !ok {
		t.Fatalf("Get complex: expected map, got %T", val)
	}
	nested, ok := m["nested"].(map[string]any)
	if !ok || nested["key"] != "val" {
		t.Fatalf("Get complex nested: got %+v", m)
	}
}
