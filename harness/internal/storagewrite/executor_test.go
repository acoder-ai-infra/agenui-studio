package storagewrite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestExecutorRunsRequiredWritesBeforeOptionalAndProjection(t *testing.T) {
	sink := NewMemorySink()
	executor := NewExecutor(map[StoreName]StorePort{
		StoreRun:        sink,
		StoreMessage:    sink,
		StoreEvent:      sink,
		StoreHotSession: sink,
	})

	plan := Plan{
		TraceID:        "trace_1",
		TenantID:       "tenant_1",
		SessionID:      "session_1",
		RunID:          "run_1",
		IdempotencyKey: "idem_1",
		Reason:         "user_input",
		RequiredWrites: []Write{
			{Store: StoreMessage, Operation: OperationInsert, Ref: "message:user"},
			{Store: StoreRun, Operation: OperationUpdate, Ref: "run:created"},
		},
		OptionalWrites: []Write{
			{Store: StoreHotSession, Operation: OperationUpsert, Ref: "hot:cursor"},
		},
		Projections: []Projection{
			{Store: StoreEvent, Operation: OperationAppend, Ref: "event:user_message_received"},
		},
	}

	result, err := executor.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if len(result.Required) != 2 {
		t.Fatalf("required writes mismatch: %#v", result)
	}
	if len(result.Optional) != 1 {
		t.Fatalf("optional writes mismatch: %#v", result)
	}
	if len(result.Projections) != 1 {
		t.Fatalf("projection writes mismatch: %#v", result)
	}

	records := sink.Records()
	wantOrder := []StoreName{StoreMessage, StoreRun, StoreHotSession, StoreEvent}
	if len(records) != len(wantOrder) {
		t.Fatalf("record count mismatch: got %d want %d", len(records), len(wantOrder))
	}
	for i, want := range wantOrder {
		if records[i].Store != want {
			t.Fatalf("record %d store mismatch: got %s want %s; records=%#v", i, records[i].Store, want, records)
		}
	}
	if records[0].TraceID != "trace_1" || records[0].TenantID != "tenant_1" || records[0].RunID != "run_1" || records[0].IdempotencyKey != "idem_1" {
		t.Fatalf("write identity not propagated: %#v", records[0])
	}
}

func TestExecutorStopsWhenRequiredWriteFails(t *testing.T) {
	runSink := NewMemorySink()
	messageSink := NewMemorySink()
	storeErr := errors.New("message store down")
	messageSink.Fail(OperationInsert, storeErr)
	eventSink := NewMemorySink()
	executor := NewExecutor(map[StoreName]StorePort{
		StoreMessage: messageSink,
		StoreRun:     runSink,
		StoreEvent:   eventSink,
	})

	plan := Plan{
		TraceID:        "trace_1",
		TenantID:       "tenant_1",
		SessionID:      "session_1",
		RunID:          "run_1",
		IdempotencyKey: "idem_1",
		Reason:         "user_input",
		RequiredWrites: []Write{
			{Store: StoreMessage, Operation: OperationInsert, Ref: "message:user"},
			{Store: StoreRun, Operation: OperationUpdate, Ref: "run:created"},
		},
		Projections: []Projection{
			{Store: StoreEvent, Operation: OperationAppend, Ref: "event:user_message_received"},
		},
	}

	result, err := executor.Execute(context.Background(), plan)
	if err == nil {
		t.Fatal("expected required write error")
	}
	if !errors.Is(err, ErrRequiredWriteFailed) {
		t.Fatalf("unexpected error: %v", err)
	}
	if !errors.Is(err, storeErr) {
		t.Fatalf("required write should preserve store cause: %v", err)
	}
	if len(result.Required) != 1 || result.Required[0].Err == nil {
		t.Fatalf("required failure not recorded: %#v", result.Required)
	}
	if got := runSink.Records(); len(got) != 0 {
		t.Fatalf("run write should not happen after required failure: %#v", got)
	}
	if got := eventSink.Records(); len(got) != 0 {
		t.Fatalf("projection should not happen after required failure: %#v", got)
	}
}

