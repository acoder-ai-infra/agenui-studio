package artifact_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	. "github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/objectstore"
)

func TestStoreUsesOpaqueScopedIdempotencyIndexKey(t *testing.T) {
	metadata := &idempotencyKeyRecordingStore{MetadataStore: metastore.NewMemory()}
	store := NewStore(StoreConfig{
		ObjectStore:   objectstore.NewMemory(),
		MetadataStore: metadata,
	})
	req := validPutRequest()
	req.IdempotencyKey = "logical-retry-key"

	if _, err := store.Put(runtimeContext(), req); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	material := req.TenantID + "\x00" + req.SessionID + "\x00" + req.RunID + "\x00" + req.IdempotencyKey
	sum := sha256.Sum256([]byte(material))
	want := "artifact:" + hex.EncodeToString(sum[:])
	if len(metadata.getKeys) != 1 || metadata.getKeys[0] != want {
		t.Fatalf("GetByIdempotencyKey() keys = %#v, want [%q]", metadata.getKeys, want)
	}
	if len(metadata.createKeys) != 1 || metadata.createKeys[0] != want {
		t.Fatalf("Create() keys = %#v, want [%q]", metadata.createKeys, want)
	}
	if !regexp.MustCompile(`^artifact:[0-9a-f]{64}$`).MatchString(want) {
		t.Fatalf("scoped key %q does not have opaque digest format", want)
	}
	for _, raw := range []string{req.TenantID, req.SessionID, req.RunID, req.IdempotencyKey} {
		if strings.Contains(want, raw) {
			t.Fatalf("scoped key %q exposes raw input %q", want, raw)
		}
	}
}

func TestIdempotencyKeyCannotCrossTenantSessionOrRun(t *testing.T) {
	tests := []struct {
		name      string
		tenantID  string
		sessionID string
		runID     string
	}{
		{name: "tenant", tenantID: "tenant-b", sessionID: "sess-1", runID: "run-1"},
		{name: "session", tenantID: "tenant-a", sessionID: "sess-2", runID: "run-1"},
		{name: "run", tenantID: "tenant-a", sessionID: "sess-1", runID: "run-2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			firstReq := validPutRequest()
			firstReq.IdempotencyKey = "same-logical-key"
			firstReq.Content = strings.NewReader("same content")
			first, err := store.Put(runtimeContext(), firstReq)
			if err != nil {
				t.Fatalf("first Put() error = %v", err)
			}

			secondReq := firstReq
			secondReq.TenantID = tt.tenantID
			secondReq.SessionID = tt.sessionID
			secondReq.RunID = tt.runID
			secondReq.Content = strings.NewReader("same content")
			second, err := store.Put(runtimeContextFor(tt.tenantID, tt.sessionID, tt.runID), secondReq)
			if err != nil {
				t.Fatalf("second Put() error = %v", err)
			}
			if second.ArtifactID == first.ArtifactID {
				t.Fatalf("cross-%s request reused ArtifactID %q", tt.name, first.ArtifactID)
			}
			if second.TenantID != tt.tenantID || second.SessionID != tt.sessionID || second.RunID != tt.runID {
				t.Fatalf("second metadata scope = %q/%q/%q, want %q/%q/%q",
					second.TenantID, second.SessionID, second.RunID, tt.tenantID, tt.sessionID, tt.runID)
			}
		})
	}
}

func TestEmptyIdempotencyKeyDoesNotCreateIndex(t *testing.T) {
	metadata := &idempotencyKeyRecordingStore{MetadataStore: metastore.NewMemory()}
	store := NewStore(StoreConfig{
		ObjectStore:   objectstore.NewMemory(),
		MetadataStore: metadata,
	})
	req := validPutRequest()
	req.IdempotencyKey = ""

	if _, err := store.Put(runtimeContext(), req); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if len(metadata.getKeys) != 0 {
		t.Fatalf("GetByIdempotencyKey() keys = %#v, want none", metadata.getKeys)
	}
	if len(metadata.createKeys) != 1 || metadata.createKeys[0] != "" {
		t.Fatalf("Create() keys = %#v, want one empty key", metadata.createKeys)
	}
}

