package storage

import (
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestSameResumeEventComparesStableSemantics(t *testing.T) {
	desired := observability.AgentEvent{
		EventID: "event_resume", IdempotencyKey: "resume_once",
		RunID: "run_1", SessionID: "session_1", AgentID: "agent_1", TraceID: "trace_1", SpanID: "span_1",
		EventType: observability.EventResumeFailed, Visibility: observability.VisibilityInternal,
		Payload: []byte(`{"attempt_id":"attempt_1","code":"FAILED"}`),
		Error:   &observability.EventError{Code: "FAILED", Message: "failed", Retryable: true},
	}
	existing := desired
	existing.Sequence = 12
	existing.CreatedAt = time.Now()
	existing.SchemaVersion = observability.AgentEventSchemaVersion
	if !SameResumeEvent(existing, desired, "attempt_1") {
		t.Fatal("storage-assigned fields must not break idempotent replay")
	}

	tests := []struct {
		name   string
		mutate func(*observability.AgentEvent)
	}{
		{name: "payload", mutate: func(event *observability.AgentEvent) {
			event.Payload = []byte(`{"attempt_id":"attempt_1","code":"TAMPERED"}`)
		}},
		{name: "error", mutate: func(event *observability.AgentEvent) {
			event.Error = &observability.EventError{Code: "OTHER", Message: "failed", Retryable: true}
		}},
		{name: "session", mutate: func(event *observability.AgentEvent) { event.SessionID = "session_2" }},
		{name: "trace", mutate: func(event *observability.AgentEvent) { event.TraceID = "trace_2" }},
		{name: "parent span", mutate: func(event *observability.AgentEvent) { event.ParentSpanID = "parent_2" }},
		{name: "agent type", mutate: func(event *observability.AgentEvent) { event.AgentType = "deep_agent" }},
		{name: "runtime", mutate: func(event *observability.AgentEvent) { event.Runtime = "eino" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			modified := desired
			test.mutate(&modified)
			if SameResumeEvent(existing, modified, "attempt_1") {
				t.Fatalf("modified %s was accepted as exact replay", test.name)
			}
		})
	}
}
