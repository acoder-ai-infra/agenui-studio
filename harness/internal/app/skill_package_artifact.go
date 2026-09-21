package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/skill"
)

const (
	skillPackageSessionID = "skill-packages"
	globalSkillArtifactID = "harness-global-skills"
)

type skillPackageArtifactStore struct {
	artifacts artifact.ArtifactStore
}

func newSkillPackageArtifactStore(artifacts artifact.ArtifactStore) skill.PackageObjectStore {
	return &skillPackageArtifactStore{artifacts: artifacts}
}

func (s *skillPackageArtifactStore) PutPackage(ctx context.Context, object skill.PackageObject) (string, error) {
	if s == nil || s.artifacts == nil {
		return "", errors.New("skill package artifact store is not configured")
	}
	identity := skillPackageIdentity(object.TenantID, object.SkillID, object.Version)
	tenantID := skillArtifactTenant(object.TenantID)
	artifactCtx := artifact.ContextWithActor(ctx, artifact.Actor{TenantID: tenantID, Role: artifact.ActorAudit})
	meta, err := s.artifacts.Put(artifactCtx, artifact.PutArtifactRequest{
		ArtifactID:      "art_skillpkg_" + identity[:32],
		TenantID:        tenantID,
		SessionID:       skillPackageSessionID,
		RunID:           "skill_" + identity[:32],
		OwnerModule:     artifact.OwnerModuleSkill,
		OwnerID:         object.SkillID + "@" + object.Version,
		ArtifactType:    artifact.ArtifactTypeFile,
		MimeType:        "application/zip",
		Name:            object.SkillID + "-" + object.Version + ".zip",
		Visibility:      artifact.VisibilityRestricted,
		Content:         bytes.NewReader(object.Archive),
		RetentionPolicy: artifact.RetentionAuditTTL,
		CreatedBy:       "skill-management",
		IdempotencyKey:  "skill-package:" + identity,
		Metadata: map[string]string{
			"skill_id": object.SkillID,
			"version":  object.Version,
		},
	})
	if err != nil {
		if artifact.IsErrorCode(err, artifact.ErrConflict) {
			return "", fmt.Errorf("%w: %s@%s", skill.ErrVersionConflict, object.SkillID, object.Version)
		}
		return "", err
	}
	return meta.ArtifactRef, nil
}

func (s *skillPackageArtifactStore) GetPackage(ctx context.Context, tenantID, artifactRef string) ([]byte, error) {
	if s == nil || s.artifacts == nil {
		return nil, errors.New("skill package artifact store is not configured")
	}
	artifactCtx := artifact.ContextWithActor(ctx, artifact.Actor{TenantID: skillArtifactTenant(tenantID), Role: artifact.ActorAudit})
	object, err := s.artifacts.Get(artifactCtx, artifactRef, artifact.GetOptions{Purpose: artifact.PurposeReplay})
	if err != nil {
		return nil, err
	}
	content, readErr := io.ReadAll(object.Content)
	closeErr := object.Content.Close()
	if readErr != nil || closeErr != nil {
		return nil, fmt.Errorf("read skill package artifact: %w", errors.Join(readErr, closeErr))
	}
	return content, nil
}

func skillPackageIdentity(values ...string) string {
	hash := sha256.New()
	for _, value := range values {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(value))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func skillArtifactTenant(tenantID string) string {
	if tenantID == "" {
		return globalSkillArtifactID
	}
	return tenantID
}

var _ skill.PackageObjectStore = (*skillPackageArtifactStore)(nil)
