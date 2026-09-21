// Package protocol is the Harness top layer (Phase 3): it projects the Phase 1
// storage ledger (canonical observability.AgentEvent, ControlRequest, Artifact,
// history) into client-consumable protocols (P0 = SSE + HTTP JSON).
//
// Dependency direction (single, acyclic):
//
//	protocol -> {storage, control, artifact, observability, dispatcher}
//
// protocol is the top layer: nothing under internal/ imports it. Runtime only
// produces AgentEvent; it never emits a client protocol directly (canonical §8,
// extension §4.4).
//
// Invariants preserved here (see docs/online-protocol-rendering-landing-design.md):
//   - Single source of truth: everything a client sees derives from
//     storage.EventStore (+ artifact ref). The HotStreamBuffer is an
//     experience-only optimisation that can be dropped and recovered from the
//     EventStore.
//   - Persist-before-push: replay reads storage.EventStore.Query; the live
//     EventBroker only publishes post-persist events.
//   - view_type names (tool_start/tool_delta/tool_end/message_delta) live only
//     in viewmodel.go and are NEVER written back to the EventStore as an
//     EventType (canonical §6.9, conformance P-002).
//   - Visibility is enforced, never redefined: ordinary clients receive only
//     user_visible; debug/internal/restricted require authorization.
package protocol
