package artifact_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/downloadtoken"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/objectstore"
)

func TestArtifactDeleteRecoversDurablePurgeAfterObjectFailure(t *testing.T) {
	metadata := metastore.NewMemory()
	deleteErr := errors.New("object store unavailable")
	objects := &failDeleteOnceObjectStore{ObjectStore: objectstore.NewMemory(), err: deleteErr}
	store := artifact.NewStore(artifact.StoreConfig{ObjectStore: objects, MetadataStore: metadata})
	ctx := artifact.ContextWithActor(context.Background(), artifact.Actor{
		TenantID: "tenant_1", UserID: "user_1", SessionID: "session_1", RunID: "run_1", Role: artifact.ActorRuntime,
	})
	meta, err := store.Put(ctx, artifact.PutArtifactRequest{
		TenantID: "tenant_1", UserID: "user_1", SessionID: "session_1", RunID: "run_1",
		OwnerModule: artifact.OwnerModuleToolGateway, OwnerID: "tool_1", ArtifactType: artifact.ArtifactTypeToolResult,
		MimeType: "application/json", Visibility: artifact.VisibilityInternal, Content: strings.NewReader(`{"ok":true}`),
		RetentionPolicy: artifact.RetentionRunTTL,
	})
	if err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	if err := store.Delete(ctx, meta.ArtifactRef, artifact.DeleteReasonCleanup); !errors.Is(err, deleteErr) {
		t.Fatalf("first delete error = %v", err)
	}
	job, err := metadata.GetPurgeJob(ctx, meta.ArtifactRef)
	if err != nil || job.Status != artifact.PurgeStatusRetrying || job.Attempts != 1 {
		t.Fatalf("recoverable job = %#v, error = %v", job, err)
	}
	if _, err := store.Get(ctx, meta.ArtifactRef, artifact.GetOptions{Purpose: artifact.PurposeView}); !artifact.IsErrorCode(err, artifact.ErrDeleted) {
		t.Fatalf("pending purge artifact remained readable: %v", err)
	}
	if err := store.Delete(ctx, meta.ArtifactRef, artifact.DeleteReasonCleanup); err != nil {
		t.Fatalf("retry delete: %v", err)
	}
	job, err = metadata.GetPurgeJob(ctx, meta.ArtifactRef)
	if err != nil || job.Status != artifact.PurgeStatusPurged {
		t.Fatalf("purged job = %#v, error = %v", job, err)
	}
	deleted, err := metadata.GetByRef(ctx, meta.ArtifactRef)
	if err != nil || deleted.PurgeStatus != artifact.PurgeStatusPurged || deleted.PurgedAt.IsZero() {
		t.Fatalf("purged metadata = %#v, error = %v", deleted, err)
	}
}

