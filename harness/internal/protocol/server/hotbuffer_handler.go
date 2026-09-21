package server

import (
	"net/http"
	"strconv"

	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
)

// handleStreamBuffer serves the Hot Buffer after_delta_seq pull (§5.3). On
// expiry it returns HOT_BUFFER_EXPIRED so the client degrades to the coarse
// after_sequence EventStore replay (canonical §8.2).
func (d *Deps) handleStreamBuffer(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	rid := r.PathValue("rid")
	if _, ok := d.authorizeRunInSession(w, r, sid, rid); !ok {
		return
	}
	if d.HotBuffer == nil {
		writeError(w, http.StatusServiceUnavailable, "HOT_BUFFER_UNAVAILABLE", "hot buffer not configured")
		return
	}
	var after int64
	if q := r.URL.Query().Get("after_delta_seq"); q != "" {
		n, err := strconv.ParseInt(q, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "INVALID_AFTER_DELTA_SEQ", "after_delta_seq must be a non-negative integer")
			return
		}
		after = n
	}
	deltas, expired, err := d.HotBuffer.Read(r.Context(), rid, after, parseLimit(r, 200))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "HOT_BUFFER_READ_FAILED", err.Error())
		return
	}
	if expired {
		writeError(w, http.StatusGone, "HOT_BUFFER_EXPIRED", "hot buffer expired; degrade to after_sequence replay")
		return
	}
	if deltas == nil {
		deltas = []protocol.HotDelta{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"deltas": deltas})
}
