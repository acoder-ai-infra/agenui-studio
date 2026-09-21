package redisstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
)

// MCPRegistry is a distributed mcp.Registry backed by Redis. Server definitions
// are JSON-encoded under a namespaced key. The Register method is intended for
// startup-time batch loading (same as InMemoryRegistry.Register); at runtime,
// all instances should share the same definitions.
type MCPRegistry struct {
	client Client
	prefix string
}

// NewMCPRegistry builds a Redis-backed MCP Registry.
func NewMCPRegistry(client Client, cfg Config) *MCPRegistry {
	return &MCPRegistry{
		client: client,
		prefix: prefix(cfg) + "mcp-reg:",
	}
}

func (r *MCPRegistry) key(serverID string) string { return r.prefix + serverID }

// Register stores a server definition. Returns an error if the ID is already
// registered (matching InMemoryRegistry duplicate-ID behavior).
func (r *MCPRegistry) Register(ctx context.Context, def mcp.ServerDefinition) error {
	if def.ID == "" || def.Scope == "" {
		return fmt.Errorf("mcp registry: server id and scope are required")
	}
	if def.Scope == mcp.ScopeTenant && def.TenantID == "" {
		return fmt.Errorf("mcp registry: tenant scoped server requires tenant id")
	}
	if def.Scope == mcp.ScopeUser && (def.TenantID == "" || def.UserID == "") {
		return fmt.Errorf("mcp registry: user scoped server requires tenant and user id")
	}
	data, err := json.Marshal(def)
	if err != nil {
		return fmt.Errorf("marshal server definition: %w", err)
	}
	key := r.key(def.ID)
	ok, err := r.client.SetNX(ctx, key, data, 0).Result()
	if err != nil {
		return fmt.Errorf("redis setnx: %w", err)
	}
	if !ok {
		return fmt.Errorf("mcp registry: duplicate server id %s", def.ID)
	}
	return nil
}

// Get retrieves a server definition by ID. Returns ErrServerNotFound when the
// key is missing.
func (r *MCPRegistry) Get(ctx context.Context, serverID string) (mcp.ServerDefinition, error) {
	data, err := r.client.Get(ctx, r.key(serverID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return mcp.ServerDefinition{}, mcp.ErrServerNotFound
	}
	if err != nil {
		return mcp.ServerDefinition{}, fmt.Errorf("redis get: %w", err)
	}
	var def mcp.ServerDefinition
	if err := json.Unmarshal(data, &def); err != nil {
		return mcp.ServerDefinition{}, fmt.Errorf("unmarshal server definition: %w", err)
	}
	return def, nil
}

// Upsert overwrites a server definition regardless of whether it already exists.
// This is useful for config-driven refresh where all instances should converge.
func (r *MCPRegistry) Upsert(ctx context.Context, def mcp.ServerDefinition) error {
	if def.ID == "" || def.Scope == "" {
		return fmt.Errorf("mcp registry: server id and scope are required")
	}
	data, err := json.Marshal(def)
	if err != nil {
		return fmt.Errorf("marshal server definition: %w", err)
	}
	return r.client.Set(ctx, r.key(def.ID), data, 0).Err()
}

var _ mcp.Registry = (*MCPRegistry)(nil)
