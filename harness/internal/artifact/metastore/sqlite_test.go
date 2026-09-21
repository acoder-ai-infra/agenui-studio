package metastore_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
)

func openSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func sampleMeta(ref, id string) artifact.ArtifactMeta {
	return artifact.ArtifactMeta{
		ArtifactID:      id,
		ArtifactRef:     ref,
		TenantID:        "public",
		SessionID:       "sess1",
		RunID:           "run1",
		OwnerModule:     artifact.OwnerModuleRuntime,
		OwnerID:         "owner1",
		ArtifactType:    artifact.ArtifactTypeContextSnapshot,
		MimeType:        "application/json",
		SizeBytes:       10,
		Hash:            "sha256:deadbeef",
		Visibility:      artifact.VisibilityInternal,
		StorageBackend:  "file",
		StorageKey:      "tenants/public/x",
		RetentionPolicy: artifact.RetentionRunTTL,
		CreatedBy:       "test",
		CreatedAt:       time.Unix(1700000000, 0).UTC(),
		Status:          artifact.ArtifactStatusReady,
		SchemaVersion:   "harness.artifact.v1",
		Metadata:        map[string]string{"k": "v"},
	}
}

func TestSQLiteMetadataStoreCRUDAndIdempotency(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t, filepath.Join(t.TempDir(), "meta.db"))
	store, err := metastore.NewSQLiteMetadataStore(db)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	meta := sampleMeta("artifact://a", "art_a")
	created, err := store.Create(ctx, meta, "idem-1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ArtifactRef != meta.ArtifactRef || created.Metadata["k"] != "v" {
		t.Fatalf("create returned unexpected meta: %+v", created)
	}

	// Idempotent replay returns the same artifact, no conflict.
	replay, err := store.Create(ctx, sampleMeta("artifact://different", "art_diff"), "idem-1")
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if replay.ArtifactRef != "artifact://a" {
		t.Fatalf("idempotent replay returned %q, want artifact://a", replay.ArtifactRef)
	}

	// Duplicate ref without idempotency key conflicts.
	if _, err := store.Create(ctx, sampleMeta("artifact://a", "art_other"), ""); !artifact.IsErrorCode(err, artifact.ErrConflict) {
		t.Fatalf("duplicate ref error = %v, want conflict", err)
	}

	byRef, err := store.GetByRef(ctx, "artifact://a")
	if err != nil || byRef.ArtifactID != "art_a" {
		t.Fatalf("GetByRef = %+v, %v", byRef, err)
	}
	byID, err := store.GetByID(ctx, "art_a")
	if err != nil || byID.ArtifactRef != "artifact://a" {
		t.Fatalf("GetByID = %+v, %v", byID, err)
	}
	byIdem, err := store.GetByIdempotencyKey(ctx, "idem-1")
	if err != nil || byIdem.ArtifactRef != "artifact://a" {
		t.Fatalf("GetByIdempotencyKey = %+v, %v", byIdem, err)
	}
	if _, err := store.GetByRef(ctx, "artifact://missing"); !artifact.IsErrorCode(err, artifact.ErrNotFound) {
		t.Fatalf("missing ref error = %v, want not_found", err)
	}
}

func TestSQLiteMetadataStoreList(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t, filepath.Join(t.TempDir(), "meta.db"))
	store, _ := metastore.NewSQLiteMetadataStore(db)

	a := sampleMeta("artifact://a", "art_a")
	a.CreatedAt = time.Unix(1, 0)
	b := sampleMeta("artifact://b", "art_b")
	b.CreatedAt = time.Unix(2, 0)
	b.RunID = "run2"
	if _, err := store.Create(ctx, a, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, b, ""); err != nil {
		t.Fatal(err)
	}

	all, err := store.List(ctx, artifact.ListQuery{TenantID: "public"})
	if err != nil || len(all) != 2 {
		t.Fatalf("list all = %d, %v", len(all), err)
	}
	if all[0].ArtifactRef != "artifact://a" || all[1].ArtifactRef != "artifact://b" {
		t.Fatalf("list order wrong: %s, %s", all[0].ArtifactRef, all[1].ArtifactRef)
	}
	scoped, err := store.List(ctx, artifact.ListQuery{RunID: "run2"})
	if err != nil || len(scoped) != 1 || scoped[0].ArtifactRef != "artifact://b" {
		t.Fatalf("scoped list = %+v, %v", scoped, err)
	}
}

