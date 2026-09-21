package artifact_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	. "github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/objectstore"
)

type refAuditMetadataStore struct {
	MetadataStore
	getByRefCalls    int
	markDeletedCalls int
}

func (s *refAuditMetadataStore) GetByRef(ctx context.Context, ref string) (*ArtifactMeta, error) {
	s.getByRefCalls++
	return s.MetadataStore.GetByRef(ctx, ref)
}

func (s *refAuditMetadataStore) MarkDeleted(ctx context.Context, ref string, reason DeleteReason, at time.Time) (*ArtifactMeta, error) {
	s.markDeletedCalls++
	return s.MetadataStore.MarkDeleted(ctx, ref, reason, at)
}

type refAuditObjectStore struct {
	ObjectStore
	getCalls      int
	deleteCalls   int
	downloadCalls int
}

func (s *refAuditObjectStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	s.getCalls++
	return s.ObjectStore.Get(ctx, key)
}

func (s *refAuditObjectStore) Delete(ctx context.Context, key string) error {
	s.deleteCalls++
	return s.ObjectStore.Delete(ctx, key)
}

func (s *refAuditObjectStore) CreateDownloadURL(ctx context.Context, key string, ttl time.Duration) (*DownloadURL, error) {
	s.downloadCalls++
	return s.ObjectStore.CreateDownloadURL(ctx, key, ttl)
}

func newRefAuditStoreWith(objects ObjectStore, metadata MetadataStore) (*Store, *refAuditMetadataStore, *refAuditObjectStore) {
	countedMetadata := &refAuditMetadataStore{MetadataStore: metadata}
	countedObjects := &refAuditObjectStore{ObjectStore: objects}
	store := NewStore(StoreConfig{
		ObjectStore:    countedObjects,
		MetadataStore:  countedMetadata,
		Clock:          fixedClock{now: time.Date(2026, time.July, 11, 12, 0, 0, 0, time.UTC)},
		MaxObjectBytes: 1 << 20,
	})
	return store, countedMetadata, countedObjects
}

func newRefAuditStore() (*Store, *refAuditMetadataStore, *refAuditObjectStore) {
	return newRefAuditStoreWith(objectstore.NewMemory(), metastore.NewMemory())
}

func assertNoRefDownstreamCalls(t *testing.T, metadata *refAuditMetadataStore, objects *refAuditObjectStore) {
	t.Helper()
	if metadata.getByRefCalls != 0 || metadata.markDeletedCalls != 0 ||
		objects.getCalls != 0 || objects.deleteCalls != 0 || objects.downloadCalls != 0 {
		t.Fatalf("unexpected calls: metadata get=%d mark=%d object get=%d delete=%d URL=%d",
			metadata.getByRefCalls, metadata.markDeletedCalls,
			objects.getCalls, objects.deleteCalls, objects.downloadCalls)
	}
}

var refOperations = []struct {
	name string
	call func(*Store, context.Context, string) error
}{
	{name: "get", call: func(s *Store, ctx context.Context, ref string) error {
		_, err := s.Get(ctx, ref, GetOptions{Purpose: PurposeView})
		return err
	}},
	{name: "head", call: func(s *Store, ctx context.Context, ref string) error {
		_, err := s.Head(ctx, ref)
		return err
	}},
	{name: "delete", call: func(s *Store, ctx context.Context, ref string) error {
		return s.Delete(ctx, ref, DeleteReasonUser)
	}},
	{name: "download_url", call: func(s *Store, ctx context.Context, ref string) error {
		_, err := s.CreateDownloadURL(ctx, ref, DownloadURLOptions{})
		return err
	}},
}

