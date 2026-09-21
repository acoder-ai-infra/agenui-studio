package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"reflect"
	"strings"
	"time"
)

func validateObjectInfo(info *ObjectInfo, expectedBackend, expectedKey string, expectedSize int64) error {
	if info == nil {
		return errorf(ErrInvalidArgument, "object store returned nil object info")
	}
	if info.Backend == "" {
		return errorf(ErrInvalidArgument, "object store returned empty backend")
	}
	if info.Backend != expectedBackend {
		return errorf(ErrInvalidArgument, "object store returned unexpected backend")
	}
	if info.Key != expectedKey {
		return errorf(ErrInvalidArgument, "object store returned unexpected key")
	}
	if info.Size != expectedSize {
		return errorf(ErrInvalidArgument, "object store returned unexpected size")
	}
	return nil
}

type artifactMetaExpectation struct {
	expectedRef     string
	requireRef      bool
	expectedBackend string
	requireBackend  bool
}

func validateArtifactMeta(meta *ArtifactMeta, expectation artifactMetaExpectation) error {
	if meta == nil {
		return errorf(ErrInvalidArgument, "metadata store returned nil artifact metadata")
	}

	parts, err := parseCanonicalArtifactRef(meta.ArtifactRef)
	if err != nil {
		return err
	}
	if expectation.requireRef && meta.ArtifactRef != expectation.expectedRef {
		return errorf(ErrConflict, "metadata store returned unexpected artifact ref")
	}

	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "artifact_id", value: meta.ArtifactID},
		{name: "tenant_id", value: meta.TenantID},
		{name: "session_id", value: meta.SessionID},
		{name: "run_id", value: meta.RunID},
		{name: "owner_id", value: meta.OwnerID},
		{name: "storage_backend", value: meta.StorageBackend},
		{name: "storage_key", value: meta.StorageKey},
		{name: "created_by", value: meta.CreatedBy},
	} {
		if field.value == "" {
			return errorf(ErrInvalidArgument, "metadata store returned empty %s", field.name)
		}
	}

	if parts.TenantID != meta.TenantID || parts.SessionID != meta.SessionID ||
		parts.RunID != meta.RunID || parts.ArtifactID != meta.ArtifactID {
		return errorf(ErrConflict, "artifact metadata identity does not match artifact ref")
	}
	if !isOwnerModule(meta.OwnerModule) {
		return errorf(ErrInvalidArgument, "metadata store returned invalid owner_module")
	}
	if !isArtifactType(meta.ArtifactType) {
		return errorf(ErrInvalidArgument, "metadata store returned invalid artifact_type")
	}
	if _, _, err := mime.ParseMediaType(meta.MimeType); err != nil {
		return errorf(ErrInvalidArgument, "metadata store returned invalid mime_type")
	}
	if meta.SizeBytes < 0 {
		return errorf(ErrInvalidArgument, "metadata store returned negative size_bytes")
	}
	if !validSHA256(meta.Hash) {
		return errorf(ErrInvalidArgument, "metadata store returned invalid sha256 hash")
	}
	if !isVisibility(meta.Visibility) {
		return errorf(ErrInvalidArgument, "metadata store returned invalid visibility")
	}
	if !isRetentionPolicy(meta.RetentionPolicy) {
		return errorf(ErrInvalidArgument, "metadata store returned invalid retention_policy")
	}
	if !isArtifactStatus(meta.Status) {
		return errorf(ErrInvalidArgument, "metadata store returned invalid status")
	}
	switch meta.Status {
	case ArtifactStatusReady:
		if !meta.DeletedAt.IsZero() || meta.DeleteReason != "" || (meta.PurgeStatus != "" && meta.PurgeStatus != PurgeStatusNone) || !meta.PurgedAt.IsZero() {
			return errorf(ErrInvalidArgument, "metadata store returned deletion fields for ready artifact")
		}
	case ArtifactStatusDeleted:
		if meta.DeletedAt.IsZero() {
			return errorf(ErrInvalidArgument, "metadata store returned zero deleted_at for deleted artifact")
		}
		if err := validateDeleteReason(meta.DeleteReason); err != nil {
			return errorf(ErrInvalidArgument, "metadata store returned invalid delete_reason for deleted artifact")
		}
		if meta.PurgeStatus != "" && !isPurgeStatus(meta.PurgeStatus) {
			return errorf(ErrInvalidArgument, "metadata store returned invalid purge_status")
		}
		if meta.PurgeStatus == PurgeStatusPurged && meta.PurgedAt.IsZero() {
			return errorf(ErrInvalidArgument, "metadata store returned zero purged_at for purged artifact")
		}
		if meta.PurgeStatus != PurgeStatusPurged && !meta.PurgedAt.IsZero() {
			return errorf(ErrInvalidArgument, "metadata store returned purged_at before purge completion")
		}
	}
	if meta.CreatedAt.IsZero() {
		return errorf(ErrInvalidArgument, "metadata store returned zero created_at")
	}
	if meta.SchemaVersion != ArtifactMetaSchemaVersion {
		return errorf(ErrInvalidArgument, "metadata store returned invalid schema_version")
	}
	for _, lineage := range meta.DerivedFrom {
		if !isLineageRelation(lineage.Relation) {
			return errorf(ErrInvalidArgument, "metadata store returned invalid lineage relation")
		}
		if _, err := parseCanonicalArtifactRef(lineage.ArtifactRef); err != nil {
			return err
		}
	}

	storageRef, err := refFromStorageKey(meta.StorageKey)
	if err != nil {
		return errorf(ErrConflict, "artifact metadata storage key does not match identity")
	}
	if storageRef != meta.ArtifactRef {
		return errorf(ErrConflict, "artifact metadata storage key does not match identity")
	}
	if expectation.requireBackend && meta.StorageBackend != expectation.expectedBackend {
		return errorf(ErrConflict, "artifact metadata storage backend does not match object store")
	}
	return nil
}

