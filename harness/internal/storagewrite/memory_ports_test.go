package storagewrite

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestMemoryStoresProvideTypedPorts(t *testing.T) {
	stores := NewMemoryStores()
	executor := NewExecutor(stores.Ports())

	_, err := executor.Execute(context.Background(), Plan{
		TraceID:        "trace_1",
		TenantID:       "tenant_1",
		SessionID:      "session_1",
		RunID:          "run_1",
		IdempotencyKey: "idem_1",
		Reason:         "final_response",
		RequiredWrites: []Write{
			{Store: StoreArtifact, Operation: OperationPut, Ref: "artifact:final"},
			{Store: StoreMessage, Operation: OperationInsert, Ref: "message:assistant"},
			{Store: StoreEvent, Operation: OperationAppend, Ref: "event:final_response"},
			{Store: StoreRun, Operation: OperationUpdate, Ref: "run:completed"},
		},
		OptionalWrites: []Write{
			{Store: StoreHotSession, Operation: OperationUpsert, Ref: "hot_session:cursor"},
			{Store: StoreHotContext, Operation: OperationUpsert, Ref: "hot_context:summary"},
			{Store: StoreHotStream, Operation: OperationAppend, Ref: "hot_stream:end"},
		},
	})
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	assertRecordCount(t, stores, StoreArtifact, 1)
	assertRecordCount(t, stores, StoreMessage, 1)
	assertRecordCount(t, stores, StoreEvent, 1)
	assertRecordCount(t, stores, StoreRun, 1)
	assertRecordCount(t, stores, StoreHotSession, 1)
	assertRecordCount(t, stores, StoreHotContext, 1)
	assertRecordCount(t, stores, StoreHotStream, 1)
}

func TestMemoryPortRejectsWrongStore(t *testing.T) {
	port := NewMemoryPort(StoreRun)
	_, err := port.Write(context.Background(), Write{Store: StoreEvent, Operation: OperationAppend, Ref: "event"})
	if !errors.Is(err, ErrStoreMismatch) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMemoryStoresConcurrentWritesAreIsolatedByStore(t *testing.T) {
	stores := NewMemoryStores()
	executor := NewExecutor(stores.Ports())
	const workers = 16

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := executor.Execute(context.Background(), Plan{
				TraceID:        "trace_1",
				TenantID:       "tenant_1",
				SessionID:      "session_1",
				RunID:          "run_1",
				IdempotencyKey: "idem_concurrent_",
				Reason:         "delta",
				RequiredWrites: []Write{
					{Store: StoreEvent, Operation: OperationAppend, Ref: "event:delta:" + string(rune('a'+i))},
					{Store: StoreHotStream, Operation: OperationAppend, Ref: "hot:delta:" + string(rune('a'+i))},
				},
			})
			if err != nil {
				t.Errorf("execute failed: %v", err)
			}
		}(i)
	}
	wg.Wait()

	assertRecordCount(t, stores, StoreEvent, workers)
	assertRecordCount(t, stores, StoreHotStream, workers)
	assertRecordCount(t, stores, StoreRun, 0)
}

func assertRecordCount(t *testing.T, stores *MemoryStores, store StoreName, want int) {
	t.Helper()
	got := stores.Records(store)
	if len(got) != want {
		t.Fatalf("%s record count mismatch: got %d want %d records=%#v", store, len(got), want, got)
	}
}
