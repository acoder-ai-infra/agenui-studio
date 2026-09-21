package ruleworker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalRevisionPublisherPersistsPointerActivatesProviderAndAdvancesBaseline(t *testing.T) {
	root := filepath.Join(t.TempDir(), "revisions")
	copyPublicDemoRevision(t, root, "demo-v1")
	pointerPath := filepath.Join(filepath.Dir(root), "current.json")
	var activated string
	publisher := LocalRevisionPointerPublisher{
		RevisionRoot: root,
		PointerPath:  pointerPath,
		Activate: func(_ context.Context, revisionID string) error {
			activated = revisionID
			return nil
		},
	}
	if err := publisher.Publish(context.Background(), "", "demo-v1", "index-hash"); err != nil {
		t.Fatal(err)
	}
	pointer, found, err := ReadLocalRevisionPointer(pointerPath)
	if err != nil || !found || pointer.RevisionID != "demo-v1" || pointer.IndexHash != "index-hash" {
		t.Fatalf("pointer=%+v found=%v err=%v", pointer, found, err)
	}
	if activated != "demo-v1" {
		t.Fatalf("activated=%q", activated)
	}
	baseline, cleanup, err := (LocalRevisionBaselineResolver{
		FallbackDir: filepath.Join(root, "demo-v1"), RevisionRoot: root, PointerPath: pointerPath,
	}).Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if baseline != filepath.Join(root, "demo-v1") {
		t.Fatalf("baseline=%q", baseline)
	}
}

func copyPublicDemoRevision(t *testing.T, root, revisionID string) {
	t.Helper()
	for _, name := range []string{
		"index.json", "manifest.json", "rules/foundation.md",
		"rules/card_structure.md",
		"layouts/summary-card.md", "layouts/information-action.md", "layouts/repeated-item-list.md",
		"elements/title.md", "elements/supporting_text.md",
		"elements/primary_action.md", "elements/repeated_item.md",
	} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "configs", "design-public", "revisions", "demo-v1", name))
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, revisionID, name)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
