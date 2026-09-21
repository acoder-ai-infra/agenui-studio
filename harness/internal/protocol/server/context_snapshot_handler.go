package server

import (
	"context"
	"net/http"

	ctxpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// ContextSnapshotResolver resolves a frozen context snapshot (metadata + full
// message bodies) by its artifact ref. context.SnapshotManager satisfies this.
type ContextSnapshotResolver interface {
	Resolve(ctx context.Context, snapshotID string) (ctxpkg.Snapshot, []ctxpkg.Message, error)
}

// handleContextSnapshot returns the frozen context snapshot for a run:
// the snapshot metadata plus the resolved message bodies. This is a debug/
// observability endpoint useful for inspecting what context the model received.
func (d *Deps) handleContextSnapshot(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.requireDebug(w, r); !ok {
		return
	}
	run, ok := d.authorizeRun(w, r, r.PathValue("rid"))
	if !ok {
		return
	}
	if run.ContextSnapshotRef == "" {
		writeError(w, http.StatusNotFound, "CONTEXT_SNAPSHOT_MISSING", "run has no context snapshot")
		return
	}
	if d.ContextSnapshots == nil {
		writeError(w, http.StatusNotImplemented, "CONTEXT_SNAPSHOT_UNAVAILABLE", "context snapshot resolver not configured")
		return
	}
	// Enrich the request context with the run's identity so the artifact
	// store can dereference content_ref on messages from earlier turns.
	// Without RunID/SessionID/TenantID the storageMessageLedger.resolveContent
	// fails closed ("incomplete or mismatched context identity").
	tc := observability.MustTraceContext(r.Context())
	tc.SessionID = run.SessionID
	tc.RunID = run.RunID
	if tc.TenantID == "" {
		tc.TenantID = run.TenantID
	}
	ctx := observability.WithTraceContext(r.Context(), tc)

	snapshot, messages, err := d.ContextSnapshots.Resolve(ctx, run.ContextSnapshotRef)
	if err != nil {
		if d.Logger != nil {
			d.Logger.Warn(r.Context(), "context snapshot resolve failed",
				observability.String("run_id", run.RunID),
				observability.String("snapshot_ref", run.ContextSnapshotRef),
				observability.Error(err))
		}
		writeError(w, http.StatusNotFound, "CONTEXT_SNAPSHOT_NOT_FOUND", "context snapshot not found or expired")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshot": snapshot,
		"messages": messages,
	})
}
