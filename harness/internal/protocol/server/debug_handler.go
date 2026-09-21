package server

import (
	"net/http"
	"strconv"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// requireDebug enforces the debug trust boundary shared by all /debug endpoints.
// Debug visibility comes from the credential (JWT debug claim or, in insecure
// mode, x-debug:1) — never from an unauthenticated header. Writes 403 and
// returns false when not granted.
func (d *Deps) requireDebug(w http.ResponseWriter, r *http.Request) (observability.TraceContext, bool) {
	tc := observability.MustTraceContext(r.Context())
	if !tc.DebugEnabled {
		writeError(w, http.StatusForbidden, "DEBUG_FORBIDDEN", "debug visibility required")
		return tc, false
	}
	return tc, true
}

// authorizeDebugRun loads a run and confirms it belongs to the caller's tenant.
// Debug tooling is tenant-scoped (not user-owned): any run within the tenant may
// be inspected. Writes the error and returns false on failure.
func (d *Deps) authorizeDebugRun(w http.ResponseWriter, r *http.Request, tc observability.TraceContext, runID string) (*storage.Run, bool) {
	if runID == "" {
		writeError(w, http.StatusBadRequest, "MISSING_RUN_ID", "run_id required")
		return nil, false
	}
	run, err := d.Stores.Runs.Get(r.Context(), runID)
	if err != nil {
		writeError(w, statusForStorageErr(err), "RUN_GET_FAILED", err.Error())
		return nil, false
	}
	if tc.TenantID != "" && run.TenantID != "" && run.TenantID != tc.TenantID {
		writeError(w, http.StatusForbidden, "RUN_TENANT_MISMATCH", "run belongs to another tenant")
		return nil, false
	}
	return run, true
}

// handleDebugInfo serves the redacted config snapshot built at composition time.
func (d *Deps) handleDebugInfo(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.requireDebug(w, r); !ok {
		return
	}
	if len(d.System) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(d.System)
}

// handleDebugAgents lists the registered agents (agent_id, version, status,
// execution modes, ...) resolved from the Agent Registry at startup. Read-only.
func (d *Deps) handleDebugAgents(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.requireDebug(w, r); !ok {
		return
	}
	if len(d.AgentCatalog) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"agents": []any{}})
		return
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(d.AgentCatalog)
}

// handleDebugAgent serves one agent's effective config: capability card plus the
// resolved definition (prompt ref, tool/sub-agent refs) and resolved dependencies.
func (d *Deps) handleDebugAgent(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.requireDebug(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "MISSING_AGENT_ID", "agent_id required")
		return
	}
	raw, ok := d.AgentConfigs[id]
	if !ok {
		writeError(w, http.StatusNotFound, "AGENT_NOT_FOUND", "no registered agent "+id)
		return
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// handleDebugEvents returns the raw stored AgentEvent rows for a run (full
// fields, all visibilities) — a faithful view the protocol projection hides.
func (d *Deps) handleDebugEvents(w http.ResponseWriter, r *http.Request) {
	tc, ok := d.requireDebug(w, r)
	if !ok {
		return
	}
	runID := r.URL.Query().Get("run_id")
	if _, ok := d.authorizeDebugRun(w, r, tc, runID); !ok {
		return
	}
	q := storage.EventQuery{RunID: runID, Limit: parseLimit(r, 500)}
	if after := r.URL.Query().Get("after_sequence"); after != "" {
		if n, err := strconv.ParseInt(after, 10, 64); err == nil {
			q.AfterSequence = n
		}
	}
	if et := r.URL.Query().Get("event_type"); et != "" {
		q.EventTypes = []observability.EventType{observability.EventType(et)}
	}
	// Visibilities left empty = admin view (all visibilities).
	events, err := d.Stores.Events.Query(r.Context(), q)
	if err != nil {
		writeError(w, statusForStorageErr(err), "EVENT_QUERY_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// handleDebugSteps returns the run-internal Step timeline (which stage ran/failed).
func (d *Deps) handleDebugSteps(w http.ResponseWriter, r *http.Request) {
	tc, ok := d.requireDebug(w, r)
	if !ok {
		return
	}
	runID := r.URL.Query().Get("run_id")
	if _, ok := d.authorizeDebugRun(w, r, tc, runID); !ok {
		return
	}
	steps, err := d.Stores.Steps.ListByRun(r.Context(), runID)
	if err != nil {
		writeError(w, statusForStorageErr(err), "STEP_LIST_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"steps": steps})
}

func (d *Deps) handleDebugControlRequests(w http.ResponseWriter, r *http.Request) {
	tc, ok := d.requireDebug(w, r)
	if !ok {
		return
	}
	runID := r.URL.Query().Get("run_id")
	if _, ok := d.authorizeDebugRun(w, r, tc, runID); !ok {
		return
	}
	controls, err := d.Stores.Controls.ListByRun(r.Context(), runID)
	if err != nil {
		writeError(w, statusForStorageErr(err), "CONTROL_LIST_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"control_requests": controls})
}

// handleDebugCheckpoints returns the latest resume checkpoint for a run.
func (d *Deps) handleDebugCheckpoints(w http.ResponseWriter, r *http.Request) {
	tc, ok := d.requireDebug(w, r)
	if !ok {
		return
	}
	runID := r.URL.Query().Get("run_id")
	if _, ok := d.authorizeDebugRun(w, r, tc, runID); !ok {
		return
	}
	cp, err := d.Stores.Checkpoints.LatestByRun(r.Context(), runID)
	if err != nil {
		if storage.IsErrorCode(err, storage.ErrNotFound) {
			writeJSON(w, http.StatusOK, map[string]any{"checkpoint": nil})
			return
		}
		writeError(w, statusForStorageErr(err), "CHECKPOINT_GET_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checkpoint": cp})
}

// handleDebugLogs returns retained structured logs correlated by trace_id/run_id.
// Requires a RingLogger-backed LogQuery; degrades to 501 otherwise.
func (d *Deps) handleDebugLogs(w http.ResponseWriter, r *http.Request) {
	tc, ok := d.requireDebug(w, r)
	if !ok {
		return
	}
	if d.LogQuery == nil {
		writeError(w, http.StatusNotImplemented, "LOG_QUERY_UNAVAILABLE", "in-memory log retention not enabled")
		return
	}
	q := observability.LogQuery{
		TraceID:  r.URL.Query().Get("trace_id"),
		RunID:    r.URL.Query().Get("run_id"),
		Level:    r.URL.Query().Get("level"),
		TenantID: tc.TenantID, // tenant isolation: never trust a client-supplied tenant
		Limit:    parseLimit(r, 200),
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": d.LogQuery.QueryLogs(q)})
}