func TestExecutorOptionalFailureDoesNotBlockProjection(t *testing.T) {
	requiredSink := NewMemorySink()
	optionalSink := NewMemorySink()
	optionalSink.Fail(OperationUpsert, errors.New("redis unavailable"))
	projectionSink := NewMemorySink()
	executor := NewExecutor(map[StoreName]StorePort{
		StoreRun:        requiredSink,
		StoreHotContext: optionalSink,
		StoreEvent:      projectionSink,
	})

	result, err := executor.Execute(context.Background(), Plan{
		TraceID:        "trace_1",
		TenantID:       "tenant_1",
		SessionID:      "session_1",
		RunID:          "run_1",
		IdempotencyKey: "idem_1",
		Reason:         "context_snapshot",
		RequiredWrites: []Write{{Store: StoreRun, Operation: OperationUpdate, Ref: "run:ctx"}},
		OptionalWrites: []Write{{Store: StoreHotContext, Operation: OperationUpsert, Ref: "hot:summary"}},
		Projections:    []Projection{{Store: StoreEvent, Operation: OperationAppend, Ref: "event:context_snapshot_created"}},
	})
	if err != nil {
		t.Fatalf("optional failure should not block: %v", err)
	}
	if len(result.Optional) != 1 || result.Optional[0].Err == nil {
		t.Fatalf("optional failure not recorded: %#v", result.Optional)
	}
	if got := projectionSink.Records(); len(got) != 1 {
		t.Fatalf("projection should still execute: %#v", got)
	}
}

func TestExecutorIdempotencySkipsDuplicateWrite(t *testing.T) {
	sink := NewMemorySink()
	executor := NewExecutor(map[StoreName]StorePort{StoreEvent: sink})
	plan := Plan{
		TraceID:        "trace_1",
		TenantID:       "tenant_1",
		SessionID:      "session_1",
		RunID:          "run_1",
		IdempotencyKey: "idem_1",
		Reason:         "final_response",
		RequiredWrites: []Write{{Store: StoreEvent, Operation: OperationAppend, Ref: "event:final_response"}},
	}

	first, err := executor.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("first execute failed: %v", err)
	}
	second, err := executor.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("second execute failed: %v", err)
	}
	if first.Required[0].Skipped {
		t.Fatal("first write should not be skipped")
	}
	if !second.Required[0].Skipped {
		t.Fatal("second write should be skipped by idempotency")
	}
	if got := sink.Records(); len(got) != 1 {
		t.Fatalf("duplicate write happened: %#v", got)
	}
}

func TestExecutorUsesWriteLevelIdempotencyKey(t *testing.T) {
	sink := NewMemorySink()
	executor := NewExecutor(map[StoreName]StorePort{StoreEvent: sink})
	plan := Plan{
		TraceID:        "trace_1",
		TenantID:       "tenant_1",
		SessionID:      "session_1",
		RunID:          "run_1",
		IdempotencyKey: "plan_idem",
		Reason:         "run_completed",
		RequiredWrites: []Write{{
			Store: StoreEvent, Operation: OperationAppend, Ref: "event:run_completed", IdempotencyKey: "write_idem_1",
		}},
	}

	first, err := executor.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("first execute failed: %v", err)
	}

	plan.RequiredWrites[0].IdempotencyKey = "write_idem_2"
	second, err := executor.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("second execute failed: %v", err)
	}
	duplicate, err := executor.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("duplicate execute failed: %v", err)
	}

	if first.Required[0].Skipped || second.Required[0].Skipped {
		t.Fatalf("different write-level keys must execute independently: first=%#v second=%#v", first.Required[0], second.Required[0])
	}
	if !duplicate.Required[0].Skipped {
		t.Fatal("same write-level key should be skipped by idempotency")
	}
	records := sink.Records()
	if len(records) != 2 {
		t.Fatalf("expected two physical writes, got %d: %#v", len(records), records)
	}
	if records[0].IdempotencyKey != "write_idem_1" || records[1].IdempotencyKey != "write_idem_2" {
		t.Fatalf("write-level keys not propagated: %#v", records)
	}
}

