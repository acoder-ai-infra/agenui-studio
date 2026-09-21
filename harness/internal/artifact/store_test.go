package artifact_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/objectstore"
)

func TestStorePutHeadAndGetTextArtifact(t *testing.T) {
	ctx := runtimeContext()
	store := newTestStore(t)

	meta, err := store.Put(ctx, PutArtifactRequest{
		TenantID:        "tenant-a",
		UserID:          "user-1",
		SessionID:       "sess-1",
		RunID:           "run-1",
		OwnerModule:     OwnerModuleToolGateway,
		OwnerID:         "tool-call-1",
		ArtifactType:    ArtifactTypeToolResult,
		MimeType:        "text/plain",
		Name:            "result.txt",
		Visibility:      VisibilityInternal,
		RetentionPolicy: RetentionRunTTL,
		Content:         strings.NewReader("hello artifact store"),
	})
	if err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	if meta.ArtifactRef == "" || meta.StorageKey == "" {
		t.Fatalf("expected ref and storage key: %#v", meta)
	}
	if meta.SizeBytes != int64(len("hello artifact store")) {
		t.Fatalf("unexpected size: %d", meta.SizeBytes)
	}
	if meta.Hash == "" || !strings.HasPrefix(meta.Hash, "sha256:") {
		t.Fatalf("unexpected hash: %s", meta.Hash)
	}
	if meta.Preview.Text != "hello artifact store" || meta.Preview.Truncated {
		t.Fatalf("unexpected preview: %#v", meta.Preview)
	}
	if meta.ExpiresAt.IsZero() {
		t.Fatal("expected expires_at from retention policy")
	}
	if meta.SchemaVersion != ArtifactMetaSchemaVersion {
		t.Fatalf("unexpected schema version: %s", meta.SchemaVersion)
	}

	head, err := store.Head(ctx, meta.ArtifactRef)
	if err != nil {
		t.Fatalf("head artifact: %v", err)
	}
	if head.ArtifactID != meta.ArtifactID {
		t.Fatalf("head returned different artifact: %#v", head)
	}

	obj, err := store.Get(ctx, meta.ArtifactRef, GetOptions{Purpose: PurposeModelContext})
	if err != nil {
		t.Fatalf("get artifact: %v", err)
	}
	defer obj.Content.Close()
	got, err := io.ReadAll(obj.Content)
	if err != nil {
		t.Fatalf("read content: %v", err)
	}
	if string(got) != "hello artifact store" {
		t.Fatalf("unexpected content: %s", got)
	}
}

func TestStorePutJSONPreview(t *testing.T) {
	ctx := actorContext()
	store := newTestStore(t)

	meta, err := store.Put(ctx, PutArtifactRequest{
		TenantID:     "tenant-a",
		UserID:       "user-1",
		SessionID:    "sess-1",
		RunID:        "run-1",
		OwnerModule:  OwnerModuleContextEngine,
		OwnerID:      "context-1",
		ArtifactType: ArtifactTypeContextSnapshot,
		MimeType:     "application/json",
		Visibility:   VisibilityInternal,
		Content:      strings.NewReader(`{"count":2,"source":"kb","items":[1,2]}`),
	})
	if err != nil {
		t.Fatalf("put json artifact: %v", err)
	}
	if meta.Preview.Fields["count"] != float64(2) || meta.Preview.Fields["source"] != "kb" {
		t.Fatalf("unexpected json preview fields: %#v", meta.Preview.Fields)
	}
	if !strings.Contains(meta.Preview.Text, "JSON") {
		t.Fatalf("expected json preview text: %#v", meta.Preview)
	}
}

