package metastore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

type task6NamedMap map[string][]int
type task6NamedSlice []map[string]string
type task6NamedArray [2][]int

func TestMemoryMetadataStoreDeepCopyOwnsIngress(t *testing.T) {
	store := NewMemory()
	meta := task6MetadataFixture("art-ingress", "tenant-a", "sess-1", "run-1", time.Unix(1, 0))
	if _, err := store.Create(context.Background(), meta, "ingress-key"); err != nil {
		t.Fatalf("Create(): %v", err)
	}

	meta.DerivedFrom[0].ArtifactRef = "mutated-lineage"
	meta.Metadata["owner"] = "mutated-owner"
	meta.Preview.Fields["top"] = "mutated-top"
	meta.Preview.Fields["typed"].(map[string][]int)["values"][0] = 99

	got, err := store.GetByRef(context.Background(), meta.ArtifactRef)
	if err != nil {
		t.Fatalf("GetByRef(): %v", err)
	}
	task6AssertBasicContainersPristine(t, got)
}

func TestMemoryMetadataStoreDuplicateArtifactIDIsAtomic(t *testing.T) {
	store := NewMemory()
	first := task6MetadataFixture("art-shared", "tenant-a", "sess-1", "run-1", time.Unix(1, 0))
	if _, err := store.Create(context.Background(), first, "first-key"); err != nil {
		t.Fatalf("Create(first): %v", err)
	}
	candidate := task6MetadataFixture("art-shared", "tenant-a", "sess-2", "run-2", time.Unix(2, 0))
	if candidate.ArtifactRef == first.ArtifactRef {
		t.Fatal("duplicate-ID fixture must use a different canonical ref")
	}
	if created, err := store.Create(context.Background(), candidate, "failed-key"); created != nil || !artifact.IsErrorCode(err, artifact.ErrConflict) {
		t.Fatalf("Create(duplicate ID) = %#v, error = %v; want conflict", created, err)
	}

	byID, err := store.GetByID(context.Background(), first.ArtifactID)
	if err != nil || byID.ArtifactRef != first.ArtifactRef {
		t.Fatalf("GetByID() = %#v, error = %v; want first ref %q", byID, err, first.ArtifactRef)
	}
	if got, err := store.GetByRef(context.Background(), candidate.ArtifactRef); got != nil || !artifact.IsErrorCode(err, artifact.ErrNotFound) {
		t.Fatalf("candidate GetByRef() = %#v, error = %v; want not_found", got, err)
	}
	if got, err := store.GetByIdempotencyKey(context.Background(), "failed-key"); got != nil || !artifact.IsErrorCode(err, artifact.ErrNotFound) {
		t.Fatalf("failed key lookup = %#v, error = %v; want not_found", got, err)
	}
	recovery := task6MetadataFixture("art-recovery", "tenant-a", "sess-3", "run-3", time.Unix(3, 0))
	if got, err := store.Create(context.Background(), recovery, "failed-key"); err != nil || got == nil || got.ArtifactRef != recovery.ArtifactRef {
		t.Fatalf("Create(recovery with failed key) = %#v, error = %v", got, err)
	}
}

