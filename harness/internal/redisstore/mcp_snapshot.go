package redisstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
)

// MCPSnapshotStore is a distributed mcp.SnapshotStore backed by Redis. MCP
// capability snapshots are immutable once resolved: SETNX guarantees exactly-once
// writes, and a content-hash comparison allows safe retries across processes.
type MCPSnapshotStore struct {
	client Client
	prefix string
	ttl    time.Duration
}

// NewMCPSnapshotStore builds a Redis-backed MCP SnapshotStore.
// When cfg.SnapshotTTL is 0, snapshots never expire (use with caution for
// abandoned sessions). Negative values fall back to the 24h default.
func NewMCPSnapshotStore(client Client, cfg Config) *MCPSnapshotStore {
	ttl := cfg.SnapshotTTL
	if ttl < 0 {
		ttl = 24 * time.Hour
	}
	return &MCPSnapshotStore{
		client: client,
		prefix: prefix(cfg) + "mcp-snap:",
		ttl:    ttl,
	}
}

func (s *MCPSnapshotStore) key(id string) string { return s.prefix + id }

// Save persists a capability snapshot with SETNX semantics. Duplicate saves with
// matching identity are idempotent; any identity mismatch is an error.
func (s *MCPSnapshotStore) Save(ctx context.Context, snap mcp.CapabilitySnapshot) error {
	if snap.ID == "" {
		return fmt.Errorf("mcp snapshot save: %w", mcp.ErrSnapshotNotFound)
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal mcp snapshot: %w", err)
	}
	key := s.key(snap.ID)
	ok, err := s.client.SetNX(ctx, key, data, s.ttl).Result()
	if err != nil {
		return fmt.Errorf("redis setnx: %w", err)
	}
	if ok {
		return nil
	}
	existing, err := s.client.Get(ctx, key).Bytes()
	if err != nil {
		return fmt.Errorf("redis get for conflict check: %w", err)
	}
	var prev mcp.CapabilitySnapshot
	if err := json.Unmarshal(existing, &prev); err != nil {
		return fmt.Errorf("unmarshal existing mcp snapshot: %w", err)
	}
	// Compare all identity fields.
	if !mcpSnapshotIdentityMatch(prev, snap) {
		return fmt.Errorf("mcp snapshot identity conflict: %s (existing: server=%s version=%s, new: server=%s version=%s)",
			snap.ID, prev.ServerID, prev.ServerVersion, snap.ServerID, snap.ServerVersion)
	}
	return nil
}

func mcpSnapshotIdentityMatch(a, b mcp.CapabilitySnapshot) bool {
	return a.CapabilityHash == b.CapabilityHash &&
		a.ServerID == b.ServerID &&
		a.ServerVersion == b.ServerVersion &&
		a.PrincipalHash == b.PrincipalHash &&
		a.PolicyHash == b.PolicyHash
}

// Load retrieves a capability snapshot by ID. Returns ErrSnapshotNotFound when
// the key is missing.
func (s *MCPSnapshotStore) Load(ctx context.Context, snapshotID string) (mcp.CapabilitySnapshot, error) {
	data, err := s.client.Get(ctx, s.key(snapshotID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return mcp.CapabilitySnapshot{}, mcp.ErrSnapshotNotFound
	}
	if err != nil {
		return mcp.CapabilitySnapshot{}, fmt.Errorf("redis get: %w", err)
	}
	var snap mcp.CapabilitySnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return mcp.CapabilitySnapshot{}, fmt.Errorf("unmarshal mcp snapshot: %w", err)
	}
	return snap, nil
}

var _ mcp.SnapshotStore = (*MCPSnapshotStore)(nil)
