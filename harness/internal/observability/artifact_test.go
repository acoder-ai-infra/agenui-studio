package observability

import "testing"

func TestDecideArtifactPayload(t *testing.T) {
	decision := DecideArtifactPayload("0123456789", ArtifactPolicy{
		MaxInlineBytes:  4,
		MaxPreviewBytes: 5,
	})

	if !decision.UseArtifact {
		t.Fatal("large payload should use artifact")
	}
	if decision.Reason != "payload_too_large" {
		t.Fatalf("unexpected reason: %s", decision.Reason)
	}
	if decision.Preview != "01234" || !decision.Truncated {
		t.Fatalf("unexpected preview: %#v", decision)
	}
}
