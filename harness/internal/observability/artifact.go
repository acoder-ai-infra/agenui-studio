package observability

type ArtifactDecision struct {
	UseArtifact bool
	Reason      string
	Preview     string
	Truncated   bool
	SizeBytes   int
}

type ArtifactPolicy struct {
	MaxInlineBytes  int
	MaxPreviewBytes int
}

func DecideArtifactPayload(payload string, policy ArtifactPolicy) ArtifactDecision {
	maxInline := policy.MaxInlineBytes
	if maxInline <= 0 {
		maxInline = 4096
	}
	maxPreview := policy.MaxPreviewBytes
	if maxPreview <= 0 {
		maxPreview = 1024
	}
	preview, truncated := PreviewText(payload, PreviewConfig{MaxBytes: maxPreview})
	decision := ArtifactDecision{
		UseArtifact: len(payload) > maxInline,
		Preview:     preview,
		Truncated:   truncated,
		SizeBytes:   len(payload),
	}
	if decision.UseArtifact {
		decision.Reason = "payload_too_large"
	}
	return decision
}
