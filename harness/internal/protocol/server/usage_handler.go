package server

import (
	"net/http"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// handleUsage exposes the model-usage ledger (token/cost accounting) for the
// caller's tenant. TenantID is forced from the trusted identity; a debug caller
// may override it via ?tenant= for cross-tenant inspection.
func (d *Deps) handleUsage(w http.ResponseWriter, r *http.Request) {
	tc := observability.MustTraceContext(r.Context())
	q := storage.ModelUsageQuery{
		TenantID:  tc.TenantID,
		SessionID: r.URL.Query().Get("session_id"),
		RunID:     r.URL.Query().Get("run_id"),
		Provider:  r.URL.Query().Get("provider"),
		Model:     r.URL.Query().Get("model"),
		Limit:     parseLimit(r, 50),
	}
	if tc.DebugEnabled {
		if t := r.URL.Query().Get("tenant"); t != "" {
			q.TenantID = t
		}
	}
	records, err := d.Stores.Usage.List(r.Context(), q)
	if err != nil {
		writeError(w, statusForStorageErr(err), "USAGE_LIST_FAILED", err.Error())
		return
	}
	summary, err := d.Stores.Usage.Summary(r.Context(), q)
	if err != nil {
		writeError(w, statusForStorageErr(err), "USAGE_SUMMARY_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": records, "summary": summary})
}
