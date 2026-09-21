package agentruntime

import (
	"context"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// P1-8: an event delivered on the realtime channel must carry the same
// store-allocated run-monotonic sequence as its persisted/replayed copy, so live
// SSE and historical replay agree (Last-Event-ID reconnect dedup depends on it).
// Before the fix the realtime copy carried Sequence==0 while the stored copy had
// the real sequence.
func TestRealtimeSequenceMatchesStored(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime(
		observability.AgentEvent{EventType: observability.EventAgentCompleted, Visibility: observability.VisibilityUserVisible},
	)
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))

	req := testRunRequest()
	ch, err := service.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	live := collect(ch)

	stored := state.Events(req.RunID)
	storedSeq := make(map[string]int64, len(stored))
	for _, e := range stored {
		storedSeq[e.EventID] = e.Sequence
	}

	var checked int
	var last int64
	for _, ev := range live {
		if observability.IsEphemeralDelta(ev.EventType) {
			continue // token deltas are intentionally unsequenced
		}
		want, ok := storedSeq[ev.EventID]
		if !ok {
			continue
		}
		checked++
		if ev.Sequence == 0 {
			t.Fatalf("realtime event %s carries zero sequence (should match stored %d)", ev.EventType, want)
		}
		if ev.Sequence != want {
			t.Fatalf("realtime seq %d != stored seq %d for %s", ev.Sequence, want, ev.EventType)
		}
		if ev.Sequence <= last {
			t.Fatalf("sequence not increasing: %d after %d (%s)", ev.Sequence, last, ev.EventType)
		}
		last = ev.Sequence
	}
	if checked == 0 {
		t.Fatal("no persisted events were cross-checked")
	}
}
