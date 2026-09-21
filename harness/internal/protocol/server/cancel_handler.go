package server

import (
	"encoding/json"
	"net/http"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type cancelRunRequest struct {
	Reason string `json:"reason,omitempty"`
}

func (d *Deps) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	if d.Canceller == nil {
		writeError(w, http.StatusNotImplemented, "RUN_CANCELLATION_UNAVAILABLE", "run cancellation is not configured")
		return
	}
	sessionID, runID := r.PathValue("sid"), r.PathValue("rid")
	if !d.authorizeSessionForOpen(w, r, sessionID) {
		return
	}
	tc := observability.MustTraceContext(r.Context())
	run, err := d.Stores.Runs.Get(r.Context(), runID)
	if err != nil {
		writeError(w, statusForStorageErr(err), "RUN_NOT_FOUND", "run not found")
		return
	}
	if run.SessionID != sessionID || run.TenantID != tc.TenantID {
		writeError(w, http.StatusForbidden, "RUN_ACCESS_DENIED", "run does not belong to the requested session")
		return
	}
	var body cancelRunRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err.Error() != "EOF" {
			writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
			return
		}
	}
	if err := d.Canceller.Cancel(r.Context(), sessionID, runID, body.Reason); err != nil {
		writeError(w, http.StatusConflict, "RUN_CANCEL_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"run_id": runID, "status": "cancellation_requested"})
}
