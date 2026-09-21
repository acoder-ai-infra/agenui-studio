package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
)

// wsUpgrader upgrades the events route to a WebSocket. Origin is not checked at
// P0 — the auth edge / gateway owns cross-origin policy; tighten in P1.
var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

const (
	// wsPongWait bounds how long we wait for a pong before treating the peer as
	// gone; the read pump extends the deadline on every pong.
	wsPongWait = 2 * streamHeartbeatInterval
	// wsWriteWait bounds a single frame write.
	wsWriteWait = 10 * time.Second
)

// streamWS is the WebSocket transport (维度①): upgrade the connection, run a
// read pump so gorilla can process control frames (pong/close) and cancel on
// disconnect, then hand off to the shared replay-then-subscribe driver with a WS
// sink. It projects the very same transport-neutral EventData as SSE — only the
// shell differs (harness.ws.v1 text frames). Reconnect uses ?after_sequence=
// since a browser WebSocket cannot set the Last-Event-ID header.
func (d *Deps) streamWS(w http.ResponseWriter, r *http.Request, rid string, after int64, target protocol.ClientTarget) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote the HTTP error response.
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Read pump: this stream is push-only, but we must drain reads so gorilla can
	// handle pong/close control frames. Any read error (client gone) cancels the
	// stream. WriteControl (below) is safe to call concurrently with reads.
	_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})
	go func() {
		defer cancel()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	d.streamRun(ctx, rid, after, target, wsSink{conn: conn, deps: d, target: target})

	// Best-effort graceful close once the run reaches a terminal state.
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(wsWriteWait))
}

// wsSink writes projected events as WebSocket text frames (harness.ws.v1). All
// writes happen on streamRun's single goroutine, satisfying gorilla's
// one-concurrent-writer constraint (pings via WriteControl are the exception and
// are concurrency-safe).
type wsSink struct {
	conn   *websocket.Conn
	deps   *Deps
	target protocol.ClientTarget
}

func (s wsSink) send(ev observability.AgentEvent) bool {
	data := protocol.WSData{
		SchemaVersion: protocol.WSSchemaVersion,
		EventData:     protocol.ProjectEvent(ev, s.target, s.deps.sse().Mapper),
	}
	body, err := json.Marshal(data)
	if err != nil {
		return true // skip unconvertible event, keep the stream alive
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
	return s.conn.WriteMessage(websocket.TextMessage, body) == nil
}

func (s wsSink) heartbeat() bool {
	return s.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteWait)) == nil
}
