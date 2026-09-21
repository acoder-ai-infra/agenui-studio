package skill

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

type memoryPackageObjectStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
}

func newMemoryPackageObjectStore() *memoryPackageObjectStore {
	return &memoryPackageObjectStore{objects: make(map[string][]byte)}
}

func (s *memoryPackageObjectStore) PutPackage(_ context.Context, object PackageObject) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sum := sha256.Sum256([]byte(object.TenantID + "\x00" + object.SkillID + "\x00" + object.Version))
	ref := "package://" + hex.EncodeToString(sum[:])
	if existing, ok := s.objects[ref]; ok {
		if !bytes.Equal(existing, object.Archive) {
			return "", fmt.Errorf("%w: package object differs", ErrVersionConflict)
		}
		return ref, nil
	}
	s.objects[ref] = append([]byte(nil), object.Archive...)
	s.puts++
	return ref, nil
}

func (s *memoryPackageObjectStore) GetPackage(_ context.Context, _ string, ref string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	content, ok := s.objects[ref]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), content...), nil
}

func TestSQLRepositoryPersistsAndIsolatesTenantSkills(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "skills.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := ApplySQLiteSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := ApplySQLiteLifecycleSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := ApplySQLitePackageFileSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	packages := newMemoryPackageObjectStore()
	service, err := NewService(NewSQLRepository(db, packages), nil, func() string { return "snapshot" })
	if err != nil {
		t.Fatal(err)
	}
	definition := Definition{ID: "planner", Version: "1.0.0", TenantID: "tenant-a", InjectionStrategy: InjectOnDemand, Policy: Policy{Scope: ScopeTenant}}
	if _, created, err := service.Publish(context.Background(), Principal{TenantID: "tenant-a", AgentID: "agent"}, definition, []byte("instructions")); err != nil || !created {
		t.Fatalf("publish created=%v err=%v", created, err)
	}
	if _, err := service.Resolve(context.Background(), Principal{TenantID: "tenant-b", AgentID: "agent"}, Ref{ID: "planner", Version: "1.0.0"}); err != ErrNotFound {
		t.Fatalf("cross-tenant resolve error = %v", err)
	}
	listed, err := service.List(context.Background(), Principal{TenantID: "tenant-a", AgentID: "agent"})
	if err != nil || len(listed) != 1 {
		t.Fatalf("list = %#v err=%v", listed, err)
	}
}

func TestSQLRepositoryPersistsPackageFilesAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "skills-files.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplySQLiteSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := ApplySQLiteLifecycleSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := ApplySQLitePackageFileSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	packages := newMemoryPackageObjectStore()
	service, err := NewService(NewSQLRepository(db, packages), nil, func() string { return "snapshot" })
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{TenantID: "tenant-a", AgentID: "agent"}
	content := []byte("# Planner\n\nRead references/policy.md.")
	files := []FileRecord{
		defaultFileRecord("SKILL.md", content),
		defaultFileRecord("references/policy.md", []byte("# Policy\n\nDurable reference.")),
	}
	definition := Definition{ID: "planner", Version: "1.0.0", TenantID: "tenant-a", InjectionStrategy: InjectOnDemand, Policy: Policy{Scope: ScopeTenant}}
	if _, created, err := service.PublishPackage(context.Background(), principal, definition, content, files); err != nil || !created {
		t.Fatalf("publish package created=%v err=%v", created, err)
	}
	if _, created, err := service.PublishPackage(context.Background(), principal, definition, content, []FileRecord{files[1], files[0]}); err != nil || created {
		t.Fatalf("idempotent reordered package created=%v err=%v", created, err)
	}
	var persistedContent []byte
	var artifactRef string
	if err := db.QueryRow(`SELECT content FROM skill_versions WHERE tenant_id='tenant-a' AND skill_id='planner'`).Scan(&persistedContent); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT artifact_ref FROM skill_package_files WHERE tenant_id='tenant-a' AND skill_id='planner' LIMIT 1`).Scan(&artifactRef); err != nil {
		t.Fatal(err)
	}
	if len(persistedContent) != 0 || artifactRef == "" || packages.puts != 1 {
		t.Fatalf("database content bytes=%d artifact_ref=%q package puts=%d", len(persistedContent), artifactRef, packages.puts)
	}
	packages.mu.Lock()
	originalPackage := append([]byte(nil), packages.objects[artifactRef]...)
	packages.objects[artifactRef] = []byte("corrupt ZIP")
	packages.mu.Unlock()
	if _, err := service.PreviewFile(context.Background(), principal, Ref{ID: "planner", Version: "1.0.0"}, "references/policy.md"); !errors.Is(err, ErrContentIntegrity) {
		t.Fatalf("corrupt package preview error=%v, want content integrity failure", err)
	}
	packages.mu.Lock()
	packages.objects[artifactRef] = originalPackage
	packages.mu.Unlock()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := NewService(NewSQLRepository(reopened, packages), nil, func() string { return "restored-snapshot" })
	if err != nil {
		t.Fatal(err)
	}
	list, err := restored.ListFiles(context.Background(), principal, Ref{ID: "planner", Version: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Files) != 2 || list.Files[1].Path != "references/policy.md" {
		t.Fatalf("list files after restart = %#v", list)
	}
	preview, err := restored.PreviewFile(context.Background(), principal, Ref{ID: "planner", Version: "1.0.0"}, "references/policy.md")
	if err != nil || preview.Content != "# Policy\n\nDurable reference." {
		t.Fatalf("preview after restart = %#v err=%v", preview, err)
	}
}

func TestSQLRepositoryRequiresPackageObjectStore(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "skills.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ApplySQLiteSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := ApplySQLitePackageFileSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(NewSQLRepository(db, nil), nil, func() string { return "snapshot" })
	if err != nil {
		t.Fatal(err)
	}
	definition := Definition{ID: "planner", Version: "1.0.0", TenantID: "tenant-a", InjectionStrategy: InjectOnDemand, Policy: Policy{Scope: ScopeTenant}}
	_, _, err = service.Publish(context.Background(), Principal{TenantID: "tenant-a", AgentID: "publisher"}, definition, []byte("instructions"))
	if err == nil || !strings.Contains(err.Error(), "package object store is required") {
		t.Fatalf("publish without package store error=%v", err)
	}
}

func TestSQLRepositoryPersistsRetirementWithoutBreakingResolution(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "skills-retirement.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplySQLiteSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := ApplySQLiteLifecycleSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := ApplySQLitePackageFileSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	packages := newMemoryPackageObjectStore()
	service, err := NewService(NewSQLRepository(db, packages), nil, func() string { return "snapshot" })
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{TenantID: "tenant-a", AgentID: "publisher"}
	definition := Definition{ID: "planner", Version: "1.0.0", TenantID: "tenant-a", InjectionStrategy: InjectOnDemand, Policy: Policy{Scope: ScopeTenant}}
	if _, _, err := service.Publish(context.Background(), principal, definition, []byte("instructions")); err != nil {
		t.Fatal(err)
	}
	if _, created, err := service.Retire(context.Background(), principal, Ref{ID: "planner", Version: "1.0.0"}); err != nil || !created {
		t.Fatalf("retire created=%v err=%v", created, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := NewService(NewSQLRepository(reopened, packages), nil, func() string { return "restored-snapshot" })
	if err != nil {
		t.Fatal(err)
	}
	if listed, err := restored.List(context.Background(), principal); err != nil || len(listed) != 0 {
		t.Fatalf("list after restart = %#v err=%v", listed, err)
	}
	if resolution, err := restored.Resolve(context.Background(), principal, Ref{ID: "planner", Version: "1.0.0"}); err != nil || resolution.Instructions != "instructions" {
		t.Fatalf("resolve after restart = %#v err=%v", resolution, err)
	}
	if _, _, err := restored.Retire(context.Background(), Principal{TenantID: "tenant-b", AgentID: "publisher"}, Ref{ID: "planner", Version: "1.0.0"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant retire error = %v", err)
	}
}
