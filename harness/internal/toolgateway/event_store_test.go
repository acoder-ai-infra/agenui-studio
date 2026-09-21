package toolgateway

import (
	"context"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestMemoryEventStoreAllocatesSequencePerRun(t *testing.T) {
	store := NewMemoryEventStore()
	ctx := context.Background()
	base := canonicalStoreTestEvent()
	first, err := store.AppendEvent(ctx, base)
	if err != nil {
		t.Fatalf("append first event: %v", err)
	}
	second := base
	second.EventID = "evt-2"
	second.EventType = observability.EventToolCallCompleted
	second.IdempotencyKey = "run-a:tc-1:completed"
	secondRun, err := store.AppendEvent(ctx, second)
	if err != nil {
		t.Fatalf("append second event: %v", err)
	}
	otherRun := base
	otherRun.EventID = "evt-3"
	otherRun.IdempotencyKey = "run-b:tc-2:started"
	otherRun.RunID = "run-b"
	otherRunResult, err := store.AppendEvent(ctx, otherRun)
	if err != nil {
		t.Fatalf("append other run: %v", err)
	}
	if first.Event.Sequence != 1 || secondRun.Event.Sequence != 2 || otherRunResult.Event.Sequence != 1 {
		t.Fatalf("unexpected sequences: %d %d %d", first.Event.Sequence, secondRun.Event.Sequence, otherRunResult.Event.Sequence)
	}
	if got := len(store.Events()); got != 3 {
		t.Fatalf("event count = %d, want 3", got)
	}
}

func TestMemoryEventStoreDeduplicatesSameEventIDDifferentIdempotencyKey(t *testing.T) {
	store := NewMemoryEventStore()
	base := canonicalStoreTestEvent()
	first, err := store.AppendEvent(context.Background(), base)
	if err != nil {
		t.Fatalf("append first event: %v", err)
	}
	duplicate := base
	duplicate.IdempotencyKey = "run-a:tc-1:alternate-started"
	persisted, err := store.AppendEvent(context.Background(), duplicate)
	if err != nil {
		t.Fatalf("append duplicate event id: %v", err)
	}
	if persisted.Event.EventID != first.Event.EventID || persisted.Event.IdempotencyKey != first.Event.IdempotencyKey || persisted.Event.Sequence != 1 {
		t.Fatalf("duplicate did not return first event: %#v", persisted.Event)
	}
	if got := len(store.Events()); got != 1 {
		t.Fatalf("event count = %d, want 1", got)
	}
}

func TestMemoryEventStoreDeduplicatesDifferentEventIDSameIdempotencyKey(t *testing.T) {
	store := NewMemoryEventStore()
	base := canonicalStoreTestEvent()
	first, err := store.AppendEvent(context.Background(), base)
	if err != nil {
		t.Fatalf("append first event: %v", err)
	}
	duplicate := base
	duplicate.EventID = "evt-2"
	persisted, err := store.AppendEvent(context.Background(), duplicate)
	if err != nil {
		t.Fatalf("append duplicate idempotency key: %v", err)
	}
	if persisted.Event.EventID != first.Event.EventID || persisted.Event.IdempotencyKey != first.Event.IdempotencyKey || persisted.Event.Sequence != 1 {
		t.Fatalf("duplicate did not return first event: %#v", persisted.Event)
	}
	if got := len(store.Events()); got != 1 {
		t.Fatalf("event count = %d, want 1", got)
	}
}

func TestMemoryEventStoreRejectsNonCanonicalEvent(t *testing.T) {
	store := NewMemoryEventStore()
	_, err := store.AppendEvent(context.Background(), observability.AgentEvent{
		EventID:    "evt-bad",
		TraceID:    "trace-1",
		RunID:      "run-1",
		EventType:  observability.EventType("tool_warning"),
		Visibility: observability.VisibilityUserVisible,
		CreatedAt:  time.Now(),
	})
	if err == nil {
		t.Fatal("non-canonical event type must be rejected")
	}
}

func TestMemoryEventStoreSupportsZeroValue(t *testing.T) {
	var store MemoryEventStore
	result, err := store.AppendEvent(context.Background(), observability.AgentEvent{
		EventID:    "evt-1",
		TraceID:    "trace-1",
		RunID:      "run-1",
		SessionID:  "sess-1",
		EventType:  observability.EventToolCallStarted,
		Visibility: observability.VisibilityUserVisible,
		CreatedAt:  time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("append with zero-value store: %v", err)
	}
	if result.Event.Sequence != 1 {
		t.Fatalf("sequence = %d, want 1", result.Event.Sequence)
	}
}

func canonicalStoreTestEvent() observability.AgentEvent {
	return observability.AgentEvent{
		EventID:        "evt-1",
		TraceID:        "trace-1",
		RunID:          "run-a",
		SessionID:      "sess-1",
		EventType:      observability.EventToolCallStarted,
		Visibility:     observability.VisibilityUserVisible,
		CreatedAt:      time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC),
		IdempotencyKey: "run-a:tc-1:started",
	}
}