func TestMemoryMetadataStoreDeepCopyIsolatesEveryEgress(t *testing.T) {
	t.Run("rich ingress", func(t *testing.T) {
		store := NewMemory()
		meta := task6RichMetadataFixture("art-rich-ingress", "tenant-a", "sess-1", "run-1", time.Unix(1, 0))
		ref := meta.ArtifactRef
		if _, err := store.Create(context.Background(), meta, "rich-ingress-key"); err != nil {
			t.Fatalf("Create(): %v", err)
		}
		task6MutateAllContainers(t, &meta)
		task6AssertStoredRichPristine(t, store, ref, artifact.ArtifactStatusReady, "", time.Time{})
	})

	t.Run("new Create", func(t *testing.T) {
		store, meta, created := task6CreateRichMetadata(t, "art-create", "create-key")
		task6MutateAllContainers(t, created)
		task6AssertStoredRichPristine(t, store, meta.ArtifactRef, artifact.ArtifactStatusReady, "", time.Time{})
	})

	t.Run("idempotency-hit Create", func(t *testing.T) {
		store, meta, _ := task6CreateRichMetadata(t, "art-idempotent", "same-key")
		candidate := task6RichMetadataFixture("art-candidate", "tenant-a", "sess-2", "run-2", time.Unix(2, 0))
		hit, err := store.Create(context.Background(), candidate, "same-key")
		if err != nil || hit == nil || hit.ArtifactRef != meta.ArtifactRef {
			t.Fatalf("idempotency-hit Create() = %#v, error = %v", hit, err)
		}
		task6MutateAllContainers(t, hit)
		task6AssertStoredRichPristine(t, store, meta.ArtifactRef, artifact.ArtifactStatusReady, "", time.Time{})
	})

	getters := []struct {
		name string
		get  func(context.Context, *MemoryMetadataStore, artifact.ArtifactMeta) (*artifact.ArtifactMeta, error)
	}{
		{name: "GetByRef", get: func(ctx context.Context, store *MemoryMetadataStore, meta artifact.ArtifactMeta) (*artifact.ArtifactMeta, error) {
			return store.GetByRef(ctx, meta.ArtifactRef)
		}},
		{name: "GetByID", get: func(ctx context.Context, store *MemoryMetadataStore, meta artifact.ArtifactMeta) (*artifact.ArtifactMeta, error) {
			return store.GetByID(ctx, meta.ArtifactID)
		}},
		{name: "GetByIdempotencyKey", get: func(ctx context.Context, store *MemoryMetadataStore, _ artifact.ArtifactMeta) (*artifact.ArtifactMeta, error) {
			return store.GetByIdempotencyKey(ctx, "lookup-key")
		}},
	}
	for _, getter := range getters {
		getter := getter
		t.Run(getter.name, func(t *testing.T) {
			store, meta, _ := task6CreateRichMetadata(t, "art-get", "lookup-key")
			got, err := getter.get(context.Background(), store, meta)
			if err != nil {
				t.Fatalf("%s(): %v", getter.name, err)
			}
			task6MutateAllContainers(t, got)
			task6AssertStoredRichPristine(t, store, meta.ArtifactRef, artifact.ArtifactStatusReady, "", time.Time{})
		})
	}

	t.Run("every List row", func(t *testing.T) {
		store := NewMemory()
		metas := []artifact.ArtifactMeta{
			task6RichMetadataFixture("art-list-a", "tenant-a", "sess-1", "run-1", time.Unix(1, 0)),
			task6RichMetadataFixture("art-list-b", "tenant-a", "sess-1", "run-1", time.Unix(2, 0)),
		}
		for i := range metas {
			if _, err := store.Create(context.Background(), metas[i], ""); err != nil {
				t.Fatalf("Create(%s): %v", metas[i].ArtifactID, err)
			}
		}
		rows, err := store.List(context.Background(), artifact.ListQuery{})
		if err != nil || len(rows) != len(metas) {
			t.Fatalf("List() rows = %d, error = %v; want %d", len(rows), err, len(metas))
		}
		for i := range rows {
			task6MutateAllContainers(t, &rows[i])
		}
		for i := range metas {
			task6AssertStoredRichPristine(t, store, metas[i].ArtifactRef, artifact.ArtifactStatusReady, "", time.Time{})
		}
	})

	t.Run("first MarkDeleted", func(t *testing.T) {
		store, meta, _ := task6CreateRichMetadata(t, "art-delete-first", "")
		at := time.Unix(10, 0)
		deleted, err := store.MarkDeleted(context.Background(), meta.ArtifactRef, artifact.DeleteReasonUser, at)
		if err != nil {
			t.Fatalf("MarkDeleted(): %v", err)
		}
		task6MutateAllContainers(t, deleted)
		task6AssertStoredRichPristine(t, store, meta.ArtifactRef, artifact.ArtifactStatusDeleted, artifact.DeleteReasonUser, at)
	})

	t.Run("repeat MarkDeleted", func(t *testing.T) {
		store, meta, _ := task6CreateRichMetadata(t, "art-delete-repeat", "")
		at := time.Unix(11, 0)
		if _, err := store.MarkDeleted(context.Background(), meta.ArtifactRef, artifact.DeleteReasonCleanup, at); err != nil {
			t.Fatalf("first MarkDeleted(): %v", err)
		}
		repeated, err := store.MarkDeleted(context.Background(), meta.ArtifactRef, artifact.DeleteReasonCleanup, at)
		if err != nil {
			t.Fatalf("repeat MarkDeleted(): %v", err)
		}
		task6MutateAllContainers(t, repeated)
		task6AssertStoredRichPristine(t, store, meta.ArtifactRef, artifact.ArtifactStatusDeleted, artifact.DeleteReasonCleanup, at)
	})
}

func TestMemoryMetadataStoreClonePreservesTypedNamedNilEmptyAndCycles(t *testing.T) {
	original := task6RichMetadataFixture("art-clone", "tenant-a", "sess-1", "run-1", time.Unix(1, 0))
	cloned := cloneMeta(original)
	task6AssertRichContainersPristine(t, cloned)
	task6MutateAllContainers(t, cloned)
	task6AssertRichContainersPristine(t, &original)
}

