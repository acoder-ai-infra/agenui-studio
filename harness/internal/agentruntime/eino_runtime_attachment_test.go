package agentruntime

import (
	"testing"

	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
)

func TestInheritableTaskInputPartsOnlyForwardsImageReferences(t *testing.T) {
	input := []Message{{Role: "user", Parts: []contextpkg.ContentPart{
		{Kind: "text", Text: "参考这张图"},
		{Kind: "image_ref", ArtifactRef: "artifact-image", MIME: "image/png"},
		{Kind: "file_ref", ArtifactRef: "artifact-document", MIME: "application/pdf"},
		{Kind: "image_ref", MIME: "image/jpeg"},
	}}}

	parts := inheritableTaskInputParts(input)
	if len(parts) != 1 || parts[0].ArtifactRef != "artifact-image" || parts[0].Kind != "image_ref" {
		t.Fatalf("unexpected inheritable parts: %+v", parts)
	}
}
