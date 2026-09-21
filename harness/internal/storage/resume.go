package storage

import (
	"encoding/json"
	"reflect"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// ValidateResumeEvent binds the durable event to the same fenced resume
// attempt as the state mutation. This prevents an EventID replay from being
// mistaken for a successful action by another owner.
func ValidateResumeEvent(ev observability.AgentEvent, runID, attemptID string, eventType observability.EventType) error {
	if ev.EventID == "" || ev.RunID != runID || ev.EventType != eventType {
		return NewError(ErrInvalidArgument, "invalid resume event")
	}
	if resumeEventAttemptID(ev) != attemptID {
		return NewError(ErrInvalidArgument, "resume event attempt_id mismatch")
	}
	return nil
}

// SameResumeEvent compares the stable, persisted event semantics. Sequence,
// CreatedAt and storage-assigned defaults are intentionally ignored; changing
// payload, error or execution identity is never considered an idempotent replay.
func SameResumeEvent(existing, desired observability.AgentEvent, attemptID string) bool {
	sameIdentity := existing.EventID == desired.EventID ||
		(desired.IdempotencyKey != "" && existing.IdempotencyKey == desired.IdempotencyKey)
	if !sameIdentity || existing.RunID != desired.RunID ||
		existing.SessionID != desired.SessionID || existing.StepID != desired.StepID ||
		existing.AgentID != desired.AgentID || existing.TraceID != desired.TraceID ||
		existing.SpanID != desired.SpanID || existing.ParentSpanID != desired.ParentSpanID ||
		existing.AgentType != desired.AgentType || existing.Runtime != desired.Runtime ||
		existing.EventType != desired.EventType ||
		existing.IdempotencyKey != desired.IdempotencyKey ||
		normalizeEventVisibility(existing.Visibility) != normalizeEventVisibility(desired.Visibility) ||
		normalizeEventSchema(existing.SchemaVersion) != normalizeEventSchema(desired.SchemaVersion) ||
		existing.PayloadRef != desired.PayloadRef || existing.DebugRef != desired.DebugRef ||
		resumeEventAttemptID(existing) != attemptID || resumeEventAttemptID(desired) != attemptID ||
		!sameJSON(existing.Payload, desired.Payload) ||
		!sameJSON(existing.PayloadPreview, desired.PayloadPreview) ||
		!sameJSON(existing.Usage, desired.Usage) || !sameEventError(existing.Error, desired.Error) {
		return false
	}
	return true
}

func normalizeEventVisibility(visibility observability.EventVisibility) observability.EventVisibility {
	if visibility == "" {
		return observability.VisibilityDebug
	}
	return visibility
}

func normalizeEventSchema(schema string) string {
	if schema == "" {
		return observability.AgentEventSchemaVersion
	}
	return schema
}

func sameJSON(left, right json.RawMessage) bool {
	if len(left) == 0 || len(right) == 0 {
		return len(left) == 0 && len(right) == 0
	}
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return string(left) == string(right)
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func sameEventError(left, right *observability.EventError) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func resumeEventAttemptID(ev observability.AgentEvent) string {
	var payload struct {
		AttemptID string `json:"attempt_id"`
	}
	if len(ev.Payload) == 0 || json.Unmarshal(ev.Payload, &payload) != nil {
		return ""
	}
	return payload.AttemptID
}
