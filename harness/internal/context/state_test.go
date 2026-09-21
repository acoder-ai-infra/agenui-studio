package context

import (
	"context"
	"errors"
	"testing"
)

func TestInMemoryStateStoreSetAndGet(t *testing.T) {
	store := NewInMemoryStateStore(nil, nil)
	ctx := context.Background()

	err := store.Set(ctx, WriteRequest{
		SessionID: "s1",
		Path:      "task.status",
		Value:     "running",
		Writer:    Writer{Role: "rule", Name: "rule:start"},
	})
	if err != nil {
		t.Fatalf("set: %v", err)
	}

	val, err := store.Get(ctx, "s1", "task.status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if val != "running" {
		t.Fatalf("expected 'running', got: %v", val)
	}
}

func TestInMemoryStateStoreGetNotFound(t *testing.T) {
	store := NewInMemoryStateStore(nil, nil)
	ctx := context.Background()

	// Non-existent session → nil, nil (no data).
	val, err := store.Get(ctx, "s1", "nonexistent")
	if err != nil || val != nil {
		t.Fatalf("expected nil,nil for non-existent session, got val=%v err=%v", val, err)
	}

	// Existing session, non-existent path → ErrStatePathNotFound.
	_ = store.Set(ctx, WriteRequest{SessionID: "s1", Path: "a", Value: 1})
	_, err = store.Get(ctx, "s1", "missing")
	if !errors.Is(err, ErrStatePathNotFound) {
		t.Fatalf("expected ErrStatePathNotFound, got: %v", err)
	}
}

func TestInMemoryStateStoreSetMissingSessionID(t *testing.T) {
	store := NewInMemoryStateStore(nil, nil)
	err := store.Set(context.Background(), WriteRequest{Path: "x", Value: 1})
	if !errors.Is(err, ErrSessionIDMissing) {
		t.Fatalf("expected ErrSessionIDMissing, got: %v", err)
	}
}

func TestInMemoryStateStoreDelete(t *testing.T) {
	store := NewInMemoryStateStore(nil, nil)
	ctx := context.Background()

	_ = store.Set(ctx, WriteRequest{SessionID: "s1", Path: "a", Value: 1})
	_ = store.Delete(ctx, "s1", "a")

	_, err := store.Get(ctx, "s1", "a")
	if !errors.Is(err, ErrStatePathNotFound) {
		t.Fatalf("expected ErrStatePathNotFound after delete, got: %v", err)
	}
}

func TestInMemoryStateStoreSnapshot(t *testing.T) {
	store := NewInMemoryStateStore(nil, nil)
	ctx := context.Background()

	_ = store.Set(ctx, WriteRequest{SessionID: "s1", Path: "a", Value: 1})
	_ = store.Set(ctx, WriteRequest{SessionID: "s1", Path: "b", Value: "two"})

	snap, err := store.Snapshot(ctx, "s1")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(snap))
	}
	if snap["a"] != 1 || snap["b"] != "two" {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
}

