package context

import "testing"

func TestMessagePartsFromAttachmentsPreservesTextAndTypedReferences(t *testing.T) {
	parts := MessagePartsFromAttachments("照这个生成", []AttachmentFact{
		{Name: "reference.jpg", MimeType: "image/jpeg", Type: "image", ArtifactRef: "artifact://image-1"},
		{Name: "spec.pdf", MimeType: "application/pdf", Type: "file", ArtifactRef: "artifact://file-1"},
	})
	if len(parts) != 3 || parts[0].Kind != "text" || parts[0].Text != "照这个生成" {
		t.Fatalf("text part missing: %#v", parts)
	}
	if parts[1].Kind != "image_ref" || parts[1].ArtifactRef != "artifact://image-1" || parts[1].Filename != "reference.jpg" {
		t.Fatalf("image projection mismatch: %#v", parts[1])
	}
	if parts[2].Kind != "file_ref" || parts[2].ArtifactRef != "artifact://file-1" {
		t.Fatalf("file projection mismatch: %#v", parts[2])
	}
}

func TestMessagePartsFromAttachmentsDoesNotCreateTextOnlyMultipart(t *testing.T) {
	if parts := MessagePartsFromAttachments("hello", []AttachmentFact{{Name: "missing-ref.png", MimeType: "image/png"}}); parts != nil {
		t.Fatalf("metadata-only attachment must not replace plain content: %#v", parts)
	}
}