func TestMemoryMetadataStoreDuplicateRefIsAtomic(t *testing.T) {
	store := NewMemory()
	first := task6MetadataFixture("art-ref", "tenant-a", "sess-1", "run-1", time.Unix(1, 0))
	if _, err := store.Create(context.Background(), first, "first-ref-key"); err != nil {
		t.Fatalf("Create(first): %v", err)
	}
	candidate := task6MetadataFixture("art-ref", "tenant-a", "sess-1", "run-1", time.Unix(2, 0))
	candidate.Name = "candidate-name"
	if candidate.ArtifactID != first.ArtifactID || candidate.ArtifactRef != first.ArtifactRef {
		t.Fatal("duplicate-ref fixture must retain its canonical ArtifactID and ref")
	}
	if got, err := store.Create(context.Background(), candidate, "failed-ref-key"); got != nil || !artifact.IsErrorCode(err, artifact.ErrConflict) {
		t.Fatalf("Create(duplicate ref) = %#v, error = %v; want conflict", got, err)
	}
	stored, err := store.GetByRef(context.Background(), first.ArtifactRef)
	if err != nil || stored.Name != first.Name {
		t.Fatalf("GetByRef() = %#v, error = %v; want original name %q", stored, err, first.Name)
	}
	if got, err := store.GetByIdempotencyKey(context.Background(), "failed-ref-key"); got != nil || !artifact.IsErrorCode(err, artifact.ErrNotFound) {
		t.Fatalf("failed ref key lookup = %#v, error = %v; want not_found", got, err)
	}
	recovery := task6MetadataFixture("art-ref-recovery", "tenant-a", "sess-2", "run-2", time.Unix(3, 0))
	if got, err := store.Create(context.Background(), recovery, "failed-ref-key"); err != nil || got == nil || got.ArtifactRef != recovery.ArtifactRef {
		t.Fatalf("Create(recovery with failed ref key) = %#v, error = %v", got, err)
	}
}

func TestMemoryMetadataStoreDuplicateIdempotencyKeyFirstWins(t *testing.T) {
	store, first, _ := task6CreateRichMetadata(t, "art-first", "opaque-idempotency-key")
	candidate := task6RichMetadataFixture("art-second", "tenant-a", "sess-2", "run-2", time.Unix(2, 0))
	hit, err := store.Create(context.Background(), candidate, "opaque-idempotency-key")
	if err != nil || hit == nil || hit.ArtifactRef != first.ArtifactRef {
		t.Fatalf("Create(idempotency hit) = %#v, error = %v; want first ref %q", hit, err, first.ArtifactRef)
	}
	task6MutateAllContainers(t, hit)
	task6AssertStoredRichPristine(t, store, first.ArtifactRef, artifact.ArtifactStatusReady, "", time.Time{})
	if got, err := store.GetByRef(context.Background(), candidate.ArtifactRef); got != nil || !artifact.IsErrorCode(err, artifact.ErrNotFound) {
		t.Fatalf("candidate ref lookup = %#v, error = %v; want not_found", got, err)
	}
	if got, err := store.GetByID(context.Background(), candidate.ArtifactID); got != nil || !artifact.IsErrorCode(err, artifact.ErrNotFound) {
		t.Fatalf("candidate ID lookup = %#v, error = %v; want not_found", got, err)
	}
}

func TestMemoryMetadataStoreListIsStableAndIsolated(t *testing.T) {
	store := NewMemory()
	fixtures := []artifact.ArtifactMeta{
		task6MetadataFixture("art-f", "tenant-a", "sess-1", "run-1", time.Unix(3, 0)),
		task6MetadataFixture("art-b", "tenant-a", "sess-1", "run-1", time.Unix(1, 0)),
		task6MetadataFixture("art-e", "tenant-a", "sess-1", "run-1", time.Unix(2, 0)),
		task6MetadataFixture("art-a", "tenant-a", "sess-1", "run-1", time.Unix(1, 0)),
		task6MetadataFixture("art-d", "tenant-a", "sess-1", "run-1", time.Unix(2, 0)),
		task6MetadataFixture("art-c", "tenant-a", "sess-1", "run-1", time.Unix(2, 0)),
	}
	for i := range fixtures {
		if _, err := store.Create(context.Background(), fixtures[i], ""); err != nil {
			t.Fatalf("Create(%s): %v", fixtures[i].ArtifactID, err)
		}
	}
	wantIDs := []string{"art-a", "art-b", "art-c", "art-d", "art-e", "art-f"}
	for iteration := 0; iteration < 50; iteration++ {
		rows, err := store.List(context.Background(), artifact.ListQuery{})
		if err != nil {
			t.Fatalf("List(iteration %d): %v", iteration, err)
		}
		if gotIDs := task6ArtifactIDs(rows); !reflect.DeepEqual(gotIDs, wantIDs) {
			t.Fatalf("List(iteration %d) IDs = %v, want %v", iteration, gotIDs, wantIDs)
		}
		task6AssertRowsSorted(t, rows)
	}

	rows, err := store.List(context.Background(), artifact.ListQuery{})
	if err != nil {
		t.Fatalf("List(for mutation): %v", err)
	}
	rows[0].Name = "mutated-name"
	rows[0].Metadata["owner"] = "mutated-owner"
	rows[0].Preview.Fields["typed"].(map[string][]int)["values"][0] = 99
	rows[0].DerivedFrom[0].ArtifactRef = "mutated-lineage"
	fresh, err := store.List(context.Background(), artifact.ListQuery{})
	if err != nil {
		t.Fatalf("List(after mutation): %v", err)
	}
	if gotIDs := task6ArtifactIDs(fresh); !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("List(after mutation) IDs = %v, want %v", gotIDs, wantIDs)
	}
	task6AssertBasicContainersPristine(t, &fresh[0])
	if fresh[0].Name == "mutated-name" {
		t.Fatalf("List mutation reached storage: %#v", fresh[0].Name)
	}
}

