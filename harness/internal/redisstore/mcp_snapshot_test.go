package redisstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
)

func newTestMCPSnapshotStore(t *testing.T) (*MCPSnapshotStore, Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	t.Cleanup(func() { _ = client.Close() })
	return NewMCPSnapshotStore(client, Config{}), client
}

func testMCPSnapshot(id string) mcp.CapabilitySnapshot {
	return mcp.CapabilitySnapshot{
		ID:             id,
		ServerID:       "srv-1",
		ServerVersion:  "1.0",
		PrincipalHash:  "ph-1",
		PolicyHash:     "pol-1",
		CapabilityHash: "cap-abc",
		Tools:          []mcp.Tool{{Name: "search", Description: "search tool"}},
		CreatedAt:      time.Now().Truncate(time.Millisecond),
	}
}

// TestMCPSnapshotSaveLoad: basic round-trip.
func TestMCPSnapshotSaveLoad(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestMCPSnapshotStore(t)
	snap := testMCPSnapshot("ms-1")

	if err := store.Save(ctx, snap); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := store.Load(ctx, "ms-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ID != snap.ID || loaded.ServerID != snap.ServerID {
		t.Fatalf("Load mismatch: got %+v, want %+v", loaded, snap)
	}
	if loaded.CapabilityHash != snap.CapabilityHash {
		t.Fatalf("CapabilityHash: got %q, want %q", loaded.CapabilityHash, snap.CapabilityHash)
	}
	if len(loaded.Tools) != 1 || loaded.Tools[0].Name != "search" {
		t.Fatalf("Tools mismatch: got %+v", loaded.Tools)
	}
}

// TestMCPSnapshotLoadNotFound: missing key returns ErrSnapshotNotFound.
func TestMCPSnapshotLoadNotFound(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestMCPSnapshotStore(t)
	_, err := store.Load(ctx, "nonexistent")
	if !errors.Is(err, mcp.ErrSnapshotNotFound) {
		t.Fatalf("Load nonexistent: got %v, want ErrSnapshotNotFound", err)
	}
}

// TestMCPSnapshotIdempotentSave: duplicate save with same capability-hash is OK.
func TestMCPSnapshotIdempotentSave(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestMCPSnapshotStore(t)
	snap := testMCPSnapshot("ms-2")

	if err := store.Save(ctx, snap); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := store.Save(ctx, snap); err != nil {
		t.Fatalf("idempotent Save: %v", err)
	}
}

// TestMCPSnapshotConflict: duplicate save with different hash is an error.
func TestMCPSnapshotConflict(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestMCPSnapshotStore(t)
	snap := testMCPSnapshot("ms-3")

	if err := store.Save(ctx, snap); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	snap.CapabilityHash = "cap-different"
	if err := store.Save(ctx, snap); err == nil {
		t.Fatal("Save with different hash should fail")
	}
}

// TestMCPSnapshotIdentityConflict: same ID but different ServerID/Version is rejected.
func TestMCPSnapshotIdentityConflict(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestMCPSnapshotStore(t)

	snap1 := testMCPSnapshot("ms-identity")
	if err := store.Save(ctx, snap1); err != nil {
		t.Fatalf("first Save: %v", err)
	}

	// Same ID, same CapabilityHash, but different ServerID
	snap2 := snap1
	snap2.ServerID = "different-server"
	if err := store.Save(ctx, snap2); err == nil {
		t.Fatal("Save with different ServerID should fail")
	}

	// Same ID, same CapabilityHash, but different ServerVersion
	snap3 := snap1
	snap3.ServerVersion = "2.0"
	if err := store.Save(ctx, snap3); err == nil {
		t.Fatal("Save with different ServerVersion should fail")
	}

	// Same ID, same CapabilityHash, but different PrincipalHash
	snap4 := snap1
	snap4.PrincipalHash = "different-principal"
	if err := store.Save(ctx, snap4); err == nil {
		t.Fatal("Save with different PrincipalHash should fail")
	}
}

// TestMCPSnapshotTTL: snapshot expires after TTL.
func TestMCPSnapshotTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	t.Cleanup(func() { _ = client.Close() })

	store := NewMCPSnapshotStore(client, Config{SnapshotTTL: 2 * time.Second})
	ctx := context.Background()
	snap := testMCPSnapshot("ms-ttl")

	if err := store.Save(ctx, snap); err != nil {
		t.Fatalf("Save: %v", err)
	}
	mr.FastForward(3 * time.Second)

	_, err := store.Load(ctx, "ms-ttl")
	if !errors.Is(err, mcp.ErrSnapshotNotFound) {
		t.Fatalf("after TTL: got %v, want ErrSnapshotNotFound", err)
	}
}

// TestMCPSnapshotTTLZeroNoExpiry: SnapshotTTL=0 means keys never expire.
func TestMCPSnapshotTTLZeroNoExpiry(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	t.Cleanup(func() { _ = client.Close() })

	// SnapshotTTL=0 must mean no expiry (not the 24h default).
	store := NewMCPSnapshotStore(client, Config{SnapshotTTL: 0})
	ctx := context.Background()
	snap := testMCPSnapshot("ms-ttl-zero")

	if err := store.Save(ctx, snap); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Fast forward past 24h — key must still be present.
	mr.FastForward(25 * time.Hour)

	loaded, err := store.Load(ctx, "ms-ttl-zero")
	if err != nil {
		t.Fatalf("Load after 25h with TTL=0: got %v, want nil", err)
	}
	if loaded.ID != snap.ID {
		t.Fatalf("Load mismatch: got %+v, want %+v", loaded, snap)
	}
}
