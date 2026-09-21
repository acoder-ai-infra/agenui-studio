package documentcontract

import (
	"bytes"
	"testing"
)

func TestRuleDocumentRoundTripIsDeterministic(t *testing.T) {
	document := Document{DocumentSchema: RuleSchema, ID: "rule.layout.summary-card", Version: "1.0.0", Kind: "rule", Title: "摘要卡原子规则", Summary: "摘要卡规则", Rules: []AtomicRule{{ID: "summary-card.title", Target: "summary-card.content", Strength: StrengthRequired, Effect: "标题优先", SourceTrace: []string{"design-owner"}}}}
	first, err := RoundTrip(document)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RoundTrip(document)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("canonical markdown is not deterministic")
	}
}

func TestLayoutRejectsMissingParent(t *testing.T) {
	document := Document{DocumentSchema: LayoutSchema, ID: "layout.summary-card.test", Version: "1.0.0", Kind: "layout", Title: "Test", Summary: "test", Layout: &LayoutBody{Nodes: []LayoutNode{{ID: "child", ParentID: "missing", Type: "region"}}}}
	if _, err := Render(document); err == nil {
		t.Fatal("expected missing parent rejection")
	}
}

func TestRuleRejectsMissingEffect(t *testing.T) {
	document := Document{DocumentSchema: RuleSchema, ID: "rule.renderer", Version: "1.0.0", Kind: "rule", Title: "Renderer", Summary: "renderer", Rules: []AtomicRule{{ID: "R.RENDER", Target: "renderer", Strength: StrengthRequired, SourceTrace: []string{"renderer"}}}}
	if _, err := Render(document); err == nil {
		t.Fatal("expected empty effect to be rejected")
	}
}

func TestParserRejectsUnknownFields(t *testing.T) {
	raw := []byte("# x\n\n<!-- agenui-document-contract\n{\"documentSchema\":\"atomic_rule_doc.v1\",\"id\":\"r\",\"version\":\"1\",\"kind\":\"rule\",\"title\":\"t\",\"summary\":\"s\",\"rules\":[],\"unknown\":true}\n-->\n")
	if _, err := Parse(raw); err == nil {
		t.Fatal("expected unknown field rejection")
	}
}
