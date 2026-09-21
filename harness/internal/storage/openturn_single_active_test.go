package storage_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	"github.com/AGenUI/agenui-studio/harness/internal/storage/memory"
	storagesqlite "github.com/AGenUI/agenui-studio/harness/internal/storage/sqlite"
)

type openTurnBackend struct {
	name   string
	stores storage.Stores
	close  func()
}

func openTurnBackends(t *testing.T) []openTurnBackend {
	t.Helper()
	memoryStores := memory.New().Stores()
	sqliteBackend, err := storagesqlite.Open(filepath.Join(t.TempDir(), "open-turn.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sqliteBackend.Migrate(context.Background()); err != nil {
		_ = sqliteBackend.Close()
		t.Fatal(err)
	}
	return []openTurnBackend{
		{name: "memory", stores: memoryStores, close: func() {}},
		{name: "sqlite", stores: sqliteBackend.Stores(), close: func() { _ = sqliteBackend.Close() }},
	}
}

func TestOpenTurnAllowsOnlyOneActiveTopLevelRunPerSession(t *testing.T) {
	for _, backend := range openTurnBackends(t) {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			defer backend.close()
			ctx := ctxT("tenant-1")
			svc := storage.NewRunService(backend.stores, nil)

			const requests = 16
			start := make(chan struct{})
			var wg sync.WaitGroup
			results := make(chan *storage.OpenTurnResult, requests)
			errs := make(chan error, requests)
			for i := 0; i < requests; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					result, err := svc.OpenTurn(ctx, storage.OpenTurnRequest{
						SessionID: "session-1", UserID: "user-1", AgentID: "agent-1",
						UserContentPreview: "message", IdempotencyKey: "request-" + string(rune('a'+i)),
					})
					if err != nil {
						errs <- err
						return
					}
					results <- result
				}(i)
			}
			close(start)
			wg.Wait()
			close(results)
			close(errs)

			if got := len(results); got != 1 {
				t.Fatalf("successful OpenTurn count = %d, want 1", got)
			}
			if got := len(errs); got != requests-1 {
				t.Fatalf("conflicting OpenTurn count = %d, want %d", got, requests-1)
			}
			for err := range errs {
				if !storage.IsErrorCode(err, storage.ErrConflict) {
					t.Fatalf("concurrent OpenTurn error = %v, want conflict", err)
				}
			}
			runs, err := backend.stores.Runs.ListBySession(ctx, "session-1")
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) != 1 {
				t.Fatalf("persisted runs = %d, want 1", len(runs))
			}
		})
	}
}

func TestOpenTurnAllowsNextTurnAfterPreviousRunTerminates(t *testing.T) {
	for _, backend := range openTurnBackends(t) {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			defer backend.close()
			ctx := ctxT("tenant-1")
			svc := storage.NewRunService(backend.stores, nil)
			first, err := svc.OpenTurn(ctx, storage.OpenTurnRequest{
				SessionID: "session-1", UserID: "user-1", AgentID: "agent-1",
				UserContentPreview: "first", IdempotencyKey: "first",
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := backend.stores.Runs.CompareAndSetStatus(ctx, first.Run.RunID, storage.RunStatusCreated, storage.RunStatusFailed, storage.RunMutation{}); err != nil {
				t.Fatal(err)
			}
			second, err := svc.OpenTurn(ctx, storage.OpenTurnRequest{
				SessionID: "session-1", UserID: "user-1", AgentID: "agent-1",
				UserContentPreview: "second", IdempotencyKey: "second",
			})
			if err != nil {
				t.Fatalf("OpenTurn after terminal run: %v", err)
			}
			if second.Run.RunID == first.Run.RunID {
				t.Fatal("next turn reused the terminal run")
			}
		})
	}
}

func TestOpenTurnDoesNotSerializeDifferentSessions(t *testing.T) {
	for _, backend := range openTurnBackends(t) {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			defer backend.close()
			ctx := ctxT("tenant-1")
			svc := storage.NewRunService(backend.stores, nil)
			for _, sessionID := range []string{"session-1", "session-2"} {
				if _, err := svc.OpenTurn(ctx, storage.OpenTurnRequest{
					SessionID: sessionID, UserID: "user-1", AgentID: "agent-1",
					UserContentPreview: sessionID, IdempotencyKey: sessionID,
				}); err != nil {
					t.Fatalf("OpenTurn(%s): %v", sessionID, err)
				}
			}
		})
	}
}
