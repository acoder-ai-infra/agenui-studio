package storage

import (
	"context"
	"encoding/json"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// Stores aggregates the ledger store ports. Business code depends on this
// aggregate (or its individual ports), never on a concrete backend.
type Stores struct {
	Turns       OpenTurnStore
	Sessions    SessionStore
	Messages    MessageStore
	Usage       ModelUsageStore
	Runs        RunStore
	Steps       StepStore
	Events      EventStore
	Checkpoints CheckpointStore
	Controls    ControlRequestStore
	Resumes     ResumeStore
	Idem        IdempotencyStore
}

// OpenTurnStore atomically creates or resolves the durable facts that make a
// user turn dispatchable. Implementations use one local database transaction
// (or one process-local critical section for Memory); callers must not emulate
// this contract with compensating writes.
type OpenTurnStore interface {
	Commit(ctx context.Context, cmd OpenTurnCommand) (OpenTurnCommit, error)
}

// SessionStore persists Session records with CAS updates and cursor pagination.
type SessionStore interface {
	Create(ctx context.Context, s *Session) error
	Get(ctx context.Context, id string) (*Session, error)
	Update(ctx context.Context, s *Session) error // CAS by Version
	Archive(ctx context.Context, id string) error
	SoftDelete(ctx context.Context, id string) error
	List(ctx context.Context, q SessionListQuery) (SessionPage, error)
}

// MessageStore appends user-facing messages and pages them by message_id.
type MessageStore interface {
	Append(ctx context.Context, m *Message) error
	Get(ctx context.Context, id string) (*Message, error)
	List(ctx context.Context, q MessageListQuery) (MessagePage, error)
	// ListRecent returns the newest bounded window in chronological order. Context
	// assembly must not scan an unbounded conversation and trim it in memory.
	ListRecent(ctx context.Context, sessionID string, limit int) ([]*Message, error)
	// GetByIDs resolves an immutable context snapshot without depending on the
	// mutable pagination cursor. Results follow ids order and omit no item.
	GetByIDs(ctx context.Context, sessionID string, ids []string) ([]*Message, error)
}

// ModelUsageStore persists normalized model usage/cost facts for accounting.
type ModelUsageStore interface {
	Record(ctx context.Context, r *ModelUsageRecord) error
	List(ctx context.Context, q ModelUsageQuery) ([]*ModelUsageRecord, error)
	Summary(ctx context.Context, q ModelUsageQuery) (ModelUsageSummary, error)
}

// RunStore persists Run records; status changes go through CAS + transition check.
type RunStore interface {
	Create(ctx context.Context, r *Run) error
	Get(ctx context.Context, runID string) (*Run, error)
	CompareAndSetStatus(ctx context.Context, runID string, from, to RunStatus, mut RunMutation) (*Run, error)
	BindContextSnapshot(ctx context.Context, runID, ref string) error
	// BindAgentConfig atomically freezes the effective binding and config snapshot
	// on the Run. They are one execution fact and must never drift independently.
	BindAgentConfig(ctx context.Context, runID, bindingID, configSnapshotRef string) error
	BindAgentBinding(ctx context.Context, runID, bindingID string) error
	BindRuntimeBinding(ctx context.Context, runID string, binding json.RawMessage) error
	// CompareAndSetRuntimeBinding freezes capability facts without allowing two
	// concurrent first-bind requests to overwrite each other.
	CompareAndSetRuntimeBinding(ctx context.Context, runID string, expected, replacement json.RawMessage) (bool, error)
	ListBySession(ctx context.Context, sessionID string) ([]*Run, error)
}

// StepStore persists run-internal Step records.
type StepStore interface {
	Upsert(ctx context.Context, s *Step) error
	ListByRun(ctx context.Context, runID string) ([]*Step, error)
}

// EventStore is the sole owner of sequence allocation and idempotency for
// AgentEvent persistence (canonical §12.2, conformance E-001..E-004).
type EventStore interface {
	// Append allocates a run-monotonic sequence and persists the event.
	// Duplicate event_id or idempotency_key must not create a duplicate event;
	// the already-persisted event is returned with Idempotent=true.
	Append(ctx context.Context, e observability.AgentEvent) (AppendResult, error)
	// Query returns events after (exclusive) q.AfterSequence in ascending
	// sequence order, filtered by visibility/type.
	Query(ctx context.Context, q EventQuery) ([]observability.AgentEvent, error)
	// LastSequence returns the highest sequence for a run (0 if none).
	LastSequence(ctx context.Context, runID string) (int64, error)
	// SequenceOf resolves an event_id to its run-scoped sequence. It backs
	// SSE reconnect when Last-Event-ID carries the event_id (protocol P3-D1):
	// the caller resolves the sequence, then replays via Query(AfterSequence).
	// Returns ErrNotFound when the event is unknown or aged out of retention.
	SequenceOf(ctx context.Context, runID, eventID string) (int64, error)
}

// CheckpointStore persists checkpoint metadata (content lives in artifact).
type CheckpointStore interface {
	Create(ctx context.Context, c *CheckpointMeta) error
	Get(ctx context.Context, checkpointID string) (*CheckpointMeta, error)
	LatestByRun(ctx context.Context, runID string) (*CheckpointMeta, error)
	// CompareAndSwapState atomically replaces the immutable payload reference.
	// It returns false when expectedStateRef is stale.
	CompareAndSwapState(ctx context.Context, checkpointID, expectedStateRef string, replacement *CheckpointMeta) (bool, error)
	Delete(ctx context.Context, checkpointID string) error
}

// ControlRequestStore persists ControlRequest records. Status is a canonical
// string; the enum/transitions are owned by internal/control (D2).
type ControlRequestStore interface {
	Create(ctx context.Context, c *ControlRequest) error
	Get(ctx context.Context, requestID string) (*ControlRequest, error)
	ListByRun(ctx context.Context, runID string) ([]*ControlRequest, error)
	// CompareAndAnswer atomically moves a request from expectFrom (e.g. pending)
	// to a terminal status, succeeding at most once (CAS).
	CompareAndAnswer(ctx context.Context, requestID string, expectFrom, to string, responseRef string) (*ControlRequest, error)
	// ExpirePending moves all pending requests whose ExpiresAt <= now to expired,
	// returning the affected requests so the caller can emit events and drive
	// the owning runs to a terminal state.
	ExpirePending(ctx context.Context, now time.Time) ([]*ControlRequest, error)
}

// ResumeStore owns the transaction boundary for a resumable Run. Implementations
// must update the Run claim and append its lifecycle event in one transaction;
// callers must not emulate this contract with separate RunStore/EventStore calls.
type ResumeStore interface {
	Wait(ctx context.Context, cmd ResumeWaitCommand) error
	Claim(ctx context.Context, cmd ResumeClaimCommand) error
	Fail(ctx context.Context, cmd ResumeFailureCommand) error
	Activate(ctx context.Context, cmd ResumeActivationCommand) error
}

// IdempotencyStore provides one-time consumption of generic namespaced keys.
// Resume claims belong to ResumeStore because retryable recovery requires a
// transactional attempt owner instead of permanently consuming the token.
type IdempotencyStore interface {
	Consume(ctx context.Context, key IdemKey) (ok bool, err error)
}