func TestStoreRefPreflightPreservesValidationOrder(t *testing.T) {
	configured, metadata, objects := newRefAuditStore()
	foreignRef := BuildRef("tenant-b", "sess-1", "run-1", "art_foreign")
	for _, op := range refOperations {
		t.Run(op.name+"/missing actor before malformed ref", func(t *testing.T) {
			if err := op.call(configured, context.Background(), ""); !IsErrorCode(err, ErrPermissionDenied) {
				t.Fatalf("error=%v want permission_denied", err)
			}
		})
		t.Run(op.name+"/missing adapter before scope preflight", func(t *testing.T) {
			if err := op.call(NewStore(StoreConfig{}), actorContext(), foreignRef); !IsErrorCode(err, ErrInvalidArgument) {
				t.Fatalf("error=%v want config invalid_argument", err)
			}
		})
	}
	if err := configured.Delete(actorContext(), foreignRef, DeleteReason("unknown")); !IsErrorCode(err, ErrInvalidArgument) {
		t.Fatalf("invalid reason precedence error=%v", err)
	}
	if _, err := configured.Get(actorContext(), foreignRef, GetOptions{Purpose: Purpose("unknown")}); !IsErrorCode(err, ErrInvalidArgument) {
		t.Fatalf("invalid purpose precedence error=%v", err)
	}
	assertNoRefDownstreamCalls(t, metadata, objects)
}

func TestStoreRefOperationsRejectMalformedCanonicalRefBeforeMetadataLookup(t *testing.T) {
	refs := []string{
		"",
		"http://tenants/tenant-a/sessions/sess-1/runs/run-1/art_x",
		"artifact://user@tenants/tenant-a/sessions/sess-1/runs/run-1/art_x",
		"artifact://tenants/tenant-a/sessions/sess-1/runs/run-1/art_x?x=1",
		"artifact://tenants/tenant-a/sessions/sess-1/runs/run-1/art_x#fragment",
		"artifact://tenants/tenant-a/sessions/sess%2F1/runs/run-1/art_x",
		"artifact://tenants/tenant-a/sessions/sess-1/runs/run-1/%61rt_x",
	}
	for _, op := range refOperations {
		for _, ref := range refs {
			t.Run(op.name+"/"+ref, func(t *testing.T) {
				store, metadata, objects := newRefAuditStore()
				err := op.call(store, actorContext(), ref)
				if !IsErrorCode(err, ErrInvalidArgument) {
					t.Fatalf("error=%v want invalid_argument", err)
				}
				assertNoRefDownstreamCalls(t, metadata, objects)
			})
		}
	}
}

type refScopeCase struct {
	name                       string
	actor                      Actor
	tenantID, sessionID, runID string
}

