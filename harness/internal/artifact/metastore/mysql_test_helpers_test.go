package metastore

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

func roundTripMySQLPreview(t *testing.T, original artifact.Preview) (artifact.Preview, []byte) {
	t.Helper()
	payload, err := encodeMySQLPreview(original)
	if err != nil {
		t.Fatalf("encodeMySQLPreview() error = %v", err)
	}
	decoded, err := decodeMySQLPreview(payload)
	if err != nil {
		t.Fatalf("decodeMySQLPreview() error = %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Fatalf("Preview round trip = %#v, want %#v", decoded, original)
	}
	return decoded, payload
}

func requireMySQLCodecInvalidArgument(t *testing.T, err error) {
	t.Helper()
	if !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
		t.Fatalf("error = %v, want %s", err, artifact.ErrInvalidArgument)
	}
}

func portableMySQLMetaFixture() artifact.ArtifactMeta {
	createdAt := time.Now()
	expiresAt := createdAt.Add(2*time.Hour + 17*time.Nanosecond)
	deletedAt := createdAt.Add(3*time.Hour + 999999999*time.Nanosecond)
	return artifact.ArtifactMeta{
		ArtifactID:     "artifact-\x00\xff",
		ArtifactRef:    "artifact://tenant/session/run/artifact-\x00\xff",
		TenantID:       "tenant-\xff",
		UserID:         "user-\x00",
		SessionID:      "session-1",
		RunID:          "run-1",
		StepID:         "step-1",
		OwnerModule:    artifact.OwnerModule("owner-\xff"),
		OwnerID:        "owner-1",
		ArtifactType:   artifact.ArtifactType("type-\x00"),
		MimeType:       "application/octet-stream",
		Name:           "name-\xff",
		SizeBytes:      -7,
		Hash:           "hash-\x00",
		Visibility:     artifact.Visibility("visibility-alias"),
		StorageBackend: "backend-\xff",
		StorageKey:     "key-\x00",
		Preview: artifact.Preview{
			Text: "preview-\x00\xff",
			Fields: map[string]any{
				"nested": []any{int64(math.MinInt64), []string{"\x00", "\xff"}},
			},
			Truncated: true,
		},
		RetentionPolicy: artifact.RetentionPolicy("retention-alias"),
		ExpiresAt:       expiresAt,
		CreatedBy:       "creator-\xff",
		CreatedAt:       createdAt,
		Status:          artifact.ArtifactStatus("status-alias"),
		DerivedFrom: []artifact.ArtifactLineage{{
			ArtifactRef: "source-\x00\xff",
			Relation:    artifact.LineageRelation("relation-\xff"),
		}},
		SchemaVersion: "schema-\x00",
		DeletedAt:     deletedAt,
		DeleteReason:  artifact.DeleteReason("reason-\xff"),
		Metadata:      map[string]string{"key-\xff": "value-\x00"},
	}
}
