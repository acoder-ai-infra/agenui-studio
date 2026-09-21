package artifact_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/objectstore"
)

func TestStoreDeleteCreatesStableTombstoneAndRejectsRepeat(t *testing.T) {
	t1 := time.Date(2026, time.July, 12, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	tests := []struct {
		name         string
		repeatReason DeleteReason
		repeatAt     time.Time
	}{
		{name: "different reason and time", repeatReason: DeleteReasonCleanup, repeatAt: t2},
		{name: "same reason and time", repeatReason: DeleteReasonUser, repeatAt: t1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newTask7LifecycleFixture(t, t1)
			if err := fixture.store.Delete(actorContext(), fixture.meta.ArtifactRef, DeleteReasonUser); err != nil {
				t.Fatalf("first Delete(): %v", err)
			}
			fixture.clock.Set(tc.repeatAt)
			if err := fixture.store.Delete(actorContext(), fixture.meta.ArtifactRef, tc.repeatReason); !IsErrorCode(err, ErrDeleted) {
				t.Fatalf("repeat Delete() error = %v, want deleted", err)
			}

			assertTask7Tombstone(t, fixture.rawMetadata, fixture.meta.ArtifactRef, DeleteReasonUser, t1)
			if got := fixture.metadata.MarkDeletedCalls(); got != 1 {
				t.Fatalf("MarkDeleted calls = %d, want 1", got)
			}
			if got := fixture.objects.DeleteCalls(); got != 1 {
				t.Fatalf("ObjectStore.Delete calls = %d, want 1", got)
			}
		})
	}
}

func TestStoreDeleteAuthorizesBeforeReportingTerminalState(t *testing.T) {
	t1 := time.Date(2026, time.July, 12, 10, 0, 0, 0, time.UTC)
	fixture := newTask7LifecycleFixture(t, t1)
	if err := fixture.store.Delete(actorContext(), fixture.meta.ArtifactRef, DeleteReasonUser); err != nil {
		t.Fatalf("first Delete(): %v", err)
	}

	unauthorized := []struct {
		name string
		ctx  context.Context
	}{
		{
			name: "wrong user",
			ctx: ContextWithActor(context.Background(), Actor{
				TenantID: "tenant-a", UserID: "user-2", SessionID: "sess-1", RunID: "run-1", Role: ActorUser,
			}),
		},
		{
			name: "wrong run",
			ctx: ContextWithActor(context.Background(), Actor{
				TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-2", Role: ActorUser,
			}),
		},
	}
	for _, tc := range unauthorized {
		t.Run(tc.name, func(t *testing.T) {
			beforeMark, beforeDelete := fixture.metadata.MarkDeletedCalls(), fixture.objects.DeleteCalls()
			if err := fixture.store.Delete(tc.ctx, fixture.meta.ArtifactRef, DeleteReasonCleanup); !IsErrorCode(err, ErrPermissionDenied) {
				t.Fatalf("Delete() error = %v, want permission_denied", err)
			}
			if fixture.metadata.MarkDeletedCalls() != beforeMark || fixture.objects.DeleteCalls() != beforeDelete {
				t.Fatalf("unauthorized Delete mutated adapters: MarkDeleted %d->%d, object Delete %d->%d",
					beforeMark, fixture.metadata.MarkDeletedCalls(), beforeDelete, fixture.objects.DeleteCalls())
			}
		})
	}

	beforeLookups := fixture.metadata.GetByRefCalls()
	beforeMark, beforeDelete := fixture.metadata.MarkDeletedCalls(), fixture.objects.DeleteCalls()
	if err := fixture.store.Delete(actorContext(), fixture.meta.ArtifactRef, DeleteReason("admin")); !IsErrorCode(err, ErrInvalidArgument) {
		t.Fatalf("Delete(invalid reason) error = %v, want invalid_argument", err)
	}
	if fixture.metadata.GetByRefCalls() != beforeLookups || fixture.metadata.MarkDeletedCalls() != beforeMark || fixture.objects.DeleteCalls() != beforeDelete {
		t.Fatalf("invalid reason accessed adapters: GetByRef %d->%d, MarkDeleted %d->%d, object Delete %d->%d",
			beforeLookups, fixture.metadata.GetByRefCalls(), beforeMark, fixture.metadata.MarkDeletedCalls(), beforeDelete, fixture.objects.DeleteCalls())
	}
	assertTask7Tombstone(t, fixture.rawMetadata, fixture.meta.ArtifactRef, DeleteReasonUser, t1)
}