func TestMemoryMetadataStoreTombstoneIsStableAndMissingIsNotFound(t *testing.T) {
	store := NewMemory()
	missingRef := artifact.BuildRef("tenant-a", "sess-1", "run-1", "missing")
	if got, err := store.MarkDeleted(context.Background(), missingRef, artifact.DeleteReasonCleanup, time.Unix(1, 0)); got != nil || !artifact.IsErrorCode(err, artifact.ErrNotFound) {
		t.Fatalf("MarkDeleted(missing) = %#v, error = %v; want not_found", got, err)
	}
	if got, err := store.GetByRef(context.Background(), missingRef); got != nil || !artifact.IsErrorCode(err, artifact.ErrNotFound) {
		t.Fatalf("GetByRef(missing) = %#v, error = %v; want not_found", got, err)
	}
	if got, err := store.GetByID(context.Background(), "missing"); got != nil || !artifact.IsErrorCode(err, artifact.ErrNotFound) {
		t.Fatalf("GetByID(missing) = %#v, error = %v; want not_found", got, err)
	}
	if got, err := store.GetByIdempotencyKey(context.Background(), "missing"); got != nil || !artifact.IsErrorCode(err, artifact.ErrNotFound) {
		t.Fatalf("GetByIdempotencyKey(missing) = %#v, error = %v; want not_found", got, err)
	}

	meta := task6RichMetadataFixture("art-tombstone", "tenant-a", "sess-1", "run-1", time.Unix(1, 0))
	if _, err := store.Create(context.Background(), meta, ""); err != nil {
		t.Fatalf("Create(): %v", err)
	}
	firstAt := time.Unix(10, 123)
	first, err := store.MarkDeleted(context.Background(), meta.ArtifactRef, artifact.DeleteReasonUser, firstAt)
	if err != nil {
		t.Fatalf("first MarkDeleted(): %v", err)
	}
	first.Status = artifact.ArtifactStatusReady
	first.DeleteReason = artifact.DeleteReasonCleanup
	first.DeletedAt = time.Unix(99, 0)
	task6MutateAllContainers(t, first)

	repeated, err := store.MarkDeleted(context.Background(), meta.ArtifactRef, artifact.DeleteReasonTTL, time.Unix(20, 456))
	if err != nil {
		t.Fatalf("repeat MarkDeleted(): %v", err)
	}
	if repeated.Status != artifact.ArtifactStatusDeleted || repeated.DeleteReason != artifact.DeleteReasonUser || !repeated.DeletedAt.Equal(firstAt) {
		t.Fatalf("repeat tombstone = (%q, %q, %v), want first (%q, %q, %v)", repeated.Status, repeated.DeleteReason, repeated.DeletedAt, artifact.ArtifactStatusDeleted, artifact.DeleteReasonUser, firstAt)
	}
	task6MutateAllContainers(t, repeated)
	task6AssertStoredRichPristine(t, store, meta.ArtifactRef, artifact.ArtifactStatusDeleted, artifact.DeleteReasonUser, firstAt)
}

