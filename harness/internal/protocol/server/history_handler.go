package server

import (
	"net/http"
	"strings"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

type sessionListItem struct {
	*storage.Session
	ActiveRunStatus storage.RunStatus `json:"active_run_status,omitempty"`
}

// handleListSessions lists the caller's sessions (§5.6, keyset pagination).
func (d *Deps) handleListSessions(w http.ResponseWriter, r *http.Request) {
	tc := observability.MustTraceContext(r.Context())
	page, err := d.Stores.Sessions.List(r.Context(), storage.SessionListQuery{
		UserID:  tc.UserID,
		AgentID: strings.TrimSpace(r.URL.Query().Get("agent_id")),
		Limit:   parseLimit(r, 50),
	})
	if err != nil {
		writeError(w, statusForStorageErr(err), "SESSION_LIST_FAILED", err.Error())
		return
	}
	sessions := any(page.Items)
	if r.URL.Query().Get("include_active_run") == "true" {
		items := make([]sessionListItem, 0, len(page.Items))
		for _, session := range page.Items {
			runs, listErr := d.Stores.Runs.ListBySession(r.Context(), session.ID)
			if listErr != nil {
				writeError(w, statusForStorageErr(listErr), "RUN_LIST_FAILED", listErr.Error())
				return
			}
			item := sessionListItem{Session: session}
			for _, run := range runs {
				if run.ParentRunID == "" && run.Status.BlocksNewTopLevelTurn() {
					item.ActiveRunStatus = run.Status
					break
				}
			}
			items = append(items, item)
		}
		sessions = items
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions":               sessions,
		"has_more":               page.HasMore,
		"next_before_session_id": page.NextBeforeSessionID,
	})
}

// handleListMessages pages a session's messages (§5.6, default user_visible only).
func (d *Deps) handleListMessages(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	if _, ok := d.authorizeSession(w, r, sid); !ok {
		return
	}
	q := storage.MessageListQuery{
		SessionID:       sid,
		BeforeMessageID: r.URL.Query().Get("before_message_id"),
		AfterMessageID:  r.URL.Query().Get("after_message_id"),
		Limit:           parseLimit(r, 50),
	}
	// Default to user_visible; only an authorized target widens visibility.
	target := targetFor(r)
	q.Visibilities = protocol.VisibilitiesForTarget(target)
	page, err := d.Stores.Messages.List(r.Context(), q)
	if err != nil {
		writeError(w, statusForStorageErr(err), "MESSAGE_LIST_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"messages":               page.Items,
		"has_more":               page.HasMore,
		"next_before_message_id": page.NextBeforeMessageID,
		"next_after_message_id":  page.NextAfterMessageID,
	})
}

// handleListRuns lists the runs of a session (§5.6).
func (d *Deps) handleListRuns(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	if _, ok := d.authorizeSession(w, r, sid); !ok {
		return
	}
	runs, err := d.Stores.Runs.ListBySession(r.Context(), sid)
	if err != nil {
		writeError(w, statusForStorageErr(err), "RUN_LIST_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// handleRunResult returns the run snapshot: the final_response event(s) plus the
// run record (§5.6). This is the fallback endpoint after EVENT_REPLAY_EXPIRED.
func (d *Deps) handleRunResult(w http.ResponseWriter, r *http.Request) {
	rid := r.PathValue("rid")
	run, ok := d.authorizeRun(w, r, rid)
	if !ok {
		return
	}
	target := targetFor(r)
	events, err := d.Stores.Events.Query(r.Context(), storage.EventQuery{
		RunID:        rid,
		Visibilities: protocol.VisibilitiesForTarget(target),
		EventTypes:   []observability.EventType{observability.EventFinalResponse},
	})
	if err != nil {
		writeError(w, statusForStorageErr(err), "RUN_RESULT_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run":            run,
		"final_response": events,
	})
}
