package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type artifactContextSnapshotStore struct {
	artifacts artifact.ArtifactStore
}

const maxContextSnapshotBytes = 4 << 20

func (s artifactContextSnapshotStore) Save(ctx context.Context, snapshot contextpkg.Snapshot) (string, error) {
	if s.artifacts == nil || snapshot.ID == "" || snapshot.SessionID == "" || snapshot.RunID == "" {
		return "", errors.New("context snapshot artifact dependencies and identity are required")
	}
	tc := observability.MustTraceContext(ctx)
	if tc.TenantID == "" || tc.UserID == "" {
		return "", errors.New("context snapshot tenant and user identity are required")
	}
	ref := artifact.BuildRef(tc.TenantID, snapshot.SessionID, snapshot.RunID, snapshot.ID)
	snapshot.ID = ref
	snapshot.SchemaVersion = contextpkg.SnapshotSchemaVersion
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return "", fmt.Errorf("marshal context snapshot: %w", err)
	}
	if len(payload) > maxContextSnapshotBytes {
		return "", errors.New("context snapshot exceeds maximum size")
	}
	parts, err := artifact.ParseRef(ref)
	if err != nil {
		return "", err
	}
	actorCtx := artifact.ContextWithActor(ctx, artifact.Actor{
		TenantID: tc.TenantID, UserID: tc.UserID, SessionID: snapshot.SessionID,
		RunID: snapshot.RunID, AgentID: tc.AgentID, Role: artifact.ActorContextEngine,
	})
	meta, err := s.artifacts.Put(actorCtx, artifact.PutArtifactRequest{
		ArtifactID: parts.ArtifactID, TenantID: tc.TenantID, UserID: tc.UserID,
		SessionID: snapshot.SessionID, RunID: snapshot.RunID,
		OwnerModule: artifact.OwnerModuleContextEngine, OwnerID: snapshot.RunID,
		ArtifactType: artifact.ArtifactTypeContextSnapshot, MimeType: "application/json",
		Name: "context-snapshot.json", Visibility: artifact.VisibilityInternal,
		Content: bytes.NewReader(payload), RetentionPolicy: artifact.RetentionSessionTTL,
		CreatedBy: "context_engine", IdempotencyKey: snapshot.RunID + ":context_snapshot:" + snapshot.ContentHash,
		Metadata: map[string]string{"content_hash": snapshot.ContentHash, "schema_version": snapshot.SchemaVersion},
	})
	if err != nil {
		return "", err
	}
	if meta == nil || meta.ArtifactRef != ref {
		return "", errors.New("context snapshot artifact ref mismatch")
	}
	return ref, nil
}

func (s artifactContextSnapshotStore) Load(ctx context.Context, ref string) (contextpkg.Snapshot, error) {
	if s.artifacts == nil || ref == "" {
		return contextpkg.Snapshot{}, contextpkg.ErrSnapshotNotFound
	}
	tc := observability.MustTraceContext(ctx)
	actorCtx := artifact.ContextWithActor(ctx, artifact.Actor{
		TenantID: tc.TenantID, UserID: tc.UserID, SessionID: tc.SessionID,
		RunID: tc.RunID, AgentID: tc.AgentID, Role: artifact.ActorContextEngine,
	})
	object, err := s.artifacts.Get(actorCtx, ref, artifact.GetOptions{Purpose: artifact.PurposeModelContext})
	if err != nil {
		return contextpkg.Snapshot{}, err
	}
	defer object.Content.Close()
	if object.Meta.ArtifactType != artifact.ArtifactTypeContextSnapshot || object.Meta.MimeType != "application/json" {
		return contextpkg.Snapshot{}, errors.New("artifact is not a context snapshot")
	}
	if object.Meta.SizeBytes > maxContextSnapshotBytes {
		return contextpkg.Snapshot{}, errors.New("context snapshot exceeds maximum size")
	}
	payload, err := io.ReadAll(io.LimitReader(object.Content, maxContextSnapshotBytes+1))
	if err != nil {
		return contextpkg.Snapshot{}, err
	}
	if len(payload) > maxContextSnapshotBytes {
		return contextpkg.Snapshot{}, errors.New("context snapshot exceeds maximum size")
	}
	var snapshot contextpkg.Snapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return contextpkg.Snapshot{}, fmt.Errorf("decode context snapshot: %w", err)
	}
	if snapshot.SchemaVersion != contextpkg.SnapshotSchemaVersion || snapshot.ID != ref ||
		snapshot.SessionID != object.Meta.SessionID || snapshot.RunID != object.Meta.RunID ||
		snapshot.ContentHash == "" || object.Meta.Metadata["content_hash"] != snapshot.ContentHash {
		return contextpkg.Snapshot{}, errors.New("context snapshot artifact identity or hash mismatch")
	}
	return snapshot, nil
}

var _ contextpkg.SnapshotStore = artifactContextSnapshotStore{}
