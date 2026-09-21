package protocol

import (
	"encoding/json"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// SSESchemaVersion is the SSE shell's schema tag (canonical §8.1). It identifies
// the SSE transport shell, not the projection — it is bound to SSE and must not
// appear in a non-SSE response.
const SSESchemaVersion = "harness.sse.v1"

// JSONSchemaVersion is the HTTP-JSON snapshot shell's schema tag. It lives on the
// envelope, not on each event.
const JSONSchemaVersion = "harness.json.v1"

// WSSchemaVersion is the WebSocket text-frame shell's schema tag.
const WSSchemaVersion = "harness.ws.v1"

// SSEEventName is the fixed SSE "event:" name for projected agent events.
const SSEEventName = "agent_event"

// view_type constants — end-side semantic view names (canonical §6.9). They are
// client-side only and MUST NOT be written back to the EventStore as EventType.
const (
	ViewTypeToolStart    = "tool_start"
	ViewTypeToolDelta    = "tool_delta"
	ViewTypeToolEnd      = "tool_end"
	ViewTypeMessageDelta = "message_delta"
)

// SSEFrame is the SSE frame envelope (canonical §8.1, harness.sse.v1).
type SSEFrame struct {
	ID    string // SSE "id:" line == event_id (P3-D1)
	Event string // fixed "agent_event"
	Data  SSEData
}

// EventData is the transport-neutral canonical projection (维度②) of an
// observability.AgentEvent. It carries no schema_version and no envelope: those
// belong to the transport shell (SSE / HTTP-JSON / WebSocket), each of which
// wraps EventData its own way. It is projected via ProjectEvent; it is not a
// source of truth.
type EventData struct {
	Sequence  int64                   `json:"sequence"`
	EventID   string                  `json:"event_id"`
	EventType observability.EventType `json:"event_type"`
	ViewType  string                  `json:"view_type,omitempty"` // end-side semantic; not a source of truth
	TraceID   string                  `json:"trace_id,omitempty"`
	SessionID string                  `json:"session_id,omitempty"`
	RunID     string                  `json:"run_id,omitempty"`
	// ParentRunID is set only on sub-agent child-run events fanned into a parent
	// run-tree stream, so clients can nest them under their parent run.
	ParentRunID string `json:"parent_run_id,omitempty"`
	// AgentID attributes the event to the emitting agent (agent identity for
	// nested rendering); empty when unknown.
	AgentID     string                        `json:"agent_id,omitempty"`
	Visibility  observability.EventVisibility `json:"visibility"`
	Payload     json.RawMessage               `json:"payload,omitempty"`      // from payload_preview
	ArtifactRef string                        `json:"artifact_ref,omitempty"` // from payload_ref
	Usage       json.RawMessage               `json:"usage,omitempty"`
	Debug       json.RawMessage               `json:"debug,omitempty"` // authorized debug clients only
	CreatedAt   time.Time                     `json:"created_at"`
}

// SSEData is the JSON body of an SSE frame: the transport-neutral EventData
// wrapped with the SSE shell's schema tag (harness.sse.v1). The embedded
// EventData has no JSON tag, so its fields are promoted and the wire JSON stays
// flat and byte-identical to the pre-refactor shape.
type SSEData struct {
	SchemaVersion string `json:"schema_version"`
	EventData
}

// JSONEnvelope is the HTTP one-shot pull response shell (harness.json.v1). The
// schema tag lives on the envelope, not on each event.
type JSONEnvelope struct {
	Schema string      `json:"schema"`
	Events []EventData `json:"events"`
}

// WSData is the JSON body of one WebSocket text message: the transport-neutral
// EventData wrapped with the WS shell's schema tag (harness.ws.v1).
type WSData struct {
	SchemaVersion string `json:"schema_version"`
	EventData
}

// ProtocolFrame is a protocol-agnostic downstream frame (SSE/WS/HTTP JSON each
// serialize their own way).
type ProtocolFrame struct {
	Kind    string // "sse" | "http_json" | ...
	SSE     *SSEFrame
	Payload []byte
}

// HotDelta is one increment on the realtime channel (not a source of truth).
type HotDelta struct {
	DeltaSeq  int64     `json:"delta_seq"`
	Content   []byte    `json:"content"` // client_delta (typewriter granularity)
	CreatedAt time.Time `json:"created_at"`
}

// ClientCapabilities is the client capability report (negotiate, P1).
type ClientCapabilities struct {
	Platform         string
	ProtocolSupport  map[string]bool // sse/websocket/http_polling
	RenderingSupport map[string]bool // markdown/atoui/adaptive_card/form/artifact_preview/debug_panel
	MaxEventPayload  int
}

// ClientTarget describes the end that will receive frames. It carries the
// authorization needed to decide visibility filtering. user_visible is always
// allowed; any other visibility must be explicitly authorized.
type ClientTarget struct {
	Platform string
	// AllowedVisibilities are non-user_visible visibilities this target is
	// authorized to receive (e.g. debug/internal/restricted for an admin/debug
	// console). Empty means an ordinary client (user_visible only).
	AllowedVisibilities []observability.EventVisibility
}

// Allows reports whether the target may receive an event of visibility v.
func (t ClientTarget) Allows(v observability.EventVisibility) bool {
	if v == observability.VisibilityUserVisible || v == "" {
		return true
	}
	for _, av := range t.AllowedVisibilities {
		if av == v {
			return true
		}
	}
	return false
}

// Visibilities returns the visibility set this target may receive, suitable for
// storage.EventQuery.Visibilities. Ordinary clients get [user_visible].
func (t ClientTarget) Visibilities() []observability.EventVisibility {
	out := []observability.EventVisibility{observability.VisibilityUserVisible}
	for _, v := range t.AllowedVisibilities {
		if v != observability.VisibilityUserVisible {
			out = append(out, v)
		}
	}
	return out
}
