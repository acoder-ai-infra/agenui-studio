package contract

import (
	"encoding/json"
	"testing"
)

func TestEncodeExecutablePlanCanonicalizesAbsentMappingsToArrays(t *testing.T) {
	encoded, err := EncodeExecutablePlan(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := ParseExecutablePlan(encoded)
	if err != nil {
		t.Fatalf("canonical empty plan was rejected: %v; plan=%s", err, encoded)
	}
	if plan.FieldMappings == nil || plan.ActionMappings == nil {
		t.Fatalf("empty mappings must be arrays: %#v", plan)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal([]byte(encoded), &wire); err != nil {
		t.Fatal(err)
	}
	if string(wire["field_mappings"]) != "[]" || string(wire["action_mappings"]) != "[]" {
		t.Fatalf("non-canonical empty plan: %s", encoded)
	}
}