func TestSQLiteMetadataStoreMarkDeletedAndDurability(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "meta.db")
	db := openSQLite(t, path)
	store, _ := metastore.NewSQLiteMetadataStore(db)

	if _, err := store.Create(ctx, sampleMeta("artifact://a", "art_a"), ""); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000100, 0).UTC()
	deleted, err := store.MarkDeleted(ctx, "artifact://a", artifact.DeleteReasonCleanup, now)
	if err != nil || deleted.Status != artifact.ArtifactStatusDeleted || deleted.PurgeStatus != artifact.PurgeStatusPending {
		t.Fatalf("MarkDeleted = %+v, %v", deleted, err)
	}
	// Idempotent second delete.
	if again, err := store.MarkDeleted(ctx, "artifact://a", artifact.DeleteReasonCleanup, now); err != nil || again.Status != artifact.ArtifactStatusDeleted {
		t.Fatalf("second MarkDeleted = %+v, %v", again, err)
	}

	// Durability: reopen the same file with a fresh store — the row must survive,
	// which is the entire point of this backend (resume across restart).
	_ = db.Close()
	reopened := openSQLite(t, path)
	store2, err := metastore.NewSQLiteMetadataStore(reopened)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	got, err := store2.GetByRef(ctx, "artifact://a")
	if err != nil {
		t.Fatalf("GetByRef after reopen: %v", err)
	}
	if got.Status != artifact.ArtifactStatusDeleted || got.CreatedAt.IsZero() {
		t.Fatalf("reopened meta lost fidelity: %+v", got)
	}
}

func TestSQLiteMetadataStorePurgeLifecycle(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t, filepath.Join(t.TempDir(), "meta.db"))
	store, _ := metastore.NewSQLiteMetadataStore(db)

	if _, err := store.Create(ctx, sampleMeta("artifact://a", "art_a"), ""); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000200, 0).UTC()
	_, job, err := store.RequestPurge(ctx, "artifact://a", artifact.DeleteReasonTTL, now)
	if err != nil || job.Status != artifact.PurgeStatusPending {
		t.Fatalf("RequestPurge = %+v, %v", job, err)
	}
	pending, err := store.ListPendingPurges(ctx, artifact.PurgeQuery{ReadyAt: now})
	if err != nil || len(pending) != 1 {
		t.Fatalf("ListPendingPurges = %d, %v", len(pending), err)
	}
	claimed, ok, err := store.ClaimPurge(ctx, "artifact://a", "worker-1", now, now.Add(time.Minute))
	if err != nil || !ok || claimed.Status != artifact.PurgeStatusLeased {
		t.Fatalf("ClaimPurge = %+v, ok=%v, %v", claimed, ok, err)
	}
	// A stale lease version is rejected.
	if _, err := store.MarkPurgeSucceeded(ctx, "artifact://a", "worker-1", claimed.LeaseVersion-1, now); !artifact.IsErrorCode(err, artifact.ErrConflict) {
		t.Fatalf("stale lease succeed error = %v, want conflict", err)
	}
	meta, err := store.MarkPurgeSucceeded(ctx, "artifact://a", "worker-1", claimed.LeaseVersion, now)
	if err != nil || meta.PurgeStatus != artifact.PurgeStatusPurged {
		t.Fatalf("MarkPurgeSucceeded = %+v, %v", meta, err)
	}
}

// TestSQLiteMetadataStoreCreateReturnsFaithfulClone guards the invariant that
// the artifact Store enforces with reflect.DeepEqual(created, original): Create
// must return the exact in-memory input, not a JSON round-trip (which would
// strip time.Time monotonic clocks and coerce Preview.Fields number types).
func TestSQLiteMetadataStoreCreateReturnsFaithfulClone(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t, filepath.Join(t.TempDir(), "meta.db"))
	store, _ := metastore.NewSQLiteMetadataStore(db)

	meta := sampleMeta("artifact://a", "art_a")
	meta.CreatedAt = time.Now() // carries a monotonic clock reading
	meta.Preview = artifact.Preview{Fields: map[string]any{"count": 3, "label": "x"}}

	created, err := store.Create(ctx, meta, "idem-1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !reflect.DeepEqual(created, &meta) {
		t.Fatalf("Create result not DeepEqual to input:\n got=%#v\nwant=%#v", created, &meta)
	}
}

func TestNewSQLiteMetadataStoreRejectsNilDB(t *testing.T) {
	if _, err := metastore.NewSQLiteMetadataStore(nil); !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
		t.Fatalf("nil db error = %v, want invalid_argument", err)
	}
}
