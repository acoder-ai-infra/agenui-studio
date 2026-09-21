package tenantadmin

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func newTestRegistry(t *testing.T) *SQLManagedRegistry {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "tenantadmin.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := ApplySQLiteSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return NewSQLManagedRegistry(db)
}

func sampleDefinition(id string) TenantDefinition {
	return TenantDefinition{ID: id, Name: "Example Workspace", Owner: "Example Team", Plan: "Enterprise", Status: StatusActive}
}

func TestCreateGeneratesUniqueKeyAndListsNewestFirst(t *testing.T) {
	ctx := context.Background()
	registry := newTestRegistry(t)

	a, err := registry.Create(ctx, "alice", sampleDefinition("tenant-a"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Revision != 1 || a.Definition.ID != "tenant-a" || len(a.Definition.SecretKey) != SecretKeyLength {
		t.Fatalf("create a = %#v", a)
	}
	b, err := registry.Create(ctx, "alice", sampleDefinition("tenant-b"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Definition.SecretKey == b.Definition.SecretKey {
		t.Fatalf("secret keys must differ: %q", a.Definition.SecretKey)
	}
	// Duplicate id → ErrDuplicateID.
	if _, err := registry.Create(ctx, "alice", sampleDefinition("tenant-a")); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate id error = %v", err)
	}
	list, err := registry.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, item := range list {
		ids[item.Definition.ID] = true
	}
	if len(list) != 2 || !ids["tenant-a"] || !ids["tenant-b"] {
		t.Fatalf("list = %#v", list)
	}
}

func TestUpdateAndDeleteCAS(t *testing.T) {
	ctx := context.Background()
	registry := newTestRegistry(t)
	created, err := registry.Create(ctx, "alice", sampleDefinition("tenant-a"))
	if err != nil {
		t.Fatal(err)
	}

	// Stale revision → conflict.
	stale := created.Definition
	stale.Name = "改名"
	if _, err := registry.Update(ctx, "bob", stale, 99); !errors.Is(err, ErrTenantConflict) {
		t.Fatalf("stale update error = %v", err)
	}
	// Correct revision bumps and preserves the immutable secret key.
	next, err := registry.Update(ctx, "bob", stale, created.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if next.Revision != 2 || next.Definition.Name != "改名" || next.Definition.SecretKey != created.Definition.SecretKey {
		t.Fatalf("update = %#v", next)
	}
	// Delete with stale revision → conflict; correct revision succeeds.
	if err := registry.Delete(ctx, "tenant-a", created.Revision); !errors.Is(err, ErrTenantConflict) {
		t.Fatalf("stale delete error = %v", err)
	}
	if err := registry.Delete(ctx, "tenant-a", next.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Get(ctx, "tenant-a"); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("get after delete = %v", err)
	}
}

func TestResolveSecretKey(t *testing.T) {
	ctx := context.Background()
	registry := newTestRegistry(t)
	created, err := registry.Create(ctx, "alice", sampleDefinition("tenant-a"))
	if err != nil {
		t.Fatal(err)
	}

	got, err := registry.ResolveSecretKey(ctx, created.Definition.SecretKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.Definition.ID != "tenant-a" {
		t.Fatalf("resolved tenant = %#v", got)
	}
	// Unknown key → not found.
	if _, err := registry.ResolveSecretKey(ctx, "ZZZZZZ"); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("unknown key error = %v", err)
	}
	// Paused tenant rejects login.
	paused := created.Definition
	paused.Status = StatusPaused
	if _, err := registry.Update(ctx, "bob", paused, created.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ResolveSecretKey(ctx, created.Definition.SecretKey); !errors.Is(err, ErrTenantPaused) {
		t.Fatalf("paused login error = %v", err)
	}
}

func TestEnsureDefaultTenantIsIdempotent(t *testing.T) {
	ctx := context.Background()
	registry := newTestRegistry(t)

	first, err := registry.EnsureDefaultTenant(ctx, "migration")
	if err != nil {
		t.Fatal(err)
	}
	if first.Definition.ID != DefaultTenantID || first.Definition.Name != DefaultTenantName || first.Definition.Status != StatusActive {
		t.Fatalf("default tenant = %#v", first)
	}
	if len(first.Definition.SecretKey) != SecretKeyLength {
		t.Fatalf("default tenant key = %q", first.Definition.SecretKey)
	}
	second, err := registry.EnsureDefaultTenant(ctx, "migration")
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision != first.Revision || second.Definition.SecretKey != first.Definition.SecretKey {
		t.Fatalf("ensure default tenant must not overwrite existing row: first=%#v second=%#v", first, second)
	}
	if err := ValidateDefaultTenant(ctx, registry.db); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidDefinitionRejected(t *testing.T) {
	ctx := context.Background()
	registry := newTestRegistry(t)
	if _, err := registry.Create(ctx, "alice", TenantDefinition{ID: "Bad_ID", Name: "x"}); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("bad id error = %v", err)
	}
	if _, err := registry.Create(ctx, "alice", TenantDefinition{ID: "tenant-a", Name: ""}); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("missing name error = %v", err)
	}
}
