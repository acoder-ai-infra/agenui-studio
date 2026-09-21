package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"time"
)

type normalizedArtifactCandidate struct {
	tenantID        string
	userID          string
	sessionID       string
	runID           string
	stepID          string
	ownerModule     OwnerModule
	ownerID         string
	artifactType    ArtifactType
	mimeType        string
	name            string
	hash            string
	sizeBytes       int64
	visibility      Visibility
	preview         Preview
	retentionPolicy RetentionPolicy
	requestedExpiry time.Time
	createdBy       string
	derivedFrom     []ArtifactLineage
	metadata        map[string]string
}

func scopedIdempotencyKey(req PutArtifactRequest) string {
	if req.IdempotencyKey == "" {
		return ""
	}
	material := req.TenantID + "\x00" + req.SessionID + "\x00" + req.RunID + "\x00" + req.IdempotencyKey
	sum := sha256.Sum256([]byte(material))
	return "artifact:" + hex.EncodeToString(sum[:])
}

func normalizeArtifactCandidate(
	req PutArtifactRequest,
	hash string,
	sizeBytes int64,
	name string,
	mimeType string,
	preview Preview,
) normalizedArtifactCandidate {
	createdBy := req.CreatedBy
	if createdBy == "" {
		createdBy = string(req.OwnerModule) + ":" + req.OwnerID
	}
	return normalizedArtifactCandidate{
		tenantID:        req.TenantID,
		userID:          req.UserID,
		sessionID:       req.SessionID,
		runID:           req.RunID,
		stepID:          req.StepID,
		ownerModule:     req.OwnerModule,
		ownerID:         req.OwnerID,
		artifactType:    req.ArtifactType,
		mimeType:        mimeType,
		name:            name,
		hash:            hash,
		sizeBytes:       sizeBytes,
		visibility:      req.Visibility,
		preview:         preview,
		retentionPolicy: defaultRetention(req.RetentionPolicy, req.ArtifactType, req.Visibility),
		requestedExpiry: req.ExpiresAt,
		createdBy:       createdBy,
		derivedFrom:     normalizedLineage(req.DerivedFrom),
		metadata:        normalizedMetadata(req.Metadata),
	}
}

func (candidate normalizedArtifactCandidate) persistedExpiry(createdAt time.Time) time.Time {
	if !candidate.requestedExpiry.IsZero() {
		return candidate.requestedExpiry
	}
	return defaultExpiresAt(createdAt, candidate.retentionPolicy)
}

func sameArtifactSemantics(meta ArtifactMeta, candidate normalizedArtifactCandidate) bool {
	if meta.TenantID != candidate.tenantID || meta.UserID != candidate.userID ||
		meta.SessionID != candidate.sessionID || meta.RunID != candidate.runID || meta.StepID != candidate.stepID {
		return false
	}
	if meta.OwnerModule != candidate.ownerModule || meta.OwnerID != candidate.ownerID ||
		meta.ArtifactType != candidate.artifactType || meta.MimeType != candidate.mimeType || meta.Name != candidate.name {
		return false
	}
	if meta.Hash != candidate.hash || meta.SizeBytes != candidate.sizeBytes || meta.Visibility != candidate.visibility ||
		meta.RetentionPolicy != candidate.retentionPolicy || meta.CreatedBy != candidate.createdBy {
		return false
	}
	if candidate.requestedExpiry.IsZero() {
		if !meta.ExpiresAt.Equal(defaultExpiresAt(meta.CreatedAt, meta.RetentionPolicy)) {
			return false
		}
	} else if !meta.ExpiresAt.Equal(candidate.requestedExpiry) {
		return false
	}
	return reflect.DeepEqual(meta.Preview, candidate.preview) &&
		reflect.DeepEqual(meta.DerivedFrom, candidate.derivedFrom) &&
		reflect.DeepEqual(meta.Metadata, candidate.metadata)
}

func normalizedLineage(lineage []ArtifactLineage) []ArtifactLineage {
	if len(lineage) == 0 {
		return nil
	}
	return append([]ArtifactLineage(nil), lineage...)
}

func normalizedMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	copyMetadata := make(map[string]string, len(metadata))
	for key, value := range metadata {
		copyMetadata[key] = value
	}
	return copyMetadata
}
