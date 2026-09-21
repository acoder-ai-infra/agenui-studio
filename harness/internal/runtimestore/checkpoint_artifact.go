package runtimestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

const checkpointSaveAttempts = 4

// ArtifactCheckpointStore keeps immutable runtime payloads in Artifact Store
// and atomically advances the durable checkpoint_meta pointer with store CAS.
type ArtifactCheckpointStore struct {
	Metadata  storage.CheckpointStore
	Artifacts *artifact.Store
	Clock     func() time.Time
}

func (s *ArtifactCheckpointStore) Load(ctx context.Context, key agentruntime.RuntimeCheckpointKey) (agentruntime.RuntimeCheckpointRecord, bool, error) {
	if err := validateCheckpointKey(key); err != nil {
		return agentruntime.RuntimeCheckpointRecord{}, false, err
	}
	if s == nil || s.Metadata == nil || s.Artifacts == nil {
		return agentruntime.RuntimeCheckpointRecord{}, false, agentruntime.ErrCheckpointStoreMissing
	}
	ctx = checkpointActorContext(ctx, key.Scope)
	pointer, err := s.Metadata.Get(ctx, key.CheckpointID)
	if storage.IsErrorCode(err, storage.ErrNotFound) {
		return agentruntime.RuntimeCheckpointRecord{}, false, nil
	}
	if err != nil {
		return agentruntime.RuntimeCheckpointRecord{}, false, err
	}
	if err := validateCheckpointPointer(pointer, key.Scope); err != nil {
		return agentruntime.RuntimeCheckpointRecord{}, false, err
	}
	if !pointer.ExpiresAt.IsZero() && !pointer.ExpiresAt.After(s.now()) {
		return agentruntime.RuntimeCheckpointRecord{}, false, nil
	}
	object, err := s.Artifacts.Get(ctx, pointer.StateRef, artifact.GetOptions{Purpose: artifact.PurposeReplay})
	if err != nil {
		return agentruntime.RuntimeCheckpointRecord{}, false, err
	}
	defer object.Content.Close()
	payload, err := io.ReadAll(object.Content)
	if err != nil {
		return agentruntime.RuntimeCheckpointRecord{}, false, err
	}
	metadata := object.Meta.Metadata
	revision, err := strconv.ParseInt(metadata["revision"], 10, 64)
	if err != nil || revision < 1 {
		return agentruntime.RuntimeCheckpointRecord{}, false, agentruntime.ErrCheckpointInvalid
	}
	hash := checkpointHash(payload)
	if metadata["payload_hash"] != hash || metadata["checkpoint_id"] != key.CheckpointID ||
		metadata["runtime_version"] != key.Scope.RuntimeVersion || metadata["adapter_version"] != key.Scope.AdapterVersion {
		return agentruntime.RuntimeCheckpointRecord{}, false, agentruntime.ErrCheckpointHashMismatch
	}
	return agentruntime.RuntimeCheckpointRecord{
		SchemaVersion: agentruntime.RuntimeCheckpointSchemaVersion, Scope: key.Scope, CheckpointID: key.CheckpointID,
		Payload: payload, PayloadHash: hash, SizeBytes: int64(len(payload)), BackendRef: pointer.StateRef,
		Revision: revision, CreatedAt: pointer.CreatedAt, UpdatedAt: object.Meta.CreatedAt, ExpiresAt: pointer.ExpiresAt,
	}, true, nil
}

