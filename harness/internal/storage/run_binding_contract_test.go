package storage_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	"github.com/AGenUI/agenui-studio/harness/internal/storage/memory"
	storagesqlite "github.com/AGenUI/agenui-studio/harness/internal/storage/sqlite"
)

func TestRunBindingContractMemory(t *testing.T) {
	runBindingContract(t, memory.New().Stores().Runs)
}

func TestRunBindingContractSQLite(t *testing.T) {
	backend, err := storagesqlite.Open(filepath.Join(t.TempDir(), "run-binding.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	if err := backend.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	runBindingContract(t, backend.Stores().Runs)
}

func runBindingContract(t *testing.T, runs storage.RunStore) {
	t.Helper()
	owner := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "tenant-a"})
	other := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "tenant-b"})
	const runID = "run-binding-contract"
	if err := runs.Create(owner, &storage.Run{RunID: runID, SessionID: "session-1"}); err != nil {
		t.Fatal(err)
	}

	assertFrozen := func(name string, bind func(context.Context, string) error) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			if err := bind(other, "value-1"); !storage.IsErrorCode(err, storage.ErrTenantMismatch) {
				t.Fatalf("cross-tenant bind error = %v, want tenant mismatch", err)
			}
			if err := bind(owner, "value-1"); err != nil {
				t.Fatalf("first bind: %v", err)
			}
			if err := bind(owner, "value-1"); err != nil {
				t.Fatalf("idempotent replay: %v", err)
			}
			if err := bind(owner, "value-2"); !storage.IsErrorCode(err, storage.ErrCASMismatch) {
				t.Fatalf("replacement error = %v, want CAS mismatch", err)
			}
		})
	}

	assertFrozen("context_snapshot", func(ctx context.Context, value string) error {
		return runs.BindContextSnapshot(ctx, runID, value)
	})
	assertFrozen("runtime_binding", func(ctx context.Context, value string) error {
		return runs.BindRuntimeBinding(ctx, runID, json.RawMessage(value))
	})

	if err := runs.BindAgentBinding(other, runID, "binding-1"); !storage.IsErrorCode(err, storage.ErrTenantMismatch) {
		t.Fatalf("cross-tenant agent binding error = %v, want tenant mismatch", err)
	}
	if err := runs.BindAgentBinding(owner, runID, "binding-1"); err != nil {
		t.Fatalf("first agent binding: %v", err)
	}
	if err := runs.BindAgentConfig(other, runID, "binding-1", "config-1"); !storage.IsErrorCode(err, storage.ErrTenantMismatch) {
		t.Fatalf("cross-tenant agent config error = %v, want tenant mismatch", err)
	}
	if err := runs.BindAgentConfig(owner, runID, "binding-1", "config-1"); err != nil {
		t.Fatalf("first agent config bind: %v", err)
	}
	if err := runs.BindAgentConfig(owner, runID, "binding-1", "config-1"); err != nil {
		t.Fatalf("agent config replay: %v", err)
	}
	if err := runs.BindAgentConfig(owner, runID, "binding-2", "config-1"); !storage.IsErrorCode(err, storage.ErrCASMismatch) {
		t.Fatalf("agent config replacement error = %v, want CAS mismatch", err)
	}
}