func TestIdempotencyRejectsEveryPersistedSemanticMismatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*PutArtifactRequest, *ArtifactMeta)
	}{
		{name: "user id", mutate: func(req *PutArtifactRequest, _ *ArtifactMeta) { req.UserID = "user-2" }},
		{name: "step id", mutate: func(req *PutArtifactRequest, _ *ArtifactMeta) { req.StepID = "step-2" }},
		{name: "owner module", mutate: func(req *PutArtifactRequest, _ *ArtifactMeta) { req.OwnerModule = OwnerModuleContextEngine }},
		{name: "owner id", mutate: func(req *PutArtifactRequest, _ *ArtifactMeta) { req.OwnerID = "semantic-child-2" }},
		{name: "artifact type", mutate: func(req *PutArtifactRequest, _ *ArtifactMeta) { req.ArtifactType = ArtifactTypeContextSnapshot }},
		{name: "name", mutate: func(req *PutArtifactRequest, _ *ArtifactMeta) { req.Name = "beta.txt" }},
		{name: "visibility", mutate: func(req *PutArtifactRequest, _ *ArtifactMeta) { req.Visibility = VisibilityDebug }},
		{name: "retention", mutate: func(req *PutArtifactRequest, _ *ArtifactMeta) { req.RetentionPolicy = RetentionSessionTTL }},
		{name: "created by", mutate: func(req *PutArtifactRequest, _ *ArtifactMeta) { req.CreatedBy = "creator-2" }},
		{name: "explicit expiry", mutate: func(req *PutArtifactRequest, _ *ArtifactMeta) { req.ExpiresAt = req.ExpiresAt.Add(time.Hour) }},
		{name: "lineage", mutate: func(req *PutArtifactRequest, _ *ArtifactMeta) { req.DerivedFrom[0].Relation = LineageTransformedFrom }},
		{name: "metadata", mutate: func(req *PutArtifactRequest, _ *ArtifactMeta) { req.Metadata = map[string]string{"source": "two"} }},
		{name: "preview", mutate: func(req *PutArtifactRequest, _ *ArtifactMeta) { req.PreviewHint = PreviewHint{MaxBytes: 12} }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			parent := putTask3Parent(t, store, "semantic-parent")
			firstReq := task3SemanticRequest(parent)
			first, err := store.Put(runtimeContext(), firstReq)
			if err != nil {
				t.Fatalf("first Put() error = %v", err)
			}

			secondReq := task3SemanticRequest(parent)
			tt.mutate(&secondReq, first)
			if _, err := store.Put(runtimeContext(), secondReq); !IsErrorCode(err, ErrConflict) {
				t.Fatalf("second Put() error = %v, want conflict", err)
			}
		})
	}
}