func TestMemoryMetadataStoreConcurrentTombstoneFirstWins(t *testing.T) {
	store := NewMemory()
	meta := task6MetadataFixture("art-concurrent-tombstone", "tenant-a", "sess-1", "run-1", time.Unix(1, 0))
	if _, err := store.Create(context.Background(), meta, ""); err != nil {
		t.Fatalf("Create(): %v", err)
	}

	const workers = 48
	type candidate struct {
		reason artifact.DeleteReason
		at     time.Time
	}
	candidates := make([]candidate, workers)
	wantReasonByTime := make(map[int64]artifact.DeleteReason, workers)
	reasons := []artifact.DeleteReason{artifact.DeleteReasonUser, artifact.DeleteReasonTTL, artifact.DeleteReasonCleanup}
	for i := range candidates {
		candidates[i] = candidate{reason: reasons[i%len(reasons)], at: time.Unix(100, int64(i+1))}
		wantReasonByTime[candidates[i].at.UnixNano()] = candidates[i].reason
	}
	type result struct {
		meta *artifact.ArtifactMeta
		err  error
	}
	start := make(chan struct{})
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for i := range candidates {
		candidate := candidates[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := store.MarkDeleted(context.Background(), meta.ArtifactRef, candidate.reason, candidate.at)
			results <- result{meta: got, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var winner *artifact.ArtifactMeta
	for result := range results {
		if result.err != nil || result.meta == nil {
			t.Fatalf("concurrent MarkDeleted() = %#v, error = %v", result.meta, result.err)
		}
		if winner == nil {
			winner = result.meta
		}
		if result.meta.DeleteReason != winner.DeleteReason || !result.meta.DeletedAt.Equal(winner.DeletedAt) {
			t.Fatalf("tombstones did not converge: winner=(%q,%v) got=(%q,%v)", winner.DeleteReason, winner.DeletedAt, result.meta.DeleteReason, result.meta.DeletedAt)
		}
	}
	if wantReason, ok := wantReasonByTime[winner.DeletedAt.UnixNano()]; !ok || wantReason != winner.DeleteReason {
		t.Fatalf("winning tombstone is not one submitted pair: (%q, %v)", winner.DeleteReason, winner.DeletedAt)
	}
	stored, err := store.GetByRef(context.Background(), meta.ArtifactRef)
	if err != nil || stored.DeleteReason != winner.DeleteReason || !stored.DeletedAt.Equal(winner.DeletedAt) {
		t.Fatalf("stored tombstone = %#v, error = %v; winner = (%q, %v)", stored, err, winner.DeleteReason, winner.DeletedAt)
	}
}

func TestMemoryMetadataStoreConcurrentCreateListReadDelete(t *testing.T) {
	store := NewMemory()
	const workers = 32
	metas := make([]artifact.ArtifactMeta, workers)
	wantDeletedAt := make(map[string]time.Time, workers)
	for i := range metas {
		id := fmt.Sprintf("art-concurrent-%02d", i)
		metas[i] = task6MetadataFixture(id, "tenant-a", "sess-1", "run-1", time.Unix(int64(i%4+1), 0))
		wantDeletedAt[id] = time.Unix(1000+int64(i), int64(i))
	}

	start := make(chan struct{})
	errorsFound := make(chan error, workers*4)
	var wg sync.WaitGroup
	for i := range metas {
		meta := metas[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := store.Create(context.Background(), meta, "idem-"+meta.ArtifactID); err != nil {
				errorsFound <- fmt.Errorf("Create(%s): %w", meta.ArtifactID, err)
				return
			}
			if _, err := store.GetByRef(context.Background(), meta.ArtifactRef); err != nil {
				errorsFound <- fmt.Errorf("GetByRef(%s): %w", meta.ArtifactID, err)
			}
			if _, err := store.GetByID(context.Background(), meta.ArtifactID); err != nil {
				errorsFound <- fmt.Errorf("GetByID(%s): %w", meta.ArtifactID, err)
			}
			if _, err := store.List(context.Background(), artifact.ListQuery{IncludeDeleted: true}); err != nil {
				errorsFound <- fmt.Errorf("List(%s): %w", meta.ArtifactID, err)
			}
			if _, err := store.MarkDeleted(context.Background(), meta.ArtifactRef, artifact.DeleteReasonCleanup, wantDeletedAt[meta.ArtifactID]); err != nil {
				errorsFound <- fmt.Errorf("MarkDeleted(%s): %w", meta.ArtifactID, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}

	rows, err := store.List(context.Background(), artifact.ListQuery{IncludeDeleted: true})
	if err != nil || len(rows) != workers {
		t.Fatalf("final List() rows = %d, error = %v; want %d", len(rows), err, workers)
	}
	task6AssertRowsSorted(t, rows)
	wantIDs := task6ArtifactIDs(rows)
	for i := range rows {
		row := rows[i]
		if row.Status != artifact.ArtifactStatusDeleted || row.DeleteReason != artifact.DeleteReasonCleanup || !row.DeletedAt.Equal(wantDeletedAt[row.ArtifactID]) {
			t.Fatalf("row %s tombstone = (%q,%q,%v), want deleted/cleanup/%v", row.ArtifactID, row.Status, row.DeleteReason, row.DeletedAt, wantDeletedAt[row.ArtifactID])
		}
		byRef, refErr := store.GetByRef(context.Background(), row.ArtifactRef)
		byID, idErr := store.GetByID(context.Background(), row.ArtifactID)
		if refErr != nil || idErr != nil || byRef.ArtifactID != row.ArtifactID || byID.ArtifactRef != row.ArtifactRef {
			t.Fatalf("row %s reachability: byRef=%#v err=%v, byID=%#v err=%v", row.ArtifactID, byRef, refErr, byID, idErr)
		}
	}
	for iteration := 0; iteration < 20; iteration++ {
		repeated, err := store.List(context.Background(), artifact.ListQuery{IncludeDeleted: true})
		if err != nil {
			t.Fatalf("repeat final List(%d): %v", iteration, err)
		}
		if gotIDs := task6ArtifactIDs(repeated); !reflect.DeepEqual(gotIDs, wantIDs) {
			t.Fatalf("repeat final List(%d) IDs = %v, want %v", iteration, gotIDs, wantIDs)
		}
	}
}

func task6ArtifactIDs(rows []artifact.ArtifactMeta) []string {
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].ArtifactID
	}
	return ids
}

func task6AssertRowsSorted(t *testing.T, rows []artifact.ArtifactMeta) {
	t.Helper()
	for i := 1; i < len(rows); i++ {
		previous, current := rows[i-1], rows[i]
		if current.CreatedAt.Before(previous.CreatedAt) || (current.CreatedAt.Equal(previous.CreatedAt) && current.ArtifactID < previous.ArtifactID) {
			t.Fatalf("rows are not sorted at %d: (%v,%q) before (%v,%q)", i, previous.CreatedAt, previous.ArtifactID, current.CreatedAt, current.ArtifactID)
		}
	}
}

func task6CreateRichMetadata(t *testing.T, id, idempotencyKey string) (*MemoryMetadataStore, artifact.ArtifactMeta, *artifact.ArtifactMeta) {
	t.Helper()
	store := NewMemory()
	meta := task6RichMetadataFixture(id, "tenant-a", "sess-1", "run-1", time.Unix(1, 0))
	created, err := store.Create(context.Background(), meta, idempotencyKey)
	if err != nil {
		t.Fatalf("Create(%s): %v", id, err)
	}
	return store, meta, created
}

func task6RichMetadataFixture(id, tenantID, sessionID, runID string, createdAt time.Time) artifact.ArtifactMeta {
	meta := task6MetadataFixture(id, tenantID, sessionID, runID, createdAt)
	cycleMap := make(map[string]any)
	cycleMap["self"] = cycleMap
	cycleMap["items"] = []int{7, 8}
	cycleSlice := make([]any, 2)
	cycleSlice[0] = cycleSlice
	cycleSlice[1] = map[string]string{"value": "original"}
	meta.Preview.Fields = map[string]any{
		"top":           "original",
		"string_map":    map[string]string{"value": "original"},
		"any_map":       map[any]any{"strings": []string{"alpha", "beta"}},
		"any_slice":     []any{map[string]any{"ints": []int{1, 2}}},
		"strings":       []string{"one", "two"},
		"bytes":         []byte{1, 2},
		"ints":          []int{3, 4},
		"named_map":     task6NamedMap{"numbers": {5, 6}},
		"named_slice":   task6NamedSlice{{"value": "original"}},
		"named_array":   task6NamedArray{[]int{9, 10}, []int{11, 12}},
		"nil_strings":   []string(nil),
		"empty_strings": make([]string, 0),
		"nil_map":       map[string]string(nil),
		"empty_map":     make(map[string]string),
		"cycle_map":     cycleMap,
		"cycle_slice":   cycleSlice,
	}
	return meta
}

func task6MutateAllContainers(t *testing.T, meta *artifact.ArtifactMeta) {
	t.Helper()
	if meta == nil || len(meta.DerivedFrom) == 0 || meta.Metadata == nil || meta.Preview.Fields == nil {
		t.Fatalf("metadata lacks owned containers: %#v", meta)
	}
	meta.DerivedFrom[0].ArtifactRef = "mutated-lineage"
	meta.DerivedFrom = append(meta.DerivedFrom, artifact.ArtifactLineage{ArtifactRef: "added-lineage"})
	meta.Metadata["owner"] = "mutated-owner"
	meta.Metadata["added"] = "mutated"
	fields := meta.Preview.Fields
	fields["top"] = "mutated-top"
	fields["added"] = "mutated"
	fields["string_map"].(map[string]string)["value"] = "mutated"
	fields["any_map"].(map[any]any)["strings"].([]string)[0] = "mutated"
	fields["any_slice"].([]any)[0].(map[string]any)["ints"].([]int)[0] = 99
	fields["strings"].([]string)[0] = "mutated"
	fields["bytes"].([]byte)[0] = 99
	fields["ints"].([]int)[0] = 99
	fields["named_map"].(task6NamedMap)["numbers"][0] = 99
	fields["named_slice"].(task6NamedSlice)[0]["value"] = "mutated"
	fields["named_array"].(task6NamedArray)[0][0] = 99
	cycleMap := fields["cycle_map"].(map[string]any)
	cycleMap["items"].([]int)[0] = 99
	cycleMap["self"].(map[string]any)["added"] = "mutated"
	cycleSlice := fields["cycle_slice"].([]any)
	cycleSlice[0].([]any)[1] = map[string]string{"value": "mutated"}
}

func task6AssertStoredRichPristine(t *testing.T, store *MemoryMetadataStore, ref string, status artifact.ArtifactStatus, reason artifact.DeleteReason, deletedAt time.Time) {
	t.Helper()
	got, err := store.GetByRef(context.Background(), ref)
	if err != nil {
		t.Fatalf("GetByRef(%q): %v", ref, err)
	}
	if got.Status != status || got.DeleteReason != reason || !got.DeletedAt.Equal(deletedAt) {
		t.Fatalf("tombstone = (%q, %q, %v), want (%q, %q, %v)", got.Status, got.DeleteReason, got.DeletedAt, status, reason, deletedAt)
	}
	task6AssertRichContainersPristine(t, got)
}

func task6AssertRichContainersPristine(t *testing.T, got *artifact.ArtifactMeta) {
	t.Helper()
	if got == nil {
		t.Fatal("metadata is nil")
	}
	wantLineage := artifact.BuildRef(got.TenantID, got.SessionID, got.RunID, got.ArtifactID+"-source")
	if len(got.DerivedFrom) != 1 || got.DerivedFrom[0].ArtifactRef != wantLineage || got.DerivedFrom[0].Relation != artifact.LineageGeneratedFrom {
		t.Fatalf("DerivedFrom = %#v, want original lineage %q", got.DerivedFrom, wantLineage)
	}
	if len(got.Metadata) != 1 || got.Metadata["owner"] != "original-owner" {
		t.Fatalf("Metadata = %#v, want original owner only", got.Metadata)
	}
	fields := got.Preview.Fields
	if len(fields) != 16 || fields["top"] != "original" {
		t.Fatalf("Preview top-level fields mutated: len=%d top=%#v", len(fields), fields["top"])
	}
	if value, ok := fields["string_map"].(map[string]string); !ok || value["value"] != "original" {
		t.Fatalf("string_map = %#v", fields["string_map"])
	}
	if value, ok := fields["any_map"].(map[any]any); !ok {
		t.Fatalf("any_map type = %T", fields["any_map"])
	} else if stringsValue, ok := value["strings"].([]string); !ok || len(stringsValue) != 2 || stringsValue[0] != "alpha" {
		t.Fatalf("any_map strings = %#v", value["strings"])
	}
	if value, ok := fields["any_slice"].([]any); !ok || len(value) != 1 {
		t.Fatalf("any_slice = %#v", fields["any_slice"])
	} else if nested, ok := value[0].(map[string]any); !ok {
		t.Fatalf("any_slice nested type = %T", value[0])
	} else if ints, ok := nested["ints"].([]int); !ok || len(ints) != 2 || ints[0] != 1 {
		t.Fatalf("any_slice nested ints = %#v", nested["ints"])
	}
	if value, ok := fields["strings"].([]string); !ok || len(value) != 2 || value[0] != "one" {
		t.Fatalf("strings = %#v", fields["strings"])
	}
	if value, ok := fields["bytes"].([]byte); !ok || len(value) != 2 || value[0] != 1 {
		t.Fatalf("bytes = %#v", fields["bytes"])
	}
	if value, ok := fields["ints"].([]int); !ok || len(value) != 2 || value[0] != 3 {
		t.Fatalf("ints = %#v", fields["ints"])
	}
	if value, ok := fields["named_map"].(task6NamedMap); !ok || len(value["numbers"]) != 2 || value["numbers"][0] != 5 {
		t.Fatalf("named_map type/value = %T %#v", fields["named_map"], fields["named_map"])
	}
	if value, ok := fields["named_slice"].(task6NamedSlice); !ok || len(value) != 1 || value[0]["value"] != "original" {
		t.Fatalf("named_slice type/value = %T %#v", fields["named_slice"], fields["named_slice"])
	}
	if value, ok := fields["named_array"].(task6NamedArray); !ok || value[0][0] != 9 || value[1][0] != 11 {
		t.Fatalf("named_array type/value = %T %#v", fields["named_array"], fields["named_array"])
	}
	if value, ok := fields["nil_strings"].([]string); !ok || value != nil {
		t.Fatalf("nil_strings type/value = %T %#v", fields["nil_strings"], fields["nil_strings"])
	}
	if value, ok := fields["empty_strings"].([]string); !ok || value == nil || len(value) != 0 {
		t.Fatalf("empty_strings type/value = %T %#v", fields["empty_strings"], fields["empty_strings"])
	}
	if value, ok := fields["nil_map"].(map[string]string); !ok || value != nil {
		t.Fatalf("nil_map type/value = %T %#v", fields["nil_map"], fields["nil_map"])
	}
	if value, ok := fields["empty_map"].(map[string]string); !ok || value == nil || len(value) != 0 {
		t.Fatalf("empty_map type/value = %T %#v", fields["empty_map"], fields["empty_map"])
	}
	cycleMap, ok := fields["cycle_map"].(map[string]any)
	if !ok {
		t.Fatalf("cycle_map type = %T", fields["cycle_map"])
	}
	cycleItems, ok := cycleMap["items"].([]int)
	if !ok || len(cycleItems) != 2 || cycleItems[0] != 7 {
		t.Fatalf("cycle_map items type/value = %T, len=%d", cycleMap["items"], len(cycleItems))
	}
	selfMap, ok := cycleMap["self"].(map[string]any)
	if !ok {
		t.Fatalf("cycle_map self type = %T", cycleMap["self"])
	}
	selfMap["cycle-probe"] = true
	if cycleMap["cycle-probe"] != true {
		t.Fatal("cycle_map self reference does not target cloned map")
	}
	delete(selfMap, "cycle-probe")
	cycleSlice, ok := fields["cycle_slice"].([]any)
	if !ok || len(cycleSlice) != 2 {
		t.Fatalf("cycle_slice type/len = %T/%d", fields["cycle_slice"], len(cycleSlice))
	}
	selfSlice, ok := cycleSlice[0].([]any)
	if !ok {
		t.Fatalf("cycle_slice self type = %T", cycleSlice[0])
	}
	originalSecond := selfSlice[1]
	selfSlice[1] = "cycle-probe"
	if cycleSlice[1] != "cycle-probe" {
		t.Fatal("cycle_slice self reference does not target cloned slice")
	}
	selfSlice[1] = originalSecond
	if value, ok := cycleSlice[1].(map[string]string); !ok || value["value"] != "original" {
		t.Fatalf("cycle_slice nested value = %#v", cycleSlice[1])
	}
}

func task6MetadataFixture(id, tenantID, sessionID, runID string, createdAt time.Time) artifact.ArtifactMeta {
	content := []byte("content:" + tenantID + ":" + sessionID + ":" + runID + ":" + id)
	sum := sha256.Sum256(content)
	return artifact.ArtifactMeta{
		ArtifactID:      id,
		ArtifactRef:     artifact.BuildRef(tenantID, sessionID, runID, id),
		TenantID:        tenantID,
		UserID:          "user-1",
		SessionID:       sessionID,
		RunID:           runID,
		OwnerModule:     artifact.OwnerModuleToolGateway,
		OwnerID:         "tool-call-1",
		ArtifactType:    artifact.ArtifactTypeToolResult,
		MimeType:        "application/json",
		Name:            id + ".json",
		SizeBytes:       int64(len(content)),
		Hash:            "sha256:" + hex.EncodeToString(sum[:]),
		Visibility:      artifact.VisibilityInternal,
		StorageBackend:  "memory",
		StorageKey:      "tenants/" + tenantID + "/sessions/" + sessionID + "/runs/" + runID + "/" + id,
		Preview:         artifact.Preview{Text: "preview", Fields: map[string]any{"top": "original", "typed": map[string][]int{"values": {1, 2}}}},
		RetentionPolicy: artifact.RetentionRunTTL,
		ExpiresAt:       createdAt.Add(24 * time.Hour),
		CreatedBy:       "tool_gateway:tool-call-1",
		CreatedAt:       createdAt,
		Status:          artifact.ArtifactStatusReady,
		DerivedFrom: []artifact.ArtifactLineage{{
			ArtifactRef: artifact.BuildRef(tenantID, sessionID, runID, id+"-source"),
			Relation:    artifact.LineageGeneratedFrom,
		}},
		SchemaVersion: artifact.ArtifactMetaSchemaVersion,
		Metadata:      map[string]string{"owner": "original-owner"},
	}
}

func task6AssertBasicContainersPristine(t *testing.T, got *artifact.ArtifactMeta) {
	t.Helper()
	if len(got.DerivedFrom) != 1 || got.DerivedFrom[0].ArtifactRef == "mutated-lineage" {
		t.Fatalf("DerivedFrom mutated: %#v", got.DerivedFrom)
	}
	if got.Metadata["owner"] != "original-owner" {
		t.Fatalf("Metadata = %#v, want original owner", got.Metadata)
	}
	if got.Preview.Fields["top"] != "original" {
		t.Fatalf("Preview top = %#v, want original", got.Preview.Fields["top"])
	}
	typed, ok := got.Preview.Fields["typed"].(map[string][]int)
	if !ok || len(typed["values"]) != 2 || typed["values"][0] != 1 {
		t.Fatalf("typed preview = %#v, want map[string][]int with original values", got.Preview.Fields["typed"])
	}
}
