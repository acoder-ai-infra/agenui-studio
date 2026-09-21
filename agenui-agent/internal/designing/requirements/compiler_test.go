package requirements

import (
	"testing"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/contract"
)

func TestCompileRequiresKnownContractItems(t *testing.T) {
	draft := contract.Draft{Goal: "example", Type: "single", Contents: []contract.ContentItem{{ID: "title", Required: true}}}
	if _, err := Compile(Input{Contract: draft, FieldSlots: []FieldSlot{{SlotID: "title", ContractItemID: "missing", ValueType: "string", Scope: "card"}}}); err == nil {
		t.Fatal("unknown content reference unexpectedly compiled")
	}
}

func TestCompilePreservesApplicationSemanticRoles(t *testing.T) {
	draft := contract.Draft{Goal: "example", Type: "single", Contents: []contract.ContentItem{{ID: "price", Description: "price", Required: true}}}
	result, err := Compile(Input{Contract: draft, FieldSlots: []FieldSlot{{SlotID: "leading", ContractItemID: "price", Role: "application_defined_tag", ValueType: "string", Scope: "card"}}})
	if err != nil {
		t.Fatalf("application semantic role must not be rejected: %v", err)
	}
	if len(result.RequirementSet.Data) != 1 {
		t.Fatalf("compiled data = %#v", result.RequirementSet.Data)
	}
}

func TestCompileAcceptsOpenSemanticMetadataAndSafeDefaults(t *testing.T) {
	draft := contract.Draft{
		Goal: "example", Type: "single",
		Contents: []contract.ContentItem{{ID: "title", Description: "title", Required: true}},
		Actions:  []contract.ActionItem{{ID: "open", Description: "open details"}},
	}
	result, err := Compile(Input{
		Contract: draft,
		FieldSlots: []FieldSlot{
			{SlotID: "title", ContractItemID: "title", Scope: "surface"},
		},
		ActionSlots: []ActionSlot{
			{SlotID: "open", ContractActionID: "open"},
		},
	})
	if err != nil {
		t.Fatalf("open semantic metadata must compile: %v", err)
	}
	if got := result.RequirementSet.Data[0]; got.Type != "string" || got.Shape != "single" {
		t.Fatalf("default data requirement = %#v", got)
	}
	if result.RequirementSet.InputHash == "" {
		t.Fatal("deterministic input hash is missing")
	}
}

func TestCompileDoesNotOwnApplicationValueTypeVocabulary(t *testing.T) {
	draft := contract.Draft{Goal: "example", Type: "single", Contents: []contract.ContentItem{{ID: "status", Description: "status", Required: true}}}
	result, err := Compile(Input{Contract: draft, FieldSlots: []FieldSlot{{
		SlotID: "status", ContractItemID: "status", ValueType: "domain-status", Scope: "application-section",
	}}})
	if err != nil {
		t.Fatalf("application value type must not be rejected: %v", err)
	}
	if got := result.RequirementSet.Data[0].Type; got != "domain-status" {
		t.Fatalf("value type = %q", got)
	}
}

func TestCompileDerivesListShapeFromCanonicalDSLPath(t *testing.T) {
	required := true
	draft := contract.Draft{
		Goal: "show items", Type: "list",
		Contents: []contract.ContentItem{{ID: "items", Description: "items", Required: true}},
	}
	result, err := Compile(Input{Contract: draft, FieldSlots: []FieldSlot{{
		SlotID: "item.title", ContractItemID: "items", RefKey: "/items[*]/title",
		ValueType: "string", Scope: "/items[*]", Required: &required,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.RequirementSet.Data[0].Shape; got != "list_item" {
		t.Fatalf("shape = %q", got)
	}
}
