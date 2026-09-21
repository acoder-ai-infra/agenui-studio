package storage

import "context"

// ContextAttachmentSnapshot is a storage-layer DTO for facts that must be
// frozen into a ContextSnapshot without making storage depend on Context.
type ContextAttachmentSnapshot struct {
	Name        string `json:"name"`
	MimeType    string `json:"mime_type"`
	SizeBytes   int64  `json:"size_bytes"`
	ArtifactRef string `json:"artifact_ref,omitempty"`
	Type        string `json:"type,omitempty"`
}

// ContextSnapshotBuilder is a port (dependency inversion) for the Context Engine.
// The interface is defined here so that RunService can invoke context building
// before dispatching Runtime.Run (D5), while storage does NOT import
// internal/context. The real implementation is injected at wiring time.
type ContextSnapshotBuilder interface {
	// Build generates a context snapshot for the run and returns its artifact
	// ref. It runs before Runtime.Run executes; failure drives the run into a
	// failed/retryable state (fact-first: the Run record already exists).
	Build(ctx context.Context, req SnapshotBuildRequest) (ref string, err error)
}

// SnapshotBuildRequest carries the identifiers the Context Engine needs to bind
// the snapshot to this run.
type SnapshotBuildRequest struct {
	TenantID    string
	SessionID   string
	TurnID      string
	RunID       string
	MessageID   string
	AgentID     string
	TraceID     string
	Attachments []ContextAttachmentSnapshot
}
