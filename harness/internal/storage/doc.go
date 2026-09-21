// Package storage is the durable fact ledger of the online agent harness.
//
// It owns Session / Message / Turn / Run / Step / CheckpointMeta / ControlRequest
// records plus the EventStore (sequence + idempotency + after_sequence replay).
// It is the foundation layer: it depends only downward on observability
// (AgentEvent / TraceContext) and artifact (large payload / checkpoint content),
// and MUST NOT import agentruntime / scheduler / dispatcher / control / protocol,
// otherwise it would form an import cycle.
//
// Ownership boundaries (see the public protocol contract and
// docs/session-run-storage-landing-design.md):
//
//   - AgentEvent schema / EventType / Visibility  -> observability (reused here)
//   - EventStore persistence (sequence/idempotency) -> this package
//   - RunStatus + Run state machine               -> this package (status.go)
//   - StepStatus enum                             -> agentruntime (stored here as canonical string)
//   - ControlRequestStatus enum + transitions     -> internal/control (stored here as canonical string)
//   - ArtifactMeta / object store                 -> artifact (reused, not re-implemented)
//   - Lease / DispatchStatus                      -> scheduler (reused, not re-implemented)
//   - HotStreamBuffer (realtime channel)          -> Protocol Layer (Phase 3, not here)
//
// Business code depends only on the Store ports defined here, never on the
// concrete storage/memory or storage/mysql backends.
package storage
