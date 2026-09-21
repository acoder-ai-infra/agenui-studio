package redisstore

import (
	"context"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
)

func newTestMCPRegistry(t *testing.T) (*MCPRegistry, Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	t.Cleanup(func() { _ = client.Close() })
	return NewMCPRegistry(client, Config{}), client
}

func testServerDef(id string) mcp.ServerDefinition {
	return mcp.ServerDefinition{
		ID:                 id,
		Version:            "1.0",
		Scope:              mcp.ScopeSystem,
		MaxConcurrentCalls: 10,
		MaxResultBytes:     1024,
	}
}

// TestMCPRegistryRegisterGet: basic round-trip.
func TestMCPRegistryRegisterGet(t *testing.T) {
	ctx := context.Background()
	reg, _ := newTestMCPRegistry(t)

	def := testServerDef("srv-1")
	if err := reg.Register(ctx, def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, err := reg.Get(ctx, "srv-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != "srv-1" || got.Version != "1.0" {
		t.Fatalf("Get mismatch: got %+v", got)
	}
	if got.MaxConcurrentCalls != 10 {
		t.Fatalf("MaxConcurrentCalls: got %d, want 10", got.MaxConcurrentCalls)
	}
}

// TestMCPRegistryGetNotFound: returns ErrServerNotFound.
func TestMCPRegistryGetNotFound(t *testing.T) {
	ctx := context.Background()
	reg, _ := newTestMCPRegistry(t)
	_, err := reg.Get(ctx, "nonexistent")
	if !errors.Is(err, mcp.ErrServerNotFound) {
		t.Fatalf("Get missing: got %v, want ErrServerNotFound", err)
	}
}

// TestMCPRegistryDuplicate: duplicate ID returns error.
func TestMCPRegistryDuplicate(t *testing.T) {
	ctx := context.Background()
	reg, _ := newTestMCPRegistry(t)

	if err := reg.Register(ctx, testServerDef("dup")); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := reg.Register(ctx, testServerDef("dup")); err == nil {
		t.Fatal("duplicate Register should fail")
	}
}

// TestMCPRegistryValidation: missing ID/scope returns error.
func TestMCPRegistryValidation(t *testing.T) {
	ctx := context.Background()
	reg, _ := newTestMCPRegistry(t)

	// Empty ID.
	err := reg.Register(ctx, mcp.ServerDefinition{Scope: mcp.ScopeSystem})
	if err == nil {
		t.Fatal("empty ID should fail")
	}
	// Empty scope.
	err = reg.Register(ctx, mcp.ServerDefinition{ID: "x"})
	if err == nil {
		t.Fatal("empty scope should fail")
	}
	// Tenant scope without tenant ID.
	err = reg.Register(ctx, mcp.ServerDefinition{ID: "x", Scope: mcp.ScopeTenant})
	if err == nil {
		t.Fatal("tenant scope without tenant_id should fail")
	}
}

// TestMCPRegistryUpsert: overwrites existing definition.
func TestMCPRegistryUpsert(t *testing.T) {
	ctx := context.Background()
	reg, _ := newTestMCPRegistry(t)

	def := testServerDef("srv-2")
	if err := reg.Register(ctx, def); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Upsert with different version.
	def.Version = "2.0"
	if err := reg.Upsert(ctx, def); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, _ := reg.Get(ctx, "srv-2")
	if got.Version != "2.0" {
		t.Fatalf("Upsert version: got %q, want %q", got.Version, "2.0")
	}
}

// TestMCPRegistryTenantScope: tenant-scoped server with correct tenant ID.
func TestMCPRegistryTenantScope(t *testing.T) {
	ctx := context.Background()
	reg, _ := newTestMCPRegistry(t)

	def := mcp.ServerDefinition{
		ID:       "tenant-srv",
		Version:  "1.0",
		Scope:    mcp.ScopeTenant,
		TenantID: "t1",
	}
	if err := reg.Register(ctx, def); err != nil {
		t.Fatalf("Register tenant: %v", err)
	}
	got, _ := reg.Get(ctx, "tenant-srv")
	if got.TenantID != "t1" {
		t.Fatalf("TenantID: got %q, want %q", got.TenantID, "t1")
	}
}
