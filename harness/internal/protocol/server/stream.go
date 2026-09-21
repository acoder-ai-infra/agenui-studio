package server

import (
	"context"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// streamHeartbeatInterval keeps a long-lived connection alive (SSE comment / WS
// ping) during quiet periods.
const streamHeartbeatInterval = 15 * time.Second

// frameSink is the transport-specific write end of a run stream (维度①). The
// driver (streamRun) owns the transport-neutral replay-then-subscribe
// orchestration; the sink owns per-event projection+serialization for one
// transport (SSE / WebSocket / ...) and its keepalive framing.
type frameSink interface {
	// send projects and writes one event; returns false on a write failure that
	// should end the stream. An unconvertible event should be skipped (return
	// true) to keep the stream alive.
	send(ev observability.AgentEvent) bool
	// heartbeat writes one keepalive frame; returns false on failure.
	heartbeat() bool
}

// streamRun performs replay-then-subscribe (P3-D2), transport-agnostic:
//
//  1. Subscribe FIRST so no gap opens between replay and live.
//  2. Replay from the EventStore (durable, source of truth); record each
//     event_id in seen.
//  3. Forward only live events not already delivered during replay.
//
// Dedup is by event_id (not sequence): the sequence assigned at persistence is
// not stamped onto the event pushed to the broker, so a sequence-based dedup
// would drop every live event. Reconnect never triggers a new Runtime.Run
// (execution §5, conformance P-003).
func (d *Deps) streamRun(ctx context.Context, rid string, after int64, target protocol.ClientTarget, sink frameSink) {
	// 1) Subscribe first to avoid a gap between replay and live.
	// A protocol cursor is scoped to exactly one persisted Run sequence. Child
	// Runs have independent streams and are discovered through parent events that
	// carry child_run_id; they are never multiplexed into this channel.
	var live <-chan observability.AgentEvent
	if d.Broker != nil {
		if ch, cancel, err := d.Broker.Subscribe(ctx, rid); err == nil {
			live = ch
			defer cancel()
		}
	}

	// 2) Replay from the EventStore. Query already filters by visibility, so the
	// replay phase does not re-run the visibility filter (only the live phase does,
	// because broker events are unfiltered). EventStore order is the requested
	// Run's persisted sequence order; CreatedAt never participates in ordering.
	seen := map[string]bool{}
	replayAfter := after
	for {
		replayed, err := d.Stores.Events.Query(ctx, storage.EventQuery{
			RunID:         rid,
			AfterSequence: replayAfter,
			Limit:         500,
			Visibilities:  protocol.VisibilitiesForTarget(target),
		})
		if err != nil {
			return // replay failed: close the connection; the client will reconnect.
		}
		for _, ev := range replayed {
			seen[ev.EventID] = true
			if ev.Sequence > replayAfter {
				replayAfter = ev.Sequence
			}
			if !sink.send(ev) {
				return
			}
			if terminal(ev.EventType) {
				return
			}
		}
		if len(replayed) < 500 {
			break
		}
	}

	// Ordinary clients don't see the debug-visibility failure terminals
	// (run_failed/cancelled/expired) in the visibility-filtered replay above, so
	// a client reconnecting to an already-terminated run would hang on heartbeats.
	// If the run has already reached such a terminal, emit a redacted user-facing
	// terminal (type + error code only, no debug detail) so the stream closes.
	if ev, ok := d.redactedTerminalFromRun(ctx, rid); ok {
		sink.send(ev)
		return
	}

	if live == nil {
		// No live broker configured: replay-only stream ends here.
		return
	}

	// handleLive projects and forwards one live event; it returns false when the
	// requested Run reaches terminal or the transport write fails.
	handleLive := func(ev observability.AgentEvent) bool {
		if seen[ev.EventID] {
			return true // already delivered during replay
		}
		if ev.RunID != rid {
			return true // fail closed if a broker violates its Run subscription
		}
		allowed := d.filter().Allow(ev, target)
		if !allowed && !terminal(ev.EventType) {
			return true
		}
		seen[ev.EventID] = true
		out := ev
		if !allowed {
			// A run terminal the target may not see raw (failure terminals are
			// debug-visibility): send a redacted user-facing terminal so the client
			// can close, without leaking debug detail.
			out = clientFacingTerminal(ev)
		}
		if !sink.send(out) {
			return false
		}
		return !terminal(ev.EventType)
	}

	// 3) Forward live events for this Run not already delivered during replay,
	// interleaved with heartbeats.
	hb := time.NewTicker(streamHeartbeatInterval)
	defer hb.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hb.C:
			if !sink.heartbeat() {
				return
			}
		case ev, ok := <-live:
			if !ok {
				return
			}
			if !handleLive(ev) {
				return
			}
		}
	}
}

// terminal reports whether an event type ends the run (execution §5).
func terminal(t observability.EventType) bool {
	switch t {
	case observability.EventRunCompleted, observability.EventRunFailed,
		observability.EventRunCancelled, observability.EventRunExpired:
		return true
	default:
		return false
	}
}

// clientFacingTerminal builds a redacted, user_visible copy of a run terminal
// event for a target not allowed to see the raw (debug) terminal. It carries the
// terminal type and the machine error code only — never the internal error
// message/type or payload — so ordinary clients can close and show a failure
// without debug detail leaking (canonical §12.3.1 / §12.4).
func clientFacingTerminal(ev observability.AgentEvent) observability.AgentEvent {
	out := observability.AgentEvent{
		EventID:    ev.EventID,
		SessionID:  ev.SessionID,
		RunID:      ev.RunID,
		EventType:  ev.EventType,
		Visibility: observability.VisibilityUserVisible,
		CreatedAt:  ev.CreatedAt,
	}
	if ev.Error != nil {
		out.Error = &observability.EventError{Code: ev.Error.Code}
	}
	return out
}

// redactedTerminalFromRun returns a redacted user-facing terminal for a run that
// has ALREADY reached a failure terminal (whose raw event is debug-visibility and
// thus absent from the target's filtered replay). Returns ok=false when the run
// is still running or its terminal is user-visible (already delivered).
func (d *Deps) redactedTerminalFromRun(ctx context.Context, rid string) (observability.AgentEvent, bool) {
	run, err := d.Stores.Runs.Get(ctx, rid)
	if err != nil || run == nil {
		return observability.AgentEvent{}, false
	}
	var et observability.EventType
	switch run.Status {
	case storage.RunStatusFailed:
		et = observability.EventRunFailed
	case storage.RunStatusCancelled:
		et = observability.EventRunCancelled
	case storage.RunStatusExpired:
		et = observability.EventRunExpired
	default:
		return observability.AgentEvent{}, false // running, or completed (already user-visible)
	}
	ev := observability.AgentEvent{
		SessionID: run.SessionID, RunID: run.RunID,
		EventType: et, Visibility: observability.VisibilityUserVisible, CreatedAt: run.EndedAt,
	}
	if run.ErrorCode != "" {
		ev.Error = &observability.EventError{Code: run.ErrorCode}
	}
	return ev, true
}