func validateCreatedArtifactMeta(created, expected *ArtifactMeta) error {
	if !reflect.DeepEqual(created, expected) {
		return errorf(ErrConflict, "metadata store changed created artifact metadata")
	}
	return nil
}

func validateDeletedArtifactMeta(deleted, original *ArtifactMeta, ref string, reason DeleteReason, at time.Time) error {
	if deleted == nil || original == nil {
		return errorf(ErrInvalidArgument, "metadata store returned nil deleted artifact metadata")
	}
	expected := *original
	expected.ArtifactRef = ref
	expected.Status = ArtifactStatusDeleted
	expected.DeleteReason = reason
	expected.DeletedAt = at
	expected.PurgeStatus = PurgeStatusPending
	if !reflect.DeepEqual(deleted, &expected) {
		return errorf(ErrConflict, "metadata store returned inconsistent deleted artifact metadata")
	}
	return nil
}

func cloneArtifactMetaForValidation(meta ArtifactMeta) ArtifactMeta {
	cloned := meta
	if meta.DerivedFrom != nil {
		cloned.DerivedFrom = make([]ArtifactLineage, len(meta.DerivedFrom))
		copy(cloned.DerivedFrom, meta.DerivedFrom)
	}
	if meta.Metadata != nil {
		cloned.Metadata = make(map[string]string, len(meta.Metadata))
		for key, value := range meta.Metadata {
			cloned.Metadata[key] = value
		}
	}
	if meta.Preview.Fields != nil {
		cloned.Preview.Fields = make(map[string]any, len(meta.Preview.Fields))
		for key, value := range meta.Preview.Fields {
			cloned.Preview.Fields[key] = clonePreviewFieldValue(value)
		}
	}
	return cloned
}

func clonePreviewFieldValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(typed))
		for key, item := range typed {
			cloned[key] = clonePreviewFieldValue(item)
		}
		return cloned
	case map[string]string:
		cloned := make(map[string]string, len(typed))
		for key, item := range typed {
			cloned[key] = item
		}
		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = clonePreviewFieldValue(item)
		}
		return cloned
	case []string:
		cloned := make([]string, len(typed))
		copy(cloned, typed)
		return cloned
	default:
		return value
	}
}

func parseCanonicalArtifactRef(ref string) (RefParts, error) {
	parts, err := ParseRef(ref)
	if err != nil {
		return RefParts{}, err
	}
	if BuildRef(parts.TenantID, parts.SessionID, parts.RunID, parts.ArtifactID) != ref {
		return RefParts{}, errorf(ErrInvalidArgument, "non-canonical artifact ref")
	}
	return parts, nil
}

func validSHA256(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func isArtifactStatus(value ArtifactStatus) bool {
	switch value {
	case ArtifactStatusReady, ArtifactStatusDeleted, ArtifactStatusExpired:
		return true
	}
	return false
}

func isPurgeStatus(value PurgeStatus) bool {
	switch value {
	case PurgeStatusNone, PurgeStatusPending, PurgeStatusLeased, PurgeStatusRetrying, PurgeStatusPurged:
		return true
	}
	return false
}

func validateReadableObjectSize(size, maxObjectBytes int64) error {
	if size > maxObjectBytes {
		return errorf(ErrTooLarge, "artifact exceeds max object size")
	}
	_, err := checkedReadLimit(size)
	return err
}

func readAndVerifyArtifactContent(reader io.ReadCloser, meta ArtifactMeta) ([]byte, error) {
	limit, err := checkedReadLimit(meta.SizeBytes)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, limit))
	closeErr := reader.Close()
	if readErr != nil {
		primary := fmt.Errorf("read object content: %w", readErr)
		if closeErr != nil {
			return nil, errors.Join(primary, fmt.Errorf("close object reader: %w", closeErr))
		}
		return nil, primary
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close object reader: %w", closeErr)
	}
	if int64(len(data)) != meta.SizeBytes {
		return nil, errorf(ErrConflict, "object size does not match artifact metadata")
	}
	sum := sha256.Sum256(data)
	if "sha256:"+hex.EncodeToString(sum[:]) != meta.Hash {
		return nil, errorf(ErrConflict, "object hash does not match artifact metadata")
	}
	return data, nil
}

func checkedReadLimit(size int64) (int64, error) {
	if size == math.MaxInt64 {
		return 0, errorf(ErrTooLarge, "artifact read limit overflows")
	}
	return size + 1, nil
}
