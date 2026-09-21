package protocol_test

import (
	"context"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
)

// SubscribeSession fans every event in a session (across runs) to session
// subscribers, while per-run Subscribe stays scoped to its run. This backs
// run-tree fan-in: a parent stream picks up descendant child-run events (which
// share the parent's SessionID) via a session subscription.
func TestMemoryBroker_SubscribeSessionFansAcrossRuns(t *testing.T) {
	b := protocol.NewMemoryBroker()
	ctx := context.Background()

	sess, cancelSess, err := b.SubscribeSession(ctx, "s1")
	if err != nil {
		t.Fatalf("SubscribeSession: %v", err)
	}
	defer cancelSess()
	run, cancelRun, err := b.Subscribe(ctx, "run_parent")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancelRun()

	parentEv := observability.AgentEvent{EventID: "e_parent", SessionID: "s1", RunID: "run_parent"}
	childEv := observability.AgentEvent{EventID: "e_child", SessionID: "s1", RunID: "run_child"}
	otherSessionEv := observability.AgentEvent{EventID: "e_other", SessionID: "s2", RunID: "run_x"}

	for _, ev := range []observability.AgentEvent{parentEv, childEv, otherSessionEv} {
		if err := b.Publish(ctx, ev); err != nil {
			t.Fatalf("publish %s: %v", ev.EventID, err)
		}
	}

	// Session subscriber sees both s1 events (parent + child run) but not s2.
	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case ev := <-sess:
			got[ev.EventID] = true
		default:
			t.Fatalf("session subscriber missing event %d", i)
		}
	}
	if !got["e_parent"] || !got["e_child"] {
		t.Fatalf("session subscriber missing s1 events: %v", got)
	}
	select {
	case ev := <-sess:
		t.Fatalf("session subscriber leaked cross-session event %s", ev.EventID)
	default:
	}

	// Per-run subscriber only sees its own run.
	select {
	case ev := <-run:
		if ev.EventID != "e_parent" {
			t.Fatalf("run subscriber got %s, want e_parent", ev.EventID)
		}
	default:
		t.Fatalf("run subscriber missing e_parent")
	}
	select {
	case ev := <-run:
		t.Fatalf("run subscriber leaked non-run event %s", ev.EventID)
	default:
	}
}
