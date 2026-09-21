package dispatcher

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
)

func TestInMemoryRunRequestStoreFreezesAndReturnsCopies(t *testing.T) {
	store := NewInMemoryRunRequestStore()
	req := testRunRequest("run_store")
	req.Metadata = map[string]string{"key": "original"}
	ref, err := store.Put(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	req.Metadata["key"] = "mutated"
	got, err := store.Get(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["key"] != "original" {
		t.Fatalf("stored request was mutated: %#v", got.Metadata)
	}
	got.Metadata["key"] = "read-mutated"
	again, _ := store.Get(context.Background(), ref)
	if again.Metadata["key"] != "original" {
		t.Fatalf("returned request was not isolated: %#v", again.Metadata)
	}
}

func TestInMemoryRunRequestStoreRejectsDriftForSameRun(t *testing.T) {
	store := NewInMemoryRunRequestStore()
	req := testRunRequest("run_conflict")
	req.Metadata = map[string]string{"version": "v1"}
	if _, err := store.Put(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.Metadata["version"] = "v2"
	if _, err := store.Put(context.Background(), req); !errors.Is(err, ErrRunRequestConflict) {
		t.Fatalf("Put() error = %v, want conflict", err)
	}
}

func TestInMemoryRunRequestStoreFreezesCompleteRuntimeInput(t *testing.T) {
	store := NewInMemoryRunRequestStore()
	req := testRunRequest("run_complete_store")
	req.Input = []agentruntime.Message{{ID: "message_1", Role: "user", Content: "hello"}}
	req.ContextSnapshotRef = "context-snapshot://ctx_1"
	req.ConfigSnapshotRef = "config-snapshot://cfg_1"
	req.AgentBindingID = "binding_1"
	req.UserID = "user_1"
	req.TenantID = "tenant_1"
	req.Metadata = map[string]string{"binding_hash": "sha256:binding", "config_hash": "sha256:config"}

	ref, err := store.Put(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	req.Input[0].Content = "mutated"
	req.Metadata["binding_hash"] = "mutated"
	got, err := store.Get(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.Input[0].Content != "hello" || got.ContextSnapshotRef != "context-snapshot://ctx_1" ||
		got.ConfigSnapshotRef != "config-snapshot://cfg_1" || got.AgentBindingID != "binding_1" ||
		got.UserID != "user_1" || got.TenantID != "tenant_1" || got.Metadata["binding_hash"] != "sha256:binding" {
		t.Fatalf("incomplete frozen request: %#v", got)
	}
}

func TestInMemoryRunRequestStoreIgnoresDeliveryTraceDrift(t *testing.T) {
	store := NewInMemoryRunRequestStore()
	req := testRunRequest("run_trace_retry")
	req.UserID = "user_1"
	req.TenantID = "tenant_1"
	req.Trace.RequestID = "request_first"
	req.Trace.SpanID = "span_first"
	req.Trace.Baggage = map[string]string{"attempt": "first"}
	first, err := store.Put(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	req.Trace.RequestID = "request_retry"
	req.Trace.SpanID = "span_retry"
	req.Trace.Baggage = map[string]string{"attempt": "retry"}
	second, err := store.Put(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("retry ref = %q, want %q", second, first)
	}
	got, err := store.Get(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if got.Trace.RequestID != "" || got.Trace.SpanID != "" || got.Trace.Baggage != nil ||
		got.Trace.RunID != req.RunID || got.Trace.SessionID != req.SessionID ||
		got.Trace.UserID != req.UserID || got.Trace.TenantID != req.TenantID {
		t.Fatalf("frozen trace = %#v", got.Trace)
	}
}

func TestInMemoryRunRequestStoreDetectsStoredDataTampering(t *testing.T) {
	store := NewInMemoryRunRequestStore()
	ref, err := store.Put(context.Background(), testRunRequest("run_integrity"))
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	stored := store.byRef[ref]
	stored.data[0] ^= 0xff
	store.byRef[ref] = stored
	store.mu.Unlock()
	if _, err := store.Get(context.Background(), ref); !errors.Is(err, ErrRunRequestIntegrity) {
		t.Fatalf("Get() error = %v, want integrity failure", err)
	}
}

func TestInMemoryRunRequestStoreConcurrentIdempotency(t *testing.T) {
	store := NewInMemoryRunRequestStore()
	req := testRunRequest("run_concurrent_store")
	const workers = 32
	refs := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ref, err := store.Put(context.Background(), req)
			refs <- ref
			errs <- err
		}()
	}
	wg.Wait()
	close(refs)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for ref := range refs {
		if ref != "run-request://run_concurrent_store" {
			t.Fatalf("unexpected ref %q", ref)
		}
	}
}

var _ RunRequestStore = (*InMemoryRunRequestStore)(nil)