func TestIdempotencyNormalizesCollections(t *testing.T) {
	t.Run("explicit empty lineage on both requests persists nil", func(t *testing.T) {
		store := newTestStore(t)
		firstReq := validPutRequest()
		firstReq.IdempotencyKey = "explicit-empty-lineage"
		firstReq.DerivedFrom = make([]ArtifactLineage, 0)
		first, err := store.Put(runtimeContext(), firstReq)
		if err != nil {
			t.Fatalf("first Put() error = %v", err)
		}
		if first.DerivedFrom != nil {
			t.Fatalf("first persisted lineage = %#v, want nil", first.DerivedFrom)
		}

		secondReq := validPutRequest()
		secondReq.IdempotencyKey = firstReq.IdempotencyKey
		secondReq.DerivedFrom = make([]ArtifactLineage, 0)
		second, err := store.Put(runtimeContext(), secondReq)
		if err != nil || second.ArtifactID != first.ArtifactID {
			t.Fatalf("explicit empty retry = %#v, error = %v; want ArtifactID %q", second, err, first.ArtifactID)
		}
		if second.DerivedFrom != nil {
			t.Fatalf("retry persisted lineage = %#v, want nil", second.DerivedFrom)
		}
	})

	t.Run("zero length persists nil and matches nil", func(t *testing.T) {
		store := newTestStore(t)
		firstReq := validPutRequest()
		firstReq.IdempotencyKey = "empty-collections"
		firstReq.DerivedFrom = make([]ArtifactLineage, 0)
		firstReq.Metadata = map[string]string{}
		first, err := store.Put(runtimeContext(), firstReq)
		if err != nil {
			t.Fatalf("first Put() error = %v", err)
		}
		if first.DerivedFrom != nil || first.Metadata != nil {
			t.Fatalf("persisted empty collections = lineage %#v metadata %#v, want nil", first.DerivedFrom, first.Metadata)
		}

		secondReq := validPutRequest()
		secondReq.IdempotencyKey = firstReq.IdempotencyKey
		second, err := store.Put(runtimeContext(), secondReq)
		if err != nil || second.ArtifactID != first.ArtifactID {
			t.Fatalf("nil retry metadata = %#v, error = %v; want ArtifactID %q", second, err, first.ArtifactID)
		}
	})

	t.Run("metadata insertion order is irrelevant", func(t *testing.T) {
		store := newTestStore(t)
		firstReq := validPutRequest()
		firstReq.IdempotencyKey = "metadata-order"
		firstReq.Metadata = map[string]string{"alpha": "1", "beta": "2"}
		first, err := store.Put(runtimeContext(), firstReq)
		if err != nil {
			t.Fatalf("first Put() error = %v", err)
		}

		secondReq := validPutRequest()
		secondReq.IdempotencyKey = firstReq.IdempotencyKey
		secondReq.Metadata = make(map[string]string)
		secondReq.Metadata["beta"] = "2"
		secondReq.Metadata["alpha"] = "1"
		second, err := store.Put(runtimeContext(), secondReq)
		if err != nil || second.ArtifactID != first.ArtifactID {
			t.Fatalf("reordered metadata retry = %#v, error = %v; want ArtifactID %q", second, err, first.ArtifactID)
		}
	})

	t.Run("lineage order is semantic", func(t *testing.T) {
		store := newTestStore(t)
		parentA := putTask3Parent(t, store, "lineage-parent-a")
		parentB := putTask3Parent(t, store, "lineage-parent-b")
		firstReq := validPutRequest()
		firstReq.OwnerID = "lineage-child"
		firstReq.IdempotencyKey = "lineage-order"
		firstReq.DerivedFrom = []ArtifactLineage{
			{ArtifactRef: parentA.ArtifactRef, Relation: LineageReferenced},
			{ArtifactRef: parentB.ArtifactRef, Relation: LineageMergedFrom},
		}
		if _, err := store.Put(runtimeContext(), firstReq); err != nil {
			t.Fatalf("first Put() error = %v", err)
		}

		secondReq := validPutRequest()
		secondReq.OwnerID = firstReq.OwnerID
		secondReq.IdempotencyKey = firstReq.IdempotencyKey
		secondReq.DerivedFrom = []ArtifactLineage{
			{ArtifactRef: parentB.ArtifactRef, Relation: LineageMergedFrom},
			{ArtifactRef: parentA.ArtifactRef, Relation: LineageReferenced},
		}
		if _, err := store.Put(runtimeContext(), secondReq); !IsErrorCode(err, ErrConflict) {
			t.Fatalf("reordered lineage Put() error = %v, want conflict", err)
		}
	})
}

func TestIdempotencyComparesPersistedMIMEExactly(t *testing.T) {
	t.Run("exact MIME retries", func(t *testing.T) {
		store := newTestStore(t)
		req := validPutRequest()
		req.MimeType = "application/json; charset=utf-8"
		req.IdempotencyKey = "mime-exact"
		first, err := store.Put(runtimeContext(), req)
		if err != nil {
			t.Fatalf("first Put() error = %v", err)
		}
		if first.MimeType != req.MimeType {
			t.Fatalf("persisted MIME = %q, want exact %q", first.MimeType, req.MimeType)
		}
		retry := req
		retry.Content = strings.NewReader(`{"ok":true}`)
		second, err := store.Put(runtimeContext(), retry)
		if err != nil || second.ArtifactID != first.ArtifactID {
			t.Fatalf("exact MIME retry = %#v, error = %v; want ArtifactID %q", second, err, first.ArtifactID)
		}
	})

	for _, tt := range []struct {
		name       string
		firstMIME  string
		secondMIME string
	}{
		{name: "spelling", firstMIME: "text/plain", secondMIME: "TEXT/PLAIN"},
		{name: "parameter order", firstMIME: "text/plain; charset=utf-8; format=flowed", secondMIME: "text/plain; format=flowed; charset=utf-8"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			firstReq := validPutRequest()
			firstReq.MimeType = tt.firstMIME
			firstReq.Content = strings.NewReader("same")
			firstReq.IdempotencyKey = "mime-different-" + tt.name
			if _, err := store.Put(runtimeContext(), firstReq); err != nil {
				t.Fatalf("first Put() error = %v", err)
			}
			secondReq := firstReq
			secondReq.MimeType = tt.secondMIME
			secondReq.Content = strings.NewReader("same")
			if _, err := store.Put(runtimeContext(), secondReq); !IsErrorCode(err, ErrConflict) {
				t.Fatalf("different MIME Put() error = %v, want conflict", err)
			}
		})
	}
}

