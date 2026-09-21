package storage_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	"github.com/AGenUI/agenui-studio/harness/internal/storage/memory"
)

type failingOpenTurnStore struct {
	err error
}

func (f failingOpenTurnStore) Commit(context.Context, storage.OpenTurnCommand) (storage.OpenTurnCommit, error) {
	return storage.OpenTurnCommit{}, f.err
}

// P0-7: an HTTP retry carrying the same Idempotency-Key must not create a second
// Turn/Run/Message — it resolves the same run.
func TestOpenTurnIdempotentRetry(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	svc := storage.NewRunService(stores, nil)
	req := storage.OpenTurnRequest{SessionID: "s1", AgentID: "a", UserContentPreview: "hi", IdempotencyKey: "key-1"}

	r1, err := svc.OpenTurn(ctx, req)
	if err != nil {
		t.Fatalf("first OpenTurn: %v", err)
	}
	r2, err := svc.OpenTurn(ctx, req)
	if err != nil {
		t.Fatalf("retry OpenTurn: %v", err)
	}
	if r1.Run.RunID != r2.Run.RunID {
		t.Fatalf("retry created a different run: %q vs %q", r1.Run.RunID, r2.Run.RunID)
	}
	runs, err := stores.Runs.ListBySession(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("idempotent retry must not duplicate runs, got %d", len(runs))
	}
	page, err := stores.Messages.List(ctx, storage.MessageListQuery{SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("idempotent retry must not duplicate messages, got %d", len(page.Items))
	}
}

// P0-7: concurrent requests with the same key converge to one run.
func TestOpenTurnConcurrentSameKey(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	// Pre-create the session so GetOrCreate always hits (isolate the run-creation race).
	if err := stores.Sessions.Create(ctx, &storage.Session{ID: "s1", TenantID: "t1", Status: storage.SessionStatusActive}); err != nil {
		t.Fatal(err)
	}
	svc := storage.NewRunService(stores, nil)

	const n = 12
	var wg sync.WaitGroup
	ids := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := svc.OpenTurn(ctx, storage.OpenTurnRequest{SessionID: "s1", AgentID: "a", UserContentPreview: "hi", IdempotencyKey: "same"})
			errs[i] = err
			if res != nil {
				ids[i] = res.Run.RunID
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	for i := 1; i < n; i++ {
		if ids[i] != ids[0] {
			t.Fatalf("concurrent same-key runs diverged: %q vs %q", ids[i], ids[0])
		}
	}
	runs, err := stores.Runs.ListBySession(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("concurrent same-key must create exactly one run, got %d", len(runs))
	}
}

// An atomic command failure leaves no partial Session/Run/Message facts.
func TestOpenTurnAtomicFailureLeavesNoPartialFacts(t *testing.T) {
	ctx := ctxT("t1")
	stores := memory.New().Stores()
	stores.Turns = failingOpenTurnStore{err: errors.New("disk full")}
	svc := storage.NewRunService(stores, nil)

	_, err := svc.OpenTurn(ctx, storage.OpenTurnRequest{SessionID: "s1", AgentID: "a", UserContentPreview: "hi"})
	if err == nil {
		t.Fatal("expected OpenTurn to fail")
	}
	runs, err := stores.Runs.ListBySession(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("atomic failure left %d partial runs", len(runs))
	}
}

func TestOpenTurnRejectsIdempotencyKeyPayloadMismatch(t *testing.T) {
	ctx := ctxT("t1")
	svc := storage.NewRunService(memory.New().Stores(), nil)
	first, err := svc.OpenTurn(ctx, storage.OpenTurnRequest{SessionID: "s1", UserID: "u1", AgentID: "a", UserContentPreview: "one", IdempotencyKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.OpenTurn(ctx, storage.OpenTurnRequest{SessionID: "s1", UserID: "u1", AgentID: "a", UserContentPreview: "two", IdempotencyKey: "k"})
	if !storage.IsErrorCode(err, storage.ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	replay, err := svc.OpenTurn(ctx, storage.OpenTurnRequest{SessionID: "s1", UserID: "u1", AgentID: "a", UserContentPreview: "one", IdempotencyKey: "k"})
	if err != nil || replay.Run.RunID != first.Run.RunID {
		t.Fatalf("original request must remain replayable: %#v %v", replay, err)
	}
}
