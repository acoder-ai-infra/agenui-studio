package skill

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestSkillConcurrentPublishIsAtomicAndIdempotent(t *testing.T) {
	service := newTestService(t)
	definition := testDefinition("route", "1.0.0")
	principal := Principal{TenantID: "tenant", AgentID: "publisher"}
	const count = 64
	var wg sync.WaitGroup
	created := 0
	var mu sync.Mutex
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, wasCreated, err := service.Publish(context.Background(), principal, definition, []byte("instructions"))
			if err != nil {
				t.Errorf("publish: %v", err)
			}
			if wasCreated {
				mu.Lock()
				created++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if created != 1 {
		t.Fatalf("created=%d, want 1", created)
	}
	_, _, err := service.Publish(context.Background(), principal, definition, []byte("different"))
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("expected immutable version conflict, got %v", err)
	}
}

func TestSkillResolutionEnforcesPrincipalAndPolicy(t *testing.T) {
	service := newTestService(t)
	definition := testDefinition("route", "1.0.0")
	definition.Policy.AllowedAgents = []string{"planner"}
	_, _, err := service.Publish(context.Background(), Principal{TenantID: "tenant", AgentID: "publisher"}, definition, []byte("instructions"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Resolve(context.Background(), Principal{TenantID: "tenant", AgentID: "other"}, Ref{ID: "route", Version: "1.0.0"})
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("expected policy denial, got %v", err)
	}
	resolution, err := service.Resolve(context.Background(), Principal{TenantID: "tenant", AgentID: "planner"}, Ref{ID: "route", Version: "1.0.0"})
	if err != nil || resolution.Instructions != "instructions" {
		t.Fatalf("authorized resolution failed: %#v %v", resolution, err)
	}
}

func TestSkillPreviewFileReturnsStoredSkillMarkdown(t *testing.T) {
	service := newTestService(t)
	principal := Principal{TenantID: "tenant", AgentID: "planner"}
	content := []byte("# Route\n\nUse the approved route.")
	record, _, err := service.Publish(context.Background(), principal, testDefinition("route", "1.0.0"), content)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.PreviewFile(context.Background(), principal, Ref{ID: "route", Version: "1.0.0"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Path != "SKILL.md" || preview.Content != string(content) || preview.ContentHash != record.ContentHash || preview.SizeBytes != int64(len(content)) {
		t.Fatalf("preview = %#v, record=%#v", preview, record)
	}
	if preview.MimeType != "text/markdown; charset=utf-8" {
		t.Fatalf("mime type = %q", preview.MimeType)
	}
}

func TestSkillPreviewFileListsPackageReferences(t *testing.T) {
	service := newTestService(t)
	principal := Principal{TenantID: "tenant", AgentID: "planner"}
	content := []byte("# Route\n\nRead references/policy.md.")
	files := []FileRecord{
		defaultFileRecord("SKILL.md", content),
		defaultFileRecord("references/policy.md", []byte("# Policy\n\nUse approved terms.")),
	}
	if _, _, err := service.PublishPackage(context.Background(), principal, testDefinition("route", "1.0.0"), content, files); err != nil {
		t.Fatal(err)
	}
	list, err := service.ListFiles(context.Background(), principal, Ref{ID: "route", Version: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Files) != 2 || list.Files[0].Path != "SKILL.md" || list.Files[1].Path != "references/policy.md" || list.Files[1].Content != nil {
		t.Fatalf("list files = %#v", list)
	}
	preview, err := service.PreviewFile(context.Background(), principal, Ref{ID: "route", Version: "1.0.0"}, "references/policy.md")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Path != "references/policy.md" || preview.Content != "# Policy\n\nUse approved terms." {
		t.Fatalf("reference preview = %#v", preview)
	}
}

func TestSkillPreviewFileRejectsUnsafeOrUnpersistedPaths(t *testing.T) {
	service := newTestService(t)
	principal := Principal{TenantID: "tenant", AgentID: "planner"}
	if _, _, err := service.Publish(context.Background(), principal, testDefinition("route", "1.0.0"), []byte("instructions")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PreviewFile(context.Background(), principal, Ref{ID: "route", Version: "1.0.0"}, "../secret"); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("path traversal error = %v", err)
	}
	if _, err := service.PreviewFile(context.Background(), principal, Ref{ID: "route", Version: "1.0.0"}, "references/details.md"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unpersisted asset error = %v", err)
	}
	if _, err := service.PreviewFile(context.Background(), Principal{TenantID: "other", AgentID: "planner"}, Ref{ID: "route", Version: "1.0.0"}, "SKILL.md"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant preview error = %v", err)
	}
}

func TestSkillResolutionSnapshotsDependencies(t *testing.T) {
	service := newTestService(t)
	principal := Principal{TenantID: "tenant", AgentID: "planner"}
	dependency := testDefinition("base", "1.0.0")
	root := testDefinition("route", "1.0.0")
	root.Dependencies.Skills = []Ref{{ID: "base", Version: "1.0.0"}}
	_, _, _ = service.Publish(context.Background(), principal, dependency, []byte("base"))
	_, _, _ = service.Publish(context.Background(), principal, root, []byte("route"))
	resolution, err := service.Resolve(context.Background(), principal, Ref{ID: "route", Version: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolution.Dependencies) != 1 || resolution.Dependencies[0].Snapshot.SkillID != "base" || resolution.Dependencies[0].Instructions != "base" {
		t.Fatalf("dependencies not frozen in resolution: %#v", resolution)
	}
}

func TestSkillResolutionDeduplicatesSharedDependencies(t *testing.T) {
	service := newTestService(t)
	principal := Principal{TenantID: "tenant", AgentID: "planner"}
	base := testDefinition("base", "1.0.0")
	left := testDefinition("left", "1.0.0")
	right := testDefinition("right", "1.0.0")
	root := testDefinition("root", "1.0.0")
	left.Dependencies.Skills = []Ref{{ID: "base", Version: "1.0.0"}}
	right.Dependencies.Skills = []Ref{{ID: "base", Version: "1.0.0"}}
	root.Dependencies.Skills = []Ref{{ID: "left", Version: "1.0.0"}, {ID: "right", Version: "1.0.0"}}
	for _, item := range []struct {
		definition Definition
		content    string
	}{{base, "base"}, {left, "left"}, {right, "right"}, {root, "root"}} {
		if _, _, err := service.Publish(context.Background(), principal, item.definition, []byte(item.content)); err != nil {
			t.Fatal(err)
		}
	}
	resolution, err := service.Resolve(context.Background(), principal, Ref{ID: "root", Version: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int)
	for _, dependency := range resolution.Dependencies {
		counts[dependency.Snapshot.SkillID]++
	}
	if counts["base"] != 1 || counts["left"] != 1 || counts["right"] != 1 || len(counts) != 3 {
		t.Fatalf("dependencies were not resolved once: %#v", counts)
	}
}

func TestSkillPublishRejectsOversizedContent(t *testing.T) {
	service, err := NewServiceWithOptions(NewInMemoryRepository(), nil, func() string { return "snapshot" }, ServiceOptions{MaxContentBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = service.Publish(context.Background(), Principal{TenantID: "tenant", AgentID: "publisher"}, testDefinition("route", "1.0.0"), []byte("12345"))
	if !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("oversized skill was accepted: %v", err)
	}
}

func TestSkillUpdateRequiresSameIDAndNewerVersion(t *testing.T) {
	base := Ref{ID: "route", Version: "1.2.0"}
	for _, test := range []struct {
		name       string
		definition Definition
		wantErr    bool
	}{
		{name: "newer", definition: testDefinition("route", "1.3.0")},
		{name: "newer with build metadata", definition: testDefinition("route", "1.3.0+build.2")},
		{name: "different id", definition: testDefinition("other", "1.3.0"), wantErr: true},
		{name: "same version", definition: testDefinition("route", "1.2.0"), wantErr: true},
		{name: "older version", definition: testDefinition("route", "1.1.9"), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateUpdate(base, test.definition)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateUpdate() error = %v, wantErr=%v", err, test.wantErr)
			}
		})
	}
	service := newTestService(t)
	principal := Principal{TenantID: "tenant", AgentID: "publisher"}
	if _, _, err := service.PublishUpdate(context.Background(), principal, base, testDefinition("route", "1.3.0"), []byte("new")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update without base error = %v", err)
	}
	if _, _, err := service.Publish(context.Background(), principal, testDefinition("route", "1.2.0"), []byte("base")); err != nil {
		t.Fatal(err)
	}
	if record, created, err := service.PublishUpdate(context.Background(), principal, base, testDefinition("route", "1.3.0"), []byte("new")); err != nil || !created || record.Definition.Version != "1.3.0" {
		t.Fatalf("publish update = %#v created=%v err=%v", record, created, err)
	}
}

func TestSkillRetireHidesDiscoveryButPreservesExactResolution(t *testing.T) {
	service := newTestService(t)
	principal := Principal{TenantID: "tenant", AgentID: "publisher"}
	definition := testDefinition("route", "1.0.0")
	if _, _, err := service.Publish(context.Background(), principal, definition, []byte("instructions")); err != nil {
		t.Fatal(err)
	}
	retirement, created, err := service.Retire(context.Background(), principal, Ref{ID: "route", Version: "1.0.0"})
	if err != nil || !created || retirement.SkillID != "route" || retirement.TenantID != "tenant" {
		t.Fatalf("retire = %#v created=%v err=%v", retirement, created, err)
	}
	if listed, err := service.List(context.Background(), principal); err != nil || len(listed) != 0 {
		t.Fatalf("list after retire = %#v err=%v", listed, err)
	}
	if resolution, err := service.Resolve(context.Background(), principal, Ref{ID: "route", Version: "1.0.0"}); err != nil || resolution.Instructions != "instructions" {
		t.Fatalf("historical resolve = %#v err=%v", resolution, err)
	}
	if repeated, created, err := service.Retire(context.Background(), principal, Ref{ID: "route", Version: "1.0.0"}); err != nil || created || repeated.RetiredAt != retirement.RetiredAt {
		t.Fatalf("repeated retire = %#v created=%v err=%v", repeated, created, err)
	}
	if _, _, err := service.Retire(context.Background(), Principal{TenantID: "other", AgentID: "publisher"}, Ref{ID: "route", Version: "1.0.0"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant retire error = %v", err)
	}
}

func TestSkillConcurrentRetireIsIdempotent(t *testing.T) {
	service := newTestService(t)
	principal := Principal{TenantID: "tenant", AgentID: "publisher"}
	definition := testDefinition("route", "1.0.0")
	if _, _, err := service.Publish(context.Background(), principal, definition, []byte("instructions")); err != nil {
		t.Fatal(err)
	}
	const count = 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	for index := 0; index < count; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, wasCreated, err := service.Retire(context.Background(), principal, Ref{ID: "route", Version: "1.0.0"})
			if err != nil {
				t.Errorf("retire: %v", err)
			}
			if wasCreated {
				mu.Lock()
				created++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if created != 1 {
		t.Fatalf("created=%d, want 1", created)
	}
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	var sequence int
	var mu sync.Mutex
	service, err := NewService(NewInMemoryRepository(), nil, func() string {
		mu.Lock()
		defer mu.Unlock()
		sequence++
		return fmt.Sprintf("skill-snapshot-%d", sequence)
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func testDefinition(id, version string) Definition {
	return Definition{
		ID: id, Version: version, TenantID: "tenant", InjectionStrategy: InjectSystem,
		Policy: Policy{Scope: ScopeTenant},
	}
}