func TestStorePersistsSanitizedBinaryNameWithoutChangingBytesOrHash(t *testing.T) {
	store := newTestStore(t)
	raw := []byte("token=secret original binary bytes")
	req := validPutRequest()
	req.Name = `C:\private\TOKEN=secret.bin`
	req.MimeType = "application/octet-stream"
	req.Visibility = VisibilityUserVisible
	req.Content = bytes.NewReader(raw)

	meta, err := store.Put(runtimeContext(), req)
	if err != nil {
		t.Fatalf("Put(): %v", err)
	}
	if meta.Name != "TOKEN=***" || meta.Preview.Fields["name"] != meta.Name {
		t.Fatalf("metadata persisted unsafe/inconsistent name: %#v", meta)
	}
	if strings.Contains(meta.Name, "private") || strings.Contains(meta.Name, "secret") || strings.ContainsAny(meta.Name, `/\`) {
		t.Fatalf("metadata name leaked raw path/value: %q", meta.Name)
	}
	object, err := store.Get(runtimeContext(), meta.ArtifactRef, GetOptions{Purpose: PurposeView})
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	got, err := io.ReadAll(object.Content)
	if err != nil {
		t.Fatalf("read object: %v", err)
	}
	if err := object.Content.Close(); err != nil {
		t.Fatalf("close object: %v", err)
	}
	sum := sha256.Sum256(raw)
	wantHash := "sha256:" + hex.EncodeToString(sum[:])
	if !bytes.Equal(got, raw) || meta.SizeBytes != int64(len(raw)) || meta.Hash != wantHash || meta.Preview.Fields["hash"] != wantHash {
		t.Fatalf("source bytes/hash changed: got=%q meta=%#v", got, meta)
	}
}

func TestStorePutIsIdempotent(t *testing.T) {
	ctx := actorContext()
	store := newTestStore(t)
	req := PutArtifactRequest{
		TenantID:       "tenant-a",
		UserID:         "user-1",
		SessionID:      "sess-1",
		RunID:          "run-1",
		OwnerModule:    OwnerModuleToolGateway,
		OwnerID:        "tool-call-1",
		ArtifactType:   ArtifactTypeToolResult,
		MimeType:       "text/plain",
		Visibility:     VisibilityInternal,
		Content:        strings.NewReader("same content"),
		IdempotencyKey: "run-1:tool-call-1:result",
	}

	first, err := store.Put(ctx, req)
	if err != nil {
		t.Fatalf("first put: %v", err)
	}
	req.Content = strings.NewReader("same content")
	second, err := store.Put(ctx, req)
	if err != nil {
		t.Fatalf("second put: %v", err)
	}
	if second.ArtifactID != first.ArtifactID {
		t.Fatalf("expected same artifact on retry: first=%s second=%s", first.ArtifactID, second.ArtifactID)
	}

	req.Content = strings.NewReader("different content")
	_, err = store.Put(ctx, req)
	if !IsErrorCode(err, ErrConflict) {
		t.Fatalf("expected conflict for reused idempotency key with different content, got %v", err)
	}
}

func TestStoreConcurrentIdempotentPutCleansDuplicateObject(t *testing.T) {
	ctx := actorContext()
	objectStore := newTrackingObjectStore()
	store := NewStore(StoreConfig{
		ObjectStore:    objectStore,
		MetadataStore:  metastore.NewMemory(),
		Clock:          fixedClock{now: time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)},
		MaxObjectBytes: 1024 * 1024,
	})

	const workers = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	metas := make([]*ArtifactMeta, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			metas[i], errs[i] = store.Put(ctx, PutArtifactRequest{
				TenantID:       "tenant-a",
				UserID:         "user-1",
				SessionID:      "sess-1",
				RunID:          "run-1",
				OwnerModule:    OwnerModuleToolGateway,
				OwnerID:        "tool-call-1",
				ArtifactType:   ArtifactTypeToolResult,
				MimeType:       "text/plain",
				Visibility:     VisibilityInternal,
				Content:        strings.NewReader("same concurrent content"),
				IdempotencyKey: "run-1:tool-call-1:concurrent",
			})
		}(i)
	}
	close(start)
	wg.Wait()

	var artifactID string
	for i, err := range errs {
		if err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
		if metas[i] == nil {
			t.Fatalf("missing meta %d", i)
		}
		if artifactID == "" {
			artifactID = metas[i].ArtifactID
			continue
		}
		if metas[i].ArtifactID != artifactID {
			t.Fatalf("expected same artifact id from idempotent puts: first=%s got=%s", artifactID, metas[i].ArtifactID)
		}
	}
	if objectStore.LiveObjects() != 1 {
		t.Fatalf("expected duplicate object writes to be cleaned, live=%d", objectStore.LiveObjects())
	}
}

func TestIdempotencyRaceConvergesDeterministically(t *testing.T) {
	objectStore := newTrackingObjectStore()
	metadataStore := newBarrierMissMetadataStore(metastore.NewMemory(), 2)
	store := NewStore(StoreConfig{
		ObjectStore:    objectStore,
		MetadataStore:  metadataStore,
		Clock:          fixedClock{now: time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)},
		MaxObjectBytes: 1024 * 1024,
	})

	results := runTwoTask3IdempotentPuts(store)
	for i, result := range results {
		if result.err != nil {
			t.Fatalf("Put() result %d error = %v", i, result.err)
		}
		if result.meta == nil {
			t.Fatalf("Put() result %d metadata is nil", i)
		}
	}
	if results[0].meta.ArtifactID != results[1].meta.ArtifactID {
		t.Fatalf("ArtifactIDs = %q and %q, want convergence", results[0].meta.ArtifactID, results[1].meta.ArtifactID)
	}
	winnerKey := results[0].meta.StorageKey
	if results[1].meta.StorageKey != winnerKey {
		t.Fatalf("authoritative StorageKeys = %q and %q, want one winner", winnerKey, results[1].meta.StorageKey)
	}
	if objectStore.PutCalls() != 2 || objectStore.DeleteCalls() != 1 || objectStore.LiveObjects() != 1 {
		t.Fatalf("object calls/live = put %d delete %d live %d, want 2/1/1",
			objectStore.PutCalls(), objectStore.DeleteCalls(), objectStore.LiveObjects())
	}
	putKeys := objectStore.PutKeys()
	deleteKeys := objectStore.DeleteKeys()
	liveKeys := objectStore.LiveKeys()
	loserKey := task3LoserObjectKey(t, putKeys, winnerKey)
	if len(deleteKeys) != 1 || deleteKeys[0] != loserKey || deleteKeys[0] == winnerKey {
		t.Fatalf("deleted keys = %#v, want loser %q and not winner %q", deleteKeys, loserKey, winnerKey)
	}
	if len(liveKeys) != 1 || liveKeys[0] != winnerKey {
		t.Fatalf("live keys = %#v, want authoritative winner %q", liveKeys, winnerKey)
	}
}

func TestIdempotencyRaceLoserSurfacesObjectCleanupFailure(t *testing.T) {
	cleanupErr := errors.New("task 3 loser cleanup failed")
	objectStore := newTrackingObjectStore()
	objectStore.deleteErr = cleanupErr
	metadataStore := newBarrierMissMetadataStore(metastore.NewMemory(), 2)
	store := NewStore(StoreConfig{
		ObjectStore:    objectStore,
		MetadataStore:  metadataStore,
		Clock:          fixedClock{now: time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)},
		MaxObjectBytes: 1024 * 1024,
	})

	results := runTwoTask3IdempotentPuts(store)
	var successes int
	var loserErr error
	var winner *ArtifactMeta
	for _, result := range results {
		if result.err == nil {
			successes++
			if result.meta == nil {
				t.Fatal("successful Put() returned nil metadata")
			}
			winner = result.meta
			continue
		}
		if result.meta != nil {
			t.Fatalf("failed Put() returned metadata %#v", result.meta)
		}
		loserErr = result.err
	}
	if successes != 1 || loserErr == nil {
		t.Fatalf("race results = %#v, want one success and one failure", results)
	}
	if !IsErrorCode(loserErr, ErrConflict) {
		t.Fatalf("loser error = %v, want conflict", loserErr)
	}
	if !errors.Is(loserErr, cleanupErr) {
		t.Fatalf("loser error = %v, want cleanup cause %v", loserErr, cleanupErr)
	}
	if !strings.Contains(loserErr.Error(), "duplicate artifact object cleanup") {
		t.Fatalf("loser error = %v, want duplicate cleanup context", loserErr)
	}
	if objectStore.PutCalls() != 2 || objectStore.DeleteCalls() != 1 || objectStore.LiveObjects() != 2 {
		t.Fatalf("object calls/live = put %d delete %d live %d, want 2/1/2",
			objectStore.PutCalls(), objectStore.DeleteCalls(), objectStore.LiveObjects())
	}
	putKeys := objectStore.PutKeys()
	deleteKeys := objectStore.DeleteKeys()
	liveKeys := objectStore.LiveKeys()
	loserKey := task3LoserObjectKey(t, putKeys, winner.StorageKey)
	if len(deleteKeys) != 1 || deleteKeys[0] != loserKey || deleteKeys[0] == winner.StorageKey {
		t.Fatalf("attempted delete keys = %#v, want loser %q and not winner %q", deleteKeys, loserKey, winner.StorageKey)
	}
	if !task3ContainsKey(liveKeys, winner.StorageKey) || !task3ContainsKey(liveKeys, loserKey) {
		t.Fatalf("live keys = %#v, want winner %q and unresolved loser %q", liveKeys, winner.StorageKey, loserKey)
	}
}

func TestStoreValidatesDerivedFromRefs(t *testing.T) {
	ctx := runtimeContext()
	store := newTestStore(t)

	_, err := store.Put(ctx, PutArtifactRequest{
		TenantID:     "tenant-a",
		UserID:       "user-1",
		SessionID:    "sess-1",
		RunID:        "run-1",
		OwnerModule:  OwnerModuleToolGateway,
		OwnerID:      "tool-call-2",
		ArtifactType: ArtifactTypeToolResult,
		MimeType:     "text/plain",
		Visibility:   VisibilityInternal,
		Content:      strings.NewReader("child"),
		DerivedFrom: []ArtifactLineage{{
			ArtifactRef: BuildRef("tenant-a", "sess-1", "run-1", "missing"),
			Relation:    LineageTransformedFrom,
		}},
	})
	if !IsErrorCode(err, ErrNotFound) {
		t.Fatalf("expected missing lineage ref to fail, got %v", err)
	}

	parent, err := store.Put(ctx, PutArtifactRequest{
		TenantID:     "tenant-a",
		UserID:       "user-1",
		SessionID:    "sess-1",
		RunID:        "run-1",
		OwnerModule:  OwnerModuleToolGateway,
		OwnerID:      "tool-call-1",
		ArtifactType: ArtifactTypeFile,
		MimeType:     "text/csv",
		Visibility:   VisibilityInternal,
		Content:      strings.NewReader("a,b\n1,2"),
	})
	if err != nil {
		t.Fatalf("put parent: %v", err)
	}
	child, err := store.Put(ctx, PutArtifactRequest{
		TenantID:     "tenant-a",
		UserID:       "user-1",
		SessionID:    "sess-1",
		RunID:        "run-1",
		OwnerModule:  OwnerModuleToolGateway,
		OwnerID:      "tool-call-2",
		ArtifactType: ArtifactTypeToolResult,
		MimeType:     "text/csv",
		Visibility:   VisibilityInternal,
		Content:      strings.NewReader("a,b\n1,2"),
		DerivedFrom: []ArtifactLineage{{
			ArtifactRef: parent.ArtifactRef,
			Relation:    LineageTransformedFrom,
		}},
	})
	if err != nil {
		t.Fatalf("put child: %v", err)
	}
	if len(child.DerivedFrom) != 1 || child.DerivedFrom[0].ArtifactRef != parent.ArtifactRef {
		t.Fatalf("unexpected lineage: %#v", child.DerivedFrom)
	}
}

func TestStoreRejectsLineageRefMetadataIdentityMismatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ArtifactMeta)
	}{
		{name: "artifact ref", mutate: func(meta *ArtifactMeta) {
			meta.ArtifactRef = BuildRef(meta.TenantID, meta.SessionID, meta.RunID, "art-other")
		}},
		{name: "artifact id", mutate: func(meta *ArtifactMeta) { meta.ArtifactID = "art-other" }},
		{name: "tenant id", mutate: func(meta *ArtifactMeta) { meta.TenantID = "tenant-b" }},
		{name: "session id", mutate: func(meta *ArtifactMeta) { meta.SessionID = "sess-2" }},
		{name: "run id", mutate: func(meta *ArtifactMeta) { meta.RunID = "run-2" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := objectstore.NewMemory()
			metadata := metastore.NewMemory()
			base := NewStore(StoreConfig{ObjectStore: objects, MetadataStore: metadata})
			parent := putTask3Parent(t, base, "identity-parent")
			mutated := *parent
			tt.mutate(&mutated)
			override := &lineageOverrideMetadataStore{
				MetadataStore: metadata,
				requested:     parent.ArtifactRef,
				meta:          mutated,
			}
			store := NewStore(StoreConfig{ObjectStore: objects, MetadataStore: override})
			childReq := validPutRequest()
			childReq.OwnerID = "identity-child"
			childReq.Content = strings.NewReader("child")
			childReq.DerivedFrom = []ArtifactLineage{{ArtifactRef: parent.ArtifactRef, Relation: LineageReferenced}}

			if _, err := store.Put(runtimeContext(), childReq); !IsErrorCode(err, ErrConflict) {
				t.Fatalf("Put() error = %v, want conflict", err)
			}
		})
	}
}

func TestStoreRejectsMalformedLineageRefBeforeMetadataLookup(t *testing.T) {
	refs := []string{
		"artifact://tenants/tenant-a/sessions/sess-1/runs/run-1/art?query=1",
		"artifact://tenants/tenant-a/sessions/sess-1/runs/run-1/%61rt_parent",
	}
	for _, ref := range refs {
		t.Run(ref, func(t *testing.T) {
			metadata := &recordingMetadataStore{MetadataStore: metastore.NewMemory()}
			store := NewStore(StoreConfig{ObjectStore: objectstore.NewMemory(), MetadataStore: metadata})
			childReq := validPutRequest()
			childReq.DerivedFrom = []ArtifactLineage{{
				ArtifactRef: ref,
				Relation:    LineageReferenced,
			}}

			if _, err := store.Put(runtimeContext(), childReq); !IsErrorCode(err, ErrInvalidArgument) {
				t.Fatalf("Put() error = %v, want invalid_argument", err)
			}
			if metadata.getByRefCalls != 0 {
				t.Fatalf("GetByRef() calls = %d, want zero", metadata.getByRefCalls)
			}
		})
	}
}

func TestStoreRejectsCrossTenantLineageBeforeMetadataLookup(t *testing.T) {
	metadata := &recordingMetadataStore{MetadataStore: metastore.NewMemory()}
	store := NewStore(StoreConfig{ObjectStore: objectstore.NewMemory(), MetadataStore: metadata})
	childReq := validPutRequest()
	childReq.DerivedFrom = []ArtifactLineage{{
		ArtifactRef: BuildRef("tenant-b", "sess-1", "run-1", "art-parent"),
		Relation:    LineageReferenced,
	}}

	if _, err := store.Put(runtimeContext(), childReq); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("Put() error = %v, want permission_denied", err)
	}
	if metadata.getByRefCalls != 0 {
		t.Fatalf("GetByRef() calls = %d, want zero", metadata.getByRefCalls)
	}
}

func TestStoreRejectsCrossScopeLineageBeforeMetadataLookup(t *testing.T) {
	objects := objectstore.NewMemory()
	metadata := metastore.NewMemory()
	seedStore := NewStore(StoreConfig{ObjectStore: objects, MetadataStore: metadata})
	parent := putTask3ParentInRun(t, seedStore, "run-parent", "scope-parent", VisibilityInternal)

	actors := []struct {
		name string
		ctx  context.Context
	}{
		{name: "runtime", ctx: runtimeContextFor("tenant-a", "sess-1", "run-child")},
		{name: "scoped debug", ctx: ContextWithActor(context.Background(), Actor{
			TenantID: "tenant-a", UserID: "debugger", SessionID: "sess-1", RunID: "run-child", Role: ActorDebug,
		})},
		{name: "scoped audit", ctx: ContextWithActor(context.Background(), Actor{
			TenantID: "tenant-a", UserID: "auditor", SessionID: "sess-1", RunID: "run-child", Role: ActorAudit,
		})},
	}
	refs := []struct {
		name string
		ref  string
	}{
		{name: "existing", ref: parent.ArtifactRef},
		{name: "missing", ref: BuildRef("tenant-a", "sess-1", "run-parent", "art_missing")},
	}

	for _, actor := range actors {
		for _, target := range refs {
			t.Run(actor.name+"/"+target.name, func(t *testing.T) {
				countedMetadata := &recordingMetadataStore{MetadataStore: metadata}
				store := NewStore(StoreConfig{ObjectStore: objects, MetadataStore: countedMetadata})
				childReq := validPutRequest()
				childReq.RunID = "run-child"
				childReq.OwnerID = "scope-child"
				childReq.DerivedFrom = []ArtifactLineage{{
					ArtifactRef: target.ref,
					Relation:    LineageReferenced,
				}}

				if _, err := store.Put(actor.ctx, childReq); !IsErrorCode(err, ErrPermissionDenied) {
					t.Fatalf("Put() error = %v, want permission_denied", err)
				}
				if countedMetadata.getByRefCalls != 0 {
					t.Fatalf("GetByRef() calls = %d, want zero", countedMetadata.getByRefCalls)
				}
			})
		}
	}
}

func TestRuntimeCannotUseSameRunDebugArtifactAsLineage(t *testing.T) {
	store := newTestStore(t)
	parentReq := validPutRequest()
	parentReq.OwnerModule = OwnerModuleObservability
	parentReq.OwnerID = "debug-parent"
	parentReq.ArtifactType = ArtifactTypeDebugPayload
	parentReq.Visibility = VisibilityDebug
	parentReq.Content = strings.NewReader("debug parent")
	parent, err := store.Put(runtimeContext(), parentReq)
	if err != nil {
		t.Fatalf("put debug parent: %v", err)
	}

	childReq := validPutRequest()
	childReq.OwnerID = "debug-child"
	childReq.Content = strings.NewReader("child")
	childReq.DerivedFrom = []ArtifactLineage{{ArtifactRef: parent.ArtifactRef, Relation: LineageReferenced}}
	if _, err := store.Put(runtimeContext(), childReq); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("Put() error = %v, want permission_denied", err)
	}
}

func TestRuntimeCannotRecordCrossRunLineage(t *testing.T) {
	store := newTestStore(t)
	parent := putTask3ParentInRun(t, store, "run-parent", "cross-run-parent", VisibilityInternal)
	childReq := validPutRequest()
	childReq.RunID = "run-child"
	childReq.OwnerID = "runtime-cross-run-child"
	childReq.Content = strings.NewReader("child")
	childReq.DerivedFrom = []ArtifactLineage{{ArtifactRef: parent.ArtifactRef, Relation: LineageReferenced}}

	if _, err := store.Put(runtimeContextFor("tenant-a", "sess-1", "run-child"), childReq); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("Put() error = %v, want permission_denied", err)
	}
}

func TestTenantAuditCanRecordExplicitCrossRunLineage(t *testing.T) {
	store := newTestStore(t)
	parent := putTask3ParentInRun(t, store, "run-parent", "audit-parent", VisibilityInternal)
	childReq := validPutRequest()
	childReq.RunID = "run-child"
	childReq.OwnerID = "audit-cross-run-child"
	childReq.Content = strings.NewReader("child")
	childReq.DerivedFrom = []ArtifactLineage{{ArtifactRef: parent.ArtifactRef, Relation: LineageReferenced}}
	auditCtx := ContextWithActor(context.Background(), Actor{TenantID: "tenant-a", UserID: "auditor", Role: ActorAudit})

	child, err := store.Put(auditCtx, childReq)
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if len(child.DerivedFrom) != 1 || child.DerivedFrom[0] != childReq.DerivedFrom[0] {
		t.Fatalf("persisted lineage = %#v, want %#v", child.DerivedFrom, childReq.DerivedFrom)
	}
}

func TestScopedAuditCannotRecordCrossRunLineage(t *testing.T) {
	store := newTestStore(t)
	parent := putTask3ParentInRun(t, store, "run-parent", "scoped-audit-parent", VisibilityInternal)
	childReq := validPutRequest()
	childReq.RunID = "run-child"
	childReq.OwnerID = "scoped-audit-child"
	childReq.Content = strings.NewReader("child")
	childReq.DerivedFrom = []ArtifactLineage{{ArtifactRef: parent.ArtifactRef, Relation: LineageReferenced}}
	auditCtx := ContextWithActor(context.Background(), Actor{
		TenantID: "tenant-a", UserID: "auditor", RunID: "run-child", Role: ActorAudit,
	})

	if _, err := store.Put(auditCtx, childReq); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("Put() error = %v, want permission_denied", err)
	}
}

func TestIdempotencyHitRevalidatesLineageAuthorization(t *testing.T) {
	store := newTestStore(t)
	parent := putTask3ParentInRun(t, store, "run-parent", "hit-parent", VisibilityInternal)
	childReq := validPutRequest()
	childReq.RunID = "run-child"
	childReq.OwnerID = "hit-child"
	childReq.IdempotencyKey = "lineage-hit-authorization"
	childReq.Content = strings.NewReader("child")
	childReq.DerivedFrom = []ArtifactLineage{{ArtifactRef: parent.ArtifactRef, Relation: LineageReferenced}}
	auditCtx := ContextWithActor(context.Background(), Actor{TenantID: "tenant-a", UserID: "auditor", Role: ActorAudit})
	if _, err := store.Put(auditCtx, childReq); err != nil {
		t.Fatalf("first Put() error = %v", err)
	}

	retry := childReq
	retry.Content = strings.NewReader("child")
	runtimeCtx := runtimeContextFor("tenant-a", "sess-1", "run-child")
	if _, err := store.Put(runtimeCtx, retry); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("retry Put() error = %v, want permission_denied", err)
	}
}

func TestStoreRejectsUnknownLineageRelation(t *testing.T) {
	store := newTestStore(t)
	childReq := validPutRequest()
	childReq.DerivedFrom = []ArtifactLineage{{
		ArtifactRef: BuildRef("tenant-a", "sess-1", "run-1", "art-parent"),
		Relation:    LineageRelation("copied"),
	}}
	if _, err := store.Put(runtimeContext(), childReq); !IsErrorCode(err, ErrInvalidArgument) {
		t.Fatalf("Put() error = %v, want invalid_argument", err)
	}
}

func TestStoreEnforcesVisibility(t *testing.T) {
	store := newTestStore(t)
	runtimeCtx := ContextWithActor(context.Background(), Actor{
		TenantID:  "tenant-a",
		UserID:    "runtime",
		SessionID: "sess-1",
		RunID:     "run-1",
		Role:      ActorRuntime,
	})
	meta, err := store.Put(runtimeCtx, PutArtifactRequest{
		TenantID:     "tenant-a",
		UserID:       "user-1",
		SessionID:    "sess-1",
		RunID:        "run-1",
		OwnerModule:  OwnerModuleObservability,
		OwnerID:      "debug-1",
		ArtifactType: ArtifactTypeDebugPayload,
		MimeType:     "text/plain",
		Visibility:   VisibilityDebug,
		Content:      strings.NewReader("debug payload"),
	})
	if err != nil {
		t.Fatalf("put debug artifact: %v", err)
	}

	userCtx := actorContext()
	_, err = store.Get(userCtx, meta.ArtifactRef, GetOptions{Purpose: PurposeView})
	if !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("expected user denied for debug artifact, got %v", err)
	}
	_, err = store.Get(runtimeCtx, meta.ArtifactRef, GetOptions{Purpose: PurposeDebug})
	if err != nil {
		t.Fatalf("runtime should read debug artifact: %v", err)
	}
}

func TestStoreDoesNotLetUserReadInternalArtifactByClaimingModelContext(t *testing.T) {
	store := newTestStore(t)
	runtimeCtx := runtimeContext()
	meta, err := store.Put(runtimeCtx, PutArtifactRequest{
		TenantID:     "tenant-a",
		UserID:       "user-1",
		SessionID:    "sess-1",
		RunID:        "run-1",
		OwnerModule:  OwnerModuleContextEngine,
		OwnerID:      "context-snapshot-1",
		ArtifactType: ArtifactTypeContextSnapshot,
		MimeType:     "application/json",
		Visibility:   VisibilityInternal,
		Content:      strings.NewReader(`{"messages":3}`),
	})
	if err != nil {
		t.Fatalf("put internal artifact: %v", err)
	}

	userCtx := actorContext()
	_, err = store.Get(userCtx, meta.ArtifactRef, GetOptions{Purpose: PurposeModelContext})
	if !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("expected user to be denied for internal artifact even with model_context purpose, got %v", err)
	}

	_, err = store.Get(runtimeCtx, meta.ArtifactRef, GetOptions{Purpose: PurposeModelContext})
	if err != nil {
		t.Fatalf("runtime should read internal artifact for model context: %v", err)
	}
}

func TestStoreRejectsPutWhenRequestScopeDoesNotMatchActor(t *testing.T) {
	store := newTestStore(t)
	ctx := runtimeContext()

	tests := []struct {
		name      string
		tenantID  string
		sessionID string
		runID     string
	}{
		{
			name:      "tenant mismatch",
			tenantID:  "tenant-b",
			sessionID: "sess-1",
			runID:     "run-1",
		},
		{
			name:      "session mismatch",
			tenantID:  "tenant-a",
			sessionID: "sess-2",
			runID:     "run-1",
		},
		{
			name:      "run mismatch",
			tenantID:  "tenant-a",
			sessionID: "sess-1",
			runID:     "run-2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := store.Put(ctx, PutArtifactRequest{
				TenantID:     tt.tenantID,
				UserID:       "user-1",
				SessionID:    tt.sessionID,
				RunID:        tt.runID,
				OwnerModule:  OwnerModuleRuntime,
				OwnerID:      "checkpoint-1",
				ArtifactType: ArtifactTypeCheckpointState,
				MimeType:     "application/json",
				Visibility:   VisibilityInternal,
				Content:      strings.NewReader(`{"state":"ok"}`),
			})
			if !IsErrorCode(err, ErrPermissionDenied) {
				t.Fatalf("expected permission denied for mismatched request scope, got %v", err)
			}
		})
	}
}

func TestStoreDeletesExpiredArtifacts(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	objects := objectstore.NewMemory()
	metadata := metastore.NewMemory()
	store := NewStore(StoreConfig{
		ObjectStore:    objects,
		MetadataStore:  metadata,
		Clock:          fixedClock{now: now},
		MaxObjectBytes: 1024 * 1024,
	})
	ctx := actorContext()
	meta, err := store.Put(ctx, PutArtifactRequest{
		TenantID:        "tenant-a",
		UserID:          "user-1",
		SessionID:       "sess-1",
		RunID:           "run-1",
		OwnerModule:     OwnerModuleRuntime,
		OwnerID:         "checkpoint-1",
		ArtifactType:    ArtifactTypeCheckpointState,
		MimeType:        "application/json",
		Visibility:      VisibilityInternal,
		RetentionPolicy: RetentionCheckpointTTL,
		Content:         strings.NewReader(`{"state":"ok"}`),
		ExpiresAt:       now.Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("put expired artifact: %v", err)
	}

	maintenanceCtx := ContextWithActor(context.Background(), Actor{
		TenantID:  "tenant-a",
		SessionID: "sess-1",
		RunID:     "run-1",
		Role:      ActorAudit,
	})
	result, err := store.CleanupExpired(maintenanceCtx, now)
	if err != nil {
		t.Fatalf("cleanup expired: %v", err)
	}
	if result.Expired != 1 || result.Deleted != 1 {
		t.Fatalf("expected one expired and deleted artifact: %#v", result)
	}
	deleted, err := metadata.GetByRef(context.Background(), meta.ArtifactRef)
	if err != nil {
		t.Fatalf("get cleanup tombstone: %v", err)
	}
	if deleted.Status != ArtifactStatusDeleted || deleted.DeleteReason != DeleteReasonTTL || !deleted.DeletedAt.Equal(now) {
		t.Fatalf("unexpected cleanup tombstone: %#v", deleted)
	}
	reader, err := objects.Get(context.Background(), meta.StorageKey)
	if reader != nil {
		_ = reader.Close()
	}
	if !IsErrorCode(err, ErrNotFound) {
		t.Fatalf("expected cleaned object to be absent, got %v", err)
	}
	_, err = store.Get(maintenanceCtx, meta.ArtifactRef, GetOptions{Purpose: PurposeModelContext})
	if !IsErrorCode(err, ErrDeleted) {
		t.Fatalf("expected deleted artifact after cleanup, got %v", err)
	}
}

func TestStoreRejectsOversizedArtifact(t *testing.T) {
	store := NewStore(StoreConfig{
		ObjectStore:    objectstore.NewMemory(),
		MetadataStore:  metastore.NewMemory(),
		Clock:          fixedClock{now: time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)},
		MaxObjectBytes: 4,
	})
	_, err := store.Put(actorContext(), PutArtifactRequest{
		TenantID:     "tenant-a",
		UserID:       "user-1",
		SessionID:    "sess-1",
		RunID:        "run-1",
		OwnerModule:  OwnerModuleToolGateway,
		OwnerID:      "tool-call-1",
		ArtifactType: ArtifactTypeToolResult,
		MimeType:     "text/plain",
		Visibility:   VisibilityUserVisible,
		Content:      strings.NewReader("too large"),
	})
	if !IsErrorCode(err, ErrTooLarge) {
		t.Fatalf("expected too large error, got %v", err)
	}
}

func TestStoreListsArtifactsByRunTypeAndVisibility(t *testing.T) {
	ctx := actorContext()
	store := newTestStore(t)
	_, err := store.Put(ctx, PutArtifactRequest{
		TenantID:     "tenant-a",
		UserID:       "user-1",
		SessionID:    "sess-1",
		RunID:        "run-1",
		OwnerModule:  OwnerModuleProtocol,
		OwnerID:      "final-1",
		ArtifactType: ArtifactTypeFinalResult,
		MimeType:     "text/plain",
		Visibility:   VisibilityUserVisible,
		Content:      strings.NewReader("final"),
	})
	if err != nil {
		t.Fatalf("put final artifact: %v", err)
	}
	_, err = store.Put(ctx, PutArtifactRequest{
		TenantID:     "tenant-a",
		UserID:       "user-1",
		SessionID:    "sess-1",
		RunID:        "run-1",
		OwnerModule:  OwnerModuleToolGateway,
		OwnerID:      "tool-call-1",
		ArtifactType: ArtifactTypeToolResult,
		MimeType:     "text/plain",
		Visibility:   VisibilityInternal,
		Content:      strings.NewReader("internal"),
	})
	if err != nil {
		t.Fatalf("put internal artifact: %v", err)
	}

	metas, err := store.List(ctx, ListQuery{
		RunID:      "run-1",
		Type:       ArtifactTypeFinalResult,
		Visibility: VisibilityUserVisible,
	})
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(metas) != 1 || metas[0].ArtifactType != ArtifactTypeFinalResult {
		t.Fatalf("unexpected list result: %#v", metas)
	}
}

func TestStoreFailsClosedForMissingOrInvalidActor(t *testing.T) {
	operations := []struct {
		name string
		call func(*Store, context.Context, string) error
	}{
		{
			name: "put",
			call: func(store *Store, ctx context.Context, _ string) error {
				_, err := store.Put(ctx, validTask2PutRequest())
				return err
			},
		},
		{
			name: "get",
			call: func(store *Store, ctx context.Context, ref string) error {
				_, err := store.Get(ctx, ref, GetOptions{Purpose: PurposeView})
				return err
			},
		},
		{
			name: "head",
			call: func(store *Store, ctx context.Context, ref string) error {
				_, err := store.Head(ctx, ref)
				return err
			},
		},
		{
			name: "delete",
			call: func(store *Store, ctx context.Context, ref string) error {
				return store.Delete(ctx, ref, DeleteReasonCleanup)
			},
		},
		{
			name: "download url",
			call: func(store *Store, ctx context.Context, ref string) error {
				_, err := store.CreateDownloadURL(ctx, ref, DownloadURLOptions{})
				return err
			},
		},
		{
			name: "list",
			call: func(store *Store, ctx context.Context, _ string) error {
				_, err := store.List(ctx, ListQuery{})
				return err
			},
		},
		{
			name: "cleanup",
			call: func(store *Store, ctx context.Context, _ string) error {
				_, err := store.CleanupExpired(ctx, time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC))
				return err
			},
		},
	}
	actors := []struct {
		name string
		ctx  context.Context
	}{
		{name: "missing", ctx: context.Background()},
		{name: "empty tenant", ctx: ContextWithActor(context.Background(), Actor{Role: ActorAudit})},
		{name: "unknown role", ctx: ContextWithActor(context.Background(), Actor{TenantID: "tenant-a", Role: ActorRole("root")})},
		{name: "incomplete runtime", ctx: ContextWithActor(context.Background(), Actor{TenantID: "tenant-a", Role: ActorRuntime})},
	}

	for _, operation := range operations {
		for _, actor := range actors {
			t.Run(operation.name+"/"+actor.name, func(t *testing.T) {
				store, ref := newTask2SeededStore(t, true)
				err := operation.call(store, actor.ctx, ref)
				if !IsErrorCode(err, ErrPermissionDenied) {
					t.Fatalf("operation error = %v, want permission_denied", err)
				}
			})
		}
	}
}

func TestStoreRejectsUnknownPurposeAndDeleteReasonBeforeAdapterAccess(t *testing.T) {
	ctx := ContextWithActor(context.Background(), Actor{
		TenantID:  "tenant-a",
		SessionID: "sess-1",
		RunID:     "run-1",
		Role:      ActorRuntime,
	})
	ref := BuildRef("tenant-a", "sess-1", "run-1", "art-ready")

	for _, purpose := range []Purpose{"", Purpose("private")} {
		t.Run("get purpose "+string(purpose), func(t *testing.T) {
			metadata := &recordingMetadataStore{MetadataStore: metastore.NewMemory()}
			store := NewStore(StoreConfig{ObjectStore: objectstore.NewMemory(), MetadataStore: metadata})
			_, err := store.Get(ctx, ref, GetOptions{Purpose: purpose})
			if !IsErrorCode(err, ErrInvalidArgument) {
				t.Fatalf("Get() error = %v, want invalid_argument", err)
			}
			if metadata.getByRefCalls != 0 {
				t.Fatalf("Get() accessed metadata %d times for invalid purpose", metadata.getByRefCalls)
			}
		})
	}

	for _, reason := range []DeleteReason{"", DeleteReason("admin")} {
		t.Run("delete reason "+string(reason), func(t *testing.T) {
			metadata := &recordingMetadataStore{MetadataStore: metastore.NewMemory()}
			store := NewStore(StoreConfig{ObjectStore: objectstore.NewMemory(), MetadataStore: metadata})
			err := store.Delete(ctx, ref, reason)
			if !IsErrorCode(err, ErrInvalidArgument) {
				t.Fatalf("Delete() error = %v, want invalid_argument", err)
			}
			if metadata.getByRefCalls != 0 {
				t.Fatalf("Delete() accessed metadata %d times for invalid reason", metadata.getByRefCalls)
			}
		})
	}
}

func TestStoreOperationsRejectMissingAdaptersInsteadOfPanicking(t *testing.T) {
	ctx := ContextWithActor(context.Background(), Actor{TenantID: "tenant-a", Role: ActorAudit})
	tests := []struct {
		name         string
		withObject   bool
		withMetadata bool
		call         func(*Store, string) error
	}{
		{
			name: "put missing object", withMetadata: true,
			call: func(store *Store, _ string) error { _, err := store.Put(ctx, validTask2PutRequest()); return err },
		},
		{
			name: "put missing metadata", withObject: true,
			call: func(store *Store, _ string) error { _, err := store.Put(ctx, validTask2PutRequest()); return err },
		},
		{
			name: "get missing object", withMetadata: true,
			call: func(store *Store, ref string) error {
				_, err := store.Get(ctx, ref, GetOptions{Purpose: PurposeView})
				return err
			},
		},
		{
			name: "get missing metadata", withObject: true,
			call: func(store *Store, ref string) error {
				_, err := store.Get(ctx, ref, GetOptions{Purpose: PurposeView})
				return err
			},
		},
		{
			name: "head missing metadata", withObject: true,
			call: func(store *Store, ref string) error { _, err := store.Head(ctx, ref); return err },
		},
		{
			name: "delete missing object", withMetadata: true,
			call: func(store *Store, ref string) error { return store.Delete(ctx, ref, DeleteReasonCleanup) },
		},
		{
			name: "delete missing metadata", withObject: true,
			call: func(store *Store, ref string) error { return store.Delete(ctx, ref, DeleteReasonCleanup) },
		},
		{
			name: "download url missing object", withMetadata: true,
			call: func(store *Store, ref string) error {
				_, err := store.CreateDownloadURL(ctx, ref, DownloadURLOptions{})
				return err
			},
		},
		{
			name: "download url missing metadata", withObject: true,
			call: func(store *Store, ref string) error {
				_, err := store.CreateDownloadURL(ctx, ref, DownloadURLOptions{})
				return err
			},
		},
		{
			name: "list missing metadata", withObject: true,
			call: func(store *Store, _ string) error { _, err := store.List(ctx, ListQuery{}); return err },
		},
		{
			name: "cleanup missing object", withMetadata: true,
			call: func(store *Store, _ string) error {
				_, err := store.CleanupExpired(ctx, time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC))
				return err
			},
		},
		{
			name: "cleanup missing metadata", withObject: true,
			call: func(store *Store, _ string) error {
				_, err := store.CleanupExpired(ctx, time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC))
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects ObjectStore
			if tt.withObject {
				objects = objectstore.NewMemory()
			}
			var metadata MetadataStore
			if tt.withMetadata {
				metadata = metastore.NewMemory()
			}
			ref := seedTask2ReadyArtifact(t, metadata, objects)
			store := NewStore(StoreConfig{ObjectStore: objects, MetadataStore: metadata})
			err, panicValue := callWithoutPanic(func() error { return tt.call(store, ref) })
			if panicValue != nil {
				t.Fatalf("operation panicked: %v", panicValue)
			}
			if !IsErrorCode(err, ErrInvalidArgument) {
				t.Fatalf("operation error = %v, want invalid_argument", err)
			}
		})
	}
}

func TestMetadataOnlyHeadAndList(t *testing.T) {
	metadata := metastore.NewMemory()
	ref := seedTask2ReadyArtifact(t, metadata, nil)
	store := NewStore(StoreConfig{MetadataStore: metadata})
	ctx := ContextWithActor(context.Background(), Actor{
		TenantID:  "tenant-a",
		SessionID: "sess-1",
		RunID:     "run-1",
		Role:      ActorAudit,
	})

	meta, err := store.Head(ctx, ref)
	if err != nil || meta == nil || meta.ArtifactRef != ref {
		t.Fatalf("Head() meta = %#v, error = %v", meta, err)
	}
	metas, err := store.List(ctx, ListQuery{})
	if err != nil || len(metas) != 1 || metas[0].ArtifactRef != ref {
		t.Fatalf("List() metas = %#v, error = %v", metas, err)
	}
}

func TestListAllowsContextEngineSessionScopeAcrossRuns(t *testing.T) {
	metadata := metastore.NewMemory()
	ref := seedTask2Artifact(t, metadata, nil, "art-prev", "tenant-a", "user-1", "sess-1", "run-1", VisibilityUserVisible, time.Time{})
	store := NewStore(StoreConfig{MetadataStore: metadata})

	runtimeCtx := ContextWithActor(context.Background(), Actor{
		TenantID: "tenant-a", UserID: "runtime", SessionID: "sess-1", RunID: "run-2", Role: ActorRuntime,
	})
	runtimeMetas, err := store.List(runtimeCtx, ListQuery{TenantID: "tenant-a", SessionID: "sess-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(runtimeMetas) != 0 {
		t.Fatalf("runtime listed previous run artifacts: %#v", runtimeMetas)
	}

	contextCtx := ContextWithActor(context.Background(), Actor{
		TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-2", Role: ActorContextEngine,
	})
	contextMetas, err := store.List(contextCtx, ListQuery{TenantID: "tenant-a", SessionID: "sess-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(contextMetas) != 1 || contextMetas[0].ArtifactRef != ref {
		t.Fatalf("context engine list = %#v, want %s", contextMetas, ref)
	}
}

func TestStoreOperationsEnforceActorScope(t *testing.T) {
	operations := []struct {
		name string
		call func(*Store, context.Context, string) error
	}{
		{name: "get", call: func(store *Store, ctx context.Context, ref string) error {
			_, err := store.Get(ctx, ref, GetOptions{Purpose: PurposeView})
			return err
		}},
		{name: "head", call: func(store *Store, ctx context.Context, ref string) error {
			_, err := store.Head(ctx, ref)
			return err
		}},
		{name: "delete", call: func(store *Store, ctx context.Context, ref string) error {
			return store.Delete(ctx, ref, DeleteReasonCleanup)
		}},
		{name: "download url", call: func(store *Store, ctx context.Context, ref string) error {
			_, err := store.CreateDownloadURL(ctx, ref, DownloadURLOptions{})
			return err
		}},
	}
	actors := []struct {
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
			name: "runtime wrong run",
			ctx: ContextWithActor(context.Background(), Actor{
				TenantID: "tenant-a", SessionID: "sess-1", RunID: "run-2", Role: ActorRuntime,
			}),
		},
	}

	for _, operation := range operations {
		for _, actor := range actors {
			t.Run(operation.name+"/"+actor.name, func(t *testing.T) {
				store, ref := newTask2SeededStore(t, true)
				if err := operation.call(store, actor.ctx, ref); !IsErrorCode(err, ErrPermissionDenied) {
					t.Fatalf("operation error = %v, want permission_denied", err)
				}
			})
		}
	}
}

func TestStoreOperationsAuthorizeBeforeReportingDeleted(t *testing.T) {
	metadata := metastore.NewMemory()
	objects := objectstore.NewMemory()
	ref := seedTask2Artifact(t, metadata, objects, "art-deleted", "tenant-a", "user-1", "sess-1", "run-1", VisibilityUserVisible, time.Time{})
	if _, err := metadata.MarkDeleted(context.Background(), ref, DeleteReasonCleanup, time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("mark deleted fixture: %v", err)
	}
	store := NewStore(StoreConfig{ObjectStore: objects, MetadataStore: metadata})
	operations := []struct {
		name string
		call func(context.Context) error
	}{
		{name: "get", call: func(ctx context.Context) error {
			_, err := store.Get(ctx, ref, GetOptions{Purpose: PurposeView})
			return err
		}},
		{name: "head", call: func(ctx context.Context) error {
			_, err := store.Head(ctx, ref)
			return err
		}},
		{name: "download url", call: func(ctx context.Context) error {
			_, err := store.CreateDownloadURL(ctx, ref, DownloadURLOptions{})
			return err
		}},
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
			name: "runtime wrong run",
			ctx: ContextWithActor(context.Background(), Actor{
				TenantID: "tenant-a", SessionID: "sess-1", RunID: "run-2", Role: ActorRuntime,
			}),
		},
	}

	for _, operation := range operations {
		for _, actor := range unauthorized {
			t.Run(operation.name+"/"+actor.name, func(t *testing.T) {
				if err := operation.call(actor.ctx); !IsErrorCode(err, ErrPermissionDenied) {
					t.Fatalf("operation error = %v, want permission_denied", err)
				}
			})
		}
	}

	authorizedCtx := ContextWithActor(context.Background(), Actor{
		TenantID: "tenant-a", SessionID: "sess-1", RunID: "run-1", Role: ActorRuntime,
	})
	for _, operation := range operations {
		t.Run(operation.name+"/authorized", func(t *testing.T) {
			if err := operation.call(authorizedCtx); !IsErrorCode(err, ErrDeleted) {
				t.Fatalf("operation error = %v, want deleted", err)
			}
		})
	}
}

func TestStoreListRequiresActorAndFiltersEveryRow(t *testing.T) {
	metadata := metastore.NewMemory()
	ownVisible := seedTask2Artifact(t, metadata, nil, "art-own-visible", "tenant-a", "user-1", "sess-1", "run-1", VisibilityUserVisible, time.Time{})
	otherVisible := seedTask2Artifact(t, metadata, nil, "art-other-visible", "tenant-a", "user-2", "sess-1", "run-1", VisibilityUserVisible, time.Time{})
	internal := seedTask2Artifact(t, metadata, nil, "art-internal", "tenant-a", "user-1", "sess-1", "run-1", VisibilityInternal, time.Time{})
	seedTask2Artifact(t, metadata, nil, "art-debug", "tenant-a", "user-1", "sess-1", "run-1", VisibilityDebug, time.Time{})
	seedTask2Artifact(t, metadata, nil, "art-restricted", "tenant-a", "user-1", "sess-1", "run-1", VisibilityRestricted, time.Time{})
	seedTask2Artifact(t, metadata, nil, "art-other-run", "tenant-a", "user-1", "sess-1", "run-2", VisibilityUserVisible, time.Time{})
	seedTask2Artifact(t, metadata, nil, "art-other-tenant", "tenant-b", "user-1", "sess-1", "run-1", VisibilityUserVisible, time.Time{})
	recording := &recordingListMetadataStore{MetadataStore: metadata}
	store := NewStore(StoreConfig{MetadataStore: recording})

	if _, err := store.List(context.Background(), ListQuery{}); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("List() without actor error = %v, want permission_denied", err)
	}

	userCtx := ContextWithActor(context.Background(), Actor{
		TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-1", Role: ActorUser,
	})
	userRows, err := store.List(userCtx, ListQuery{})
	if err != nil {
		t.Fatalf("List() user error = %v", err)
	}
	assertTask2ArtifactRefs(t, userRows, ownVisible)
	assertTask2ListQuery(t, recording.lastQuery(), "tenant-a", "sess-1", "run-1")
	beforeConflict := len(recording.queries)
	if _, err := store.List(userCtx, ListQuery{RunID: "run-2"}); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("List() user conflicting run error = %v, want permission_denied", err)
	}
	if len(recording.queries) != beforeConflict {
		t.Fatal("List() accessed metadata for conflicting user scope")
	}

	runtimeCtx := ContextWithActor(context.Background(), Actor{
		TenantID: "tenant-a", SessionID: "sess-1", RunID: "run-1", Role: ActorRuntime,
	})
	runtimeRows, err := store.List(runtimeCtx, ListQuery{})
	if err != nil {
		t.Fatalf("List() runtime error = %v", err)
	}
	assertTask2ArtifactRefs(t, runtimeRows, ownVisible, otherVisible, internal)
	assertTask2ListQuery(t, recording.lastQuery(), "tenant-a", "sess-1", "run-1")
	beforeConflict = len(recording.queries)
	if _, err := store.List(runtimeCtx, ListQuery{SessionID: "sess-2"}); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("List() runtime conflicting session error = %v, want permission_denied", err)
	}
	if len(recording.queries) != beforeConflict {
		t.Fatal("List() accessed metadata for conflicting runtime scope")
	}
}

func TestStoreListHonorsPrivilegedNarrowing(t *testing.T) {
	metadata := metastore.NewMemory()
	sess1Run1 := seedTask2Artifact(t, metadata, nil, "art-sess1-run1", "tenant-a", "user-1", "sess-1", "run-1", VisibilityDebug, time.Time{})
	sess1Run2 := seedTask2Artifact(t, metadata, nil, "art-sess1-run2", "tenant-a", "user-1", "sess-1", "run-2", VisibilityRestricted, time.Time{})
	sess2Run1 := seedTask2Artifact(t, metadata, nil, "art-sess2-run1", "tenant-a", "user-1", "sess-2", "run-1", VisibilityInternal, time.Time{})
	seedTask2Artifact(t, metadata, nil, "art-tenant-b", "tenant-b", "user-1", "sess-1", "run-1", VisibilityDebug, time.Time{})
	recording := &recordingListMetadataStore{MetadataStore: metadata}
	store := NewStore(StoreConfig{MetadataStore: recording})

	debugCtx := ContextWithActor(context.Background(), Actor{
		TenantID: "tenant-a", SessionID: "sess-1", Role: ActorDebug,
	})
	debugRows, err := store.List(debugCtx, ListQuery{})
	if err != nil {
		t.Fatalf("List() narrowed debug error = %v", err)
	}
	assertTask2ArtifactRefs(t, debugRows, sess1Run1, sess1Run2)
	assertTask2ListQuery(t, recording.lastQuery(), "tenant-a", "sess-1", "")
	if _, err := store.List(debugCtx, ListQuery{SessionID: "sess-2"}); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("List() debug conflicting session error = %v, want permission_denied", err)
	}

	auditCtx := ContextWithActor(context.Background(), Actor{
		TenantID: "tenant-a", RunID: "run-1", Role: ActorAudit,
	})
	auditRows, err := store.List(auditCtx, ListQuery{})
	if err != nil {
		t.Fatalf("List() narrowed audit error = %v", err)
	}
	assertTask2ArtifactRefs(t, auditRows, sess1Run1, sess2Run1)
	assertTask2ListQuery(t, recording.lastQuery(), "tenant-a", "", "run-1")
	if _, err := store.List(auditCtx, ListQuery{RunID: "run-2"}); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("List() audit conflicting run error = %v, want permission_denied", err)
	}
	if _, err := store.List(auditCtx, ListQuery{TenantID: "tenant-b"}); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("List() audit conflicting tenant error = %v, want permission_denied", err)
	}
}

func TestStoreListPropagatesMalformedReturnedVisibility(t *testing.T) {
	metadata := metastore.NewMemory()
	seedTask2Artifact(t, metadata, nil, "art-malformed", "tenant-a", "user-1", "sess-1", "run-1", Visibility("private"), time.Time{})
	store := NewStore(StoreConfig{MetadataStore: metadata})
	ctx := ContextWithActor(context.Background(), Actor{TenantID: "tenant-a", Role: ActorAudit})

	if _, err := store.List(ctx, ListQuery{}); !IsErrorCode(err, ErrInvalidArgument) {
		t.Fatalf("List() malformed row error = %v, want invalid_argument", err)
	}
}

func TestStoreListRejectsUnknownVisibilityFilterBeforeAdapterAccess(t *testing.T) {
	metadata := &recordingListMetadataStore{MetadataStore: metastore.NewMemory()}
	store := NewStore(StoreConfig{MetadataStore: metadata})
	ctx := ContextWithActor(context.Background(), Actor{TenantID: "tenant-a", Role: ActorAudit})

	if _, err := store.List(ctx, ListQuery{Visibility: Visibility("private")}); !IsErrorCode(err, ErrInvalidArgument) {
		t.Fatalf("List() unknown visibility filter error = %v, want invalid_argument", err)
	}
	if len(metadata.queries) != 0 {
		t.Fatal("List() accessed metadata for unknown visibility filter")
	}
}

func TestCleanupExpiredRequiresTenantAuditActor(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	actors := []struct {
		name string
		ctx  context.Context
	}{
		{name: "missing", ctx: context.Background()},
		{name: "user", ctx: ContextWithActor(context.Background(), Actor{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-1", Role: ActorUser})},
		{name: "runtime", ctx: ContextWithActor(context.Background(), Actor{TenantID: "tenant-a", SessionID: "sess-1", RunID: "run-1", Role: ActorRuntime})},
		{name: "debug", ctx: ContextWithActor(context.Background(), Actor{TenantID: "tenant-a", Role: ActorDebug})},
		{name: "audit without tenant", ctx: ContextWithActor(context.Background(), Actor{Role: ActorAudit})},
	}
	for _, tt := range actors {
		t.Run(tt.name, func(t *testing.T) {
			metadata := metastore.NewMemory()
			objects := objectstore.NewMemory()
			seedTask2Artifact(t, metadata, objects, "art-expired", "tenant-a", "user-1", "sess-1", "run-1", VisibilityInternal, now.Add(-time.Minute))
			store := NewStore(StoreConfig{ObjectStore: objects, MetadataStore: metadata})
			if _, err := store.CleanupExpired(tt.ctx, now); !IsErrorCode(err, ErrPermissionDenied) {
				t.Fatalf("CleanupExpired() error = %v, want permission_denied", err)
			}
		})
	}
}

func TestCleanupExpiredDoesNotCrossScope(t *testing.T) {
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)

	t.Run("tenant", func(t *testing.T) {
		metadata := metastore.NewMemory()
		objects := objectstore.NewMemory()
		tenantA := seedTask2Artifact(t, metadata, objects, "art-tenant-a", "tenant-a", "user-1", "sess-1", "run-1", VisibilityInternal, now.Add(-time.Minute))
		tenantB := seedTask2Artifact(t, metadata, objects, "art-tenant-b", "tenant-b", "user-1", "sess-1", "run-1", VisibilityInternal, now.Add(-time.Minute))
		unfiltered := &unfilteredListMetadataStore{MetadataStore: metadata}
		store := NewStore(StoreConfig{ObjectStore: objects, MetadataStore: unfiltered})
		ctx := ContextWithActor(context.Background(), Actor{TenantID: "tenant-a", Role: ActorAudit})

		result, err := store.CleanupExpired(ctx, now)
		if err != nil || result == nil || result.Deleted != 1 {
			t.Fatalf("CleanupExpired() result = %#v, error = %v", result, err)
		}
		assertTask2ListQuery(t, unfiltered.lastQuery(), "tenant-a", "", "")
		assertTask2ArtifactStatus(t, metadata, tenantA, ArtifactStatusDeleted)
		assertTask2ArtifactStatus(t, metadata, tenantB, ArtifactStatusReady)
	})

	t.Run("session and run narrowing", func(t *testing.T) {
		metadata := metastore.NewMemory()
		objects := objectstore.NewMemory()
		target := seedTask2Artifact(t, metadata, objects, "art-target", "tenant-a", "user-1", "sess-1", "run-1", VisibilityRestricted, now.Add(-time.Minute))
		otherRun := seedTask2Artifact(t, metadata, objects, "art-other-run", "tenant-a", "user-1", "sess-1", "run-2", VisibilityInternal, now.Add(-time.Minute))
		otherSession := seedTask2Artifact(t, metadata, objects, "art-other-session", "tenant-a", "user-1", "sess-2", "run-1", VisibilityDebug, now.Add(-time.Minute))
		unfiltered := &unfilteredListMetadataStore{MetadataStore: metadata}
		store := NewStore(StoreConfig{ObjectStore: objects, MetadataStore: unfiltered})
		ctx := ContextWithActor(context.Background(), Actor{
			TenantID: "tenant-a", SessionID: "sess-1", RunID: "run-1", Role: ActorAudit,
		})

		result, err := store.CleanupExpired(ctx, now)
		if err != nil || result == nil || result.Deleted != 1 {
			t.Fatalf("CleanupExpired() result = %#v, error = %v", result, err)
		}
		assertTask2ListQuery(t, unfiltered.lastQuery(), "tenant-a", "sess-1", "run-1")
		assertTask2ArtifactStatus(t, metadata, target, ArtifactStatusDeleted)
		assertTask2ArtifactStatus(t, metadata, otherRun, ArtifactStatusReady)
		assertTask2ArtifactStatus(t, metadata, otherSession, ArtifactStatusReady)
	})
}

func TestFileObjectStorePersistsContent(t *testing.T) {
	ctx := actorContext()
	objectStore, err := objectstore.NewFile(t.TempDir(), "http://artifact.local")
	if err != nil {
		t.Fatalf("new file object store: %v", err)
	}
	store := NewStore(StoreConfig{
		ObjectStore:   objectStore,
		MetadataStore: metastore.NewMemory(),
		Clock:         fixedClock{now: time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)},
	})
	meta, err := store.Put(ctx, PutArtifactRequest{
		TenantID:     "tenant-a",
		UserID:       "user-1",
		SessionID:    "sess-1",
		RunID:        "run-1",
		OwnerModule:  OwnerModuleProtocol,
		OwnerID:      "file-1",
		ArtifactType: ArtifactTypeImage,
		MimeType:     "image/png",
		Name:         "pixel.png",
		Visibility:   VisibilityUserVisible,
		Content:      strings.NewReader("png bytes"),
	})
	if err != nil {
		t.Fatalf("put file artifact: %v", err)
	}
	obj, err := store.Get(ctx, meta.ArtifactRef, GetOptions{Purpose: PurposeView})
	if err != nil {
		t.Fatalf("get file artifact: %v", err)
	}
	defer obj.Content.Close()
	got, err := io.ReadAll(obj.Content)
	if err != nil {
		t.Fatalf("read file artifact: %v", err)
	}
	if string(got) != "png bytes" {
		t.Fatalf("unexpected file content: %s", got)
	}
	url, err := store.CreateDownloadURL(ctx, meta.ArtifactRef, DownloadURLOptions{TTL: time.Minute})
	if err != nil {
		t.Fatalf("create download url: %v", err)
	}
	if !strings.HasPrefix(url.URL, "http://artifact.local/download/") || strings.Contains(url.URL, meta.StorageKey) {
		t.Fatalf("download url should be controlled and opaque: %#v", url)
	}
}

func TestFileObjectStoreRejectsPathTraversal(t *testing.T) {
	objectStore, err := objectstore.NewFile(t.TempDir(), "http://artifact.local")
	if err != nil {
		t.Fatalf("new file object store: %v", err)
	}
	_, err = objectStore.Put(context.Background(), "../escape", strings.NewReader("bad"))
	if !IsErrorCode(err, ErrInvalidArgument) {
		t.Fatalf("expected path traversal to be rejected, got %v", err)
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(StoreConfig{
		ObjectStore:    objectstore.NewMemory(),
		MetadataStore:  metastore.NewMemory(),
		Clock:          fixedClock{now: time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)},
		MaxObjectBytes: 1024 * 1024,
	})
}

func validTask2PutRequest() PutArtifactRequest {
	return PutArtifactRequest{
		TenantID:     "tenant-a",
		UserID:       "user-1",
		SessionID:    "sess-1",
		RunID:        "run-1",
		OwnerModule:  OwnerModuleToolGateway,
		OwnerID:      "task-2",
		ArtifactType: ArtifactTypeToolResult,
		MimeType:     "text/plain",
		Visibility:   VisibilityUserVisible,
		Content:      strings.NewReader("task 2"),
	}
}

func newTask2SeededStore(t *testing.T, withObject bool) (*Store, string) {
	t.Helper()
	metadata := metastore.NewMemory()
	var objects ObjectStore
	if withObject {
		objects = objectstore.NewMemory()
	}
	ref := seedTask2ReadyArtifact(t, metadata, objects)
	return NewStore(StoreConfig{ObjectStore: objects, MetadataStore: metadata}), ref
}

func seedTask2ReadyArtifact(t *testing.T, metadata MetadataStore, objects ObjectStore) string {
	t.Helper()
	return seedTask2Artifact(
		t, metadata, objects, "art-ready", "tenant-a", "user-1", "sess-1", "run-1",
		VisibilityUserVisible, time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC),
	)
}

func seedTask2Artifact(
	t *testing.T,
	metadata MetadataStore,
	objects ObjectStore,
	artifactID string,
	tenantID string,
	userID string,
	sessionID string,
	runID string,
	visibility Visibility,
	expiresAt time.Time,
) string {
	t.Helper()
	ref := BuildRef(tenantID, sessionID, runID, artifactID)
	if metadata == nil {
		return ref
	}
	storageKey := "tenants/" + tenantID + "/sessions/" + sessionID + "/runs/" + runID + "/" + artifactID
	hash := sha256.Sum256([]byte(artifactID))
	if objects != nil {
		if _, err := objects.Put(context.Background(), storageKey, strings.NewReader(artifactID)); err != nil {
			t.Fatalf("seed object: %v", err)
		}
	}
	_, err := metadata.Create(context.Background(), ArtifactMeta{
		ArtifactID:      artifactID,
		ArtifactRef:     ref,
		TenantID:        tenantID,
		UserID:          userID,
		SessionID:       sessionID,
		RunID:           runID,
		OwnerModule:     OwnerModuleToolGateway,
		OwnerID:         artifactID,
		ArtifactType:    ArtifactTypeToolResult,
		MimeType:        "text/plain",
		SizeBytes:       int64(len(artifactID)),
		Hash:            "sha256:" + hex.EncodeToString(hash[:]),
		Visibility:      visibility,
		StorageBackend:  "memory",
		StorageKey:      storageKey,
		RetentionPolicy: RetentionRunTTL,
		ExpiresAt:       expiresAt,
		CreatedBy:       string(OwnerModuleToolGateway) + ":" + artifactID,
		CreatedAt:       time.Date(2026, 7, 8, 8, 0, 0, 0, time.UTC),
		Status:          ArtifactStatusReady,
		SchemaVersion:   ArtifactMetaSchemaVersion,
	}, "")
	if err != nil {
		t.Fatalf("seed metadata: %v", err)
	}
	return ref
}

type recordingMetadataStore struct {
	MetadataStore
	getByRefCalls int
}

type lineageOverrideMetadataStore struct {
	MetadataStore
	requested string
	meta      ArtifactMeta
}

func (s *lineageOverrideMetadataStore) GetByRef(ctx context.Context, ref string) (*ArtifactMeta, error) {
	if ref == s.requested {
		copyMeta := s.meta
		return &copyMeta, nil
	}
	return s.MetadataStore.GetByRef(ctx, ref)
}

func putTask3ParentInRun(
	t *testing.T,
	store *Store,
	runID string,
	ownerID string,
	visibility Visibility,
) *ArtifactMeta {
	t.Helper()
	req := validPutRequest()
	req.RunID = runID
	req.OwnerID = ownerID
	req.Visibility = visibility
	req.Content = strings.NewReader("parent " + ownerID)
	meta, err := store.Put(runtimeContextFor(req.TenantID, req.SessionID, req.RunID), req)
	if err != nil {
		t.Fatalf("put parent %q in run %q: %v", ownerID, runID, err)
	}
	return meta
}

func (s *recordingMetadataStore) GetByRef(ctx context.Context, ref string) (*ArtifactMeta, error) {
	s.getByRefCalls++
	return s.MetadataStore.GetByRef(ctx, ref)
}

type recordingListMetadataStore struct {
	MetadataStore
	queries []ListQuery
}

func (s *recordingListMetadataStore) List(ctx context.Context, query ListQuery) ([]ArtifactMeta, error) {
	s.queries = append(s.queries, query)
	return s.MetadataStore.List(ctx, query)
}

func (s *recordingListMetadataStore) lastQuery() ListQuery {
	if len(s.queries) == 0 {
		return ListQuery{}
	}
	return s.queries[len(s.queries)-1]
}

type unfilteredListMetadataStore struct {
	MetadataStore
	queries []ListQuery
}

func (s *unfilteredListMetadataStore) List(ctx context.Context, query ListQuery) ([]ArtifactMeta, error) {
	s.queries = append(s.queries, query)
	return s.MetadataStore.List(ctx, ListQuery{
		ExpiredAtOrBefore: query.ExpiredAtOrBefore,
		IncludeDeleted:    query.IncludeDeleted,
	})
}

func (s *unfilteredListMetadataStore) lastQuery() ListQuery {
	if len(s.queries) == 0 {
		return ListQuery{}
	}
	return s.queries[len(s.queries)-1]
}

func assertTask2ArtifactRefs(t *testing.T, metas []ArtifactMeta, refs ...string) {
	t.Helper()
	if len(metas) != len(refs) {
		t.Fatalf("artifact refs count = %d, want %d; metas = %#v", len(metas), len(refs), metas)
	}
	want := make(map[string]bool, len(refs))
	for _, ref := range refs {
		want[ref] = true
	}
	for _, meta := range metas {
		if !want[meta.ArtifactRef] {
			t.Fatalf("unexpected artifact ref %q; want %#v", meta.ArtifactRef, refs)
		}
		delete(want, meta.ArtifactRef)
	}
	if len(want) != 0 {
		t.Fatalf("missing artifact refs %#v", want)
	}
}

func assertTask2ListQuery(t *testing.T, query ListQuery, tenantID, sessionID, runID string) {
	t.Helper()
	if query.TenantID != tenantID || query.SessionID != sessionID || query.RunID != runID {
		t.Fatalf("List query scope = tenant %q session %q run %q, want tenant %q session %q run %q",
			query.TenantID, query.SessionID, query.RunID, tenantID, sessionID, runID)
	}
}

func assertTask2ArtifactStatus(t *testing.T, metadata MetadataStore, ref string, want ArtifactStatus) {
	t.Helper()
	meta, err := metadata.GetByRef(context.Background(), ref)
	if err != nil {
		t.Fatalf("GetByRef(%q): %v", ref, err)
	}
	if meta.Status != want {
		t.Fatalf("artifact %q status = %q, want %q", ref, meta.Status, want)
	}
}

func callWithoutPanic(call func() error) (err error, panicValue any) {
	defer func() {
		panicValue = recover()
	}()
	return call(), nil
}

func actorContext() context.Context {
	return ContextWithActor(context.Background(), Actor{
		TenantID:  "tenant-a",
		UserID:    "user-1",
		SessionID: "sess-1",
		RunID:     "run-1",
		Role:      ActorUser,
	})
}

func runtimeContext() context.Context {
	return ContextWithActor(context.Background(), Actor{
		TenantID:  "tenant-a",
		UserID:    "runtime",
		SessionID: "sess-1",
		RunID:     "run-1",
		Role:      ActorRuntime,
	})
}

type fixedClock struct {
	now time.Time
}

func (c fixedClock) Now() time.Time {
	return c.now
}

type trackingObjectStore struct {
	mu          sync.RWMutex
	objects     map[string][]byte
	putCalls    int
	deleteCalls int
	putKeys     []string
	deleteKeys  []string
	deleteErr   error
}

func newTrackingObjectStore() *trackingObjectStore {
	return &trackingObjectStore{objects: make(map[string][]byte)}
}

func (s *trackingObjectStore) Backend() string {
	return "tracking"
}

func (s *trackingObjectStore) Put(_ context.Context, key string, r io.Reader) (*ObjectInfo, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putCalls++
	s.putKeys = append(s.putKeys, key)
	s.objects[key] = append([]byte(nil), data...)
	return &ObjectInfo{Backend: s.Backend(), Key: key, Size: int64(len(data))}, nil
}

func (s *trackingObjectStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, &Error{Code: ErrNotFound, Message: "object not found: " + key}
	}
	return io.NopCloser(strings.NewReader(string(data))), nil
}

func (s *trackingObjectStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteCalls++
	s.deleteKeys = append(s.deleteKeys, key)
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.objects, key)
	return nil
}

func (s *trackingObjectStore) CreateDownloadURL(_ context.Context, key string, ttl time.Duration) (*DownloadURL, error) {
	if ttl <= 0 {
		ttl = time.Minute
	}
	return &DownloadURL{URL: "tracking://" + key, ExpiresAt: time.Now().Add(ttl)}, nil
}

func (s *trackingObjectStore) LiveObjects() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.objects)
}

func (s *trackingObjectStore) PutCalls() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.putCalls
}

func (s *trackingObjectStore) DeleteCalls() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.deleteCalls
}

func (s *trackingObjectStore) PutKeys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.putKeys...)
}

func (s *trackingObjectStore) DeleteKeys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.deleteKeys...)
}

func (s *trackingObjectStore) LiveKeys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.objects))
	for key := range s.objects {
		keys = append(keys, key)
	}
	return keys
}

func task3LoserObjectKey(t *testing.T, putKeys []string, winnerKey string) string {
	t.Helper()
	if len(putKeys) != 2 {
		t.Fatalf("put keys = %#v, want exactly two candidates", putKeys)
	}
	var loserKey string
	for _, key := range putKeys {
		if key == winnerKey {
			continue
		}
		if loserKey != "" {
			t.Fatalf("put keys = %#v, want exactly one loser for winner %q", putKeys, winnerKey)
		}
		loserKey = key
	}
	if loserKey == "" {
		t.Fatalf("put keys = %#v, missing loser for winner %q", putKeys, winnerKey)
	}
	return loserKey
}

func task3ContainsKey(keys []string, want string) bool {
	for _, key := range keys {
		if key == want {
			return true
		}
	}
	return false
}

type barrierMissMetadataStore struct {
	MetadataStore
	mu       sync.Mutex
	want     int
	arrived  int
	released chan struct{}
	once     sync.Once
}

func newBarrierMissMetadataStore(store MetadataStore, want int) *barrierMissMetadataStore {
	return &barrierMissMetadataStore{
		MetadataStore: store,
		want:          want,
		released:      make(chan struct{}),
	}
}

func (s *barrierMissMetadataStore) GetByIdempotencyKey(ctx context.Context, _ string) (*ArtifactMeta, error) {
	s.mu.Lock()
	s.arrived++
	if s.arrived == s.want {
		s.once.Do(func() { close(s.released) })
	}
	released := s.released
	s.mu.Unlock()

	select {
	case <-released:
		return nil, &Error{Code: ErrNotFound, Message: "simulated concurrent cache miss"}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type task3PutResult struct {
	meta *ArtifactMeta
	err  error
}

func runTwoTask3IdempotentPuts(store *Store) [2]task3PutResult {
	var results [2]task3PutResult
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := validPutRequest()
			req.MimeType = "text/plain"
			req.Content = strings.NewReader("same deterministic race content")
			req.IdempotencyKey = "deterministic-race"
			results[i].meta, results[i].err = store.Put(runtimeContext(), req)
		}(i)
	}
	wg.Wait()
	return results
}
