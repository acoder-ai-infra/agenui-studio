package agentbinding

import (
	"errors"
	"testing"
)

func TestPayloadsAreTypedAndIsolated(t *testing.T) {
	t.Parallel()
	binding := validEffectiveBinding()
	binding.BindingHash, _ = ComputeBindingHash(binding)
	success := NewSuccessPayload(Result{Binding: binding})
	binding.CapabilitySnapshotRefs[0] = "changed"
	if success.Binding.CapabilitySnapshotRefs[0] != "agent-config://agent-a/v1/hash#capabilities" {
		t.Fatal("success payload shares capability refs")
	}

	req := baseRequest(Selection{})
	req.Request = nil
	err := NewAskUserError(StageSourceSelection, "execution_mode", "agent_id")
	failure := NewFailurePayload(req, err)
	if failure.Code != CodeAskUserRequired || failure.Clarification == nil || failure.Clarification.MissingFields[0] != "agent_id" {
		t.Fatalf("failure payload = %#v", failure)
	}
	err.Clarification.MissingFields[0] = "changed"
	if failure.Clarification.MissingFields[0] != "agent_id" {
		t.Fatal("failure payload shares control request fields")
	}
	if !errors.Is(err, ErrAskUserRequired) {
		t.Fatal("AskUserError does not support errors.Is")
	}
}