func TestExecutorIdempotencyKeyCannotCollideAcrossTenantAndRunFields(t *testing.T) {
	sink := NewMemorySink()
	executor := NewExecutor(map[StoreName]StorePort{StoreEvent: sink})
	newPlan := func(tenantID, runID string) Plan {
		return Plan{
			TraceID: "trace_1", TenantID: tenantID, RunID: runID, IdempotencyKey: "idem_1",
			RequiredWrites: []Write{{Store: StoreEvent, Operation: OperationAppend, Ref: "event:completed"}},
		}
	}
	// 旧的 "|" 拼接会让这两组字段都编码成 tenant|run|1|...。
	first, err := executor.Execute(context.Background(), newPlan("tenant|run", "1"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := executor.Execute(context.Background(), newPlan("tenant", "run|1"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Required[0].Skipped || second.Required[0].Skipped {
		t.Fatalf("distinct tenant/run tuples collided: first=%#v second=%#v", first.Required[0], second.Required[0])
	}
	if records := sink.Records(); len(records) != 2 {
		t.Fatalf("expected two physical writes, got %#v", records)
	}
}

func TestExecutorConcurrentIdempotentWrites(t *testing.T) {
	sink := NewMemorySink()
	executor := NewExecutor(map[StoreName]StorePort{StoreEvent: sink})
	plan := Plan{
		TraceID:        "trace_1",
		TenantID:       "tenant_1",
		SessionID:      "session_1",
		RunID:          "run_1",
		IdempotencyKey: "idem_concurrent",
		Reason:         "run_completed",
		RequiredWrites: []Write{{Store: StoreEvent, Operation: OperationAppend, Ref: "event:run_completed"}},
	}

	const workers = 32
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := executor.Execute(context.Background(), plan)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("execute failed: %v", err)
		}
	}

	if got := sink.Records(); len(got) != 1 {
		t.Fatalf("expected one physical write, got %d: %#v", len(got), got)
	}
}

func TestExecutorBoundsProcessLocalIdempotencyCache(t *testing.T) {
	sink := NewMemorySink()
	executor := NewExecutorWithCacheLimit(map[StoreName]StorePort{StoreEvent: sink}, 8)
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("%d", i)
		_, err := executor.Execute(context.Background(), Plan{
			TraceID: "trace_1", TenantID: "tenant_1", RunID: "run_1", IdempotencyKey: "idem_" + id,
			RequiredWrites: []Write{{Store: StoreEvent, Operation: OperationAppend, Ref: "event_" + id}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	executor.mu.Lock()
	cached := len(executor.ledger)
	executor.mu.Unlock()
	if cached > 8 {
		t.Fatalf("idempotency cache grew past limit: %d", cached)
	}
}

func TestExecutorProductionValidationRejectsMemoryPorts(t *testing.T) {
	executor := NewExecutor(map[StoreName]StorePort{StoreEvent: NewMemoryPort(StoreEvent)})
	if err := executor.ValidateProduction(); !errors.Is(err, ErrMemoryPortForbidden) {
		t.Fatalf("expected memory port rejection, got %v", err)
	}
}

func TestExecutorConcurrentDuplicateWaitsForFirstWriteFailure(t *testing.T) {
	port := &blockingPort{
		started: make(chan struct{}),
		release: make(chan struct{}),
		err:     errors.New("event store unavailable"),
	}
	executor := NewExecutor(map[StoreName]StorePort{StoreEvent: port})
	plan := Plan{
		TraceID:        "trace_1",
		TenantID:       "tenant_1",
		SessionID:      "session_1",
		RunID:          "run_1",
		IdempotencyKey: "idem_wait",
		Reason:         "run_completed",
		RequiredWrites: []Write{{Store: StoreEvent, Operation: OperationAppend, Ref: "event:run_completed"}},
	}

	errs := make(chan error, 2)
	go func() {
		_, err := executor.Execute(context.Background(), plan)
		errs <- err
	}()
	<-port.started
	go func() {
		_, err := executor.Execute(context.Background(), plan)
		errs <- err
	}()
	time.Sleep(20 * time.Millisecond)
	close(port.release)

	for i := 0; i < 2; i++ {
		err := <-errs
		if !errors.Is(err, ErrRequiredWriteFailed) {
			t.Fatalf("expected required failure, got %v", err)
		}
	}
	if port.Count() != 1 {
		t.Fatalf("duplicate should wait for first physical write, got %d writes", port.Count())
	}
}

func TestExecutorValidatesPlanIdentity(t *testing.T) {
	executor := NewExecutor(map[StoreName]StorePort{StoreEvent: NewMemorySink()})
	_, err := executor.Execute(context.Background(), Plan{
		TraceID:        "trace_1",
		RunID:          "run_1",
		IdempotencyKey: "idem_1",
		RequiredWrites: []Write{{Store: StoreEvent, Operation: OperationAppend, Ref: "event"}},
	})
	if !errors.Is(err, ErrTenantIDRequired) {
		t.Fatalf("unexpected error: %v", err)
	}
}

type blockingPort struct {
	mu      sync.Mutex
	started chan struct{}
	release chan struct{}
	err     error
	count   int
	once    sync.Once
}

func (p *blockingPort) Write(context.Context, Write) (WriteReceipt, error) {
	p.mu.Lock()
	p.count++
	p.mu.Unlock()
	p.once.Do(func() { close(p.started) })
	<-p.release
	return WriteReceipt{}, p.err
}

func (p *blockingPort) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.count
}
