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

// StateStore is a distributed context.StateStore backed by Redis Hashes. Each
// session gets one hash (field = path, value = JSON-encoded any). Path-scoped
// RBAC is enforced through the same PathRegistry and ValidateGate used by the
// in-memory implementation.
type StateStore struct {
	client   Client
	prefix   string
	ttl      time.Duration
	registry ctxpkg.PathRegistry
	gate     ctxpkg.ValidateGate
}

// NewStateStore builds a Redis-backed StateStore with path governance.
func NewStateStore(client Client, cfg Config, registry ctxpkg.PathRegistry, gate ctxpkg.ValidateGate) *StateStore {
	if registry == nil {
		registry = ctxpkg.NewStaticPathRegistry()
	}
	if gate == nil {
		gate = ctxpkg.NoopValidateGate{}
	}
	ttl := cfg.StateTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &StateStore{
		client:   client,
		prefix:   prefix(cfg) + "state:",
		ttl:      ttl,
		registry: registry,
		gate:     gate,
	}
}

func (s *StateStore) key(sessionID string) string {
	return s.prefix + "{" + sessionID + "}"
}

// Get retrieves a single state value. Returns (nil, nil) when the session hash
// does not exist, and ErrStatePathNotFound when the field is missing.
func (s *StateStore) Get(ctx context.Context, sessionID string, path string) (any, error) {
	key := s.key(sessionID)
	n, err := s.client.Exists(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("redis exists: %w", err)
	}
	if n == 0 {
		return nil, nil
	}
	raw, err := s.client.HGet(ctx, key, path).Result()
	if errors.Is(err, redis.Nil) {
		return nil, ctxpkg.ErrStatePathNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("redis hget: %w", err)
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, fmt.Errorf("unmarshal state value: %w", err)
	}
	return value, nil
}

// Set writes a state value with path-scoped governance (ownership check +
// validation gate). The session hash TTL is refreshed on every write.
func (s *StateStore) Set(ctx context.Context, req ctxpkg.WriteRequest) error {
	if req.SessionID == "" {
		return ctxpkg.ErrSessionIDMissing
	}
	// Check path ownership and validation gate (same logic as InMemoryStateStore).
	if spec, found := s.registry.Lookup(req.Path); found {
		if err := checkStateWritePermission(spec, req.Writer); err != nil {
			return err
		}
		if spec.Ownership == ctxpkg.PathAgentOwned {
			if err := s.gate.Validate(req.Path, req.Value); err != nil {
				return fmt.Errorf("%w: %v", ctxpkg.ErrStateValidation, err)
			}
		}
	}
	data, err := json.Marshal(req.Value)
	if err != nil {
		return fmt.Errorf("marshal state value: %w", err)
	}
	key := s.key(req.SessionID)
	pipe := s.client.TxPipeline()
	pipe.HSet(ctx, key, req.Path, data)
	pipe.Expire(ctx, key, s.ttl)
	_, err = pipe.Exec(ctx)
	return err
}

// Delete removes a single path from a session's state hash.
func (s *StateStore) Delete(ctx context.Context, sessionID string, path string) error {
	return s.client.HDel(ctx, s.key(sessionID), path).Err()
}

// Snapshot returns all path→value pairs for a session.
func (s *StateStore) Snapshot(ctx context.Context, sessionID string) (map[string]any, error) {
	fields, err := s.client.HGetAll(ctx, s.key(sessionID)).Result()
	if err != nil {
		return nil, fmt.Errorf("redis hgetall: %w", err)
	}
	if len(fields) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(fields))
	for k, raw := range fields {
		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			continue
		}
		out[k] = value
	}
	return out, nil
}

// checkStateWritePermission mirrors context.checkWritePermission for the Redis
// adapter. It is a local copy because the original is unexported.
func checkStateWritePermission(spec *ctxpkg.PathSpec, writer ctxpkg.Writer) error {
	switch spec.Ownership {
	case ctxpkg.PathRuleOwned:
		if writer.Role != "rule" && writer.Role != "harness" {
			return fmt.Errorf("%w: path is rule-owned, writer role=%q", ctxpkg.ErrStatePathForbidden, writer.Role)
		}
	case ctxpkg.PathAgentOwned:
		if writer.Role != "agent" && writer.Role != "harness" {
			return fmt.Errorf("%w: path is agent-owned, writer role=%q", ctxpkg.ErrStatePathForbidden, writer.Role)
		}
	}
	return nil
}

var _ ctxpkg.StateStore = (*StateStore)(nil)
