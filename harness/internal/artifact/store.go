package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact/downloadtoken"
)

type StoreConfig struct {
	ObjectStore    ObjectStore
	MetadataStore  MetadataStore
	Clock          Clock
	MaxObjectBytes int64
	// DownloadCodec, when set, lets CreateDownloadURL mint tokens that
	// OpenDownload can redeem statelessly. The same codec must be installed on
	// the object store so issued and redeemed tokens share a key.
	DownloadCodec *downloadtoken.Codec
}

type Store struct {
	objectStore    ObjectStore
	metadataStore  MetadataStore
	purgeStore     PurgeStore
	purgeWorker    *PurgeWorker
	clock          Clock
	maxObjectBytes int64
	downloadCodec  *downloadtoken.Codec
}

type downloadCodecObjectStore interface {
	SetDownloadCodec(*downloadtoken.Codec)
	DownloadCodec() *downloadtoken.Codec
}

func NewStore(config StoreConfig) *Store {
	clock := config.Clock
	if clock == nil {
		clock = realClock{}
	}
	maxObjectBytes := config.MaxObjectBytes
	if maxObjectBytes <= 0 {
		maxObjectBytes = 32 << 20
	}
	store := &Store{
		objectStore:    config.ObjectStore,
		metadataStore:  config.MetadataStore,
		clock:          clock,
		maxObjectBytes: maxObjectBytes,
		downloadCodec:  config.DownloadCodec,
	}
	if objectStore, ok := config.ObjectStore.(downloadCodecObjectStore); ok && config.DownloadCodec != nil {
		objectStore.SetDownloadCodec(config.DownloadCodec)
	}
	if purgeStore, ok := config.MetadataStore.(PurgeStore); ok {
		store.purgeStore = purgeStore
		store.purgeWorker = &PurgeWorker{Objects: config.ObjectStore, Jobs: purgeStore, Clock: clock}
	}
	return store
}

