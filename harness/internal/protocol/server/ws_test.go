package server_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol/server"
)

// WebSocket 传输复用与 SSE 完全相同的 replay-then-subscribe 驱动与中性投影:先播历史
// 帧、再推 live 帧,按 event_id 去重,每帧套 harness.ws.v1 壳;终态事件后服务端关闭连接。
func TestWS_ReplayThenSubscribe(t *testing.T) {
	deps, stores := newDeps()
	runID := "run_ws"
	seedRun(t, stores, "s1", runID, "")
	e1 := seedEvent(t, stores, runID, observability.EventAgentStarted, observability.VisibilityUserVisible)
	e2 := seedEvent(t, stores, runID, observability.EventAgentTextDelta, observability.VisibilityUserVisible)
	e3 := seedEvent(t, stores, runID, observability.EventAgentTextDelta, observability.VisibilityUserVisible)

	ts := httptest.NewServer(server.NewRouter(deps))
	defer ts.Close()

	// 浏览器 WebSocket 无法设置 Last-Event-ID;重连改用 ?after_sequence=。
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/api/v1/sessions/s1/runs/" + runID + "/events?after_sequence=0"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readWS := func(n int) []protocol.WSData {
		t.Helper()
		out := make([]protocol.WSData, 0, n)
		for len(out) < n {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("read ws (have %d/%d): %v", len(out), n, err)
			}
			var d protocol.WSData
			if err := json.Unmarshal(msg, &d); err != nil {
				t.Fatalf("unmarshal ws frame: %v", err)
			}
			out = append(out, d)
		}
		return out
	}

	// replay:3 条种子事件按序返回,且都套 harness.ws.v1 壳。
	replay := readWS(3)
	for i, d := range replay {
		if d.SchemaVersion != protocol.WSSchemaVersion {
			t.Fatalf("frame %d schema=%q want %q", i, d.SchemaVersion, protocol.WSSchemaVersion)
		}
	}
	if replay[0].EventID != e1.EventID || replay[1].EventID != e2.EventID || replay[2].EventID != e3.EventID {
		t.Fatalf("replay order wrong: %v", replay)
	}

	// 先持久化后推送:先 Append 再 Publish 两条 live 事件(最后一条是终态)。
	e4 := seedEvent(t, stores, runID, observability.EventAgentTextDelta, observability.VisibilityUserVisible)
	e5 := seedEvent(t, stores, runID, observability.EventRunCompleted, observability.VisibilityUserVisible)
	if err := deps.Broker.Publish(context.Background(), e4); err != nil {
		t.Fatal(err)
	}
	if err := deps.Broker.Publish(context.Background(), e5); err != nil {
		t.Fatal(err)
	}

	live := readWS(2)
	if live[0].EventID != e4.EventID || live[1].EventID != e5.EventID {
		t.Fatalf("live order wrong: %v", live)
	}

	// 跨 replay/live 边界无重复:全程共 5 个不同的 event_id。
	seen := map[string]int{}
	for _, d := range append(replay, live...) {
		seen[d.EventID]++
	}
	if len(seen) != 5 {
		t.Fatalf("expected 5 distinct events, got %d", len(seen))
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("event %s delivered %d times, want 1", id, c)
		}
	}

	// 终态事件之后,服务端关闭流。
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("expected stream to close after terminal event")
	}
}