func TestPurgeWorkerLeasePreventsConcurrentOwnersAndExpires(t *testing.T) {
	metadata := metastore.NewMemory()
	objects := objectstore.NewMemory()
	store := artifact.NewStore(artifact.StoreConfig{ObjectStore: objects, MetadataStore: metadata})
	ctx := artifact.ContextWithActor(context.Background(), artifact.Actor{
		TenantID: "tenant_1", SessionID: "session_1", RunID: "run_1", Role: artifact.ActorRuntime,
	})
	meta, err := store.Put(ctx, artifact.PutArtifactRequest{
		TenantID: "tenant_1", SessionID: "session_1", RunID: "run_1", OwnerModule: artifact.OwnerModuleRuntime,
		OwnerID: "runtime_1", ArtifactType: artifact.ArtifactTypeFile, MimeType: "text/plain", Visibility: artifact.VisibilityInternal,
		Content: strings.NewReader("content"), RetentionPolicy: artifact.RetentionRunTTL,
	})
	if err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	now := time.Now()
	if _, _, err := metadata.RequestPurge(ctx, meta.ArtifactRef, artifact.DeleteReasonCleanup, now); err != nil {
		t.Fatalf("request purge: %v", err)
	}
	firstLease, claimed, err := metadata.ClaimPurge(ctx, meta.ArtifactRef, "worker_1", now, now.Add(time.Minute))
	if err != nil || !claimed {
		t.Fatalf("first claim claimed=%v error=%v", claimed, err)
	}
	if _, claimed, err := metadata.ClaimPurge(ctx, meta.ArtifactRef, "worker_2", now.Add(time.Second), now.Add(time.Minute)); err != nil || claimed {
		t.Fatalf("concurrent claim claimed=%v error=%v", claimed, err)
	}
	secondLease, claimed, err := metadata.ClaimPurge(ctx, meta.ArtifactRef, "worker_2", now.Add(2*time.Minute), now.Add(3*time.Minute))
	if err != nil || !claimed {
		t.Fatalf("expired lease claim claimed=%v error=%v", claimed, err)
	}
	if firstLease.LeaseVersion >= secondLease.LeaseVersion {
		t.Fatalf("lease fencing version did not advance: first=%d second=%d", firstLease.LeaseVersion, secondLease.LeaseVersion)
	}
	if _, err := metadata.MarkPurgeFailed(
		ctx, meta.ArtifactRef, "worker_1", firstLease.LeaseVersion, "stale worker failure", now.Add(2*time.Minute),
	); !artifact.IsErrorCode(err, artifact.ErrConflict) {
		t.Fatalf("stale worker changed purge state: %v", err)
	}
	current, err := metadata.GetPurgeJob(ctx, meta.ArtifactRef)
	if err != nil || current.LeaseOwner != "worker_2" || current.LeaseVersion != secondLease.LeaseVersion || current.Status != artifact.PurgeStatusLeased {
		t.Fatalf("new lease was overwritten: job=%#v error=%v", current, err)
	}
}

func TestProductionArtifactStoreRequiresDurablePurgeDependencies(t *testing.T) {
	if _, err := artifact.NewProductionStore(artifact.StoreConfig{
		ObjectStore: objectstore.NewMemory(), MetadataStore: metastore.NewMemory(),
	}); !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
		t.Fatalf("memory production store error = %v", err)
	}
	metadata := productionMetadataStore{MemoryMetadataStore: metastore.NewMemory()}
	objects := &productionObjectStore{ObjectStore: objectstore.NewMemory()}
	if _, err := artifact.NewProductionStore(artifact.StoreConfig{ObjectStore: objects, MetadataStore: metadata}); !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
		t.Fatalf("production store without codec error = %v", err)
	}
	codec, err := downloadtoken.New([]byte("purge-production-download-token-key-32-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.NewProductionStore(artifact.StoreConfig{ObjectStore: objects, MetadataStore: metadata, DownloadCodec: codec})
	if err != nil || store == nil {
		t.Fatalf("production store = %#v, error = %v", store, err)
	}
}

type failDeleteOnceObjectStore struct {
	artifact.ObjectStore
	mu  sync.Mutex
	err error
}

type productionMetadataStore struct {
	*metastore.MemoryMetadataStore
}

func (productionMetadataStore) ProductionReady() bool { return true }

type productionObjectStore struct {
	artifact.ObjectStore
	codec *downloadtoken.Codec
}

func (*productionObjectStore) ProductionReady() bool { return true }

func (s *productionObjectStore) SetDownloadCodec(codec *downloadtoken.Codec) {
	s.codec = codec
	if store, ok := s.ObjectStore.(interface{ SetDownloadCodec(*downloadtoken.Codec) }); ok {
		store.SetDownloadCodec(codec)
	}
}

func (s *productionObjectStore) DownloadCodec() *downloadtoken.Codec { return s.codec }

func (s *failDeleteOnceObjectStore) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	err := s.err
	s.err = nil
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.ObjectStore.Delete(ctx, key)
}
