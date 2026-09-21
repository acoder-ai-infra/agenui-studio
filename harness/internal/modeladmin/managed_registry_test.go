package modeladmin

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	_ "modernc.org/sqlite"
)

func newTestRegistry(t *testing.T) *SQLManagedRegistry {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "modeladmin.db"))
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

func sampleDefinition(id string) ProviderDefinition {
	return ProviderDefinition{
		ID: id, DisplayName: "通义千问", Version: "v1", Protocol: "openai_compatible",
		BaseURL: "https://dashscope.example.test/v1", APIKeyEnv: "HARNESS_TEST_KEY",
		Models: []string{"qwen-max"}, IsDefault: true, DefaultModel: "qwen-max",
	}
}

func TestManagedModelRegistryCASAndTenantIsolation(t *testing.T) {
	ctx := context.Background()
	registry := newTestRegistry(t)

	a, err := registry.Save(ctx, "tenant-a", "alice", sampleDefinition("dashscope"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if a.Revision != 1 || !a.Definition.IsDefault || a.Definition.TenantID != "tenant-a" {
		t.Fatalf("first save = %#v", a)
	}
	// Duplicate create loses (already exists) → conflict.
	if _, err := registry.Save(ctx, "tenant-a", "alice", sampleDefinition("dashscope"), 0); !errors.Is(err, ErrManagedConflict) {
		t.Fatalf("duplicate create error = %v", err)
	}
	// Stale-revision update → conflict.
	if _, err := registry.Save(ctx, "tenant-a", "alice", sampleDefinition("dashscope"), 99); !errors.Is(err, ErrManagedConflict) {
		t.Fatalf("stale update error = %v", err)
	}
	// Correct-revision update bumps revision.
	updated := sampleDefinition("dashscope")
	updated.DisplayName = "改名"
	next, err := registry.Save(ctx, "tenant-a", "alice", updated, a.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if next.Revision != 2 || next.Definition.DisplayName != "改名" {
		t.Fatalf("update = %#v", next)
	}
	// Same id in another tenant is isolated.
	if _, err := registry.Save(ctx, "tenant-b", "bob", sampleDefinition("dashscope"), 0); err != nil {
		t.Fatal(err)
	}
	list, err := registry.List(ctx, "tenant-a")
	if err != nil || len(list) != 1 {
		t.Fatalf("tenant-a list = %#v, err=%v", list, err)
	}
}

func TestManagedModelRegistryDeleteAndFingerprint(t *testing.T) {
	ctx := context.Background()
	registry := newTestRegistry(t)

	if _, emptyCount, err := registry.TenantFingerprint(ctx, "tenant-a"); err != nil || emptyCount != 0 {
		t.Fatalf("empty fingerprint = %d, err=%v", emptyCount, err)
	}
	saved, err := registry.Save(ctx, "tenant-a", "alice", sampleDefinition("dashscope"), 0)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, count, err := registry.TenantFingerprint(ctx, "tenant-a")
	if err != nil || count != 1 || fingerprint == "" {
		t.Fatalf("fingerprint = (%q,%d), err=%v", fingerprint, count, err)
	}
	// Delete with wrong revision → conflict.
	if err := registry.Delete(ctx, "tenant-a", "dashscope", 99); !errors.Is(err, ErrManagedConflict) {
		t.Fatalf("stale delete error = %v", err)
	}
	if err := registry.Delete(ctx, "tenant-a", "dashscope", saved.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.GetManaged(ctx, "tenant-a", "dashscope"); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("get after delete error = %v", err)
	}
	// Delete missing → not found.
	if err := registry.Delete(ctx, "tenant-a", "ghost", 1); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("delete missing error = %v", err)
	}
	_, count, _ = registry.TenantFingerprint(ctx, "tenant-a")
	if count != 0 {
		t.Fatalf("fingerprint count after delete = %d", count)
	}
}

func TestManagedModelRegistryFingerprintChangesOnSameIDDeleteRecreate(t *testing.T) {
	ctx := context.Background()
	registry := newTestRegistry(t)
	first := sampleDefinition("dashscope")
	saved, err := registry.Save(ctx, "tenant-a", "alice", first, 0)
	if err != nil {
		t.Fatal(err)
	}
	before, _, err := registry.TenantFingerprint(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Delete(ctx, "tenant-a", first.ID, saved.Revision); err != nil {
		t.Fatal(err)
	}
	second := sampleDefinition("dashscope")
	second.DisplayName = "recreated with changed executable definition"
	if _, err := registry.Save(ctx, "tenant-a", "bob", second, 0); err != nil {
		t.Fatal(err)
	}
	after, _, err := registry.TenantFingerprint(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if before == "" || after == "" || before == after {
		t.Fatalf("same-id revision-1 recreation did not change fingerprint: before=%q after=%q", before, after)
	}
}

func TestManagedModelRegistryRejectsInvalid(t *testing.T) {
	ctx := context.Background()
	registry := newTestRegistry(t)

	bad := sampleDefinition("dashscope")
	bad.BaseURL = "not-a-url"
	if _, err := registry.Save(ctx, "tenant-a", "alice", bad, 0); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("bad base_url error = %v", err)
	}
	noEnv := sampleDefinition("dashscope")
	noEnv.APIKeyEnv = ""
	if _, err := registry.Save(ctx, "tenant-a", "alice", noEnv, 0); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("missing api_key_env error = %v", err)
	}
	nonAnthropicAffinity := sampleDefinition("dashscope")
	nonAnthropicAffinity.PromptCacheSessionAffinity = true
	if _, err := registry.Save(ctx, "tenant-a", "alice", nonAnthropicAffinity, 0); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("non-anthropic prompt cache session affinity error = %v", err)
	}
	ordinaryAnthropic := ProviderDefinition{
		ID: "ordinary-anthropic", Version: "v1", Protocol: "anthropic",
		BaseURL: "https://api.anthropic.com", APIKeyEnv: "HARNESS_ANTHROPIC_KEY",
		Models: []string{"claude-sonnet"}, PromptCacheSessionAffinity: true,
	}
	if _, err := registry.Save(ctx, "tenant-a", "alice", ordinaryAnthropic, 0); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("ordinary anthropic prompt cache session affinity error = %v", err)
	}
	mislabeledPublic := ordinaryAnthropic
	mislabeledPublic.ID = "mislabeled-public"
	mislabeledPublic.ProviderKind = modelgateway.ProviderKindSessionAffinity
	if _, err := registry.Save(ctx, "tenant-a", "alice", mislabeledPublic, 0); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("public anthropic mislabeled as company gateway error = %v", err)
	}
	// mock protocol needs neither url nor key.
	mockDef := ProviderDefinition{ID: "mock", Version: "v1", Protocol: "mock", IsDefault: true, DefaultModel: "mock-model"}
	if _, err := registry.Save(ctx, "tenant-a", "alice", mockDef, 0); err != nil {
		t.Fatalf("mock provider save = %v", err)
	}
}