func TestIdempotentPutComparesSanitizedNameAndSafePreview(t *testing.T) {
	store := newTestStore(t)
	req := validPutRequest()
	req.Name = `C:\private\report.txt`
	req.MimeType = "text/plain"
	req.Visibility = VisibilityUserVisible
	req.IdempotencyKey = "safe-preview"
	req.Content = strings.NewReader("token=secret")

	first, err := store.Put(runtimeContext(), req)
	if err != nil {
		t.Fatalf("first Put(): %v", err)
	}
	if first.Name != "report.txt" || strings.Contains(first.Preview.Text, "secret") || !strings.Contains(first.Preview.Text, "***") {
		t.Fatalf("first metadata = %#v", first)
	}

	req.Name = `/different/path/report.txt`
	req.Content = strings.NewReader("token=secret")
	second, err := store.Put(runtimeContext(), req)
	if err != nil {
		t.Fatalf("same sanitized basename retry: %v", err)
	}
	if second.ArtifactID != first.ArtifactID || second.Name != first.Name || !reflect.DeepEqual(second.Preview, first.Preview) {
		t.Fatalf("first = %#v, second = %#v", first, second)
	}

	req.Name = "different.txt"
	req.Content = strings.NewReader("token=secret")
	if _, err := store.Put(runtimeContext(), req); !IsErrorCode(err, ErrConflict) {
		t.Fatalf("different sanitized basename error = %v, want conflict", err)
	}
}

func TestStoreDefaultsCreatedByBeforeIdempotencyComparison(t *testing.T) {
	store := newTestStore(t)
	firstReq := validPutRequest()
	firstReq.IdempotencyKey = "creator-default"
	firstReq.CreatedBy = ""
	first, err := store.Put(runtimeContext(), firstReq)
	if err != nil {
		t.Fatalf("first Put() error = %v", err)
	}
	wantCreator := string(firstReq.OwnerModule) + ":" + firstReq.OwnerID
	if first.CreatedBy != wantCreator {
		t.Fatalf("CreatedBy = %q, want %q", first.CreatedBy, wantCreator)
	}

	secondReq := validPutRequest()
	secondReq.IdempotencyKey = firstReq.IdempotencyKey
	secondReq.CreatedBy = wantCreator
	second, err := store.Put(runtimeContext(), secondReq)
	if err != nil || second.ArtifactID != first.ArtifactID {
		t.Fatalf("explicit default retry = %#v, error = %v; want ArtifactID %q", second, err, first.ArtifactID)
	}
}

