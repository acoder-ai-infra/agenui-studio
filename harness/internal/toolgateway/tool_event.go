package toolgateway

import (
	"encoding/json"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/identifiercontract"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/processpresentation"
)

const ToolEventSchemaVersion = "tool_event.v1"

type ToolEventType string

const (
	ToolEventProgress        ToolEventType = "tool_call_progress"
	ToolEventWarning         ToolEventType = "tool_warning"
	ToolEventDebug           ToolEventType = "tool_debug"
	ToolEventArtifactCreated ToolEventType = "tool_artifact_created"

	toolEventStarted   ToolEventType = "tool_call_started"
	toolEventCompleted ToolEventType = "tool_call_completed"
	toolEventFailed    ToolEventType = "tool_call_failed"
	toolEventCancelled ToolEventType = "tool_call_cancelled"
)

type ToolEvent struct {
	ToolEventID     string        `json:"tool_event_id"`
	ExternalEventID string        `json:"external_event_id,omitempty"`
	EventType       ToolEventType `json:"event_type"`
	SchemaVersion   string        `json:"schema_version,omitempty"`
	Sequence        int64         `json:"sequence,omitempty"`
	IdempotencyKey  string        `json:"idempotency_key,omitempty"`

	TenantID            string                        `json:"tenant_id,omitempty"`
	UserID              string                        `json:"user_id,omitempty"`
	TraceID             string                        `json:"trace_id"`
	SpanID              string                        `json:"span_id,omitempty"`
	ParentSpanID        string                        `json:"parent_span_id,omitempty"`
	SessionID           string                        `json:"session_id"`
	RunID               string                        `json:"run_id"`
	StepID              string                        `json:"step_id"`
	AgentID             string                        `json:"agent_id"`
	AgentType           string                        `json:"agent_type,omitempty"`
	Runtime             string                        `json:"runtime,omitempty"`
	ToolCallID          string                        `json:"tool_call_id"`
	ToolName            string                        `json:"tool_name"`
	ToolVersion         string                        `json:"tool_version,omitempty"`
	ToolType            ToolType                      `json:"tool_type,omitempty"`
	ProcessPresentation processpresentation.Metadata  `json:"process_presentation,omitempty"`
	Visibility          observability.EventVisibility `json:"visibility"`
	Payload             json.RawMessage               `json:"payload,omitempty"`
	PayloadPreview      json.RawMessage               `json:"payload_preview,omitempty"`
	PayloadRef          string                        `json:"payload_ref,omitempty"`
	DebugRef            string                        `json:"debug_ref,omitempty"`
	Error               *observability.EventError     `json:"error,omitempty"`
	CreatedAt           time.Time                     `json:"created_at"`
}

func NormalizeToolEvent(ids observability.IDGenerator, event ToolEvent) (observability.AgentEvent, error) {
	if event.SchemaVersion != "" && event.SchemaVersion != ToolEventSchemaVersion {
		return observability.AgentEvent{}, NewToolError(ErrorTypeSchemaValidationFailed, "unsupported tool event schema version", false, nil)
	}
	canonicalType, ok := canonicalToolEventType(event.EventType)
	if !ok {
		return observability.AgentEvent{}, NewToolError(ErrorTypeSchemaValidationFailed, "tool event has no canonical AgentEvent mapping", false, nil)
	}
	eventID := event.ToolEventID
	if eventID == "" && ids != nil {
		eventID = ids.NewEventID()
	}
	visibility := event.Visibility
	if visibility == "" {
		visibility = observability.VisibilityDebug
	}
	idempotencyKey := event.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = toolLifecycleIdempotencyKey(event.RunID, event.ToolCallID, canonicalType)
	}
	if idempotencyKey == "" && event.RunID != "" && event.ToolCallID != "" && eventID != "" {
		idempotencyKey = identifiercontract.ComposeIdempotencyKey(event.RunID, event.ToolCallID, "event", hashString(eventID)[:24])
	}
	payloadPreview := event.PayloadPreview
	if err := validateToolEventPreview(event, payloadPreview); err != nil {
		return observability.AgentEvent{}, err
	}
	return observability.AgentEvent{
		EventID:        eventID,
		SchemaVersion:  observability.AgentEventSchemaVersion,
		Sequence:       0,
		IdempotencyKey: idempotencyKey,
		TraceID:        event.TraceID,
		SpanID:         event.SpanID,
		ParentSpanID:   event.ParentSpanID,
		SessionID:      event.SessionID,
		RunID:          event.RunID,
		StepID:         event.StepID,
		AgentID:        event.AgentID,
		AgentType:      event.AgentType,
		Runtime:        event.Runtime,
		EventType:      canonicalType,
		Visibility:     visibility,
		PayloadPreview: append(json.RawMessage(nil), payloadPreview...),
		PayloadRef:     event.PayloadRef,
		DebugRef:       event.DebugRef,
		Error:          event.Error,
		CreatedAt:      event.CreatedAt,
	}, nil
}

func validateToolEventPreview(event ToolEvent, preview json.RawMessage) error {
	var payload map[string]any
	if len(preview) == 0 || json.Unmarshal(preview, &payload) != nil || payload == nil {
		return NewToolError(ErrorTypeSchemaValidationFailed, "tool event requires a sanitized structured payload preview", false, nil)
	}
	if event.EventType != ToolEventProgress && event.EventType != ToolEventWarning && event.EventType != ToolEventArtifactCreated {
		return nil
	}
	for key, expected := range map[string]string{
		"tool_call_id": event.ToolCallID,
		"tool_name":    event.ToolName,
		"tool_version": event.ToolVersion,
		"tool_type":    string(event.ToolType),
	} {
		actual, ok := payload[key].(string)
		if !ok || expected == "" || actual != expected {
			return NewToolError(ErrorTypeSchemaValidationFailed, "tool event payload preview has invalid canonical identity", false, nil)
		}
	}
	return nil
}

func canonicalToolEventType(eventType ToolEventType) (observability.EventType, bool) {
	switch eventType {
	case ToolEventProgress, ToolEventWarning:
		return observability.EventToolCallProgress, true
	case ToolEventArtifactCreated:
		return observability.EventToolArtifactCreated, true
	case toolEventStarted:
		return observability.EventToolCallStarted, true
	case toolEventCompleted:
		return observability.EventToolCallCompleted, true
	case toolEventFailed:
		return observability.EventToolCallFailed, true
	case toolEventCancelled:
		return observability.EventToolCallCancelled, true
	default:
		return "", false
	}
}

func toolLifecycleIdempotencyKey(runID, toolCallID string, eventType observability.EventType) string {
	if runID == "" || toolCallID == "" {
		return ""
	}
	phase := ""
	switch eventType {
	case observability.EventToolCallStarted:
		phase = "started"
	case observability.EventToolCallCompleted:
		phase = "completed"
	case observability.EventToolCallFailed:
		phase = "failed"
	case observability.EventToolCallCancelled:
		phase = "cancelled"
	case observability.EventToolArtifactCreated:
		phase = "artifact"
	}
	if phase == "" {
		return ""
	}
	return identifiercontract.ComposeIdempotencyKey(runID, toolCallID, phase)
}
