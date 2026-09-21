package app

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	metamem "github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	objmem "github.com/AGenUI/agenui-studio/harness/internal/artifact/objectstore"
	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestArtifactContextSnapshotStoreSurvivesAdapterRebuild(t *testing.T) {
	artifacts := artifact.NewStore(artifact.StoreConfig{ObjectStore: objmem.NewMemory(), MetadataStore: metamem.NewMemory()})
	ctx := snapshotTraceContext("tenant-1", "user-1", "sess-1", "run-1")
	snapshot := contextpkg.Snapshot{
		ID: "snapshot-1", SessionID: "sess-1", RunID: "run-1", MessageIDs: []string{"m1"},
		Source: "message_ledger_fallback", ContentHash: "sha256:content", CreatedAt: time.Unix(1, 0).UTC(),
	}

	ref, err := (artifactContextSnapshotStore{artifacts: artifacts}).Save(ctx, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ref, "artifact://tenants/tenant-1/sessions/sess-1/runs/run-1/") {
		t.Fatalf("snapshot ref = %q", ref)
	}
	loaded, err := (artifactContextSnapshotStore{artifacts: artifacts}).Load(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != ref || loaded.SchemaVersion != contextpkg.SnapshotSchemaVersion || loaded.ContentHash != snapshot.ContentHash {
		t.Fatalf("loaded snapshot = %#v", loaded)
	}
}

func TestArtifactContextSnapshotStoreConcurrentIdempotentSaveConverges(t *testing.T) {
	artifacts := artifact.NewStore(artifact.StoreConfig{ObjectStore: objmem.NewMemory(), MetadataStore: metamem.NewMemory()})
	store := artifactContextSnapshotStore{artifacts: artifacts}
	ctx := snapshotTraceContext("tenant-1", "user-1", "sess-1", "run-1")
	snapshot := contextpkg.Snapshot{
		ID: "snapshot-concurrent", SessionID: "sess-1", RunID: "run-1", Source: "hot_context_cache",
		ContentHash: "sha256:stable", CreatedAt: time.Unix(1, 0).UTC(),
	}
	const workers = 16
	refs := make(chan string, workers)
	errs := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			ref, err := store.Save(ctx, snapshot)
			refs <- ref
			errs <- err
		}()
	}
	group.Wait()
	close(refs)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Save() error = %v", err)
		}
	}
	var expected string
	for ref := range refs {
		if expected == "" {
			expected = ref
		}
		if ref != expected {
			t.Fatalf("concurrent refs diverged: got %q want %q", ref, expected)
		}
	}
}

func TestArtifactContextSnapshotStoreRejectsCorruptIdentityAndOversize(t *testing.T) {
	artifacts := artifact.NewStore(artifact.StoreConfig{ObjectStore: objmem.NewMemory(), MetadataStore: metamem.NewMemory(), MaxObjectBytes: 8 << 20})
	ctx := snapshotTraceContext("tenant-1", "user-1", "sess-1", "run-1")
	actorCtx := artifact.ContextWithActor(ctx, artifact.Actor{
		Role: artifact.ActorContextEngine, TenantID: "tenant-1", UserID: "user-1", SessionID: "sess-1", RunID: "run-1",
	})

	bad := contextpkg.Snapshot{SchemaVersion: contextpkg.SnapshotSchemaVersion, ID: "wrong-ref", SessionID: "sess-1", RunID: "run-1", ContentHash: "sha256:x"}
	payload, _ := json.Marshal(bad)
	meta, err := artifacts.Put(actorCtx, artifact.PutArtifactRequest{
		TenantID: "tenant-1", UserID: "user-1", SessionID: "sess-1", RunID: "run-1",
		OwnerModule: artifact.OwnerModuleContextEngine, OwnerID: "run-1", ArtifactType: artifact.ArtifactTypeContextSnapshot,
		MimeType: "application/json", Name: "bad.json", Visibility: artifact.VisibilityInternal,
		RetentionPolicy: artifact.RetentionSessionTTL, Content: bytes.NewReader(payload), Metadata: map[string]string{"content_hash": "sha256:x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (artifactContextSnapshotStore{artifacts: artifacts}).Load(ctx, meta.ArtifactRef); err == nil {
		t.Fatal("corrupt snapshot identity was accepted")
	}

	large := append([]byte(`{"schema_version":"harness.context_snapshot.v1","padding":"`), bytes.Repeat([]byte("x"), maxContextSnapshotBytes)...)
	large = append(large, []byte(`"}`)...)
	largeMeta, err := artifacts.Put(actorCtx, artifact.PutArtifactRequest{
		TenantID: "tenant-1", UserID: "user-1", SessionID: "sess-1", RunID: "run-1",
		OwnerModule: artifact.OwnerModuleContextEngine, OwnerID: "run-1", ArtifactType: artifact.ArtifactTypeContextSnapshot,
		MimeType: "application/json", Name: "large.json", Visibility: artifact.VisibilityInternal,
		RetentionPolicy: artifact.RetentionSessionTTL, Content: bytes.NewReader(large),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (artifactContextSnapshotStore{artifacts: artifacts}).Load(ctx, largeMeta.ArtifactRef); err == nil {
		t.Fatal("oversized snapshot was accepted")
	}
}

func snapshotTraceContext(tenantID, userID, sessionID, runID string) context.Context {
	return observability.WithTraceContext(context.Background(), observability.TraceContext{
		TenantID: tenantID, UserID: userID, SessionID: sessionID, RunID: runID, AgentID: "agent-1",
	})
}
