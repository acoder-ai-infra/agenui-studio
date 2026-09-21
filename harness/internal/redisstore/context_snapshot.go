package redisstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	ctxpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
)

// ContextSnapshotStore is a distributed context.SnapshotStore backed by Redis.
// Snapshots are immutable once written: SETNX ensures idempotent saves, and a
// content-hash check allows safe retries across processes. A configurable TTL
// bounds storage growth for abandoned sessions.
type ContextSnapshotStore struct {
	client Client
	prefix string
	ttl    time.Duration
}

// NewContextSnapshotStore builds a Redis-backed context SnapshotStore.
// When cfg.SnapshotTTL is 0, snapshots never expire (use with caution for
// abandoned sessions). Negative values fall back to the 24h default.
func NewContextSnapshotStore(client Client, cfg Config) *ContextSnapshotStore {
	ttl := cfg.SnapshotTTL
	if ttl < 0 {
		ttl = 24 * time.Hour
	}
	return &ContextSnapshotStore{
		client: client,
		prefix: prefix(cfg) + "ctx-snap:",
		ttl:    ttl,
	}
}

func (s *ContextSnapshotStore) key(id string) string { return s.prefix + id }

// Save persists a snapshot with SETNX semantics. A duplicate save with matching
// identity (all immutable fields) is treated as success (idempotent retry); any
// difference in identity fields is a conflict error. When the store's TTL is 0,
// the key is stored without expiry.
func (s *ContextSnapshotStore) Save(ctx context.Context, snap ctxpkg.Snapshot) (string, error) {
	if snap.ID == "" {
		return "", fmt.Errorf("snapshot save: %w", ctxpkg.ErrSnapshotNotFound)
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return "", fmt.Errorf("marshal snapshot: %w", err)
	}
	key := s.key(snap.ID)
	// TTL=0 means no expiry; pass 0 to SetNX which go-redis treats as no TTL.
	ok, err := s.client.SetNX(ctx, key, data, s.ttl).Result()
	if err != nil {
		return "", fmt.Errorf("redis setnx: %w", err)
	}
	if ok {
		return snap.ID, nil
	}
	// Key already exists: verify full identity for idempotent retry.
	existing, err := s.client.Get(ctx, key).Bytes()
	if err != nil {
		return "", fmt.Errorf("redis get for conflict check: %w", err)
	}
	var prev ctxpkg.Snapshot
	if err := json.Unmarshal(existing, &prev); err != nil {
		return "", fmt.Errorf("unmarshal existing snapshot: %w", err)
	}
	// Compare all immutable identity fields, not just ContentHash.
	if !snapshotIdentityMatch(prev, snap) {
		return "", fmt.Errorf("snapshot identity conflict: %s (existing: session=%s run=%s seq=%d, new: session=%s run=%s seq=%d)",
			snap.ID, prev.SessionID, prev.RunID, prev.LastSequence,
			snap.SessionID, snap.RunID, snap.LastSequence)
	}
	return snap.ID, nil
}

// snapshotIdentityMatch compares all immutable fields that define a snapshot's
// identity. Two snapshots with the same ID must have identical identity fields
// to be considered the same snapshot (idempotent retry).
func snapshotIdentityMatch(a, b ctxpkg.Snapshot) bool {
	return a.ContentHash == b.ContentHash &&
		a.SessionID == b.SessionID &&
		a.RunID == b.RunID &&
		a.LastSequence == b.LastSequence &&
		a.Source == b.Source &&
		a.HotVersion == b.HotVersion
}

// Load retrieves a snapshot by ID. Returns ErrSnapshotNotFound when the key is
// missing (expired or never written).
func (s *ContextSnapshotStore) Load(ctx context.Context, snapshotID string) (ctxpkg.Snapshot, error) {
	data, err := s.client.Get(ctx, s.key(snapshotID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return ctxpkg.Snapshot{}, ctxpkg.ErrSnapshotNotFound
	}
	if err != nil {
		return ctxpkg.Snapshot{}, fmt.Errorf("redis get: %w", err)
	}
	var snap ctxpkg.Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return ctxpkg.Snapshot{}, fmt.Errorf("unmarshal snapshot: %w", err)
	}
	return snap, nil
}

var _ ctxpkg.SnapshotStore = (*ContextSnapshotStore)(nil)
