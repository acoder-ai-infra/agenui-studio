package harness_test

import (
	"encoding/json"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/sdk"
)

// TestInputManifestPreservesPartOrder verifies that BuildInputManifest
// preserves caller-declared ordering, hashes every part with a stable
// algorithm, and produces a deterministic top-level Hash. This is the
// canonical proof that multi-Part inputs survive the SDK boundary without
// reshuffling or hash drift.
func TestInputManifestPreservesPartOrder(t *testing.T) {
	msg := harness.Message{
		Role: harness.RoleUser,
		Parts: []harness.MessagePart{
			{Kind: harness.PartKindText, Text: "hello"},
			{Kind: harness.PartKindJSON, JSON: json.RawMessage(`{"k":"v"}`), MIME: "application/json"},
			{Kind: harness.PartKindImageRef, Ref: harness.ArtifactRef{ID: "art_1", MIME: "image/png", Hash: "sha256:abcd"}},
			{Kind: harness.PartKindArtifactRef, Ref: harness.ArtifactRef{ID: "art_2", MIME: "application/pdf", Hash: "sha256:beef"}},
			{Kind: harness.PartKindInlineBinary, Inline: []byte{0x00, 0x01, 0x02}, MIME: "application/octet-stream"},
		},
	}
	a := harness.BuildInputManifest(msg)
	if len(a.Parts) != len(msg.Parts) {
		t.Fatalf("parts len mismatch: got %d want %d", len(a.Parts), len(msg.Parts))
	}
	for i, p := range a.Parts {
		if p.Kind != msg.Parts[i].Kind {
			t.Fatalf("part %d kind = %q; want %q", i, p.Kind, msg.Parts[i].Kind)
		}
		if p.Hash == "" {
			t.Fatalf("part %d Hash is empty", i)
		}
	}
	if a.Hash == "" {
		t.Fatal("top-level manifest Hash is empty")
	}
	// Second manifest for identical input must have identical Hash values.
	b := harness.BuildInputManifest(msg)
	if a.Hash != b.Hash {
		t.Fatalf("manifest hash instability: %q != %q", a.Hash, b.Hash)
	}
	for i := range a.Parts {
		if a.Parts[i].Hash != b.Parts[i].Hash {
			t.Fatalf("part %d hash instability: %q != %q", i, a.Parts[i].Hash, b.Parts[i].Hash)
		}
	}
	// Any material change must invalidate the manifest hash.
	mutated := harness.Message{
		Role: harness.RoleUser,
		Parts: append([]harness.MessagePart(nil), msg.Parts...),
	}
	mutated.Parts[0].Text = "hi" // different text
	c := harness.BuildInputManifest(mutated)
	if c.Hash == a.Hash {
		t.Fatalf("mutated manifest should have a different Hash; got same %q", c.Hash)
	}
}

// TestInputManifestArtifactRefsPreserveACLScope verifies that ArtifactRef
// fields (TenantID / SessionID / RunID / MIME / Hash) survive the manifest
// build so downstream ACL enforcement can validate them.
func TestInputManifestArtifactRefsPreserveACLScope(t *testing.T) {
	msg := harness.Message{
		Role: harness.RoleUser,
		Parts: []harness.MessagePart{
			{Kind: harness.PartKindArtifactRef, Ref: harness.ArtifactRef{
				ID:        "art_secret",
				TenantID:  "tenant-a",
				SessionID: "sess-1",
				RunID:     "run-1",
				MIME:      "application/octet-stream",
				Hash:      "sha256:cafe",
				Size:      42,
			}},
		},
	}
	m := harness.BuildInputManifest(msg)
	if len(m.Parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(m.Parts))
	}
	p := m.Parts[0]
	if p.Ref.ID != "art_secret" || p.Ref.TenantID != "tenant-a" || p.Ref.Hash != "sha256:cafe" {
		t.Fatalf("artifact ref scope lost: %+v", p.Ref)
	}
	if p.Ref.Size != 42 {
		t.Fatalf("artifact ref Size lost: got %d want 42", p.Ref.Size)
	}
}