func TestStoreDeleteKeepsTombstoneWhenPhysicalRemovalFails(t *testing.T) {
	t1 := time.Date(2026, time.July, 12, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	fixture := newTask7LifecycleFixture(t, t1)
	physicalFailure := errors.New("physical object removal failed")
	fixture.objects.SetDeleteError(physicalFailure)

	if err := fixture.store.Delete(actorContext(), fixture.meta.ArtifactRef, DeleteReasonUser); !errors.Is(err, physicalFailure) {
		t.Fatalf("first Delete() error = %v, want exact physical failure", err)
	}
	assertTask7Tombstone(t, fixture.rawMetadata, fixture.meta.ArtifactRef, DeleteReasonUser, t1)
	assertTask7ObjectPresent(t, fixture.rawObjects, fixture.meta.StorageKey)
	if fixture.metadata.MarkDeletedCalls() != 1 || fixture.objects.DeleteCalls() != 1 {
		t.Fatalf("first Delete calls = MarkDeleted %d, object Delete %d; want 1,1", fixture.metadata.MarkDeletedCalls(), fixture.objects.DeleteCalls())
	}

	fixture.clock.Set(t2)
	if err := fixture.store.Delete(actorContext(), fixture.meta.ArtifactRef, DeleteReasonCleanup); !IsErrorCode(err, ErrDeleted) {
		t.Fatalf("repeat Delete() error = %v, want deleted", err)
	}
	assertTask7Tombstone(t, fixture.rawMetadata, fixture.meta.ArtifactRef, DeleteReasonUser, t1)
	assertTask7ObjectPresent(t, fixture.rawObjects, fixture.meta.StorageKey)
	if fixture.metadata.MarkDeletedCalls() != 1 || fixture.objects.DeleteCalls() != 1 {
		t.Fatalf("repeat Delete retried mutation: MarkDeleted %d, object Delete %d; want 1,1", fixture.metadata.MarkDeletedCalls(), fixture.objects.DeleteCalls())
	}
}

func TestStoreExpiredArtifactRemainsReadableUntilCleanupThenAllAccessIsDeleted(t *testing.T) {
	cleanupAt := time.Date(2026, time.July, 12, 12, 0, 0, 0, time.UTC)
	fixture := newTask7LifecycleHarness(cleanupAt.Add(-2 * time.Hour))
	fixture.meta = task7PutArtifact(t, fixture.store, "expired-full-chain", cleanupAt.Add(-time.Minute))
	fixture.clock.Set(cleanupAt)
	if fixture.clock.Now().Before(fixture.meta.ExpiresAt) {
		t.Fatalf("fixture clock %v has not crossed expiry %v", fixture.clock.Now(), fixture.meta.ExpiresAt)
	}

	object, err := fixture.store.Get(actorContext(), fixture.meta.ArtifactRef, GetOptions{Purpose: PurposeView})
	if err != nil {
		t.Fatalf("Get(expired before cleanup): %v", err)
	}
	content, readErr := io.ReadAll(object.Content)
	closeErr := object.Content.Close()
	if readErr != nil || closeErr != nil || string(content) != "task 7 expired-full-chain" {
		t.Fatalf("expired content = %q, read=%v close=%v", content, readErr, closeErr)
	}
	if head, err := fixture.store.Head(actorContext(), fixture.meta.ArtifactRef); err != nil || head.ArtifactRef != fixture.meta.ArtifactRef {
		t.Fatalf("Head(expired before cleanup) = %#v, error = %v", head, err)
	}
	issuedBeforeCleanup, err := fixture.store.CreateDownloadURL(actorContext(), fixture.meta.ArtifactRef, DownloadURLOptions{TTL: time.Minute})
	if err != nil || issuedBeforeCleanup == nil || issuedBeforeCleanup.URL == "" {
		t.Fatalf("CreateDownloadURL(expired before cleanup) = %#v, error = %v", issuedBeforeCleanup, err)
	}
	rows, err := fixture.store.List(actorContext(), ListQuery{})
	if err != nil || len(rows) != 1 || rows[0].ArtifactRef != fixture.meta.ArtifactRef {
		t.Fatalf("List(expired before cleanup) = %#v, error = %v", rows, err)
	}

	result, err := fixture.store.CleanupExpired(task7AuditContext("sess-1", "run-1"), cleanupAt)
	if err != nil || result == nil || result.Expired != 1 || result.Deleted != 1 {
		t.Fatalf("CleanupExpired() = %#v, error = %v; want {Expired:1 Deleted:1}", result, err)
	}
	assertTask7Tombstone(t, fixture.rawMetadata, fixture.meta.ArtifactRef, DeleteReasonTTL, cleanupAt)
	assertTask7ObjectMissing(t, fixture.rawObjects, fixture.meta.StorageKey)
	if fixture.metadata.MarkDeletedCalls() != 1 || fixture.objects.DeleteCalls() != 1 {
		t.Fatalf("cleanup calls = MarkDeleted %d, object Delete %d; want 1,1", fixture.metadata.MarkDeletedCalls(), fixture.objects.DeleteCalls())
	}

	authorizedOperations := []struct {
		name string
		call func(context.Context) error
	}{
		{name: "Get", call: func(ctx context.Context) error {
			_, err := fixture.store.Get(ctx, fixture.meta.ArtifactRef, GetOptions{Purpose: PurposeView})
			return err
		}},
		{name: "Head", call: func(ctx context.Context) error {
			_, err := fixture.store.Head(ctx, fixture.meta.ArtifactRef)
			return err
		}},
		{name: "new download URL", call: func(ctx context.Context) error {
			_, err := fixture.store.CreateDownloadURL(ctx, fixture.meta.ArtifactRef, DownloadURLOptions{TTL: time.Minute})
			return err
		}},
	}
	for _, operation := range authorizedOperations {
		t.Run(operation.name+" after cleanup", func(t *testing.T) {
			if err := operation.call(actorContext()); !IsErrorCode(err, ErrDeleted) {
				t.Fatalf("authorized operation error = %v, want deleted", err)
			}
			wrongRun := ContextWithActor(context.Background(), Actor{
				TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-2", Role: ActorUser,
			})
			if err := operation.call(wrongRun); !IsErrorCode(err, ErrPermissionDenied) {
				t.Fatalf("wrong-run operation error = %v, want permission_denied", err)
			}
		})
	}

	rows, err = fixture.store.List(actorContext(), ListQuery{})
	if err != nil || len(rows) != 0 {
		t.Fatalf("default List(after cleanup) = %#v, error = %v; want empty", rows, err)
	}
	rows, err = fixture.store.List(actorContext(), ListQuery{IncludeDeleted: true})
	if err != nil || len(rows) != 1 {
		t.Fatalf("include-deleted List = %#v, error = %v; want one row", rows, err)
	}
	if rows[0].ArtifactRef != fixture.meta.ArtifactRef || rows[0].Status != ArtifactStatusDeleted || rows[0].DeleteReason != DeleteReasonTTL || !rows[0].DeletedAt.Equal(cleanupAt) {
		t.Fatalf("include-deleted row = %#v, want cleanup tombstone", rows[0])
	}
}

func TestCleanupExpiredHonorsExpiryBoundary(t *testing.T) {
	now := time.Date(2026, time.July, 12, 12, 0, 0, 0, time.UTC)
	fixture := newTask7LifecycleHarness(now.Add(-time.Hour))
	equalRef := seedTask2Artifact(t, fixture.metadata, fixture.objects, "art-expiry-equal", "tenant-a", "user-1", "sess-1", "run-1", VisibilityUserVisible, now)
	futureRef := seedTask2Artifact(t, fixture.metadata, fixture.objects, "art-expiry-future", "tenant-a", "user-1", "sess-1", "run-1", VisibilityUserVisible, now.Add(time.Nanosecond))
	zeroRef := seedTask2Artifact(t, fixture.metadata, fixture.objects, "art-expiry-zero", "tenant-a", "user-1", "sess-1", "run-1", VisibilityUserVisible, time.Time{})

	result, err := fixture.store.CleanupExpired(task7AuditContext("sess-1", "run-1"), now)
	if err != nil || result == nil || result.Expired != 1 || result.Deleted != 1 {
		t.Fatalf("CleanupExpired() = %#v, error = %v; want equality row only", result, err)
	}
	assertTask7Tombstone(t, fixture.rawMetadata, equalRef, DeleteReasonTTL, now)
	assertTask7Status(t, fixture.rawMetadata, futureRef, ArtifactStatusReady)
	assertTask7Status(t, fixture.rawMetadata, zeroRef, ArtifactStatusReady)
	equal, err := fixture.rawMetadata.GetByRef(context.Background(), equalRef)
	if err != nil {
		t.Fatalf("GetByRef(equal): %v", err)
	}
	assertTask7ObjectMissing(t, fixture.rawObjects, equal.StorageKey)
	future, err := fixture.rawMetadata.GetByRef(context.Background(), futureRef)
	if err != nil {
		t.Fatalf("GetByRef(future): %v", err)
	}
	zero, err := fixture.rawMetadata.GetByRef(context.Background(), zeroRef)
	if err != nil {
		t.Fatalf("GetByRef(zero): %v", err)
	}
	assertTask7ObjectPresent(t, fixture.rawObjects, future.StorageKey)
	assertTask7ObjectPresent(t, fixture.rawObjects, zero.StorageKey)
	if fixture.metadata.MarkDeletedCalls() != 1 || fixture.objects.DeleteCalls() != 1 {
		t.Fatalf("boundary calls = MarkDeleted %d, object Delete %d; want 1,1", fixture.metadata.MarkDeletedCalls(), fixture.objects.DeleteCalls())
	}
}

func TestCleanupExpiredIsIdempotentAfterSuccess(t *testing.T) {
	now := time.Date(2026, time.July, 12, 12, 0, 0, 0, time.UTC)
	fixture := newTask7LifecycleHarness(now.Add(-time.Hour))
	meta := task7SeedLifecycleArtifact(t, fixture.metadata, fixture.objects, "art-cleanup-once", now.Add(-time.Hour), now.Add(-time.Minute))

	first, err := fixture.store.CleanupExpired(task7AuditContext("sess-1", "run-1"), now)
	if err != nil {
		t.Fatalf("first CleanupExpired(): %v", err)
	}
	assertTask7CleanupResult(t, first, 1, 1)
	assertTask7Tombstone(t, fixture.rawMetadata, meta.ArtifactRef, DeleteReasonTTL, now)
	assertTask7ObjectMissing(t, fixture.rawObjects, meta.StorageKey)
	firstMarkCalls, firstDeleteCalls := fixture.metadata.MarkDeletedCalls(), fixture.objects.DeleteCalls()

	second, err := fixture.store.CleanupExpired(task7AuditContext("sess-1", "run-1"), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("second CleanupExpired(): %v", err)
	}
	assertTask7CleanupResult(t, second, 0, 0)
	if fixture.metadata.MarkDeletedCalls() != firstMarkCalls || fixture.objects.DeleteCalls() != firstDeleteCalls {
		t.Fatalf("repeat cleanup mutated adapters: MarkDeleted %d->%d, object Delete %d->%d",
			firstMarkCalls, fixture.metadata.MarkDeletedCalls(), firstDeleteCalls, fixture.objects.DeleteCalls())
	}
}

func TestCleanupExpiredReturnsPartialResultAndStopsAtFirstFailure(t *testing.T) {
	now := time.Date(2026, time.July, 12, 12, 0, 0, 0, time.UTC)
	repeatAt := now.Add(time.Hour)

	t.Run("MarkDeleted failure", func(t *testing.T) {
		fixture := newTask7LifecycleHarness(now.Add(-4 * time.Hour))
		rows := task7SeedThreeExpiredOutOfOrder(t, fixture, now)
		failure := errors.New("second row MarkDeleted failed")
		fixture.metadata.SetMarkDeletedError(rows[1].ArtifactRef, failure)

		result, err := fixture.store.CleanupExpired(task7AuditContext("sess-1", "run-1"), now)
		if !errors.Is(err, failure) {
			t.Fatalf("CleanupExpired() error = %v, want MarkDeleted failure", err)
		}
		assertTask7CleanupResult(t, result, 2, 1)
		if got, want := fixture.metadata.MarkDeletedRefs(), []string{rows[0].ArtifactRef, rows[1].ArtifactRef}; !reflect.DeepEqual(got, want) {
			t.Fatalf("MarkDeleted refs = %v, want %v", got, want)
		}
		if got, want := fixture.objects.DeleteKeys(), []string{rows[0].StorageKey}; !reflect.DeepEqual(got, want) {
			t.Fatalf("object Delete keys = %v, want %v", got, want)
		}
		assertTask7Tombstone(t, fixture.rawMetadata, rows[0].ArtifactRef, DeleteReasonTTL, now)
		assertTask7Status(t, fixture.rawMetadata, rows[1].ArtifactRef, ArtifactStatusReady)
		assertTask7Status(t, fixture.rawMetadata, rows[2].ArtifactRef, ArtifactStatusReady)
		assertTask7ObjectMissing(t, fixture.rawObjects, rows[0].StorageKey)
		assertTask7ObjectPresent(t, fixture.rawObjects, rows[1].StorageKey)
		assertTask7ObjectPresent(t, fixture.rawObjects, rows[2].StorageKey)

		fixture.metadata.SetMarkDeletedError(rows[1].ArtifactRef, nil)
		repeated, err := fixture.store.CleanupExpired(task7AuditContext("sess-1", "run-1"), repeatAt)
		if err != nil {
			t.Fatalf("repeat CleanupExpired(): %v", err)
		}
		assertTask7CleanupResult(t, repeated, 2, 2)
		assertTask7Tombstone(t, fixture.rawMetadata, rows[0].ArtifactRef, DeleteReasonTTL, now)
		assertTask7Tombstone(t, fixture.rawMetadata, rows[1].ArtifactRef, DeleteReasonTTL, repeatAt)
		assertTask7Tombstone(t, fixture.rawMetadata, rows[2].ArtifactRef, DeleteReasonTTL, repeatAt)
		if got, want := fixture.metadata.MarkDeletedRefs(), []string{rows[0].ArtifactRef, rows[1].ArtifactRef, rows[1].ArtifactRef, rows[2].ArtifactRef}; !reflect.DeepEqual(got, want) {
			t.Fatalf("all MarkDeleted refs = %v, want %v", got, want)
		}
		if got, want := fixture.objects.DeleteKeys(), []string{rows[0].StorageKey, rows[1].StorageKey, rows[2].StorageKey}; !reflect.DeepEqual(got, want) {
			t.Fatalf("all object Delete keys = %v, want %v", got, want)
		}
		assertTask7CleanupNoOp(t, fixture, repeatAt.Add(time.Hour))
	})

	t.Run("object Delete failure", func(t *testing.T) {
		fixture := newTask7LifecycleHarness(now.Add(-4 * time.Hour))
		rows := task7SeedThreeExpiredOutOfOrder(t, fixture, now)
		failure := errors.New("second row object Delete failed")
		fixture.objects.SetDeleteErrorForKey(rows[1].StorageKey, failure)

		result, err := fixture.store.CleanupExpired(task7AuditContext("sess-1", "run-1"), now)
		if !errors.Is(err, failure) {
			t.Fatalf("CleanupExpired() error = %v, want object Delete failure", err)
		}
		assertTask7CleanupResult(t, result, 2, 1)
		if got, want := fixture.metadata.MarkDeletedRefs(), []string{rows[0].ArtifactRef, rows[1].ArtifactRef}; !reflect.DeepEqual(got, want) {
			t.Fatalf("MarkDeleted refs = %v, want %v", got, want)
		}
		if got, want := fixture.objects.DeleteKeys(), []string{rows[0].StorageKey, rows[1].StorageKey}; !reflect.DeepEqual(got, want) {
			t.Fatalf("object Delete keys = %v, want %v", got, want)
		}
		assertTask7Tombstone(t, fixture.rawMetadata, rows[0].ArtifactRef, DeleteReasonTTL, now)
		assertTask7Tombstone(t, fixture.rawMetadata, rows[1].ArtifactRef, DeleteReasonTTL, now)
		assertTask7Status(t, fixture.rawMetadata, rows[2].ArtifactRef, ArtifactStatusReady)
		assertTask7ObjectMissing(t, fixture.rawObjects, rows[0].StorageKey)
		assertTask7ObjectPresent(t, fixture.rawObjects, rows[1].StorageKey)
		assertTask7ObjectPresent(t, fixture.rawObjects, rows[2].StorageKey)

		fixture.objects.SetDeleteErrorForKey(rows[1].StorageKey, nil)
		repeated, err := fixture.store.CleanupExpired(task7AuditContext("sess-1", "run-1"), repeatAt)
		if err != nil {
			t.Fatalf("repeat CleanupExpired(): %v", err)
		}
		assertTask7CleanupResult(t, repeated, 1, 1)
		assertTask7Tombstone(t, fixture.rawMetadata, rows[0].ArtifactRef, DeleteReasonTTL, now)
		assertTask7Tombstone(t, fixture.rawMetadata, rows[1].ArtifactRef, DeleteReasonTTL, now)
		assertTask7Tombstone(t, fixture.rawMetadata, rows[2].ArtifactRef, DeleteReasonTTL, repeatAt)
		assertTask7ObjectPresent(t, fixture.rawObjects, rows[1].StorageKey)
		if got, want := fixture.metadata.MarkDeletedRefs(), []string{rows[0].ArtifactRef, rows[1].ArtifactRef, rows[2].ArtifactRef}; !reflect.DeepEqual(got, want) {
			t.Fatalf("all MarkDeleted refs = %v, want %v", got, want)
		}
		if got, want := fixture.objects.DeleteKeys(), []string{rows[0].StorageKey, rows[1].StorageKey, rows[2].StorageKey}; !reflect.DeepEqual(got, want) {
			t.Fatalf("all object Delete keys = %v, want %v with no orphan retry", got, want)
		}
		assertTask7CleanupNoOp(t, fixture, repeatAt.Add(time.Hour))
	})
}

func TestCleanupExpiredReturnsNilResultWhenListFails(t *testing.T) {
	now := time.Date(2026, time.July, 12, 12, 0, 0, 0, time.UTC)
	fixture := newTask7LifecycleHarness(now.Add(-2 * time.Hour))
	meta := task7SeedLifecycleArtifact(t, fixture.metadata, fixture.objects, "art-list-failure", now.Add(-time.Hour), now.Add(-time.Minute))
	failure := errors.New("metadata list failed")
	fixture.metadata.SetListError(failure)

	result, err := fixture.store.CleanupExpired(task7AuditContext("sess-1", "run-1"), now)
	if result != nil || !errors.Is(err, failure) {
		t.Fatalf("CleanupExpired() = %#v, error = %v; want nil and list failure", result, err)
	}
	if fixture.metadata.MarkDeletedCalls() != 0 || fixture.objects.DeleteCalls() != 0 {
		t.Fatalf("list failure mutated adapters: MarkDeleted %d, object Delete %d", fixture.metadata.MarkDeletedCalls(), fixture.objects.DeleteCalls())
	}
	assertTask7Status(t, fixture.rawMetadata, meta.ArtifactRef, ArtifactStatusReady)
	assertTask7ObjectPresent(t, fixture.rawObjects, meta.StorageKey)
}

type task7LifecycleFixture struct {
	store       *Store
	clock       *task7MutableClock
	metadata    *task7MetadataStore
	objects     *task7ObjectStore
	rawMetadata *metastore.MemoryMetadataStore
	rawObjects  *objectstore.MemoryObjectStore
	meta        *ArtifactMeta
}

func newTask7LifecycleFixture(t *testing.T, now time.Time) *task7LifecycleFixture {
	t.Helper()
	fixture := newTask7LifecycleHarness(now)
	fixture.meta = task7PutArtifact(t, fixture.store, "delete", time.Time{})
	return fixture
}

func newTask7LifecycleHarness(now time.Time) *task7LifecycleFixture {
	rawMetadata := metastore.NewMemory()
	rawObjects := objectstore.NewMemory()
	metadata := &task7MetadataStore{MetadataStore: rawMetadata}
	objects := &task7ObjectStore{ObjectStore: rawObjects}
	clock := &task7MutableClock{now: now}
	store := NewStore(StoreConfig{
		ObjectStore:    objects,
		MetadataStore:  metadata,
		Clock:          clock,
		MaxObjectBytes: 1 << 20,
	})
	return &task7LifecycleFixture{
		store: store, clock: clock, metadata: metadata, objects: objects,
		rawMetadata: rawMetadata, rawObjects: rawObjects,
	}
}

func task7PutArtifact(t *testing.T, store *Store, name string, expiresAt time.Time) *ArtifactMeta {
	t.Helper()
	meta, err := store.Put(actorContext(), PutArtifactRequest{
		TenantID:        "tenant-a",
		UserID:          "user-1",
		SessionID:       "sess-1",
		RunID:           "run-1",
		OwnerModule:     OwnerModuleToolGateway,
		OwnerID:         "task-7-" + name,
		ArtifactType:    ArtifactTypeToolResult,
		MimeType:        "text/plain",
		Name:            name + ".txt",
		Visibility:      VisibilityUserVisible,
		RetentionPolicy: RetentionRunTTL,
		ExpiresAt:       expiresAt,
		Content:         strings.NewReader("task 7 " + name),
	})
	if err != nil {
		t.Fatalf("Put fixture: %v", err)
	}
	return meta
}

func task7SeedLifecycleArtifact(t *testing.T, metadata MetadataStore, objects ObjectStore, artifactID string, createdAt, expiresAt time.Time) *ArtifactMeta {
	t.Helper()
	content := []byte("task 7 lifecycle " + artifactID)
	hash := sha256.Sum256(content)
	ref := BuildRef("tenant-a", "sess-1", "run-1", artifactID)
	storageKey := "tenants/tenant-a/sessions/sess-1/runs/run-1/" + artifactID
	if _, err := objects.Put(context.Background(), storageKey, strings.NewReader(string(content))); err != nil {
		t.Fatalf("seed object %s: %v", artifactID, err)
	}
	created, err := metadata.Create(context.Background(), ArtifactMeta{
		ArtifactID:      artifactID,
		ArtifactRef:     ref,
		TenantID:        "tenant-a",
		UserID:          "user-1",
		SessionID:       "sess-1",
		RunID:           "run-1",
		OwnerModule:     OwnerModuleToolGateway,
		OwnerID:         "task-7-" + artifactID,
		ArtifactType:    ArtifactTypeToolResult,
		MimeType:        "text/plain",
		Name:            artifactID + ".txt",
		SizeBytes:       int64(len(content)),
		Hash:            "sha256:" + hex.EncodeToString(hash[:]),
		Visibility:      VisibilityUserVisible,
		StorageBackend:  objects.Backend(),
		StorageKey:      storageKey,
		RetentionPolicy: RetentionRunTTL,
		ExpiresAt:       expiresAt,
		CreatedBy:       "tool_gateway:task-7-" + artifactID,
		CreatedAt:       createdAt,
		Status:          ArtifactStatusReady,
		SchemaVersion:   ArtifactMetaSchemaVersion,
	}, "")
	if err != nil {
		t.Fatalf("seed metadata %s: %v", artifactID, err)
	}
	return created
}

func task7SeedThreeExpiredOutOfOrder(t *testing.T, fixture *task7LifecycleFixture, now time.Time) []*ArtifactMeta {
	t.Helper()
	createdBase := now.Add(-3 * time.Hour)
	third := task7SeedLifecycleArtifact(t, fixture.metadata, fixture.objects, "art-cleanup-third", createdBase.Add(2*time.Minute), now.Add(-time.Minute))
	first := task7SeedLifecycleArtifact(t, fixture.metadata, fixture.objects, "art-cleanup-first", createdBase, now.Add(-time.Minute))
	second := task7SeedLifecycleArtifact(t, fixture.metadata, fixture.objects, "art-cleanup-second", createdBase.Add(time.Minute), now.Add(-time.Minute))
	return []*ArtifactMeta{first, second, third}
}

func assertTask7CleanupResult(t *testing.T, result *CleanupResult, expired, deleted int) {
	t.Helper()
	if result == nil || result.Expired != expired || result.Deleted != deleted {
		t.Fatalf("CleanupResult = %#v, want {Expired:%d Deleted:%d}", result, expired, deleted)
	}
}

func assertTask7CleanupNoOp(t *testing.T, fixture *task7LifecycleFixture, now time.Time) {
	t.Helper()
	markCalls, deleteCalls := fixture.metadata.MarkDeletedCalls(), fixture.objects.DeleteCalls()
	result, err := fixture.store.CleanupExpired(task7AuditContext("sess-1", "run-1"), now)
	if err != nil {
		t.Fatalf("no-op CleanupExpired(): %v", err)
	}
	assertTask7CleanupResult(t, result, 0, 0)
	if fixture.metadata.MarkDeletedCalls() != markCalls || fixture.objects.DeleteCalls() != deleteCalls {
		t.Fatalf("no-op cleanup mutated adapters: MarkDeleted %d->%d, object Delete %d->%d",
			markCalls, fixture.metadata.MarkDeletedCalls(), deleteCalls, fixture.objects.DeleteCalls())
	}
}

func assertTask7Tombstone(t *testing.T, metadata MetadataStore, ref string, reason DeleteReason, deletedAt time.Time) {
	t.Helper()
	got, err := metadata.GetByRef(context.Background(), ref)
	if err != nil {
		t.Fatalf("GetByRef(%q): %v", ref, err)
	}
	if got.Status != ArtifactStatusDeleted || got.DeleteReason != reason || !got.DeletedAt.Equal(deletedAt) {
		t.Fatalf("tombstone = (%q, %q, %v), want (%q, %q, %v)",
			got.Status, got.DeleteReason, got.DeletedAt, ArtifactStatusDeleted, reason, deletedAt)
	}
}

func assertTask7ObjectPresent(t *testing.T, objects ObjectStore, key string) {
	t.Helper()
	reader, err := objects.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("ObjectStore.Get(%q): %v", key, err)
	}
	if reader == nil {
		t.Fatalf("ObjectStore.Get(%q) returned nil reader", key)
	}
	_, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close object %q: read=%v close=%v", key, readErr, closeErr)
	}
}

