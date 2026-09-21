package agentruntime

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestInMemoryRuntimeCheckpointStoreRoundTripAndIsolation(t *testing.T) {
	store := NewInMemoryRuntimeCheckpointStore()
	scope := testCheckpointScope("tenant_a", "run_1")
	record, err := store.Save(context.Background(), RuntimeCheckpointRecord{
		Scope:        scope,
		CheckpointID: "checkpoint_1",
		Payload:      []byte("state-v1"),
	})
	if err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}
	if record.Revision != 1 || record.SchemaVersion != RuntimeCheckpointSchemaVersion || record.PayloadHash == "" || record.SizeBytes != int64(len("state-v1")) {
		t.Fatalf("invalid saved record: %#v", record)
	}

	loaded, ok, err := store.Load(context.Background(), RuntimeCheckpointKey{Scope: scope, CheckpointID: "checkpoint_1"})
	if err != nil || !ok {
		t.Fatalf("load checkpoint: ok=%v err=%v", ok, err)
	}
	if !bytes.Equal(loaded.Payload, []byte("state-v1")) {
		t.Fatalf("payload mismatch: %q", loaded.Payload)
	}
	loaded.Payload[0] = 'X'
	again, _, _ := store.Load(context.Background(), RuntimeCheckpointKey{Scope: scope, CheckpointID: "checkpoint_1"})
	if !bytes.Equal(again.Payload, []byte("state-v1")) {
		t.Fatal("caller mutation leaked into checkpoint store")
	}

	otherScope := testCheckpointScope("tenant_b", "run_1")
	if _, ok, err := store.Load(context.Background(), RuntimeCheckpointKey{Scope: otherScope, CheckpointID: "checkpoint_1"}); err != nil || ok {
		t.Fatalf("checkpoint leaked across tenant scope: ok=%v err=%v", ok, err)
	}
}

func TestInMemoryRuntimeCheckpointStoreOverwriteIncrementsRevision(t *testing.T) {
	store := NewInMemoryRuntimeCheckpointStore()
	scope := testCheckpointScope("tenant_a", "run_1")
	for revision, payload := range []string{"v1", "v2", "v3"} {
		record, err := store.Save(context.Background(), RuntimeCheckpointRecord{Scope: scope, CheckpointID: "checkpoint_1", Payload: []byte(payload)})
		if err != nil {
			t.Fatalf("save revision %d: %v", revision+1, err)
		}
		if record.Revision != int64(revision+1) {
			t.Fatalf("revision = %d, want %d", record.Revision, revision+1)
		}
	}
	loaded, ok, err := store.Load(context.Background(), RuntimeCheckpointKey{Scope: scope, CheckpointID: "checkpoint_1"})
	if err != nil || !ok || string(loaded.Payload) != "v3" || loaded.Revision != 3 {
		t.Fatalf("unexpected final checkpoint: ok=%v err=%v record=%#v", ok, err, loaded)
	}
}

func TestInMemoryRuntimeCheckpointStoreIsolatesRuntimeAndAdapterVersions(t *testing.T) {
	store := NewInMemoryRuntimeCheckpointStore()
	scope := testCheckpointScope("tenant_a", "run_versioned")
	scope.RuntimeVersion = "runtime-v1"
	scope.AdapterVersion = "adapter-v1"
	if _, err := store.Save(context.Background(), RuntimeCheckpointRecord{Scope: scope, CheckpointID: "checkpoint_1", Payload: []byte("state")}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}
	for _, mutate := range []func(*RuntimeCheckpointScope){
		func(value *RuntimeCheckpointScope) { value.RuntimeVersion = "runtime-v2" },
		func(value *RuntimeCheckpointScope) { value.AdapterVersion = "adapter-v2" },
	} {
		other := scope
		mutate(&other)
		if _, ok, err := store.Load(context.Background(), RuntimeCheckpointKey{Scope: other, CheckpointID: "checkpoint_1"}); err != nil || ok {
			t.Fatalf("checkpoint leaked across runtime binding: scope=%#v ok=%v err=%v", other, ok, err)
		}
	}
}

func TestInMemoryRuntimeCheckpointStoreConcurrentWritesRemainValid(t *testing.T) {
	store := NewInMemoryRuntimeCheckpointStore()
	scope := testCheckpointScope("tenant_a", "run_concurrent")
	const writers = 32
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.Save(context.Background(), RuntimeCheckpointRecord{
				Scope:        scope,
				CheckpointID: "checkpoint_1",
				Payload:      []byte(fmt.Sprintf("state-%d", i)),
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent save failed: %v", err)
		}
	}
	loaded, ok, err := store.Load(context.Background(), RuntimeCheckpointKey{Scope: scope, CheckpointID: "checkpoint_1"})
	if err != nil || !ok || loaded.Revision != writers || runtimeCheckpointHash(loaded.Payload) != loaded.PayloadHash {
		t.Fatalf("invalid concurrent result: ok=%v err=%v record=%#v", ok, err, loaded)
	}
}

func TestInMemoryRuntimeCheckpointStoreExpiresAndDeletes(t *testing.T) {
	store := NewInMemoryRuntimeCheckpointStore()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	store.clock = func() time.Time { return now }
	scope := testCheckpointScope("tenant_a", "run_1")
	key := RuntimeCheckpointKey{Scope: scope, CheckpointID: "checkpoint_1"}
	if _, err := store.Save(context.Background(), RuntimeCheckpointRecord{Scope: scope, CheckpointID: key.CheckpointID, Payload: []byte("state"), ExpiresAt: now.Add(time.Minute)}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}
	now = now.Add(2 * time.Minute)
	if _, ok, err := store.Load(context.Background(), key); err != nil || ok {
		t.Fatalf("expired checkpoint remained visible: ok=%v err=%v", ok, err)
	}
	if err := store.Delete(context.Background(), key); err != nil {
		t.Fatalf("delete expired checkpoint: %v", err)
	}
}

func TestEinoCheckpointStoreProxyUsesHarnessStore(t *testing.T) {
	store := NewInMemoryRuntimeCheckpointStore()
	scope := testCheckpointScope("tenant_a", "run_eino")
	proxy, err := newEinoCheckpointStoreProxy(store, scope)
	if err != nil {
		t.Fatalf("create proxy: %v", err)
	}
	if err := proxy.Set(context.Background(), "checkpoint_1", []byte("eino-state")); err != nil {
		t.Fatalf("set through proxy: %v", err)
	}
	payload, ok, err := proxy.Get(context.Background(), "checkpoint_1")
	if err != nil || !ok || !bytes.Equal(payload, []byte("eino-state")) {
		t.Fatalf("get through proxy: ok=%v err=%v payload=%q", ok, err, payload)
	}
	deleter, ok := proxy.(interface {
		Delete(context.Context, string) error
	})
	if !ok {
		t.Fatal("eino checkpoint proxy must support deletion")
	}
	if err := deleter.Delete(context.Background(), "checkpoint_1"); err != nil {
		t.Fatalf("delete through proxy: %v", err)
	}
	if _, ok, err := proxy.Get(context.Background(), "checkpoint_1"); err != nil || ok {
		t.Fatalf("deleted checkpoint remained visible: ok=%v err=%v", ok, err)
	}
}

func testCheckpointScope(tenantID, runID string) RuntimeCheckpointScope {
	return RuntimeCheckpointScope{
		TenantID:  tenantID,
		SessionID: "session_1",
		RunID:     runID,
		Runtime:   RuntimeTypeEino,
	}
}
