// Package memory is the in-memory storage backend for dev, tests, MockRuntime
// and conformance fixtures. It is non-durable (a backend grade, not a capability
// gap): it provides full functional capabilities under a process-local mutex,
// but must never be used for production persistence
// (docs/simulation/harness-storage-portability.md §8).
package memory

import (
	"context"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// Backend bundles the in-memory store implementations and declares capabilities.
type Backend struct {
	turns    *openTurnStore
	sessions *sessionStore
	messages *messageStore
	usage    *usageStore
	runs     *runStore
	steps    *stepStore
	events   *eventStore
	ckpts    *checkpointStore
	controls *controlStore
	resumes  *resumeStore
	idem     *idempotencyStore
}

// New constructs a fresh in-memory backend.
func New() *Backend {
	b := &Backend{
		sessions: newSessionStore(),
		messages: newMessageStore(),
		usage:    newUsageStore(),
		runs:     newRunStore(),
		steps:    newStepStore(),
		events:   newEventStore(),
		ckpts:    newCheckpointStore(),
		controls: newControlStore(),
		idem:     newIdempotencyStore(),
	}
	b.turns = newOpenTurnStore(b.sessions, b.runs, b.messages, b.events)
	b.resumes = newResumeStore(b.runs, b.ckpts, b.controls, b.events)
	return b
}

// Stores returns the aggregate of store ports backed by this backend.
func (b *Backend) Stores() storage.Stores {
	return storage.Stores{
		Turns:       b.turns,
		Sessions:    b.sessions,
		Messages:    b.messages,
		Usage:       b.usage,
		Runs:        b.runs,
		Steps:       b.steps,
		Events:      b.events,
		Checkpoints: b.ckpts,
		Controls:    b.controls,
		Resumes:     b.resumes,
		Idem:        b.idem,
	}
}

func (b *Backend) Name() string { return "memory" }

// Capabilities reports the functional capabilities. Memory supports every
// functional capability (atomicity comes from a process-local mutex); its only
// limitation is durability, which is a backend grade rather than a capability.
func (b *Backend) Capabilities() map[storage.Capability]bool {
	return map[storage.Capability]bool{
		storage.CapCAS:             true,
		storage.CapTransaction:     true,
		storage.CapOrderedAppend:   true,
		storage.CapIdempotency:     true,
		storage.CapTTL:             true,
		storage.CapPrefixScan:      true,
		storage.CapPagination:      true,
		storage.CapSecondaryIndex:  true,
		storage.CapTenantIsolation: true,
	}
}

func (b *Backend) Health(_ context.Context) error { return nil }

var _ storage.Backend = (*Backend)(nil)
