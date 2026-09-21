package modelgateway

import (
	"bytes"
	"context"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

type OutputArtifactWriter interface {
	WriteOutput(ctx context.Context, req ModelRequest, target ModelTarget, text string) (string, error)
}

type ArtifactOutputWriter struct {
	Store artifact.ArtifactStore
}

func (w ArtifactOutputWriter) WriteOutput(ctx context.Context, req ModelRequest, target ModelTarget, text string) (string, error) {
	if w.Store == nil || text == "" {
		return "", nil
	}
	tc := req.Trace
	tenantID := tc.TenantID
	if tenantID == "" {
		tenantID = "default"
	}
	sessionID := tc.SessionID
	if sessionID == "" {
		sessionID = "sessionless"
	}
	runID := tc.RunID
	if runID == "" {
		runID = req.RequestID
	}
	if _, ok := artifact.ActorFromContext(ctx); !ok {
		ctx = artifact.ContextWithActor(ctx, artifact.Actor{
			TenantID: tenantID, UserID: tc.UserID, SessionID: sessionID, RunID: runID,
			AgentID: req.AgentID, Role: artifact.ActorRuntime,
		})
	}
	meta, err := w.Store.Put(ctx, artifact.PutArtifactRequest{
		TenantID:        tenantID,
		UserID:          tc.UserID,
		SessionID:       sessionID,
		RunID:           runID,
		OwnerModule:     artifact.OwnerModuleModelGateway,
		OwnerID:         target.Provider + ":" + target.Model,
		ArtifactType:    artifact.ArtifactTypeFinalResult,
		MimeType:        "text/plain; charset=utf-8",
		Name:            "model-output.txt",
		Visibility:      artifact.VisibilityInternal,
		Content:         bytes.NewBufferString(text),
		PreviewHint:     artifact.PreviewHint{MaxBytes: 512},
		RetentionPolicy: artifact.RetentionRunTTL,
		CreatedBy:       "modelgateway",
		Metadata: map[string]string{
			"request_id": req.RequestID,
			"provider":   target.Provider,
			"model":      target.Model,
		},
	})
	if err != nil {
		return "", err
	}
	return meta.ArtifactRef, nil
}
