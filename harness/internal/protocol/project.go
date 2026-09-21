package protocol

import (
	"encoding/json"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// ProjectEvent is the transport-neutral projection (维度②): it maps a canonical
// observability.AgentEvent into an EventData view-model, applying the view_type
// mapping and the debug-ref visibility gate. It never sets a schema_version and
// never wraps the event in a transport envelope — that is each transport shell's
// job (SSE / HTTP-JSON / WebSocket). Visibility filtering of whole events is the
// caller's responsibility; ProjectEvent only projects fields.
//
// The payload comes from PayloadPreview (never the full payload) — canonical §8.1.
func ProjectEvent(event observability.AgentEvent, target ClientTarget, mapper ViewModelMapper) EventData {
	if mapper == nil {
		mapper = DefaultViewModelMapper{}
	}
	data := EventData{
		Sequence:    event.Sequence,
		EventID:     event.EventID,
		EventType:   event.EventType,
		ViewType:    mapper.ViewType(event.EventType),
		TraceID:     event.TraceID,
		SessionID:   event.SessionID,
		RunID:       event.RunID,
		ParentRunID: event.ParentRunID,
		AgentID:     event.AgentID,
		Visibility:  event.Visibility,
		Payload:     event.PayloadPreview, // never inline the full payload
		ArtifactRef: event.PayloadRef,
		Usage:       event.Usage,
		CreatedAt:   event.CreatedAt,
	}
	// Debug ref is only surfaced to authorized (non-ordinary) targets.
	if event.DebugRef != "" && len(target.AllowedVisibilities) > 0 {
		if raw, err := json.Marshal(event.DebugRef); err == nil {
			data.Debug = raw
		}
	}
	return data
}

// BuildJSONEnvelope wraps a batch of projected events in the HTTP-JSON snapshot
// shell (harness.json.v1). events must not be nil so the JSON encodes as [] not
// null.
func BuildJSONEnvelope(events []EventData) JSONEnvelope {
	if events == nil {
		events = []EventData{}
	}
	return JSONEnvelope{Schema: JSONSchemaVersion, Events: events}
}