func assertTask7ObjectMissing(t *testing.T, objects ObjectStore, key string) {
	t.Helper()
	reader, err := objects.Get(context.Background(), key)
	if reader != nil {
		_ = reader.Close()
	}
	if !IsErrorCode(err, ErrNotFound) {
		t.Fatalf("ObjectStore.Get(%q) error = %v, want not_found", key, err)
	}
}

func assertTask7Status(t *testing.T, metadata MetadataStore, ref string, status ArtifactStatus) {
	t.Helper()
	got, err := metadata.GetByRef(context.Background(), ref)
	if err != nil {
		t.Fatalf("GetByRef(%q): %v", ref, err)
	}
	if got.Status != status {
		t.Fatalf("status for %q = %q, want %q", ref, got.Status, status)
	}
}

func task7AuditContext(sessionID, runID string) context.Context {
	return ContextWithActor(context.Background(), Actor{
		TenantID: "tenant-a", SessionID: sessionID, RunID: runID, Role: ActorAudit,
	})
}

type task7MutableClock struct {
	mu  sync.RWMutex
	now time.Time
}

func (c *task7MutableClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

func (c *task7MutableClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

type task7MetadataStore struct {
	MetadataStore
	mu                sync.Mutex
	getByRefCalls     int
	markDeletedCalls  int
	markDeletedRefs   []string
	markDeletedErrors map[string]error
	listErr           error
}

func (s *task7MetadataStore) GetByRef(ctx context.Context, ref string) (*ArtifactMeta, error) {
	s.mu.Lock()
	s.getByRefCalls++
	s.mu.Unlock()
	return s.MetadataStore.GetByRef(ctx, ref)
}

func (s *task7MetadataStore) MarkDeleted(ctx context.Context, ref string, reason DeleteReason, at time.Time) (*ArtifactMeta, error) {
	s.mu.Lock()
	s.markDeletedCalls++
	s.markDeletedRefs = append(s.markDeletedRefs, ref)
	err := s.markDeletedErrors[ref]
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.MetadataStore.MarkDeleted(ctx, ref, reason, at)
}

func (s *task7MetadataStore) List(ctx context.Context, query ListQuery) ([]ArtifactMeta, error) {
	s.mu.Lock()
	err := s.listErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.MetadataStore.List(ctx, query)
}

func (s *task7MetadataStore) GetByRefCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getByRefCalls
}

func (s *task7MetadataStore) MarkDeletedCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.markDeletedCalls
}