func TestIdempotencyNormalizesRetentionAndExpiry(t *testing.T) {
	t.Run("default retention equals explicit canonical retention", func(t *testing.T) {
		store := newTestStore(t)
		firstReq := validPutRequest()
		firstReq.IdempotencyKey = "retention-default"
		first, err := store.Put(runtimeContext(), firstReq)
		if err != nil {
			t.Fatalf("first Put() error = %v", err)
		}
		secondReq := validPutRequest()
		secondReq.IdempotencyKey = firstReq.IdempotencyKey
		secondReq.RetentionPolicy = RetentionRunTTL
		second, err := store.Put(runtimeContext(), secondReq)
		if err != nil || second.ArtifactID != first.ArtifactID {
			t.Fatalf("explicit retention retry = %#v, error = %v; want ArtifactID %q", second, err, first.ArtifactID)
		}
	})

	t.Run("zero then zero", func(t *testing.T) {
		store := newTestStore(t)
		firstReq := validPutRequest()
		firstReq.IdempotencyKey = "expiry-zero-zero"
		first, err := store.Put(runtimeContext(), firstReq)
		if err != nil {
			t.Fatalf("first Put() error = %v", err)
		}
		secondReq := validPutRequest()
		secondReq.IdempotencyKey = firstReq.IdempotencyKey
		second, err := store.Put(runtimeContext(), secondReq)
		if err != nil || second.ArtifactID != first.ArtifactID || !second.ExpiresAt.Equal(first.ExpiresAt) {
			t.Fatalf("zero expiry retry = %#v, error = %v; want ArtifactID %q expiry %v", second, err, first.ArtifactID, first.ExpiresAt)
		}
	})

	t.Run("zero then exact derived explicit", func(t *testing.T) {
		store := newTestStore(t)
		firstReq := validPutRequest()
		firstReq.IdempotencyKey = "expiry-zero-derived"
		first, err := store.Put(runtimeContext(), firstReq)
		if err != nil {
			t.Fatalf("first Put() error = %v", err)
		}
		secondReq := validPutRequest()
		secondReq.IdempotencyKey = firstReq.IdempotencyKey
		secondReq.ExpiresAt = first.ExpiresAt
		second, err := store.Put(runtimeContext(), secondReq)
		if err != nil || second.ArtifactID != first.ArtifactID {
			t.Fatalf("derived explicit retry = %#v, error = %v; want ArtifactID %q", second, err, first.ArtifactID)
		}
	})

	t.Run("zero then different explicit", func(t *testing.T) {
		store := newTestStore(t)
		firstReq := validPutRequest()
		firstReq.IdempotencyKey = "expiry-zero-different"
		first, err := store.Put(runtimeContext(), firstReq)
		if err != nil {
			t.Fatalf("first Put() error = %v", err)
		}
		secondReq := validPutRequest()
		secondReq.IdempotencyKey = firstReq.IdempotencyKey
		secondReq.ExpiresAt = first.ExpiresAt.Add(time.Second)
		if _, err := store.Put(runtimeContext(), secondReq); !IsErrorCode(err, ErrConflict) {
			t.Fatalf("different explicit retry error = %v, want conflict", err)
		}
	})

	t.Run("custom explicit then zero", func(t *testing.T) {
		store := newTestStore(t)
		firstReq := validPutRequest()
		firstReq.IdempotencyKey = "expiry-custom-zero"
		firstReq.ExpiresAt = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
		if _, err := store.Put(runtimeContext(), firstReq); err != nil {
			t.Fatalf("first Put() error = %v", err)
		}
		secondReq := validPutRequest()
		secondReq.IdempotencyKey = firstReq.IdempotencyKey
		if _, err := store.Put(runtimeContext(), secondReq); !IsErrorCode(err, ErrConflict) {
			t.Fatalf("zero retry after custom expiry error = %v, want conflict", err)
		}
	})

	t.Run("custom explicit then same explicit", func(t *testing.T) {
		store := newTestStore(t)
		firstReq := validPutRequest()
		firstReq.IdempotencyKey = "expiry-custom-same"
		firstReq.ExpiresAt = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
		first, err := store.Put(runtimeContext(), firstReq)
		if err != nil {
			t.Fatalf("first Put() error = %v", err)
		}
		secondReq := validPutRequest()
		secondReq.IdempotencyKey = firstReq.IdempotencyKey
		secondReq.ExpiresAt = firstReq.ExpiresAt
		second, err := store.Put(runtimeContext(), secondReq)
		if err != nil || second.ArtifactID != first.ArtifactID {
			t.Fatalf("same explicit retry = %#v, error = %v; want ArtifactID %q", second, err, first.ArtifactID)
		}
	})

	t.Run("custom explicit then different explicit", func(t *testing.T) {
		store := newTestStore(t)
		firstReq := validPutRequest()
		firstReq.IdempotencyKey = "expiry-custom-different"
		firstReq.ExpiresAt = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
		if _, err := store.Put(runtimeContext(), firstReq); err != nil {
			t.Fatalf("first Put() error = %v", err)
		}
		secondReq := validPutRequest()
		secondReq.IdempotencyKey = firstReq.IdempotencyKey
		secondReq.ExpiresAt = firstReq.ExpiresAt.Add(time.Second)
		if _, err := store.Put(runtimeContext(), secondReq); !IsErrorCode(err, ErrConflict) {
			t.Fatalf("different explicit retry error = %v, want conflict", err)
		}
	})
}

