package artifact_test

import (
	"testing"

	. "github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

func TestBuildAndParseArtifactRef(t *testing.T) {
	ref := BuildRef("tenant-a", "sess-1", "run-1", "art-1")
	if ref != "artifact://tenants/tenant-a/sessions/sess-1/runs/run-1/art-1" {
		t.Fatalf("unexpected ref: %s", ref)
	}

	parsed, err := ParseRef(ref)
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	if parsed.TenantID != "tenant-a" || parsed.SessionID != "sess-1" || parsed.RunID != "run-1" || parsed.ArtifactID != "art-1" {
		t.Fatalf("unexpected parsed ref: %#v", parsed)
	}
}

func TestParseRefRejectsNonCanonicalForms(t *testing.T) {
	refs := []string{
		"artifact://tenants//sessions/s/runs/r/art",
		"artifact://tenants/t/sessions/s/runs/r/art?download=1",
		"artifact://tenants/t/sessions/s/runs/r/art#fragment",
		"artifact://user@tenants/t/sessions/s/runs/r/art",
		"artifact://tenants/../sessions/s/runs/r/art",
		"artifact://tenants/t%2Fother/sessions/s/runs/r/art",
		"artifact://tenants/t%5Cother/sessions/s/runs/r/art",
		"artifact://tenants/%2e%2e/sessions/s/runs/r/art",
		"artifact://tenants/tenant-a/sessions/sess-1/runs/run-1/%61rt_x",
		"artifact://tenants/%74enant-a/sessions/sess-1/runs/run-1/art_x",
	}
	for _, ref := range refs {
		if _, err := ParseRef(ref); !IsErrorCode(err, ErrInvalidArgument) {
			t.Fatalf("ref=%q err=%v", ref, err)
		}
	}
}

func TestBuildAndParseArtifactRefWithEscapedIdentifiers(t *testing.T) {
	ref := BuildRef("tenant a", "session a", "run a", "art a")
	parts, err := ParseRef(ref)
	if err != nil || parts.TenantID != "tenant a" || parts.ArtifactID != "art a" {
		t.Fatalf("parts=%#v err=%v", parts, err)
	}
}