var refScopeCases = []refScopeCase{
	{name: "user tenant", actor: Actor{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-1", Role: ActorUser}, tenantID: "tenant-b", sessionID: "sess-1", runID: "run-1"},
	{name: "user session", actor: Actor{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-1", Role: ActorUser}, tenantID: "tenant-a", sessionID: "sess-2", runID: "run-1"},
	{name: "user run", actor: Actor{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-1", Role: ActorUser}, tenantID: "tenant-a", sessionID: "sess-1", runID: "run-2"},
	{name: "runtime tenant", actor: Actor{TenantID: "tenant-a", UserID: "runtime", SessionID: "sess-1", RunID: "run-1", Role: ActorRuntime}, tenantID: "tenant-b", sessionID: "sess-1", runID: "run-1"},
	{name: "runtime session", actor: Actor{TenantID: "tenant-a", UserID: "runtime", SessionID: "sess-1", RunID: "run-1", Role: ActorRuntime}, tenantID: "tenant-a", sessionID: "sess-2", runID: "run-1"},
	{name: "runtime run", actor: Actor{TenantID: "tenant-a", UserID: "runtime", SessionID: "sess-1", RunID: "run-1", Role: ActorRuntime}, tenantID: "tenant-a", sessionID: "sess-1", runID: "run-2"},
	{name: "debug tenant", actor: Actor{TenantID: "tenant-a", UserID: "debugger", Role: ActorDebug}, tenantID: "tenant-b", sessionID: "sess-1", runID: "run-1"},
	{name: "debug session", actor: Actor{TenantID: "tenant-a", UserID: "debugger", SessionID: "sess-1", Role: ActorDebug}, tenantID: "tenant-a", sessionID: "sess-2", runID: "run-1"},
	{name: "debug run", actor: Actor{TenantID: "tenant-a", UserID: "debugger", RunID: "run-1", Role: ActorDebug}, tenantID: "tenant-a", sessionID: "sess-1", runID: "run-2"},
	{name: "audit tenant", actor: Actor{TenantID: "tenant-a", UserID: "auditor", Role: ActorAudit}, tenantID: "tenant-b", sessionID: "sess-1", runID: "run-1"},
	{name: "audit session", actor: Actor{TenantID: "tenant-a", UserID: "auditor", SessionID: "sess-1", Role: ActorAudit}, tenantID: "tenant-a", sessionID: "sess-2", runID: "run-1"},
	{name: "audit run", actor: Actor{TenantID: "tenant-a", UserID: "auditor", RunID: "run-1", Role: ActorAudit}, tenantID: "tenant-a", sessionID: "sess-1", runID: "run-2"},
}

func seedRefScopeArtifact(t *testing.T, tc refScopeCase) (ObjectStore, MetadataStore, *ArtifactMeta) {
	t.Helper()
	objects := objectstore.NewMemory()
	metadata := metastore.NewMemory()
	store := NewStore(StoreConfig{
		ObjectStore:   objects,
		MetadataStore: metadata,
		Clock:         fixedClock{now: time.Date(2026, time.July, 11, 12, 0, 0, 0, time.UTC)},
	})
	ctx := ContextWithActor(context.Background(), Actor{
		TenantID: tc.tenantID, UserID: "seed-runtime", SessionID: tc.sessionID, RunID: tc.runID, Role: ActorRuntime,
	})
	req := validPutRequest()
	req.TenantID, req.UserID, req.SessionID, req.RunID = tc.tenantID, "user-1", tc.sessionID, tc.runID
	req.OwnerID = "scope-seed"
	req.Content = strings.NewReader("{\"scope\":\"foreign\"}")
	meta, err := store.Put(ctx, req)
	if err != nil {
		t.Fatalf("seed foreign scope: %v", err)
	}
	return objects, metadata, meta
}

func TestStoreRefOperationsRejectCrossScopeBeforeMetadataLookup(t *testing.T) {
	for _, tc := range refScopeCases {
		for _, op := range refOperations {
			for _, target := range []string{"existing", "missing"} {
				t.Run(tc.name+"/"+op.name+"/"+target, func(t *testing.T) {
					rawObjects, rawMetadata, existing := seedRefScopeArtifact(t, tc)
					ref := existing.ArtifactRef
					if target == "missing" {
						ref = BuildRef(tc.tenantID, tc.sessionID, tc.runID, "art_missing")
					}
					store, metadata, objects := newRefAuditStoreWith(rawObjects, rawMetadata)
					err := op.call(store, ContextWithActor(context.Background(), tc.actor), ref)
					if !IsErrorCode(err, ErrPermissionDenied) {
						t.Fatalf("error=%v want permission_denied", err)
					}
					assertNoRefDownstreamCalls(t, metadata, objects)
				})
			}
		}
	}
}

func TestStoreRefOperationsSameScopeMissingStillConsultMetadata(t *testing.T) {
	ref := BuildRef("tenant-a", "sess-1", "run-1", "art_missing")
	for _, op := range refOperations {
		t.Run(op.name, func(t *testing.T) {
			store, metadata, objects := newRefAuditStore()
			err := op.call(store, actorContext(), ref)
			if !IsErrorCode(err, ErrNotFound) {
				t.Fatalf("error=%v want not_found", err)
			}
			if metadata.getByRefCalls != 1 || metadata.markDeletedCalls != 0 ||
				objects.getCalls != 0 || objects.deleteCalls != 0 || objects.downloadCalls != 0 {
				t.Fatalf("unexpected calls: metadata=%#v objects=%#v", metadata, objects)
			}
		})
	}
}
