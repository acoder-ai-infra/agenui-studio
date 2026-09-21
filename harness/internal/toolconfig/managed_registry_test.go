package toolconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func newTestRegistry(t *testing.T) *SQLManagedRegistry {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "toolconfig.db"))
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

func gitCloneConfig(host string) json.RawMessage {
	return json.RawMessage(`{"token_env":"GIT_TOKEN","allowed_hosts":["` + host + `"],"default_depth":1}`)
}

func TestSaveCreatesAndUpdatesUnderRevisionCAS(t *testing.T) {
	ctx := context.Background()
	r := newTestRegistry(t)

	created, err := r.Save(ctx, "tenant-a", "alice", Definition{ToolName: "git_clone", Config: gitCloneConfig("git.example.test")}, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Revision != 1 || created.Definition.TenantID != "tenant-a" || created.Definition.Scope != ScopeTenant {
		t.Fatalf("unexpected created row: %+v", created)
	}

	// A second create (expectedRevision==0) on the same key must conflict.
	if _, err := r.Save(ctx, "tenant-a", "alice", Definition{ToolName: "git_clone", Config: gitCloneConfig("git.example.test")}, 0); !errors.Is(err, ErrManagedConflict) {
		t.Fatalf("duplicate create error = %v, want ErrManagedConflict", err)
	}

	// Update with the wrong expected revision loses the CAS.
	if _, err := r.Save(ctx, "tenant-a", "alice", Definition{ToolName: "git_clone", Config: gitCloneConfig("git.other.test")}, 99); !errors.Is(err, ErrManagedConflict) {
		t.Fatalf("stale update error = %v, want ErrManagedConflict", err)
	}

	updated, err := r.Save(ctx, "tenant-a", "bob", Definition{ToolName: "git_clone", Config: gitCloneConfig("git.other.test")}, 1)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Revision != 2 || updated.UpdatedBy != "bob" {
		t.Fatalf("unexpected updated row: %+v", updated)
	}
}

func TestTenantIsolationAndGetList(t *testing.T) {
	ctx := context.Background()
	r := newTestRegistry(t)
	if _, err := r.Save(ctx, "tenant-a", "alice", Definition{ToolName: "git_clone", Config: gitCloneConfig("a.example.test")}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Save(ctx, "tenant-b", "bob", Definition{ToolName: "git_clone", Config: gitCloneConfig("b.example.test")}, 0); err != nil {
		t.Fatal(err)
	}
	// tenant-b cannot see tenant-a's config for the same tool name.
	if _, err := r.GetManaged(ctx, "tenant-a", "git_clone"); err != nil {
		t.Fatalf("get tenant-a: %v", err)
	}
	list, err := r.List(ctx, "tenant-b")
	if err != nil || len(list) != 1 || list[0].Definition.TenantID != "tenant-b" {
		t.Fatalf("list tenant-b = %+v, err %v", list, err)
	}
	if _, err := r.GetManaged(ctx, "tenant-c", "git_clone"); !errors.Is(err, ErrToolConfigNotFound) {
		t.Fatalf("missing get error = %v, want ErrToolConfigNotFound", err)
	}
}

func TestDeleteUnderRevisionCAS(t *testing.T) {
	ctx := context.Background()
	r := newTestRegistry(t)
	created, err := r.Save(ctx, "tenant-a", "alice", Definition{ToolName: "git_clone", Config: gitCloneConfig("a.example.test")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, "tenant-a", "git_clone", 99); !errors.Is(err, ErrManagedConflict) {
		t.Fatalf("stale delete error = %v, want ErrManagedConflict", err)
	}
	if err := r.Delete(ctx, "tenant-a", "git_clone", created.Revision); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := r.GetManaged(ctx, "tenant-a", "git_clone"); !errors.Is(err, ErrToolConfigNotFound) {
		t.Fatalf("after delete get error = %v, want ErrToolConfigNotFound", err)
	}
	if err := r.Delete(ctx, "tenant-a", "git_clone", 1); !errors.Is(err, ErrToolConfigNotFound) {
		t.Fatalf("delete missing error = %v, want ErrToolConfigNotFound", err)
	}
}

func TestSaveRejectsInvalidDefinition(t *testing.T) {
	ctx := context.Background()
	r := newTestRegistry(t)
	if _, err := r.Save(ctx, "tenant-a", "alice", Definition{ToolName: "bad name!", Config: gitCloneConfig("a.test")}, 0); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("invalid tool name error = %v, want ErrInvalidDefinition", err)
	}
	if _, err := r.Save(ctx, "tenant-a", "alice", Definition{ToolName: "git_clone", Config: json.RawMessage(`[]`)}, 0); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("non-object config error = %v, want ErrInvalidDefinition", err)
	}
}
