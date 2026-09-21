package runtimestore

import (
	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// StateBackend selects a RuntimeStateManager implementation (D4).
type StateBackend string

const (
	// StateBackendMemory uses agentruntime.InMemoryStateManager (dev/test/local).
	StateBackendMemory StateBackend = "memory"
	// StateBackendStorage uses the storage-backed Bridge (production).
	StateBackendStorage StateBackend = "storage"
)

// Select returns the RuntimeStateManager for the given backend. This is the
// single config-selection point (D4): both implementations satisfy the same
// interface and are exercised by the same R-series conformance. When backend is
// StateBackendStorage, stores must be non-nil.
func Select(backend StateBackend, stores storage.Stores) agentruntime.RuntimeStateManager {
	switch backend {
	case StateBackendStorage:
		return NewBridge(stores)
	default:
		return agentruntime.NewInMemoryStateManager()
	}
}