func TestInMemoryStateStoreSnapshotEmpty(t *testing.T) {
	store := NewInMemoryStateStore(nil, nil)
	snap, err := store.Snapshot(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap != nil {
		t.Fatalf("expected nil snapshot, got %+v", snap)
	}
}

func TestInMemoryStateStoreSnapshotClone(t *testing.T) {
	store := NewInMemoryStateStore(nil, nil)
	ctx := context.Background()

	_ = store.Set(ctx, WriteRequest{SessionID: "s1", Path: "x", Value: "original"})
	snap, _ := store.Snapshot(ctx, "s1")
	snap["x"] = "mutated"

	snap2, _ := store.Snapshot(ctx, "s1")
	if snap2["x"] != "original" {
		t.Fatalf("snapshot should clone, got: %v", snap2["x"])
	}
}

// --- RBAC tests ---

func TestStateStoreRuleOwnedPath(t *testing.T) {
	registry := NewStaticPathRegistry(
		PathSpec{Pattern: "task.*", Ownership: PathRuleOwned},
	)
	store := NewInMemoryStateStore(registry, nil)
	ctx := context.Background()

	// Rule writer → OK.
	err := store.Set(ctx, WriteRequest{
		SessionID: "s1", Path: "task.status", Value: "blocked",
		Writer: Writer{Role: "rule", Name: "rule:wf"},
	})
	if err != nil {
		t.Fatalf("rule write should succeed: %v", err)
	}

	// Agent writer → forbidden.
	err = store.Set(ctx, WriteRequest{
		SessionID: "s1", Path: "task.status", Value: "running",
		Writer: Writer{Role: "agent", Name: "agent:main"},
	})
	if !errors.Is(err, ErrStatePathForbidden) {
		t.Fatalf("agent write to rule-owned path should fail: %v", err)
	}
}

func TestStateStoreAgentOwnedPath(t *testing.T) {
	registry := NewStaticPathRegistry(
		PathSpec{Pattern: "agent.*", Ownership: PathAgentOwned},
	)
	store := NewInMemoryStateStore(registry, nil)
	ctx := context.Background()

	// Agent writer → OK.
	err := store.Set(ctx, WriteRequest{
		SessionID: "s1", Path: "agent.plan", Value: "step1",
		Writer: Writer{Role: "agent", Name: "agent:main"},
	})
	if err != nil {
		t.Fatalf("agent write should succeed: %v", err)
	}

	// Rule writer → forbidden.
	err = store.Set(ctx, WriteRequest{
		SessionID: "s1", Path: "agent.plan", Value: "override",
		Writer: Writer{Role: "rule", Name: "rule:x"},
	})
	if !errors.Is(err, ErrStatePathForbidden) {
		t.Fatalf("rule write to agent-owned path should fail: %v", err)
	}
}

func TestStateStoreSharedPath(t *testing.T) {
	registry := NewStaticPathRegistry(
		PathSpec{Pattern: "shared.*", Ownership: PathShared},
	)
	store := NewInMemoryStateStore(registry, nil)
	ctx := context.Background()

	// Both rule and agent can write shared paths.
	if err := store.Set(ctx, WriteRequest{
		SessionID: "s1", Path: "shared.log", Value: "rule entry",
		Writer: Writer{Role: "rule", Name: "r"},
	}); err != nil {
		t.Fatalf("rule write shared: %v", err)
	}
	if err := store.Set(ctx, WriteRequest{
		SessionID: "s1", Path: "shared.log", Value: "agent entry",
		Writer: Writer{Role: "agent", Name: "a"},
	}); err != nil {
		t.Fatalf("agent write shared: %v", err)
	}
}

func TestStateStoreHarnessBypassesOwnership(t *testing.T) {
	registry := NewStaticPathRegistry(
		PathSpec{Pattern: "task.*", Ownership: PathRuleOwned},
		PathSpec{Pattern: "agent.*", Ownership: PathAgentOwned},
	)
	store := NewInMemoryStateStore(registry, nil)
	ctx := context.Background()

	// Harness writer bypasses both rule-owned and agent-owned.
	if err := store.Set(ctx, WriteRequest{
		SessionID: "s1", Path: "task.status", Value: "x",
		Writer: Writer{Role: "harness", Name: "harness:init"},
	}); err != nil {
		t.Fatalf("harness write rule-owned: %v", err)
	}
	if err := store.Set(ctx, WriteRequest{
		SessionID: "s1", Path: "agent.plan", Value: "y",
		Writer: Writer{Role: "harness", Name: "harness:init"},
	}); err != nil {
		t.Fatalf("harness write agent-owned: %v", err)
	}
}

func TestStateStoreValidateGate(t *testing.T) {
	registry := NewStaticPathRegistry(
		PathSpec{Pattern: "agent.tool_choice", Ownership: PathAgentOwned},
	)
	gate := &rejectGate{}
	store := NewInMemoryStateStore(registry, gate)
	ctx := context.Background()

	err := store.Set(ctx, WriteRequest{
		SessionID: "s1", Path: "agent.tool_choice", Value: "invalid",
		Writer: Writer{Role: "agent", Name: "a"},
	})
	if !errors.Is(err, ErrStateValidation) {
		t.Fatalf("expected ErrStateValidation, got: %v", err)
	}
}

func TestStateStoreUnregisteredPathAllowsAll(t *testing.T) {
	registry := NewStaticPathRegistry(
		PathSpec{Pattern: "task.*", Ownership: PathRuleOwned},
	)
	store := NewInMemoryStateStore(registry, nil)
	ctx := context.Background()

	// Unregistered path → no RBAC check → any writer OK.
	err := store.Set(ctx, WriteRequest{
		SessionID: "s1", Path: "custom.field", Value: "x",
		Writer: Writer{Role: "agent", Name: "a"},
	})
	if err != nil {
		t.Fatalf("unregistered path should allow any writer: %v", err)
	}
}

func TestStateStoreConcurrency(t *testing.T) {
	store := NewInMemoryStateStore(nil, nil)
	ctx := context.Background()

	done := make(chan struct{})
	for i := 0; i < 50; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			_ = store.Set(ctx, WriteRequest{
				SessionID: "s1",
				Path:      "counter",
				Value:     n,
				Writer:    Writer{Role: "rule", Name: "r"},
			})
			_, _ = store.Get(ctx, "s1", "counter")
			_, _ = store.Snapshot(ctx, "s1")
		}(i)
	}
	for i := 0; i < 50; i++ {
		<-done
	}
}

// --- helpers ---

type rejectGate struct{}

func (rejectGate) Validate(_ string, _ any) error {
	return errors.New("rejected by gate")
}

func TestMatchPath(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"task.*", "task.status", true},
		{"task.*", "task", true},
		{"task.*", "task.sub.deep", true},
		{"task.*", "other", false},
		{"exact.path", "exact.path", true},
		{"exact.path", "exact.other", false},
	}
	for _, tt := range tests {
		got := matchPath(tt.pattern, tt.path)
		if got != tt.want {
			t.Errorf("matchPath(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}