func (s *task7MetadataStore) MarkDeletedRefs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.markDeletedRefs...)
}

func (s *task7MetadataStore) SetMarkDeletedError(ref string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.markDeletedErrors == nil {
		s.markDeletedErrors = make(map[string]error)
	}
	if err == nil {
		delete(s.markDeletedErrors, ref)
		return
	}
	s.markDeletedErrors[ref] = err
}

func (s *task7MetadataStore) SetListError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listErr = err
}

type task7ObjectStore struct {
	ObjectStore
	mu           sync.Mutex
	deleteCalls  int
	deleteKeys   []string
	deleteErr    error
	deleteErrors map[string]error
}

func (s *task7ObjectStore) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	s.deleteCalls++
	s.deleteKeys = append(s.deleteKeys, key)
	err := s.deleteErr
	if err == nil {
		err = s.deleteErrors[key]
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.ObjectStore.Delete(ctx, key)
}

func (s *task7ObjectStore) SetDeleteError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteErr = err
}

func (s *task7ObjectStore) DeleteCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteCalls
}

func (s *task7ObjectStore) DeleteKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deleteKeys...)
}

func (s *task7ObjectStore) SetDeleteErrorForKey(key string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteErrors == nil {
		s.deleteErrors = make(map[string]error)
	}
	if err == nil {
		delete(s.deleteErrors, key)
		return
	}
	s.deleteErrors[key] = err
}
