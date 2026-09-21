package protocol_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
)

// ProjectEvent is transport-neutral: it maps view_type, gates the debug ref by
// authorization, uses payload_preview (not the full payload), and carries no
// schema_version tag.
func TestProjectEvent_NeutralProjection(t *testing.T) {
	ev := observability.AgentEvent{
		Sequence:       7,
		EventID:        "evt_1",
		EventType:      observability.EventToolCallStarted,
		Visibility:     observability.VisibilityUserVisible,
		PayloadPreview: json.RawMessage(`{"name":"search"}`),
		Payload:        json.RawMessage(`{"name":"search","secret":"full"}`),
		PayloadRef:     "artifact://a",
		DebugRef:       "debug://d",
	}

	// Ordinary target: view_type mapped, debug gated OFF, payload from preview.
	d := protocol.ProjectEvent(ev, protocol.ClientTarget{}, nil)
	if d.ViewType != protocol.ViewTypeToolStart {
		t.Fatalf("view_type=%q want tool_start", d.ViewType)
	}
	if len(d.Debug) != 0 {
		t.Fatalf("debug ref must be gated off for an ordinary target, got %s", d.Debug)
	}
	if string(d.Payload) != `{"name":"search"}` {
		t.Fatalf("payload should come from payload_preview, got %s", d.Payload)
	}

	// EventData must NOT carry a schema_version — that belongs to a transport shell.
	raw, _ := json.Marshal(d)
	if strings.Contains(string(raw), "schema_version") {
		t.Fatalf("EventData leaked a transport schema tag: %s", raw)
	}

	// Authorized (debug) target: the debug ref is surfaced.
	auth := protocol.ClientTarget{AllowedVisibilities: []observability.EventVisibility{observability.VisibilityDebug}}
	if d2 := protocol.ProjectEvent(ev, auth, nil); len(d2.Debug) == 0 {
		t.Fatal("debug ref should surface to an authorized target")
	}
}

// The SSE shell wraps the neutral EventData with its own schema tag; field
// promotion keeps the wire JSON flat and byte-compatible with the old shape.
func TestSSEData_WireCompatFlatWithSchemaTag(t *testing.T) {
	d := protocol.SSEData{
		SchemaVersion: protocol.SSESchemaVersion,
		EventData:     protocol.EventData{Sequence: 1, EventID: "e", EventType: observability.EventAgentTextDelta},
	}
	s := string(mustJSON(t, d))
	if !strings.Contains(s, `"schema_version":"harness.sse.v1"`) {
		t.Fatalf("missing SSE schema tag: %s", s)
	}
	if !strings.Contains(s, `"sequence":1`) || !strings.Contains(s, `"event_id":"e"`) {
		t.Fatalf("embedded EventData fields not promoted to a flat shape: %s", s)
	}
}

// The HTTP-JSON shell tags at the envelope level (harness.json.v1); nil events
// encode as [] not null.
func TestBuildJSONEnvelope(t *testing.T) {
	if env := protocol.BuildJSONEnvelope([]protocol.EventData{{EventID: "e"}}); env.Schema != protocol.JSONSchemaVersion {
		t.Fatalf("schema=%q want %q", env.Schema, protocol.JSONSchemaVersion)
	}
	if protocol.JSONSchemaVersion != "harness.json.v1" {
		t.Fatalf("unexpected json schema constant: %q", protocol.JSONSchemaVersion)
	}
	if s := string(mustJSON(t, protocol.BuildJSONEnvelope(nil))); !strings.Contains(s, `"events":[]`) {
		t.Fatalf("nil events should encode as []: %s", s)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
