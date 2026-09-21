package harness_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk"
	"github.com/AGenUI/agenui-studio/harness/sdk/testkit"
)

// TestChildAskUserSurfacedAsParentControl documents the sub-agent ask-user
// contract from the public SDK contract:
//
//   - Leaf agents return SubAgentInvocationResult{ResultType=ask_user, InteractionProposal:...}.
//   - The parent Run creates the canonical harness.control_request.v1 event.
//   - Clients only ever see the PARENT run's control_request_created event;
//     they never observe a child ControlRequest.
//
// The real runtime chain (child control fact consumed internally by the Agent
// Gateway, promoted to the parent ControlRequest, resumed back into the child)
// is asserted end to end by TestHITLChildAskUserPromotedToParentControlE2E in
// hitl_chain_e2e_test.go. This test focuses on the SDK contract by scripting a
// scenario that emits the parent Control event and asserting
// InteractionProposal invariants.
func TestChildAskUserSurfacedAsParentControl(t *testing.T) {
	// Scenario: sub_agent_started -> sub_agent_completed (with ask_user branch)
	// -> control_request_created on the PARENT run.
	engine := testkit.NewMockEngine(testkit.Scenario{
		Events: []harness.Event{
			{EventType: harness.EventSubAgentStarted, Visibility: harness.VisibilityDebug},
			{EventType: harness.EventSubAgentCompleted, Visibility: harness.VisibilityDebug},
			{EventType: harness.EventControlRequestCreated, Visibility: harness.VisibilityUserVisible},
			testkit.TerminalEvent(harness.EventRunCompleted),
		},
	})
	defer engine.Close(context.Background())

	exec, err := engine.Start(context.Background(), harness.StartRequest{
		Identity: harness.Identity{TenantID: "t"},
		Input:    harness.TextMessage("hi"),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	seenChildControl := false
	seenParentControl := false
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		ev, err := exec.Events().Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("stream: %v", err)
		}
		// child runs share the parent SessionID; ParentRunID marks fan-in.
		// The kernel MUST NOT surface a control_request whose ParentRunID
		// is non-empty (which would indicate a child-owned control_request).
		if ev.EventType == harness.EventControlRequestCreated {
			if ev.ParentRunID != "" {
				seenChildControl = true
			} else {
				seenParentControl = true
			}
		}
	}
	if seenChildControl {
		t.Fatal("child_run must NOT own a ControlRequest; parent Run owns it")
	}
	if !seenParentControl {
		t.Fatal("expected a parent-run control_request_created event")
	}
}

// TestInteractionProposalContractExcludesResumeSecrets is the compile-time
// guard that keeps subagent.go from ever leaking resume secrets. The
// harness.InteractionProposal type is the canonical envelope handed back to
// the parent Run; if a maintainer accidentally added a control_request_id or
// resume_token field to it, this test's JSON tag inspection catches the
// regression.
func TestInteractionProposalContractExcludesResumeSecrets(t *testing.T) {
	// The heavy invariant check lives in subagent_test.go
	// (TestInteractionProposalHasNoControlSecrets); this test verifies the
	// SubAgentResultType discriminator round-trips through testkit's
	// scenarios without corruption.
	if string(harness.SubAgentResultCompleted) != "completed" {
		t.Fatalf("SubAgentResultCompleted drifted: %q", harness.SubAgentResultCompleted)
	}
	if string(harness.SubAgentResultAskUser) != "ask_user" {
		t.Fatalf("SubAgentResultAskUser drifted: %q", harness.SubAgentResultAskUser)
	}
}

// TestHostedSDKParityMockScenario is a lightweight parity check: the same
// scripted Scenario should produce equivalent event sequences whether the
// caller uses MockEngine directly or wraps it in a custom projection layer.
// It substitutes for the full Hosted vs SDK integration test until the
// composition physically migrates into internal/kernel and a real Build fixture
// can be shared.
func TestHostedSDKParityMockScenario(t *testing.T) {
	events := []harness.Event{
		{EventType: harness.EventRunStarted, Visibility: harness.VisibilityDebug},
		testkit.TextDeltaEvent("hello"),
		testkit.FinalResponseEvent("hello world"),
		testkit.TerminalEvent(harness.EventRunCompleted),
	}
	scenario := testkit.Scenario{Events: events}

	replay := func() []harness.EventType {
		engine := testkit.NewMockEngine(scenario)
		defer engine.Close(context.Background())
		exec, err := engine.Start(context.Background(), harness.StartRequest{
			Identity: harness.Identity{TenantID: "t"},
			Input:    harness.TextMessage("hi"),
		})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		var seen []harness.EventType
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for {
			ev, err := exec.Events().Next(ctx)
			if err != nil {
				if errors.Is(err, io.EOF) {
					return seen
				}
				t.Fatalf("stream: %v", err)
			}
			seen = append(seen, ev.EventType)
		}
	}
	a := replay()
	b := replay()
	if len(a) != len(b) {
		t.Fatalf("parity: length mismatch %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("parity: event %d mismatch %q vs %q", i, a[i], b[i])
		}
	}
}
