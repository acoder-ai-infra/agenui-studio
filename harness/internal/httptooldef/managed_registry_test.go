package httptooldef

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
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "httptools.db"))
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

func sampleDefinition(name string) Definition {
	return Definition{
		ToolName:     name,
		DisplayName:  "Create Jira issue",
		Description:  "Create an issue",
		Method:       "POST",
		BaseURL:      "https://jira.example.test/rest/api/2/issue",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}}}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`),
		HeaderEnv:    map[string]string{"Authorization": "JIRA_TOKEN"},
		ResponseMode: "json",
		Write:        true,
		RiskLevel:    "high",
		TimeoutMS:    30000,
	}
}

func TestSaveCreatesUpdatesAndHashChanges(t *testing.T) {
	ctx := context.Background()
	r := newTestRegistry(t)
	created, err := r.Save(ctx, "tenant-a", "alice", sampleDefinition("jira_create"), 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Revision != 1 || created.Definition.TenantID != "tenant-a" || created.Definition.Scope != ScopeTenant {
		t.Fatalf("unexpected created row: %+v", created)
	}
	hash1 := created.Definition.Hash()

	if _, err := r.Save(ctx, "tenant-a", "alice", sampleDefinition("jira_create"), 0); !errors.Is(err, ErrManagedConflict) {
		t.Fatalf("duplicate create error = %v, want ErrManagedConflict", err)
	}

	updated := sampleDefinition("jira_create")
	updated.BaseURL = "https://jira.other.test/rest/api/2/issue"
	saved, err := r.Save(ctx, "tenant-a", "bob", updated, 1)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if saved.Revision != 2 {
		t.Fatalf("revision = %d, want 2", saved.Revision)
	}
	if saved.Definition.Hash() == hash1 {
		t.Fatal("hash must change when base_url changes")
	}
}

func TestGetForPrincipalTenantIsolation(t *testing.T) {
	ctx := context.Background()
	r := newTestRegistry(t)
	if _, err := r.Save(ctx, "tenant-a", "alice", sampleDefinition("jira_create"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetForPrincipal(ctx, "tenant-a", "jira_create"); err != nil {
		t.Fatalf("tenant-a get: %v", err)
	}
	if _, err := r.GetForPrincipal(ctx, "tenant-b", "jira_create"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant get error = %v, want ErrNotFound", err)
	}
	if _, err := r.GetForPrincipal(ctx, "", "jira_create"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty-tenant get error = %v, want ErrNotFound", err)
	}
}

func TestSaveRejectsInvalidDefinition(t *testing.T) {
	ctx := context.Background()
	r := newTestRegistry(t)
	cases := map[string]func(*Definition){
		"bad name":       func(d *Definition) { d.ToolName = "bad name!" },
		"bad method":     func(d *Definition) { d.Method = "TRACE" },
		"userinfo url":   func(d *Definition) { d.BaseURL = "https://u:p@jira.example.test/x" },
		"write not high": func(d *Definition) { d.RiskLevel = "low" },
		"get is write":   func(d *Definition) { d.Method = "GET" },
		"bad env":        func(d *Definition) { d.HeaderEnv = map[string]string{"Authorization": "not env!"} },
		"bad schema":     func(d *Definition) { d.InputSchema = json.RawMessage(`{not json`) },
	}
	for name, mutate := range cases {
		def := sampleDefinition("jira_create")
		mutate(&def)
		if _, err := r.Save(ctx, "tenant-a", "alice", def, 0); !errors.Is(err, ErrInvalidDefinition) {
			t.Fatalf("%s: error = %v, want ErrInvalidDefinition", name, err)
		}
	}
}

func TestDeleteCAS(t *testing.T) {
	ctx := context.Background()
	r := newTestRegistry(t)
	created, err := r.Save(ctx, "tenant-a", "alice", sampleDefinition("jira_create"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, "tenant-a", "jira_create", 99); !errors.Is(err, ErrManagedConflict) {
		t.Fatalf("stale delete error = %v, want ErrManagedConflict", err)
	}
	if err := r.Delete(ctx, "tenant-a", "jira_create", created.Revision); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := r.GetForPrincipal(ctx, "tenant-a", "jira_create"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}
