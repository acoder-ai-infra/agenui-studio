package contract

import "testing"

func TestValidateSourceFactsRejectsTargetPathUsedAsSourcePath(t *testing.T) {
	input := Input{Sources: []SourceSnapshot{{
		SourceID: "demo.products@v1", KnowledgeID: "demo-v1@sha256:test",
		Primary: true, Paths: []string{"$.title", "$.detail_url"},
	}}}
	result := Result{Bindings: []Binding{{
		RequirementID: "product_title", SourceID: "demo.products@v1",
		KnowledgeID: "demo-v1@sha256:test", FieldPath: "/product_title",
		RefKey: "/product_title",
	}}}
	if err := ValidateSourceFacts(input, result); err == nil {
		t.Fatal("ValidateSourceFacts() accepted a DSL target path as a source field")
	}
	result.Bindings[0].FieldPath = "$.title"
	if err := ValidateSourceFacts(input, result); err != nil {
		t.Fatalf("ValidateSourceFacts() rejected an authorized field: %v", err)
	}
}

func TestValidateSourceFactsRejectsSingleObjectForListTarget(t *testing.T) {
	input := Input{Sources: []SourceSnapshot{{
		SourceID: "demo.products@v1", KnowledgeID: "demo-v1@sha256:test",
		Primary: true, Paths: []string{"$.title"},
	}}}
	result := Result{Bindings: []Binding{{
		RequirementID: "product_title", SourceID: "demo.products@v1",
		KnowledgeID: "demo-v1@sha256:test", FieldPath: "$.title",
		RefKey: "/items[*]/title",
	}}}
	if err := ValidateSourceFacts(input, result); err == nil {
		t.Fatal("ValidateSourceFacts() accepted a single-object source for a list target")
	}
}
