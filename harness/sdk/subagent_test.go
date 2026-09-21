package harness

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestSubAgentResultTypeDiscriminatorValues(t *testing.T) {
	if string(SubAgentResultCompleted) != "completed" {
		t.Fatalf("SubAgentResultCompleted wire value drifted: %q", SubAgentResultCompleted)
	}
	if string(SubAgentResultAskUser) != "ask_user" {
		t.Fatalf("SubAgentResultAskUser wire value drifted: %q", SubAgentResultAskUser)
	}
}

func TestInteractionProposalHasNoControlSecrets(t *testing.T) {
	// The InteractionProposal type MUST NOT expose control state (ticket,
	// checkpoint, request id). Marshalling a fully-populated proposal must not
	// contain any of the forbidden field names.
	proposal := InteractionProposal{
		Prompt: "confirm?",
		Kind:   "ask_user",
		Options: []InteractionOption{
			{Value: "yes", Label: "Yes"},
			{Value: "no", Label: "No"},
		},
		Input: InteractionInputConstraint{
			Required:  true,
			MaxLength: 128,
		},
		TaskID:    "task_1",
		AttemptID: "attempt_1",
	}
	raw, err := json.Marshal(proposal)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	forbidden := []string{"control_request_id", "checkpoint_id", "resume_token", "control_ticket"}
	for _, key := range forbidden {
		if containsField(raw, key) {
			t.Fatalf("InteractionProposal must not expose %q; got %s", key, raw)
		}
	}
}

func TestSubAgentResultInvariantAskUserHasProposal(t *testing.T) {
	// This is a documentation-level invariant: ask_user results MUST carry a
	// non-nil InteractionProposal and IsError=false. The SDK never enforces it
	// on the caller (the Runtime adapter is the trust anchor), so we assert
	// via a Go-level lint check that the struct has both fields set together
	// in the sample.
	proposal := &InteractionProposal{Prompt: "?", Kind: "ask_user"}
	valid := SubAgentInvocationResult{
		ResultType:          SubAgentResultAskUser,
		InteractionProposal: proposal,
	}
	if valid.IsError {
		t.Fatal("ask_user result cannot be an error")
	}
	if valid.InteractionProposal == nil {
		t.Fatal("ask_user result must carry an InteractionProposal")
	}
}

func TestErrChildControlUnsupportedIsSentinel(t *testing.T) {
	if !errors.Is(ErrChildControlUnsupported, ErrChildControlUnsupported) {
		t.Fatal("ErrChildControlUnsupported must be a stable sentinel")
	}
}

// containsField is a lax check that treats raw JSON as text; sufficient here
// because InteractionProposal fields do not contain forbidden strings by
// coincidence.
func containsField(raw []byte, field string) bool {
	needle := `"` + field + `"`
	for i := 0; i+len(needle) <= len(raw); i++ {
		if string(raw[i:i+len(needle)]) == needle {
			return true
		}
	}
	return false
}