func TestManagedModelRegistryRoundTripsPromptCacheSessionAffinity(t *testing.T) {
	ctx := context.Background()
	registry := newTestRegistry(t)
	definition := ProviderDefinition{
		ID: "anthropic-cache", Version: "v1", Protocol: "anthropic",
		BaseURL: "http://localhost/open_api/anthropic", APIKeyEnv: "HARNESS_ANTHROPIC_KEY",
		ProviderKind: modelgateway.ProviderKindSessionAffinity,
		Models:       []string{"claude-sonnet"}, PromptCacheSessionAffinity: true, IsDefault: true, DefaultModel: "claude-sonnet",
	}

	saved, err := registry.Save(ctx, "tenant-a", "alice", definition, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Definition.PromptCacheSessionAffinity || saved.Definition.ProviderKind != modelgateway.ProviderKindSessionAffinity {
		t.Fatalf("saved definition = %#v", saved.Definition)
	}

	got, err := registry.GetManaged(ctx, "tenant-a", definition.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Definition.PromptCacheSessionAffinity || got.Definition.ProviderKind != modelgateway.ProviderKindSessionAffinity {
		t.Fatalf("get definition = %#v", got.Definition)
	}

	listed, err := registry.List(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || !listed[0].Definition.PromptCacheSessionAffinity || listed[0].Definition.ProviderKind != modelgateway.ProviderKindSessionAffinity {
		t.Fatalf("list = %#v", listed)
	}
}

func TestManagedModelRegistrySwitchesDefaultAtomically(t *testing.T) {
	ctx := context.Background()
	registry := newTestRegistry(t)
	first, err := registry.Save(ctx, "tenant-a", "alice", ProviderDefinition{
		ID: "first", Version: "v1", Protocol: "mock", IsDefault: true, DefaultModel: "m1",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.Save(ctx, "tenant-a", "alice", ProviderDefinition{
		ID: "second", Version: "v1", Protocol: "mock",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	secondDefinition := second.Definition
	secondDefinition.IsDefault = true
	secondDefinition.DefaultModel = "m2"
	second, err = registry.Save(ctx, "tenant-a", "bob", secondDefinition, second.Revision)
	if err != nil {
		t.Fatal(err)
	}
	providers, err := registry.List(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	assertExactlyOneDefault(t, providers, "second")
	for _, provider := range providers {
		if provider.Definition.ID == "first" {
			first = provider
		}
	}
	if first.Definition.IsDefault || first.Revision != 2 {
		t.Fatalf("old default was not atomically demoted: %#v", first)
	}

	ordinaryEdit := first.Definition
	ordinaryEdit.DisplayName = "edited non-default"
	first, err = registry.Save(ctx, "tenant-a", "carol", ordinaryEdit, first.Revision)
	if err != nil {
		t.Fatalf("ordinary non-default edit lost compatibility: %v", err)
	}
	removeDefault := second.Definition
	removeDefault.IsDefault = false
	if _, err := registry.Save(ctx, "tenant-a", "bob", removeDefault, second.Revision); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("save removed the only default: %v", err)
	}
	if err := registry.Delete(ctx, "tenant-a", "second", second.Revision); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("delete removed the only default while providers remained: %v", err)
	}

	firstDefinition := first.Definition
	firstDefinition.IsDefault = true
	firstDefinition.DefaultModel = "m1"
	if _, err := registry.Save(ctx, "tenant-a", "carol", firstDefinition, first.Revision); err != nil {
		t.Fatal(err)
	}
	second, err = registry.GetManaged(ctx, "tenant-a", "second")
	if err != nil {
		t.Fatal(err)
	}
	if second.Definition.IsDefault {
		t.Fatalf("replacement switch did not demote second: %#v", second)
	}
	if err := registry.Delete(ctx, "tenant-a", "second", second.Revision); err != nil {
		t.Fatalf("delete non-default: %v", err)
	}
	providers, err = registry.List(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	assertExactlyOneDefault(t, providers, "first")
}

func TestManagedModelRegistryPromotesFirstProviderToDefault(t *testing.T) {
	ctx := context.Background()
	registry := newTestRegistry(t)
	saved, err := registry.Save(ctx, "tenant-a", "legacy-client", ProviderDefinition{
		ID: "first", Version: "v1", Protocol: "mock", Models: []string{"m1"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Definition.IsDefault || saved.Definition.DefaultModel != "m1" {
		t.Fatalf("first provider was not promoted deterministically: %#v", saved.Definition)
	}

	second, err := registry.Save(ctx, "tenant-a", "legacy-client", ProviderDefinition{
		ID: "second", Version: "v1", Protocol: "mock", Models: []string{"m2"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if second.Definition.IsDefault {
		t.Fatalf("later non-default create unexpectedly switched routing: %#v", second.Definition)
	}
	providers, err := registry.List(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	assertExactlyOneDefault(t, providers, "first")
}

func TestManagedModelRegistryConcurrentDefaultSwitchesAcrossConnections(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared-modeladmin.db")
	openDB := func() *sql.DB {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout=10000`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	dbA := openDB()
	if err := ApplySQLiteSchema(ctx, dbA); err != nil {
		t.Fatal(err)
	}
	dbB := openDB()
	registryA := NewSQLManagedRegistry(dbA)
	registryB := NewSQLManagedRegistry(dbB)
	if _, err := registryA.Save(ctx, "tenant-a", "setup", ProviderDefinition{
		ID: "first", Version: "v1", Protocol: "mock", IsDefault: true, DefaultModel: "m1",
	}, 0); err != nil {
		t.Fatal(err)
	}
	second, err := registryA.Save(ctx, "tenant-a", "setup", ProviderDefinition{ID: "second", Version: "v1", Protocol: "mock"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	third, err := registryA.Save(ctx, "tenant-a", "setup", ProviderDefinition{ID: "third", Version: "v1", Protocol: "mock"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	second.Definition.IsDefault, second.Definition.DefaultModel = true, "m2"
	third.Definition.IsDefault, third.Definition.DefaultModel = true, "m3"

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, operation := range []func() error{
		func() error {
			_, err := registryA.Save(ctx, "tenant-a", "instance-a", second.Definition, second.Revision)
			return err
		},
		func() error {
			_, err := registryB.Save(ctx, "tenant-a", "instance-b", third.Definition, third.Revision)
			return err
		},
	} {
		wg.Add(1)
		go func(operation func() error) {
			defer wg.Done()
			<-start
			errs <- operation()
		}(operation)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent default switch: %v", err)
		}
	}
	providers, err := registryA.List(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	defaults := 0
	for _, provider := range providers {
		if provider.Definition.IsDefault {
			defaults++
		}
	}
	if defaults != 1 {
		t.Fatalf("concurrent switches committed %d defaults: %#v", defaults, providers)
	}
}

func TestManagedModelRegistryConcurrentFirstDefaultsAcrossConnections(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared-empty-modeladmin.db")
	openDB := func() *sql.DB {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout=10000`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	dbA := openDB()
	if err := ApplySQLiteSchema(ctx, dbA); err != nil {
		t.Fatal(err)
	}
	registryA := NewSQLManagedRegistry(dbA)
	registryB := NewSQLManagedRegistry(openDB())

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, operation := range []func() error{
		func() error {
			_, err := registryA.Save(ctx, "brand-new-tenant", "instance-a", ProviderDefinition{
				ID: "first-a", Version: "v1", Protocol: "mock", IsDefault: true, DefaultModel: "m1",
			}, 0)
			return err
		},
		func() error {
			_, err := registryB.Save(ctx, "brand-new-tenant", "instance-b", ProviderDefinition{
				ID: "first-b", Version: "v1", Protocol: "mock", IsDefault: true, DefaultModel: "m2",
			}, 0)
			return err
		},
	} {
		wg.Add(1)
		go func(operation func() error) {
			defer wg.Done()
			<-start
			errs <- operation()
		}(operation)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent first default: %v", err)
		}
	}
	providers, err := registryA.List(ctx, "brand-new-tenant")
	if err != nil {
		t.Fatal(err)
	}
	defaults := 0
	for _, provider := range providers {
		if provider.Definition.IsDefault {
			defaults++
		}
	}
	if len(providers) != 2 || defaults != 1 {
		t.Fatalf("concurrent first creates committed providers=%d defaults=%d: %#v", len(providers), defaults, providers)
	}
}

func assertExactlyOneDefault(t *testing.T, providers []ManagedProvider, wantID string) {
	t.Helper()
	var defaults []string
	for _, provider := range providers {
		if provider.Definition.IsDefault {
			defaults = append(defaults, provider.Definition.ID)
		}
	}
	if len(defaults) != 1 || defaults[0] != wantID {
		t.Fatalf("defaults=%v, want [%s]; providers=%#v", defaults, wantID, providers)
	}
}
