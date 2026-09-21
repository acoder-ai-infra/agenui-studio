package edit

import (
	"errors"
	"testing"
)

func TestCandidatesExposeRealTreeFactsWithoutSemanticMatching(t *testing.T) {
	index, err := BuildIndex("design@1", `[
      {"updateComponents":{"components":[
        {"id":"root","component":"Column","children":["confirm","confirm-label"]},
        {"id":"confirm","component":"Button","child":"confirm-label"},
        {"id":"confirm-label","component":"Text","text":"确认提交"}
      ]}}
    ]`, `[]`, `[{"slotId":"product.confirm.primary","componentId":"confirm","role":"primary_action","description":"确认提交","contractActionId":"confirm_submit"}]`)
	if err != nil {
		t.Fatal(err)
	}
	candidates := Candidates(index)
	if len(candidates) != 3 {
		t.Fatalf("candidates = %#v", candidates)
	}
	var label Element
	for _, candidate := range candidates {
		if candidate.ComponentID == "confirm-label" {
			label = candidate
		}
	}
	if label.Relation != "action_label" || label.ContractActionID != "confirm_submit" || len(label.Descriptions) == 0 {
		t.Fatalf("label facts = %#v", label)
	}
}

func TestSelectAcceptsOnlyExactRealTargetID(t *testing.T) {
	index := Index{Revision: "r1", Elements: []Element{
		{ElementID: "product.detail.primary", ComponentID: "detail", ComponentType: "Button"},
		{ElementID: "label", ComponentID: "detail-label", ComponentType: "Text"},
	}}
	got, err := Select(index, "product.detail.primary")
	if err != nil || got.ComponentID != "detail" {
		t.Fatalf("selected=%#v err=%v", got, err)
	}
	got, err = Select(index, "detail-label")
	if err != nil || got.ElementID != "label" {
		t.Fatalf("selected=%#v err=%v", got, err)
	}
	if _, err := Select(index, "invented"); !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("invented target error = %v", err)
	}
}

func TestSelectRejectsAmbiguousExactID(t *testing.T) {
	index := Index{Revision: "r1", Elements: []Element{
		{ElementID: "shared", ComponentID: "first"},
		{ElementID: "second", ComponentID: "shared"},
	}}
	if _, err := Select(index, "shared"); !errors.Is(err, ErrTargetAmbiguous) {
		t.Fatalf("ambiguous target error = %v", err)
	}
}
