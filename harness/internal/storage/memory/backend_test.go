package memory

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

func tenantCtx(tenant string) context.Context {
	return observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: tenant})
}

// X-001: memory declares all required capabilities for every store port.
func TestBackendDeclaresRequiredCapabilities(t *testing.T) {
	b := New()
	for store, required := range storage.RequiredCapabilities {
		if err := storage.RequireCapabilities(b, required...); err != nil {
			t.Fatalf("memory backend missing capability for %s: %v", store, err)
		}
	}
}

// E-004: concurrent appends to the same run produce unique, monotonic sequences.
func TestEventStoreConcurrentSequence(t *testing.T) {
	ctx := tenantCtx("t1")
	es := New().Stores().Events
	const n = 200
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := es.Append(ctx, observability.AgentEvent{
				RunID:     "run1",
				EventType: observability.EventAgentTextDelta,
			})
			if err != nil {
				t.Errorf("append: %v", err)
			}
		}()
	}
	wg.Wait()

	events, err := es.Query(ctx, storage.EventQuery{RunID: "run1"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(events) != n {
		t.Fatalf("want %d events, got %d", n, len(events))
	}
	seen := make(map[int64]bool)
	for i, ev := range events {
		if ev.Sequence == 0 {
			t.Fatalf("event has zero sequence")
		}
		if seen[ev.Sequence] {
			t.Fatalf("duplicate sequence %d", ev.Sequence)
		}
		seen[ev.Sequence] = true
		if i > 0 && events[i-1].Sequence >= ev.Sequence {
			t.Fatalf("sequence not ascending: %d then %d", events[i-1].Sequence, ev.Sequence)
		}
	}
	last, _ := es.LastSequence(ctx, "run1")
	if last != n {
		t.Fatalf("last sequence want %d got %d", n, last)
	}
}

// E-003: duplicate event_id / idempotency_key does not create a duplicate event.
func TestEventStoreIdempotency(t *testing.T) {
	ctx := tenantCtx("t1")
	es := New().Stores().Events

	r1, err := es.Append(ctx, observability.AgentEvent{RunID: "run1", EventID: "evt_fixed", EventType: observability.EventRunStarted})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := es.Append(ctx, observability.AgentEvent{RunID: "run1", EventID: "evt_fixed", EventType: observability.EventRunStarted})
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Idempotent {
		t.Fatalf("expected idempotent replay for duplicate event_id")
	}
	if r1.Event.Sequence != r2.Event.Sequence {
		t.Fatalf("replay sequence mismatch: %d vs %d", r1.Event.Sequence, r2.Event.Sequence)
	}

	// idempotency_key dedup
	_, _ = es.Append(ctx, observability.AgentEvent{RunID: "run1", IdempotencyKey: "k1", EventType: observability.EventAgentTextDelta})
	r4, _ := es.Append(ctx, observability.AgentEvent{RunID: "run1", IdempotencyKey: "k1", EventType: observability.EventAgentTextDelta})
	if !r4.Idempotent {
		t.Fatalf("expected idempotent replay for duplicate idempotency_key")
	}

	events, _ := es.Query(ctx, storage.EventQuery{RunID: "run1"})
	if len(events) != 2 {
		t.Fatalf("want 2 distinct events, got %d", len(events))
	}
}

// E-002/P-003 support: after_sequence replay is exclusive and ascending.
func TestEventStoreAfterSequence(t *testing.T) {
	ctx := tenantCtx("t1")
	es := New().Stores().Events
	for i := 0; i < 5; i++ {
		_, _ = es.Append(ctx, observability.AgentEvent{RunID: "run1", EventType: observability.EventAgentTextDelta})
	}
	events, _ := es.Query(ctx, storage.EventQuery{RunID: "run1", AfterSequence: 2})
	if len(events) != 3 {
		t.Fatalf("want 3 events after seq 2, got %d", len(events))
	}
	if events[0].Sequence != 3 {
		t.Fatalf("want first sequence 3, got %d", events[0].Sequence)
	}
}

// P3-D1: event_id -> sequence resolution for SSE reconnect via Last-Event-ID.
func TestEventStoreSequenceOf(t *testing.T) {
	ctx := tenantCtx("t1")
	es := New().Stores().Events
	r, _ := es.Append(ctx, observability.AgentEvent{RunID: "run1", EventType: observability.EventAgentTextDelta})
	seq, err := es.SequenceOf(ctx, "run1", r.Event.EventID)
	if err != nil {
		t.Fatalf("SequenceOf: %v", err)
	}
	if seq != r.Event.Sequence {
		t.Fatalf("want sequence %d, got %d", r.Event.Sequence, seq)
	}
	if _, err := es.SequenceOf(ctx, "run1", "evt_unknown"); !storage.IsErrorCode(err, storage.ErrNotFound) {
		t.Fatalf("want not_found for unknown event, got %v", err)
	}
}

// P-001: visibility filter for user-visible replay.
func TestEventStoreVisibilityFilter(t *testing.T) {
	ctx := tenantCtx("t1")
	es := New().Stores().Events
	_, _ = es.Append(ctx, observability.AgentEvent{RunID: "run1", EventType: observability.EventAgentTextDelta, Visibility: observability.VisibilityUserVisible})
	_, _ = es.Append(ctx, observability.AgentEvent{RunID: "run1", EventType: observability.EventModelTokenDelta, Visibility: observability.VisibilityDebug})

	userView, _ := es.Query(ctx, storage.EventQuery{RunID: "run1", Visibilities: []observability.EventVisibility{observability.VisibilityUserVisible}})
	if len(userView) != 1 {
		t.Fatalf("user view should have 1 event, got %d", len(userView))
	}
	all, _ := es.Query(ctx, storage.EventQuery{RunID: "run1"})
	if len(all) != 2 {
		t.Fatalf("admin view should have 2 events, got %d", len(all))
	}
}

// R-001/state-machines §1: legal transition succeeds, illegal + CAS mismatch fail.
func TestRunStoreCASTransitions(t *testing.T) {
	ctx := tenantCtx("t1")
	runs := New().Stores().Runs
	if err := runs.Create(ctx, &storage.Run{RunID: "run1", SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := runs.CompareAndSetStatus(ctx, "run1", storage.RunStatusCreated, storage.RunStatusRunning, storage.RunMutation{}); err != nil {
		t.Fatalf("created->running should succeed: %v", err)
	}
	// wrong expectFrom => CAS mismatch
	if _, err := runs.CompareAndSetStatus(ctx, "run1", storage.RunStatusCreated, storage.RunStatusCompleted, storage.RunMutation{}); !storage.IsErrorCode(err, storage.ErrCASMismatch) {
		t.Fatalf("want cas_mismatch, got %v", err)
	}
	if _, err := runs.CompareAndSetStatus(ctx, "run1", storage.RunStatusRunning, storage.RunStatusCompleted, storage.RunMutation{}); err != nil {
		t.Fatalf("running->completed should succeed: %v", err)
	}
	// terminal -> anything illegal
	if _, err := runs.CompareAndSetStatus(ctx, "run1", storage.RunStatusCompleted, storage.RunStatusRunning, storage.RunMutation{}); !storage.IsErrorCode(err, storage.ErrIllegalTransition) {
		t.Fatalf("want illegal_transition, got %v", err)
	}
}

// C-001 support: control request answered exactly once (CAS).
func TestControlRequestCompareAndAnswerOnce(t *testing.T) {
	ctx := tenantCtx("t1")
	controls := New().Stores().Controls
	if err := controls.Create(ctx, &storage.ControlRequest{RequestID: "ctrl1", RunID: "run1", Type: "ask_user"}); err != nil {
		t.Fatal(err)
	}
	if _, err := controls.CompareAndAnswer(ctx, "ctrl1", "pending", "answered", "artifact://resp"); err != nil {
		t.Fatalf("first answer should succeed: %v", err)
	}
	if _, err := controls.CompareAndAnswer(ctx, "ctrl1", "pending", "answered", "artifact://resp2"); !storage.IsErrorCode(err, storage.ErrCASMismatch) {
		t.Fatalf("second answer want cas_mismatch, got %v", err)
	}
}

// C-002: pending requests past expiry are moved to expired.
func TestControlRequestExpire(t *testing.T) {
	ctx := tenantCtx("t1")
	controls := New().Stores().Controls
	_ = controls.Create(ctx, &storage.ControlRequest{RequestID: "ctrl1", RunID: "run1", Type: "ask_user", ExpiresAt: time.Now().Add(-time.Minute)})
	expired, err := controls.ExpirePending(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 {
		t.Fatalf("want 1 expired, got %d", len(expired))
	}
	cr, _ := controls.Get(ctx, "ctrl1")
	if cr.Status != "expired" {
		t.Fatalf("want expired, got %s", cr.Status)
	}
}

// resume token consumed once (idempotency).
func TestIdempotencyConsumeOnce(t *testing.T) {
	ctx := tenantCtx("t1")
	idem := New().Stores().Idem
	key := storage.IdemKey{TenantID: "t1", Namespace: storage.IdemNamespaceResumeToken, Key: "tok1"}
	ok, _ := idem.Consume(ctx, key)
	if !ok {
		t.Fatal("first consume should return ok=true")
	}
	ok2, _ := idem.Consume(ctx, key)
	if ok2 {
		t.Fatal("second consume should return ok=false")
	}
}

// SP-003: cross-tenant read is denied.
func TestTenantIsolation(t *testing.T) {
	runs := New().Stores().Runs
	_ = runs.Create(tenantCtx("t1"), &storage.Run{RunID: "run1", SessionID: "s1"})
	if _, err := runs.Get(tenantCtx("t2"), "run1"); !storage.IsErrorCode(err, storage.ErrTenantMismatch) {
		t.Fatalf("want tenant_mismatch, got %v", err)
	}
}
