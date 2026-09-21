package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol/server"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// appendPublish persists an event and pushes it to the broker (mirrors the
// runtime's persist-before-push order) so a live SSE stream can observe it.
func appendPublish(t *testing.T, deps server.Deps, stores storage.Stores, ev observability.AgentEvent) observability.AgentEvent {
	t.Helper()
	res, err := stores.Events.Append(context.Background(), ev)
	if err != nil {
		t.Fatalf("append %s: %v", ev.EventType, err)
	}
	if err := deps.Broker.Publish(context.Background(), res.Event); err != nil {
		t.Fatalf("publish %s: %v", ev.EventType, err)
	}
	return res.Event
}

// A Run event stream owns exactly one persisted sequence space. Child Run
// events are queried through the child Run ID and must never leak into the
// parent's replay cursor or live stream.
func TestEvents_ParentRunKeepsIndependentSequenceSpace(t *testing.T) {
	deps, stores := newDeps()
	const sid, parentRID, childRID = "s1", "run_parent", "run_child"
	seedRun(t, stores, sid, parentRID, "")
	anchor := seedEvent(t, stores, parentRID, observability.EventAgentStarted, observability.VisibilityUserVisible)
	if err := stores.Runs.Create(context.Background(), &storage.Run{
		RunID: childRID, SessionID: sid, ParentRunID: parentRID, Status: storage.RunStatusRunning,
	}); err != nil {
		t.Fatalf("create child run: %v", err)
	}
	childFirst := seedEvent(t, stores, childRID, observability.EventToolCallStarted, observability.VisibilityUserVisible)
	parentSecond := seedEvent(t, stores, parentRID, observability.EventAgentTextDelta, observability.VisibilityUserVisible)
	childSecond := seedEvent(t, stores, childRID, observability.EventToolCallCompleted, observability.VisibilityUserVisible)

	// The parent's HTTP cursor is root-scoped. after_sequence=1 excludes the
	// anchor and cannot surface either child event even though both child
	// sequences are valid in the child's own stream.
	pull := httptest.NewRequest(http.MethodGet,
		"/api/v1/sessions/"+sid+"/runs/"+parentRID+"/events?after_sequence=1", nil)
	pullRec := httptest.NewRecorder()
	server.NewRouter(deps).ServeHTTP(pullRec, pull)
	if pullRec.Code != http.StatusOK {
		t.Fatalf("parent pull status=%d body=%s", pullRec.Code, pullRec.Body.String())
	}
	var parentEnvelope struct {
		Events []struct {
			EventID  string `json:"event_id"`
			RunID    string `json:"run_id"`
			Sequence int64  `json:"sequence"`
		} `json:"events"`
	}
	if err := json.Unmarshal(pullRec.Body.Bytes(), &parentEnvelope); err != nil {
		t.Fatalf("decode parent pull: %v", err)
	}
	if len(parentEnvelope.Events) != 1 || parentEnvelope.Events[0].EventID != parentSecond.EventID ||
		parentEnvelope.Events[0].RunID != parentRID || parentEnvelope.Events[0].Sequence != 2 {
		t.Fatalf("parent events=%+v, want only root sequence 2", parentEnvelope.Events)
	}

	// The child owns an independent stream and can be replayed from its own Run
	// ID without a compound cursor.
	childPull := httptest.NewRequest(http.MethodGet,
		"/api/v1/sessions/"+sid+"/runs/"+childRID+"/events?after_sequence=0", nil)
	childRec := httptest.NewRecorder()
	server.NewRouter(deps).ServeHTTP(childRec, childPull)
	if childRec.Code != http.StatusOK {
		t.Fatalf("child pull status=%d body=%s", childRec.Code, childRec.Body.String())
	}
	var childEnvelope struct {
		Events []struct {
			EventID  string `json:"event_id"`
			RunID    string `json:"run_id"`
			Sequence int64  `json:"sequence"`
		} `json:"events"`
	}
	if err := json.Unmarshal(childRec.Body.Bytes(), &childEnvelope); err != nil {
		t.Fatalf("decode child pull: %v", err)
	}
	if len(childEnvelope.Events) != 2 || childEnvelope.Events[0].EventID != childFirst.EventID ||
		childEnvelope.Events[1].EventID != childSecond.EventID || childEnvelope.Events[0].Sequence != 1 ||
		childEnvelope.Events[1].Sequence != 2 {
		t.Fatalf("child events=%+v, want child sequences 1,2", childEnvelope.Events)
	}

	// The same isolation applies to live SSE. Publishing a child event while the
	// parent is connected must not produce a parent frame.

	ts := httptest.NewServer(server.NewRouter(deps))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/sessions/"+sid+"/runs/"+parentRID+"/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)

	// Replay: both parent events, strictly in the parent's persisted sequence.
	replay := parseSSE(t, br, 2)
	if replay[0]["id"] != anchor.EventID || replay[1]["id"] != parentSecond.EventID {
		t.Fatalf("replay = %v, want parent events %s/%s", replay, anchor.EventID, parentSecond.EventID)
	}

	childLive := appendPublish(t, deps, stores, observability.AgentEvent{
		SessionID: sid, RunID: childRID, AgentID: "child_agent",
		EventType: observability.EventToolCallProgress, Visibility: observability.VisibilityUserVisible,
	})
	parentTerminal := appendPublish(t, deps, stores, observability.AgentEvent{
		SessionID: sid, RunID: parentRID,
		EventType: observability.EventRunCompleted, Visibility: observability.VisibilityUserVisible,
	})

	live := parseSSE(t, br, 1)
	if live[0]["id"] != parentTerminal.EventID {
		t.Fatalf("live frame = %v, want parent terminal %s; child %s leaked into parent stream", live[0], parentTerminal.EventID, childLive.EventID)
	}
}

func TestEvents_ReplayDrainsEveryPersistedPageInSequence(t *testing.T) {
	deps, stores := newDeps()
	const sid, runID = "s-replay-pages", "run-replay-pages"
	seedRun(t, stores, sid, runID, "")
	for index := 0; index < 501; index++ {
		seedEvent(t, stores, runID, observability.EventAgentTextDelta, observability.VisibilityUserVisible)
	}
	terminalEvent := seedEvent(t, stores, runID, observability.EventRunCompleted, observability.VisibilityUserVisible)

	ts := httptest.NewServer(server.NewRouter(deps))
	defer ts.Close()
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/sessions/"+sid+"/runs/"+runID+"/events?after_sequence=0", nil)
	if err != nil {
		t.Fatalf("create replay request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect replay stream: %v", err)
	}
	defer resp.Body.Close()
	frames := parseSSE(t, bufio.NewReader(resp.Body), 502)
	var first, pageTwo struct {
		Sequence int64 `json:"sequence"`
	}
	if err := json.Unmarshal([]byte(frames[0]["data"]), &first); err != nil {
		t.Fatalf("decode first replay frame: %v", err)
	}
	if err := json.Unmarshal([]byte(frames[500]["data"]), &pageTwo); err != nil {
		t.Fatalf("decode second page frame: %v", err)
	}
	if first.Sequence != 1 || pageTwo.Sequence != 501 || frames[501]["id"] != terminalEvent.EventID {
		t.Fatalf("paged replay boundaries first=%v page2=%v terminal=%v", frames[0], frames[500], frames[501])
	}
}
