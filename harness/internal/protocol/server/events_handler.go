package server

import (
	"context"
	"net/http"
	"strconv"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// handleEvents serves the run event stream and after_sequence replay
// (GET /api/v1/sessions/{sid}/runs/{rid}/events, P3-D1/D2/D4). One route, three
// transports, negotiated by request headers:
//
//   - Upgrade: websocket    -> WebSocket stream (harness.ws.v1).
//   - Accept: text/event-stream -> SSE stream (harness.sse.v1).
//   - otherwise             -> HTTP one-shot JSON pull (harness.json.v1, no long connection).
//
// SSE and WebSocket share the replay-then-subscribe driver (streamRun); they
// differ only in transport shell. Reconnect never triggers a new Runtime.Run
// (execution §5, conformance P-003).
func (d *Deps) handleEvents(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	rid := r.PathValue("rid")
	if _, ok := d.authorizeRunInSession(w, r, sid, rid); !ok {
		return
	}
	target := targetFor(r)

	// Resolve the replay cursor. Last-Event-ID carries an event_id (P3-D1); an
	// HTTP pull or a browser WebSocket (which cannot set request headers) may pass
	// ?after_sequence=.
	after, ok := d.resolveCursor(w, r, rid)
	if !ok {
		return
	}

	switch {
	case wantsWebSocket(r):
		d.streamWS(w, r, rid, after, target)
	case wantsSSE(r):
		d.streamEvents(w, r, rid, after, target)
	default:
		d.pullEvents(w, r, rid, after, target)
	}
}

func wantsSSE(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return accept != "" && containsFold(accept, "text/event-stream")
}

// wantsWebSocket reports whether the request is a WebSocket upgrade handshake.
func wantsWebSocket(r *http.Request) bool {
	return containsFold(r.Header.Get("Connection"), "upgrade") &&
		containsFold(r.Header.Get("Upgrade"), "websocket")
}

// resolveCursor returns the exclusive after-sequence cursor and false when it
// already wrote an error response.
func (d *Deps) resolveCursor(w http.ResponseWriter, r *http.Request, rid string) (int64, bool) {
	if eid := r.Header.Get("Last-Event-ID"); eid != "" {
		seq, err := d.Stores.Events.SequenceOf(r.Context(), rid, eid)
		if err != nil {
			if storage.IsErrorCode(err, storage.ErrNotFound) {
				// Aged out / unknown: client should pull the run snapshot.
				writeError(w, http.StatusGone, "EVENT_REPLAY_EXPIRED", "last-event-id not found; pull run snapshot")
				return 0, false
			}
			writeError(w, statusForStorageErr(err), "REPLAY_CURSOR_FAILED", err.Error())
			return 0, false
		}
		return seq, true
	}
	if q := r.URL.Query().Get("after_sequence"); q != "" {
		seq, err := strconv.ParseInt(q, 10, 64)
		if err != nil || seq < 0 {
			writeError(w, http.StatusBadRequest, "INVALID_AFTER_SEQUENCE", "after_sequence must be a non-negative integer")
			return 0, false
		}
		return seq, true
	}
	return 0, true
}

// pullEvents is the non-streaming after_sequence pull (§5.2 pure HTTP补拉). It
// projects each event with the transport-neutral ProjectEvent and wraps the
// batch in the HTTP-JSON shell (harness.json.v1) — no SSE schema tag leaks in.
func (d *Deps) pullEvents(w http.ResponseWriter, r *http.Request, rid string, after int64, target protocol.ClientTarget) {
	limit := parseLimit(r, 200)
	events, err := d.Stores.Events.Query(r.Context(), storage.EventQuery{
		RunID:         rid,
		AfterSequence: after,
		Limit:         limit,
		Visibilities:  protocol.VisibilitiesForTarget(target),
	})
	if err != nil {
		writeError(w, statusForStorageErr(err), "EVENT_QUERY_FAILED", err.Error())
		return
	}
	data := make([]protocol.EventData, 0, len(events))
	for _, ev := range events {
		data = append(data, protocol.ProjectEvent(ev, target, d.sse().Mapper))
	}
	writeJSON(w, http.StatusOK, protocol.BuildJSONEnvelope(data))
}

// streamEvents is the SSE transport: set the SSE headers, then hand off to the
// shared replay-then-subscribe driver with an SSE sink (维度① = transport shell,
// 维度② = ProjectEvent inside the sink).
func (d *Deps) streamEvents(w http.ResponseWriter, r *http.Request, rid string, after int64, target protocol.ClientTarget) {
	if _, ok := w.(http.Flusher); !ok {
		writeError(w, http.StatusInternalServerError, "STREAMING_UNSUPPORTED", "response writer is not a flusher")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()

	ctx := r.Context()
	d.streamRun(ctx, rid, after, target, sseSink{w: w, ctx: ctx, deps: d, target: target})
}

// sseSink writes projected events as SSE frames (harness.sse.v1) over an
// http.ResponseWriter.
type sseSink struct {
	w      http.ResponseWriter
	ctx    context.Context
	deps   *Deps
	target protocol.ClientTarget
}

func (s sseSink) send(ev observability.AgentEvent) bool {
	frame, err := s.deps.sse().Convert(s.ctx, ev, s.target)
	if err != nil || frame.SSE == nil {
		return true // skip unconvertible event, keep the stream alive
	}
	return protocol.WriteSSE(s.w, *frame.SSE) == nil
}

func (s sseSink) heartbeat() bool {
	return protocol.WriteSSEComment(s.w, "hb") == nil
}

func parseLimit(r *http.Request, def int) int {
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func containsFold(s, sub string) bool {
	return len(s) >= len(sub) && indexFold(s, sub) >= 0
}

func indexFold(s, sub string) int {
	ls, lsub := len(s), len(sub)
	for i := 0; i+lsub <= ls; i++ {
		match := true
		for j := 0; j < lsub; j++ {
			if lower(s[i+j]) != lower(sub[j]) {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func lower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}