func TestMemoryMetadataStoreReturnsFirstForStoreSemanticComparison(t *testing.T) {
	objects := objectstore.NewMemory()
	metadata := metastore.NewMemory()
	firstStore := NewStore(StoreConfig{ObjectStore: objects, MetadataStore: metadata})
	firstReq := validPutRequest()
	firstReq.IdempotencyKey = "adapter-first-wins"
	first, err := firstStore.Put(runtimeContext(), firstReq)
	if err != nil {
		t.Fatalf("first Put() error = %v", err)
	}

	stale := &staleRecordingMetadataStore{MetadataStore: metadata}
	secondStore := NewStore(StoreConfig{ObjectStore: objects, MetadataStore: stale})
	secondReq := validPutRequest()
	secondReq.IdempotencyKey = firstReq.IdempotencyKey
	secondReq.MimeType = "text/plain"
	secondReq.Content = strings.NewReader(`{"ok":true}`)
	if _, err := secondStore.Put(runtimeContext(), secondReq); !IsErrorCode(err, ErrConflict) {
		t.Fatalf("second Put() error = %v, want conflict", err)
	}
	if stale.createErr != nil {
		t.Fatalf("MemoryMetadataStore.Create() error = %v, want authoritative first metadata", stale.createErr)
	}
	if stale.created == nil || stale.created.ArtifactID != first.ArtifactID {
		t.Fatalf("MemoryMetadataStore.Create() result = %#v, want first ArtifactID %q", stale.created, first.ArtifactID)
	}
}

func putTask3Parent(t *testing.T, store *Store, ownerID string) *ArtifactMeta {
	t.Helper()
	req := validPutRequest()
	req.OwnerID = ownerID
	req.Content = strings.NewReader(`{"parent":true}`)
	meta, err := store.Put(runtimeContext(), req)
	if err != nil {
		t.Fatalf("put parent %q: %v", ownerID, err)
	}
	return meta
}

func task3SemanticRequest(parent *ArtifactMeta) PutArtifactRequest {
	return PutArtifactRequest{
		TenantID:        "tenant-a",
		UserID:          "user-1",
		SessionID:       "sess-1",
		RunID:           "run-1",
		StepID:          "step-1",
		OwnerModule:     OwnerModuleToolGateway,
		OwnerID:         "semantic-child",
		ArtifactType:    ArtifactTypeToolResult,
		MimeType:        "text/plain",
		Name:            "alpha.txt",
		Visibility:      VisibilityInternal,
		Content:         strings.NewReader("preview content that is long enough"),
		PreviewHint:     PreviewHint{MaxBytes: 7},
		RetentionPolicy: RetentionRunTTL,
		ExpiresAt:       time.Date(2030, 2, 3, 4, 5, 6, 0, time.UTC),
		CreatedBy:       "creator-1",
		DerivedFrom: []ArtifactLineage{{
			ArtifactRef: parent.ArtifactRef,
			Relation:    LineageReferenced,
		}},
		IdempotencyKey: "semantic-all-fields",
		Metadata:       map[string]string{"source": "one"},
	}
}

func runtimeContextFor(tenantID, sessionID, runID string) context.Context {
	return ContextWithActor(context.Background(), Actor{
		TenantID:  tenantID,
		UserID:    "runtime",
		SessionID: sessionID,
		RunID:     runID,
		Role:      ActorRuntime,
	})
}

type idempotencyKeyRecordingStore struct {
	MetadataStore
	getKeys    []string
	createKeys []string
}

type staleRecordingMetadataStore struct {
	MetadataStore
	created   *ArtifactMeta
	createErr error
}

func (s *staleRecordingMetadataStore) GetByIdempotencyKey(context.Context, string) (*ArtifactMeta, error) {
	return nil, &Error{Code: ErrNotFound, Message: "simulated stale lookup"}
}

func (s *staleRecordingMetadataStore) Create(ctx context.Context, meta ArtifactMeta, key string) (*ArtifactMeta, error) {
	s.created, s.createErr = s.MetadataStore.Create(ctx, meta, key)
	return s.created, s.createErr
}

func (s *idempotencyKeyRecordingStore) GetByIdempotencyKey(ctx context.Context, key string) (*ArtifactMeta, error) {
	s.getKeys = append(s.getKeys, key)
	return s.MetadataStore.GetByIdempotencyKey(ctx, key)
}

func (s *idempotencyKeyRecordingStore) Create(ctx context.Context, meta ArtifactMeta, key string) (*ArtifactMeta, error) {
	s.createKeys = append(s.createKeys, key)
	return s.MetadataStore.Create(ctx, meta, key)
}