func (s *ArtifactCheckpointStore) Save(ctx context.Context, record agentruntime.RuntimeCheckpointRecord) (agentruntime.RuntimeCheckpointRecord, error) {
	key := agentruntime.RuntimeCheckpointKey{Scope: record.Scope, CheckpointID: record.CheckpointID}
	if err := validateCheckpointKey(key); err != nil || len(record.Payload) == 0 {
		if err != nil {
			return agentruntime.RuntimeCheckpointRecord{}, err
		}
		return agentruntime.RuntimeCheckpointRecord{}, agentruntime.ErrCheckpointInvalid
	}
	if len(record.Payload) > agentruntime.DefaultMaxRuntimeCheckpointBytes {
		return agentruntime.RuntimeCheckpointRecord{}, fmt.Errorf("%w: size=%d max=%d", agentruntime.ErrCheckpointPayloadTooLarge, len(record.Payload), agentruntime.DefaultMaxRuntimeCheckpointBytes)
	}
	if s == nil || s.Metadata == nil || s.Artifacts == nil {
		return agentruntime.RuntimeCheckpointRecord{}, agentruntime.ErrCheckpointStoreMissing
	}
	ctx = checkpointActorContext(ctx, record.Scope)
	for attempt := 0; attempt < checkpointSaveAttempts; attempt++ {
		current, err := s.Metadata.Get(ctx, record.CheckpointID)
		missing := storage.IsErrorCode(err, storage.ErrNotFound)
		if err != nil && !missing {
			return agentruntime.RuntimeCheckpointRecord{}, err
		}
		revision := int64(1)
		expectedRef := ""
		createdAt := s.now()
		if !missing {
			if err := validateCheckpointPointer(current, record.Scope); err != nil {
				return agentruntime.RuntimeCheckpointRecord{}, err
			}
			expectedRef = current.StateRef
			createdAt = current.CreatedAt
			if previous, ok, loadErr := s.Load(ctx, key); loadErr != nil {
				return agentruntime.RuntimeCheckpointRecord{}, loadErr
			} else if ok {
				revision = previous.Revision + 1
			}
		}
		hash := checkpointHash(record.Payload)
		meta, err := s.Artifacts.Put(ctx, artifact.PutArtifactRequest{
			TenantID: record.Scope.TenantID, SessionID: record.Scope.SessionID, RunID: record.Scope.RunID,
			OwnerModule: artifact.OwnerModuleRuntime, OwnerID: record.CheckpointID,
			ArtifactType: artifact.ArtifactTypeCheckpointState, MimeType: "application/octet-stream",
			Visibility: artifact.VisibilityInternal, Content: bytes.NewReader(record.Payload),
			RetentionPolicy: artifact.RetentionCheckpointTTL, ExpiresAt: record.ExpiresAt,
			CreatedBy:      "runtime:" + string(record.Scope.Runtime),
			IdempotencyKey: fmt.Sprintf("runtime-checkpoint:%s:%d:%s", record.CheckpointID, revision, hash),
			Metadata: map[string]string{"checkpoint_id": record.CheckpointID, "payload_hash": hash,
				"revision": strconv.FormatInt(revision, 10), "runtime_version": record.Scope.RuntimeVersion,
				"adapter_version": record.Scope.AdapterVersion, "schema_version": agentruntime.RuntimeCheckpointSchemaVersion},
		})
		if err != nil {
			return agentruntime.RuntimeCheckpointRecord{}, err
		}
		pointer := &storage.CheckpointMeta{CheckpointID: record.CheckpointID, RunID: record.Scope.RunID,
			TenantID: record.Scope.TenantID, Runtime: string(record.Scope.Runtime), Type: "run", StateRef: meta.ArtifactRef,
			CreatedReason: "runtime_checkpoint", CreatedAt: createdAt, ExpiresAt: record.ExpiresAt}
		if missing {
			if err := s.Metadata.Create(ctx, pointer); err != nil {
				if storage.IsErrorCode(err, storage.ErrConflict) {
					continue
				}
				return agentruntime.RuntimeCheckpointRecord{}, err
			}
		} else {
			swapped, err := s.Metadata.CompareAndSwapState(ctx, record.CheckpointID, expectedRef, pointer)
			if err != nil {
				return agentruntime.RuntimeCheckpointRecord{}, err
			}
			if !swapped {
				continue
			}
		}
		record.SchemaVersion, record.PayloadHash = agentruntime.RuntimeCheckpointSchemaVersion, hash
		record.SizeBytes, record.BackendRef, record.Revision = int64(len(record.Payload)), meta.ArtifactRef, revision
		record.CreatedAt, record.UpdatedAt = createdAt, meta.CreatedAt
		return record, nil
	}
	return agentruntime.RuntimeCheckpointRecord{}, storage.NewError(storage.ErrCASMismatch, "checkpoint pointer changed concurrently")
}

func (s *ArtifactCheckpointStore) Delete(ctx context.Context, key agentruntime.RuntimeCheckpointKey) error {
	if err := validateCheckpointKey(key); err != nil {
		return err
	}
	ctx = checkpointActorContext(ctx, key.Scope)
	pointer, err := s.Metadata.Get(ctx, key.CheckpointID)
	if storage.IsErrorCode(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := validateCheckpointPointer(pointer, key.Scope); err != nil {
		return err
	}
	if err := s.Metadata.Delete(ctx, key.CheckpointID); err != nil {
		return err
	}
	if err := s.Artifacts.Delete(ctx, pointer.StateRef, artifact.DeleteReasonCleanup); err != nil && !artifact.IsErrorCode(err, artifact.ErrNotFound) {
		return err
	}
	return nil
}

func (s *ArtifactCheckpointStore) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

func validateCheckpointKey(key agentruntime.RuntimeCheckpointKey) error {
	if key.CheckpointID == "" || key.Scope.TenantID == "" || key.Scope.SessionID == "" || key.Scope.RunID == "" || key.Scope.Runtime == "" {
		return agentruntime.ErrCheckpointInvalid
	}
	return nil
}

func validateCheckpointPointer(pointer *storage.CheckpointMeta, scope agentruntime.RuntimeCheckpointScope) error {
	if pointer == nil || pointer.TenantID != scope.TenantID || pointer.RunID != scope.RunID || pointer.Runtime != string(scope.Runtime) || pointer.Type != "run" || pointer.StateRef == "" {
		return agentruntime.ErrCheckpointInvalid
	}
	return nil
}

func checkpointActorContext(ctx context.Context, scope agentruntime.RuntimeCheckpointScope) context.Context {
	return artifact.ContextWithActor(ctx, artifact.Actor{TenantID: scope.TenantID, SessionID: scope.SessionID, RunID: scope.RunID, Role: artifact.ActorRuntime})
}

func checkpointHash(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

var _ agentruntime.RuntimeCheckpointStore = (*ArtifactCheckpointStore)(nil)
