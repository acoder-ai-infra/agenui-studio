package runtimestore_test

import (
	"context"
	"sync"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	metastore "github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	objectstore "github.com/AGenUI/agenui-studio/harness/internal/artifact/objectstore"
	"github.com/AGenUI/agenui-studio/harness/internal/runtimestore"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	storagememory "github.com/AGenUI/agenui-studio/harness/internal/storage/memory"
)

func TestArtifactCheckpointStorePersistsRevisionsAndReloadsAcrossInstances(t *testing.T) {
	stores := storagememory.New().Stores()
	artifacts := artifact.NewStore(artifact.StoreConfig{ObjectStore: objectstore.NewMemory(), MetadataStore: metastore.NewMemory()})
	first := &runtimestore.ArtifactCheckpointStore{Metadata: stores.Checkpoints, Artifacts: artifacts}
	scope := checkpointScope()
	ctx := context.Background()
	for revision, payload := range [][]byte{[]byte("state-one"), []byte("state-two")} {
		saved, err := first.Save(ctx, agentruntime.RuntimeCheckpointRecord{Scope: scope, CheckpointID: "eino_ckpt_run-1", Payload: payload})
		if err != nil {
			t.Fatalf("Save(%d): %v", revision+1, err)
		}
		if saved.Revision != int64(revision+1) {
			t.Fatalf("revision=%d", saved.Revision)
		}
	}
	second := &runtimestore.ArtifactCheckpointStore{Metadata: stores.Checkpoints, Artifacts: artifacts}
	loaded, ok, err := second.Load(ctx, agentruntime.RuntimeCheckpointKey{Scope: scope, CheckpointID: "eino_ckpt_run-1"})
	if err != nil || !ok {
		t.Fatalf("Load() ok=%v err=%v", ok, err)
	}
	if string(loaded.Payload) != "state-two" || loaded.Revision != 2 || loaded.BackendRef == "" {
		t.Fatalf("loaded=%#v", loaded)
	}
}

func TestArtifactCheckpointStoreConcurrentSavesLeaveReadablePointer(t *testing.T) {
	stores := storagememory.New().Stores()
	artifacts := artifact.NewStore(artifact.StoreConfig{ObjectStore: objectstore.NewMemory(), MetadataStore: metastore.NewMemory()})
	store := &runtimestore.ArtifactCheckpointStore{Metadata: stores.Checkpoints, Artifacts: artifacts}
	scope := checkpointScope()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(value byte) {
			defer wg.Done()
			_, err := store.Save(context.Background(), agentruntime.RuntimeCheckpointRecord{Scope: scope, CheckpointID: "eino_ckpt_run-1", Payload: []byte{value}})
			errs <- err
		}(byte(i))
	}
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
			continue
		}
		if !storage.IsErrorCode(err, storage.ErrCASMismatch) {
			t.Fatalf("unexpected concurrent Save error: %v", err)
		}
	}
	if successes == 0 {
		t.Fatal("no concurrent checkpoint save succeeded")
	}
	loaded, ok, err := store.Load(context.Background(), agentruntime.RuntimeCheckpointKey{Scope: scope, CheckpointID: "eino_ckpt_run-1"})
	if err != nil || !ok || len(loaded.Payload) != 1 || loaded.Revision < 1 {
		t.Fatalf("Load()=%#v,%v,%v", loaded, ok, err)
	}
}

func checkpointScope() agentruntime.RuntimeCheckpointScope {
	return agentruntime.RuntimeCheckpointScope{TenantID: "tenant-1", SessionID: "session-1", RunID: "run-1", Runtime: agentruntime.RuntimeTypeEino, RuntimeVersion: agentruntime.EinoRuntimeVersion, AdapterVersion: agentruntime.EinoAdapterVersion}
}
