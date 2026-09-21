package redisstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	ctxpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
)

func newTestContextSnapshotStore(t *testing.T) (*ContextSnapshotStore, Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	t.Cleanup(func() { _ = client.Close() })
	return NewContextSnapshotStore(client, Config{}), client
}

func testContextSnapshot(id string) ctxpkg.Snapshot {
	return ctxpkg.Snapshot{
		ID:           id,
		SessionID:    "sess-1",
		RunID:        "run-1",
		MessageIDs:   []string{"m1", "m2"},
		LastSequence: 2,
		Source:       "test",
		ContentHash:  "sha256:abc",
		CreatedAt:    time.Now().Truncate(time.Millisecond),
	}
}

// TestContextSnapshotSaveLoad: basic round-trip.
func TestContextSnapshotSaveLoad(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestContextSnapshotStore(t)
	snap := testContextSnapshot("snap-1")

	if _, err := store.Save(ctx, snap); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := store.Load(ctx, "snap-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ID != snap.ID || loaded.SessionID != snap.SessionID {
		t.Fatalf("Load mismatch: got %+v, want %+v", loaded, snap)
	}
	if loaded.ContentHash != snap.ContentHash {
		t.Fatalf("ContentHash mismatch: got %q, want %q", loaded.ContentHash, snap.ContentHash)
	}
	if len(loaded.MessageIDs) != 2 || loaded.MessageIDs[0] != "m1" {
		t.Fatalf("MessageIDs mismatch: got %v", loaded.MessageIDs)
	}
}

// TestContextSnapshotLoadNotFound: missing key returns ErrSnapshotNotFound.
func TestContextSnapshotLoadNotFound(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestContextSnapshotStore(t)
	_, err := store.Load(ctx, "nonexistent")
	if !errors.Is(err, ctxpkg.ErrSnapshotNotFound) {
		t.Fatalf("Load nonexistent: got %v, want ErrSnapshotNotFound", err)
	}
}

// TestContextSnapshotIdempotentSave: duplicate save with same content-hash is OK.
func TestContextSnapshotIdempotentSave(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestContextSnapshotStore(t)
	snap := testContextSnapshot("snap-2")

	if _, err := store.Save(ctx, snap); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if _, err := store.Save(ctx, snap); err != nil {
		t.Fatalf("idempotent Save: %v", err)
	}
}

// TestContextSnapshotConflict: duplicate save with different content-hash is an error.
func TestContextSnapshotConflict(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestContextSnapshotStore(t)
	snap := testContextSnapshot("snap-3")

	if _, err := store.Save(ctx, snap); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	snap.ContentHash = "sha256:different"
	if _, err := store.Save(ctx, snap); err == nil {
		t.Fatal("Save with different hash should fail")
	}
}

// TestContextSnapshotIdentityConflict: same ID but different SessionID/RunID/Sequence is rejected.
func TestContextSnapshotIdentityConflict(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestContextSnapshotStore(t)

	snap1 := testContextSnapshot("snap-identity")
	if _, err := store.Save(ctx, snap1); err != nil {
		t.Fatalf("first Save: %v", err)
	}

	// Same ID, same ContentHash, but different SessionID
	snap2 := snap1
	snap2.SessionID = "different-session"
	if _, err := store.Save(ctx, snap2); err == nil {
		t.Fatal("Save with different SessionID should fail")
	}

	// Same ID, same ContentHash, but different RunID
	snap3 := snap1
	snap3.RunID = "different-run"
	if _, err := store.Save(ctx, snap3); err == nil {
		t.Fatal("Save with different RunID should fail")
	}

	// Same ID, same ContentHash, but different LastSequence
	snap4 := snap1
	snap4.LastSequence = 999
	if _, err := store.Save(ctx, snap4); err == nil {
		t.Fatal("Save with different LastSequence should fail")
	}
}

// TestContextSnapshotTTL: snapshot expires after TTL.
func TestContextSnapshotTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	t.Cleanup(func() { _ = client.Close() })

	store := NewContextSnapshotStore(client, Config{SnapshotTTL: 2 * time.Second})
	ctx := context.Background()
	snap := testContextSnapshot("snap-ttl")

	if _, err := store.Save(ctx, snap); err != nil {
		t.Fatalf("Save: %v", err)
	}
	mr.FastForward(3 * time.Second)

	_, err := store.Load(ctx, "snap-ttl")
	if !errors.Is(err, ctxpkg.ErrSnapshotNotFound) {
		t.Fatalf("after TTL: got %v, want ErrSnapshotNotFound", err)
	}
}

// TestContextSnapshotIsolation: loaded data is independent of stored data.
func TestContextSnapshotIsolation(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestContextSnapshotStore(t)
	snap := testContextSnapshot("snap-iso")

	if _, err := store.Save(ctx, snap); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := store.Load(ctx, "snap-iso")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Mutate the loaded copy.
	loaded.MessageIDs[0] = "mutated"
	// Re-load and verify original is unchanged.
	loaded2, err := store.Load(ctx, "snap-iso")
	if err != nil {
		t.Fatalf("Load 2: %v", err)
	}
	if loaded2.MessageIDs[0] != "m1" {
		t.Fatalf("isolation violated: got %q, want %q", loaded2.MessageIDs[0], "m1")
	}
}