func NewProductionStore(config StoreConfig) (*Store, error) {
	store := NewStore(config)
	if err := store.ValidateProduction(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) ValidateProduction() error {
	if s == nil || s.objectStore == nil || s.metadataStore == nil || s.purgeStore == nil {
		return errorf(ErrInvalidArgument, "production artifact store requires object, metadata and purge stores")
	}
	if s.downloadCodec == nil {
		return errorf(ErrInvalidArgument, "production artifact store requires a download token codec")
	}
	codecStore, ok := s.objectStore.(downloadCodecObjectStore)
	if !ok || codecStore.DownloadCodec() != s.downloadCodec {
		return errorf(ErrInvalidArgument, "production object store must issue tokens with the configured download codec")
	}
	if dependency, ok := s.objectStore.(ProductionDependency); !ok || !dependency.ProductionReady() {
		return errorf(ErrInvalidArgument, "object store must declare production readiness")
	}
	if dependency, ok := s.metadataStore.(ProductionDependency); !ok || !dependency.ProductionReady() {
		return errorf(ErrInvalidArgument, "metadata store must declare transactional production readiness")
	}
	return nil
}

func (s *Store) Put(ctx context.Context, req PutArtifactRequest) (*ArtifactMeta, error) {
	if err := s.validatePutRequest(req); err != nil {
		return nil, err
	}
	if err := validatePutScope(ctx, req); err != nil {
		return nil, err
	}
	if err := s.validateLineage(ctx, req); err != nil {
		return nil, err
	}
	data, hash, size, err := s.readContent(req.Content)
	if err != nil {
		return nil, err
	}
	name := sanitizeArtifactName(req.Name)
	mimeType := req.MimeType
	preview := buildPreview(req.Visibility, mimeType, name, hash, data, req.PreviewHint)
	candidate := normalizeArtifactCandidate(req, hash, size, name, mimeType, preview)
	idempotencyKey := scopedIdempotencyKey(req)
	if idempotencyKey != "" {
		existing, err := s.metadataStore.GetByIdempotencyKey(ctx, idempotencyKey)
		if err == nil {
			if err := validateArtifactMeta(existing, artifactMetaExpectation{
				expectedBackend: s.objectStore.Backend(),
				requireBackend:  true,
			}); err != nil {
				return nil, err
			}
			if !sameArtifactSemantics(*existing, candidate) {
				return nil, errorf(ErrConflict, "idempotency key already used for different artifact")
			}
			return existing, nil
		}
		if !IsErrorCode(err, ErrNotFound) {
			return nil, err
		}
	}
	now := s.clock.Now()
	artifactID := req.ArtifactID
	if artifactID == "" {
		artifactID = "art_" + ulid.Make().String()
	}
	ref := BuildRef(req.TenantID, req.SessionID, req.RunID, artifactID)
	storageKey, err := buildStorageKey(req.TenantID, req.SessionID, req.RunID, artifactID)
	if err != nil {
		return nil, err
	}
	if req.ArtifactID != "" {
		// A caller-supplied ArtifactID is a logical identity, not an object-store
		// reservation. Concurrent creators therefore upload to distinct attempt
		// keys and let the metadata CAS choose the only authoritative object.
		storageKey = buildAttemptStorageKey(storageKey, artifactID)
	}
	if req.ArtifactID != "" {
		existing, getErr := s.metadataStore.GetByRef(ctx, ref)
		if getErr == nil {
			if !sameArtifactSemantics(*existing, candidate) {
				return nil, errorf(ErrConflict, "artifact_id already used for different artifact")
			}
			return existing, nil
		}
		if !IsErrorCode(getErr, ErrNotFound) {
			return nil, getErr
		}
	}
	info, err := s.objectStore.Put(ctx, storageKey, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if err := validateObjectInfo(info, s.objectStore.Backend(), storageKey, size); err != nil {
		return nil, s.compensatePut(ctx, storageKey, err)
	}
	meta := ArtifactMeta{
		ArtifactID:      artifactID,
		ArtifactRef:     ref,
		TenantID:        candidate.tenantID,
		UserID:          candidate.userID,
		SessionID:       candidate.sessionID,
		RunID:           candidate.runID,
		StepID:          candidate.stepID,
		OwnerModule:     candidate.ownerModule,
		OwnerID:         candidate.ownerID,
		ArtifactType:    candidate.artifactType,
		MimeType:        candidate.mimeType,
		Name:            candidate.name,
		SizeBytes:       candidate.sizeBytes,
		Hash:            candidate.hash,
		Visibility:      candidate.visibility,
		StorageBackend:  s.objectStore.Backend(),
		StorageKey:      storageKey,
		Preview:         candidate.preview,
		RetentionPolicy: candidate.retentionPolicy,
		ExpiresAt:       candidate.persistedExpiry(now),
		CreatedBy:       candidate.createdBy,
		CreatedAt:       now,
		Status:          ArtifactStatusReady,
		DerivedFrom:     candidate.derivedFrom,
		SchemaVersion:   ArtifactMetaSchemaVersion,
		Metadata:        candidate.metadata,
	}
	created, err := s.metadataStore.Create(ctx, cloneArtifactMetaForValidation(meta), idempotencyKey)
	if err != nil {
		return nil, s.compensatePut(ctx, storageKey, err)
	}
	if err := validateArtifactMeta(created, artifactMetaExpectation{
		expectedBackend: s.objectStore.Backend(),
		requireBackend:  true,
	}); err != nil {
		return nil, s.compensatePut(ctx, storageKey, err)
	}
	if created.ArtifactRef != meta.ArtifactRef {
		if idempotencyKey == "" {
			return nil, s.compensatePut(ctx, storageKey,
				errorf(ErrConflict, "metadata store returned a different artifact ref"))
		}
		if cleanupErr := s.objectStore.Delete(ctx, storageKey); cleanupErr != nil {
			return nil, errors.Join(
				errorf(ErrConflict, "duplicate artifact object cleanup failed"),
				fmt.Errorf("object cleanup: %w", cleanupErr),
			)
		}
		if !sameArtifactSemantics(*created, candidate) {
			return nil, errorf(ErrConflict, "idempotency key already used for different artifact")
		}
		return created, nil
	}
	if err := validateCreatedArtifactMeta(created, &meta); err != nil {
		// With an explicit ref plus idempotency key, a concurrent writer may own
		// this shared object key. Never compensate that key after Create: accept
		// only the already-validated immutable winner or report a conflict.
		if idempotencyKey != "" && created.ArtifactRef == meta.ArtifactRef {
			if sameArtifactSemantics(*created, candidate) && created.Status == ArtifactStatusReady {
				return created, nil
			}
			return nil, errorf(ErrConflict, "idempotent artifact race produced different metadata")
		}
		return nil, s.compensatePut(ctx, storageKey, err)
	}
	return created, nil
}

func (s *Store) compensatePut(ctx context.Context, key string, primary error) error {
	if cleanupErr := s.objectStore.Delete(ctx, key); cleanupErr != nil {
		return errors.Join(primary, fmt.Errorf("object cleanup: %w", cleanupErr))
	}
	return primary
}

func (s *Store) Get(ctx context.Context, ref string, opts GetOptions) (*ArtifactObject, error) {
	if err := validatePurpose(opts.Purpose); err != nil {
		return nil, err
	}
	actor, err := requireActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireObjectAndMetadataStores(); err != nil {
		return nil, err
	}
	if err := validateRequestedRefScope(actor, ref); err != nil {
		return nil, err
	}
	meta, err := s.metadataStore.GetByRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := validateArtifactMeta(meta, artifactMetaExpectation{
		expectedRef:     ref,
		requireRef:      true,
		expectedBackend: s.objectStore.Backend(),
		requireBackend:  true,
	}); err != nil {
		return nil, err
	}
	if err := authorize(ctx, *meta, opts.Purpose); err != nil {
		return nil, err
	}
	if meta.Status == ArtifactStatusDeleted {
		return nil, errorf(ErrDeleted, "artifact deleted: %s", ref)
	}
	if err := validateReadableObjectSize(meta.SizeBytes, s.maxObjectBytes); err != nil {
		return nil, err
	}
	content, err := s.objectStore.Get(ctx, meta.StorageKey)
	if err != nil {
		if content != nil {
			if closeErr := content.Close(); closeErr != nil {
				return nil, errors.Join(err, fmt.Errorf("close object reader: %w", closeErr))
			}
		}
		return nil, err
	}
	if content == nil {
		return nil, errorf(ErrInvalidArgument, "object store returned nil reader")
	}
	data, err := readAndVerifyArtifactContent(content, *meta)
	if err != nil {
		return nil, err
	}
	return &ArtifactObject{Meta: *meta, Content: io.NopCloser(bytes.NewReader(data))}, nil
}

func (s *Store) Head(ctx context.Context, ref string) (*ArtifactMeta, error) {
	actor, err := requireActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireMetadataStore(); err != nil {
		return nil, err
	}
	if err := validateRequestedRefScope(actor, ref); err != nil {
		return nil, err
	}
	meta, err := s.metadataStore.GetByRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := validateArtifactMeta(meta, artifactMetaExpectation{
		expectedRef: ref,
		requireRef:  true,
	}); err != nil {
		return nil, err
	}
	if err := authorize(ctx, *meta, PurposeView); err != nil {
		return nil, err
	}
	if meta.Status == ArtifactStatusDeleted {
		return nil, errorf(ErrDeleted, "artifact deleted: %s", ref)
	}
	return meta, nil
}

func (s *Store) Delete(ctx context.Context, ref string, reason DeleteReason) error {
	if err := validateDeleteReason(reason); err != nil {
		return err
	}
	actor, err := requireActor(ctx)
	if err != nil {
		return err
	}
	if err := s.requireObjectAndMetadataStores(); err != nil {
		return err
	}
	if err := validateRequestedRefScope(actor, ref); err != nil {
		return err
	}
	meta, err := s.metadataStore.GetByRef(ctx, ref)
	if err != nil {
		return err
	}
	if err := validateArtifactMeta(meta, artifactMetaExpectation{
		expectedRef:     ref,
		requireRef:      true,
		expectedBackend: s.objectStore.Backend(),
		requireBackend:  true,
	}); err != nil {
		return err
	}
	if err := authorize(ctx, *meta, PurposeDownload); err != nil {
		return err
	}
	if meta.Status == ArtifactStatusDeleted {
		if s.purgeWorker != nil && (meta.PurgeStatus == PurgeStatusPending || meta.PurgeStatus == PurgeStatusRetrying) {
			return s.purgeWorker.Process(ctx, ref)
		}
		return errorf(ErrDeleted, "artifact deleted: %s", ref)
	}
	original := cloneArtifactMetaForValidation(*meta)
	deletedAt := s.clock.Now()
	var deleted *ArtifactMeta
	if s.purgeStore != nil {
		deleted, _, err = s.purgeStore.RequestPurge(ctx, ref, reason, deletedAt)
	} else {
		deleted, err = s.metadataStore.MarkDeleted(ctx, ref, reason, deletedAt)
	}
	if err != nil {
		return err
	}
	if err := validateArtifactMeta(deleted, artifactMetaExpectation{
		expectedRef:     ref,
		requireRef:      true,
		expectedBackend: s.objectStore.Backend(),
		requireBackend:  true,
	}); err != nil {
		return err
	}
	if err := validateDeletedArtifactMeta(deleted, &original, ref, reason, deletedAt); err != nil {
		return err
	}
	if s.purgeWorker != nil {
		return s.purgeWorker.Process(ctx, ref)
	}
	return s.objectStore.Delete(ctx, original.StorageKey)
}

func (s *Store) CreateDownloadURL(ctx context.Context, ref string, opts DownloadURLOptions) (*DownloadURL, error) {
	actor, err := requireActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireObjectAndMetadataStores(); err != nil {
		return nil, err
	}
	if err := validateRequestedRefScope(actor, ref); err != nil {
		return nil, err
	}
	meta, err := s.metadataStore.GetByRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := validateArtifactMeta(meta, artifactMetaExpectation{
		expectedRef:     ref,
		requireRef:      true,
		expectedBackend: s.objectStore.Backend(),
		requireBackend:  true,
	}); err != nil {
		return nil, err
	}
	if err := authorize(ctx, *meta, PurposeDownload); err != nil {
		return nil, err
	}
	if meta.Status == ArtifactStatusDeleted {
		return nil, errorf(ErrDeleted, "artifact deleted: %s", ref)
	}
	url, err := s.objectStore.CreateDownloadURL(ctx, meta.StorageKey, opts.TTL)
	if err != nil {
		return nil, err
	}
	if url == nil {
		return nil, errorf(ErrInvalidArgument, "object store returned nil download URL")
	}
	return url, nil
}

// OpenDownload redeems an encrypted download token minted by CreateDownloadURL
// and streams the artifact. The token is a bearer capability: possession proves
// the holder was authorized when the token was issued, so no actor is required.
// It still enforces the sealed expiry and re-checks the artifact's live status.
func (s *Store) OpenDownload(ctx context.Context, token string) (*ArtifactObject, error) {
	if s.downloadCodec == nil {
		return nil, errorf(ErrInvalidArgument, "download token redemption is not configured")
	}
	if err := s.requireObjectAndMetadataStores(); err != nil {
		return nil, err
	}
	storageKey, expiresAt, err := s.downloadCodec.Open(token)
	if err != nil {
		return nil, errorf(ErrPermissionDenied, "invalid download token")
	}
	// Token expiry is wall-clock based: the object store stamps it with
	// time.Now() at issuance (matching DownloadURL.ExpiresAt), so redemption
	// must compare against wall-clock too rather than the store's Clock, which
	// governs retention/metadata timestamps.
	if time.Now().After(expiresAt) {
		return nil, errorf(ErrPermissionDenied, "download token expired")
	}
	ref, err := refFromStorageKey(storageKey)
	if err != nil {
		return nil, err
	}
	meta, err := s.metadataStore.GetByRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := validateArtifactMeta(meta, artifactMetaExpectation{
		expectedRef:     ref,
		requireRef:      true,
		expectedBackend: s.objectStore.Backend(),
		requireBackend:  true,
	}); err != nil {
		return nil, err
	}
	if meta.Status == ArtifactStatusDeleted {
		return nil, errorf(ErrDeleted, "artifact deleted: %s", ref)
	}
	if err := validateReadableObjectSize(meta.SizeBytes, s.maxObjectBytes); err != nil {
		return nil, err
	}
	content, err := s.objectStore.Get(ctx, meta.StorageKey)
	if err != nil {
		if content != nil {
			if closeErr := content.Close(); closeErr != nil {
				return nil, errors.Join(err, fmt.Errorf("close object reader: %w", closeErr))
			}
		}
		return nil, err
	}
	if content == nil {
		return nil, errorf(ErrInvalidArgument, "object store returned nil reader")
	}
	data, err := readAndVerifyArtifactContent(content, *meta)
	if err != nil {
		return nil, err
	}
	return &ArtifactObject{Meta: *meta, Content: io.NopCloser(bytes.NewReader(data))}, nil
}

// refFromStorageKey rebuilds the canonical artifact ref from either the normal
// seven-segment key or an explicit-ID attempt key. The attempt identifier is a
// physical write detail and never changes the logical ArtifactRef.
func refFromStorageKey(key string) (string, error) {
	parts := strings.Split(key, "/")
	if len(parts) == 7 && parts[0] == "tenants" && parts[2] == "sessions" && parts[4] == "runs" {
		return BuildRef(parts[1], parts[3], parts[5], parts[6]), nil
	}
	if len(parts) != 9 || parts[0] != "tenants" || parts[2] != "sessions" || parts[4] != "runs" || parts[6] != "attempts" || !strings.HasPrefix(parts[7], "attempt_") {
		return "", errorf(ErrInvalidArgument, "invalid storage key")
	}
	return BuildRef(parts[1], parts[3], parts[5], parts[8]), nil
}

func (s *Store) List(ctx context.Context, query ListQuery) ([]ArtifactMeta, error) {
	if query.Visibility != "" && !isVisibility(query.Visibility) {
		return nil, errorf(ErrInvalidArgument, "invalid visibility")
	}
	actor, err := requireActor(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireMetadataStore(); err != nil {
		return nil, err
	}
	if err := bindListQueryToActor(actor, &query); err != nil {
		return nil, err
	}
	metas, err := s.metadataStore.List(ctx, query)
	if err != nil {
		return nil, err
	}
	authorized := make([]ArtifactMeta, 0, len(metas))
	purpose := PurposeView
	if actor.Role == ActorContextEngine {
		purpose = PurposeModelContext
	}
	for _, meta := range metas {
		if err := validateArtifactMeta(&meta, artifactMetaExpectation{}); err != nil {
			return nil, err
		}
		if err := authorize(ctx, meta, purpose); err != nil {
			if IsErrorCode(err, ErrPermissionDenied) {
				continue
			}
			return nil, err
		}
		authorized = append(authorized, meta)
	}
	return authorized, nil
}

func (s *Store) CleanupExpired(ctx context.Context, now time.Time) (*CleanupResult, error) {
	actor, err := requireActor(ctx)
	if err != nil {
		return nil, err
	}
	if actor.Role != ActorAudit {
		return nil, errorf(ErrPermissionDenied, "audit actor is required for cleanup")
	}
	if err := s.requireObjectAndMetadataStores(); err != nil {
		return nil, err
	}
	metas, err := s.metadataStore.List(ctx, ListQuery{
		TenantID:          actor.TenantID,
		SessionID:         actor.SessionID,
		RunID:             actor.RunID,
		ExpiredAtOrBefore: now,
		IncludeDeleted:    false,
	})
	if err != nil {
		return nil, err
	}
	result := &CleanupResult{}
	if s.purgeWorker != nil {
		completed, retryErr := s.purgeWorker.RunOnce(ctx, PurgeQuery{
			TenantID: actor.TenantID, SessionID: actor.SessionID, RunID: actor.RunID, Limit: defaultPurgeBatchSize,
		})
		result.Deleted += completed
		if retryErr != nil {
			return result, retryErr
		}
	}
	for _, meta := range metas {
		if err := validateArtifactMeta(&meta, artifactMetaExpectation{
			expectedBackend: s.objectStore.Backend(),
			requireBackend:  true,
		}); err != nil {
			return result, err
		}
		if err := authorize(ctx, meta, PurposeDownload); err != nil {
			if IsErrorCode(err, ErrPermissionDenied) {
				continue
			}
			return result, err
		}
		if meta.ExpiresAt.IsZero() || meta.ExpiresAt.After(now) {
			continue
		}
		result.Expired++
		original := cloneArtifactMetaForValidation(meta)
		var deleted *ArtifactMeta
		if s.purgeStore != nil {
			deleted, _, err = s.purgeStore.RequestPurge(ctx, original.ArtifactRef, DeleteReasonTTL, now)
		} else {
			deleted, err = s.metadataStore.MarkDeleted(ctx, original.ArtifactRef, DeleteReasonTTL, now)
		}
		if err != nil {
			return result, err
		}
		if err := validateArtifactMeta(deleted, artifactMetaExpectation{
			expectedRef:     original.ArtifactRef,
			requireRef:      true,
			expectedBackend: s.objectStore.Backend(),
			requireBackend:  true,
		}); err != nil {
			return result, err
		}
		if err := validateDeletedArtifactMeta(deleted, &original, original.ArtifactRef, DeleteReasonTTL, now); err != nil {
			return result, err
		}
		if s.purgeWorker != nil {
			err = s.purgeWorker.Process(ctx, original.ArtifactRef)
		} else {
			err = s.objectStore.Delete(ctx, original.StorageKey)
		}
		if err != nil {
			return result, err
		}
		result.Deleted++
	}
	return result, nil
}

func bindListQueryToActor(actor Actor, query *ListQuery) error {
	if err := bindListScopeField("tenant", actor.TenantID, &query.TenantID); err != nil {
		return err
	}
	if actor.Role == ActorUser || actor.Role == ActorRuntime || actor.SessionID != "" {
		if err := bindListScopeField("session", actor.SessionID, &query.SessionID); err != nil {
			return err
		}
	}
	if actor.Role == ActorUser || actor.Role == ActorRuntime || (actor.Role != ActorContextEngine && actor.RunID != "") {
		if err := bindListScopeField("run", actor.RunID, &query.RunID); err != nil {
			return err
		}
	}
	return nil
}

func bindListScopeField(name, actorValue string, queryValue *string) error {
	if *queryValue == "" {
		*queryValue = actorValue
		return nil
	}
	if *queryValue != actorValue {
		return errorf(ErrPermissionDenied, "%s scope mismatch", name)
	}
	return nil
}

func (s *Store) validatePutRequest(req PutArtifactRequest) error {
	if s.objectStore == nil {
		return errorf(ErrInvalidArgument, "object store is required")
	}
	if s.metadataStore == nil {
		return errorf(ErrInvalidArgument, "metadata store is required")
	}
	return validatePutContract(req)
}

func (s *Store) requireMetadataStore() error {
	if s == nil || s.metadataStore == nil {
		return errorf(ErrInvalidArgument, "metadata store is required")
	}
	return nil
}

func (s *Store) requireObjectAndMetadataStores() error {
	if s == nil || s.objectStore == nil {
		return errorf(ErrInvalidArgument, "object store is required")
	}
	return s.requireMetadataStore()
}

func (s *Store) validateLineage(ctx context.Context, req PutArtifactRequest) error {
	actor, err := requireActor(ctx)
	if err != nil {
		return err
	}
	for _, lineage := range req.DerivedFrom {
		if lineage.ArtifactRef == "" || lineage.Relation == "" {
			return errorf(ErrInvalidArgument, "lineage artifact_ref and relation are required")
		}
		parts, err := ParseRef(lineage.ArtifactRef)
		if err != nil {
			return err
		}
		if err := validateRefPartsScope(actor, parts); err != nil {
			return err
		}
		meta, err := s.metadataStore.GetByRef(ctx, lineage.ArtifactRef)
		if err != nil {
			return err
		}
		if err := validateArtifactMeta(meta, artifactMetaExpectation{
			expectedRef:     lineage.ArtifactRef,
			requireRef:      true,
			expectedBackend: s.objectStore.Backend(),
			requireBackend:  true,
		}); err != nil {
			return err
		}
		if meta.ArtifactRef != lineage.ArtifactRef || parts.TenantID != meta.TenantID ||
			parts.SessionID != meta.SessionID || parts.RunID != meta.RunID || parts.ArtifactID != meta.ArtifactID {
			return errorf(ErrConflict, "lineage artifact metadata identity mismatch")
		}
		if err := authorize(ctx, *meta, PurposeReplay); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) readContent(r io.Reader) ([]byte, string, int64, error) {
	limit, err := checkedReadLimit(s.maxObjectBytes)
	if err != nil {
		return nil, "", 0, err
	}
	limited := io.LimitReader(r, limit)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", 0, err
	}
	if int64(len(data)) > s.maxObjectBytes {
		return nil, "", 0, errorf(ErrTooLarge, "artifact exceeds max object size")
	}
	sum := sha256.Sum256(data)
	hash := "sha256:" + hex.EncodeToString(sum[:])
	return data, hash, int64(len(data)), nil
}

func buildStorageKey(tenantID, sessionID, runID, artifactID string) (string, error) {
	for _, identifier := range []struct {
		name  string
		value string
	}{
		{"tenant_id", tenantID},
		{"session_id", sessionID},
		{"run_id", runID},
		{"artifact_id", artifactID},
	} {
		if err := validateIdentifier(identifier.name, identifier.value); err != nil {
			return "", err
		}
	}
	return path.Join("tenants", tenantID, "sessions", sessionID, "runs", runID, artifactID), nil
}

func buildAttemptStorageKey(canonicalKey, artifactID string) string {
	return path.Join(path.Dir(canonicalKey), "attempts", "attempt_"+ulid.Make().String(), artifactID)
}

func defaultRetention(policy RetentionPolicy, artifactType ArtifactType, visibility Visibility) RetentionPolicy {
	if policy != "" {
		return policy
	}
	if artifactType == ArtifactTypeCheckpointState {
		return RetentionCheckpointTTL
	}
	if artifactType == ArtifactTypeDebugPayload || visibility == VisibilityDebug {
		return RetentionDebugShortTTL
	}
	if visibility == VisibilityUserVisible || artifactType == ArtifactTypeFinalResult {
		return RetentionSessionTTL
	}
	return RetentionRunTTL
}

func defaultExpiresAt(now time.Time, policy RetentionPolicy) time.Time {
	switch policy {
	case RetentionSessionTTL:
		return now.Add(30 * 24 * time.Hour)
	case RetentionDebugShortTTL:
		return now.Add(24 * time.Hour)
	case RetentionCheckpointTTL:
		return now.Add(7 * 24 * time.Hour)
	case RetentionAuditTTL:
		return time.Time{}
	case RetentionRunTTL:
		return now.Add(7 * 24 * time.Hour)
	default:
		return now.Add(7 * 24 * time.Hour)
	}
}

func (s *Store) String() string {
	if s == nil || s.objectStore == nil || s.metadataStore == nil {
		return "ArtifactStore(unconfigured)"
	}
	return fmt.Sprintf("ArtifactStore(object=%s)", s.objectStore.Backend())
}
