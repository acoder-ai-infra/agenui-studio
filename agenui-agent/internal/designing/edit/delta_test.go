package edit

import (
	"errors"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/workspace"
)

var baseDesign = typedTestDesign()

func typedTestDesign() string {
	raw, err := workspace.EncodeDesignArtifact(workspace.Document{
		SchemaVersion: workspace.SchemaVersion, SurfaceID: "default", CatalogID: "agenui.org_catalog_0_9", RootID: "root",
		Components: []map[string]any{
			{"id": "root", "component": "Column", "children": []any{"title", "price"}},
			{"id": "title", "component": "Text", "text": map[string]any{"path": "/title"}, "styles": map[string]any{"color": "#222222", "font-size": "18px"}},
			{"id": "price", "component": "Text", "text": map[string]any{"path": "/price"}, "styles": map[string]any{"color": "#FF6600"}},
		},
		DataModel: map[string]any{"title": "示例标题", "price": "88"},
		FieldSlots: []map[string]any{
			{"slotId": "food.title.primary", "componentId": "title", "role": "title", "description": "标题", "contractItemId": "food.title"},
			{"slotId": "food.price.primary", "componentId": "price", "role": "price", "description": "价格", "contractItemId": "food.price"},
		}, ActionSlots: []map[string]any{},
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func styleEditContract() Contract {
	return Contract{
		SchemaVersion: SchemaVersion, EditID: "edit-1", BaseGenerationID: "gen-1",
		BaseCardRevision: 1, ChangeScope: "design_update", Operation: "update",
		TargetSet:      []Target{{Kind: "design_slot", ID: "food.title.primary", ComponentID: "title", Path: "styles.color", Value: "#FF0000"}},
		ProtectedSet:   []Protection{{Kind: "all_other_design_slots", ID: "*"}},
		ImpactSet:      []Impact{{Kind: "design", Action: "regenerate_partial"}},
		Preconditions:  Preconditions{DesignHash: HashText(baseDesign)},
		Acceptance:     Acceptance{TargetChanged: true, ProtectedObjectsUnchanged: true, RuntimePreviewRequired: true},
		IdempotencyKey: "edit-key",
	}
}

func TestApplyAuthorizedDeltaCommitsOnlyRequestedStylePath(t *testing.T) {
	candidate := strings.Replace(baseDesign, `"color":"#222222"`, `"color":"#FF0000"`, 1)
	got, report, err := ApplyAuthorizedDelta(baseDesign, candidate, styleEditContract())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"color":"#FF0000"`) ||
		!strings.Contains(got, `"id":"price"`) || len(report.AppliedPaths) != 1 ||
		report.AppliedPaths[0] != "title.styles.color" {
		t.Fatalf("merged=%s report=%#v", got, report)
	}
}

func TestApplyAuthorizedDeltaCanAddAuthorizedStylePath(t *testing.T) {
	base := strings.Replace(baseDesign, `,"styles":{"color":"#222222","font-size":"18px"}`, "", 1)
	contract := styleEditContract()
	contract.Preconditions.DesignHash = HashText(base)
	candidate := strings.Replace(base, `"text":{"path":"/title"}`, `"text":{"path":"/title"},"styles":{"color":"#FF0000"}`, 1)

	got, report, err := ApplyAuthorizedDelta(base, candidate, contract)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"styles":{"color":"#FF0000"}`) ||
		len(report.AppliedPaths) != 1 || report.AppliedPaths[0] != "title.styles.color" {
		t.Fatalf("merged=%s report=%#v", got, report)
	}
}

func TestApplyAuthorizedDeltaDiscardsNonTargetDriftAndRejectsEmptyEdit(t *testing.T) {
	contract := styleEditContract()
	drift := strings.Replace(baseDesign, `"color":"#222222"`, `"color":"#FF0000"`, 1)
	drift = strings.Replace(drift, `"color":"#FF6600"`, `"color":"#00FF00"`, 1)
	got, report, err := ApplyAuthorizedDelta(baseDesign, drift, contract)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"color":"#FF6600"`) || strings.Contains(got, `"color":"#00FF00"`) ||
		len(report.AppliedPaths) != 1 {
		t.Fatalf("non-target drift was not discarded: merged=%s report=%#v", got, report)
	}
	if _, _, err := ApplyAuthorizedDelta(baseDesign, baseDesign, contract); !errors.Is(err, ErrEditNoChange) {
		t.Fatalf("empty edit error = %v", err)
	}
}

func TestApplyAuthorizedDeltaRejectsValueDifferentFromRequestedChange(t *testing.T) {
	contract := styleEditContract()
	candidate := strings.Replace(baseDesign, `"color":"#222222"`, `"color":"#0000FF"`, 1)
	if _, _, err := ApplyAuthorizedDelta(baseDesign, candidate, contract); !errors.Is(err, ErrEditScopeViolation) ||
		!strings.Contains(err.Error(), "does not match requested_changes") {
		t.Fatalf("requested value mismatch error = %v", err)
	}
}

func TestApplyAuthorizedDeltaAcceptsModelResolvedRelativeChange(t *testing.T) {
	contract := styleEditContract()
	contract.TargetSet[0].Path = "styles.font-size"
	contract.TargetSet[0].Value = "16px"
	candidate := strings.Replace(baseDesign, `"font-size":"18px"`, `"font-size":"16px"`, 1)

	got, report, err := ApplyAuthorizedDelta(baseDesign, candidate, contract)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"font-size":"16px"`) || len(report.AppliedPaths) != 1 ||
		report.AppliedPaths[0] != "title.styles.font-size" {
		t.Fatalf("merged=%s report=%#v", got, report)
	}
}

func TestApplyAuthorizedDeltaKeepsConcreteStyleValuesExact(t *testing.T) {
	contract := styleEditContract()
	contract.TargetSet[0].Path = "styles.font-size"
	contract.TargetSet[0].Value = "16px"
	candidate := strings.Replace(baseDesign, `"font-size":"18px"`, `"font-size":"14px"`, 1)

	if _, _, err := ApplyAuthorizedDelta(baseDesign, candidate, contract); !errors.Is(err, ErrEditScopeViolation) ||
		!strings.Contains(err.Error(), "does not match requested_changes") {
		t.Fatalf("concrete requested value mismatch error = %v", err)
	}
}

func TestApplyAuthorizedDeltaDiscardsCandidateSemanticSidecars(t *testing.T) {
	contract := styleEditContract()
	candidate := strings.Replace(baseDesign, `"color":"#222222"`, `"color":"#FF0000"`, 1)
	candidate = strings.Replace(candidate, `"description":"标题"`, `"description":"模型重写的标题槽位"`, 1)
	candidate = strings.Replace(candidate, `"action_slots":[]`, `"action_slots":[{"slotId":"invented"}]`, 1)

	got, _, err := ApplyAuthorizedDelta(baseDesign, candidate, contract)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"color":"#FF0000"`) ||
		!strings.Contains(got, `"description":"标题"`) ||
		strings.Contains(got, "模型重写的标题槽位") || strings.Contains(got, "invented") {
		t.Fatalf("candidate sidecars were not replaced by the base sidecars: %s", got)
	}
}

func TestApplyAuthorizedDeltaRequiresTypedArtifact(t *testing.T) {
	contract := styleEditContract()
	candidate := strings.Replace(baseDesign, `"color":"#222222"`, `"color":"#FF0000"`, 1)
	candidate = strings.Replace(candidate, `"schema_version":"agenui.design-artifact/v1"`, `"schema_version":"invalid"`, 1)

	if _, _, err := ApplyAuthorizedDelta(baseDesign, candidate, contract); !errors.Is(err, ErrEditScopeViolation) ||
		!strings.Contains(err.Error(), "invalid AGenUI candidate") {
		t.Fatalf("typed artifact error = %v", err)
	}
}

func TestApplyAuthorizedDeltaRejectsStaleBase(t *testing.T) {
	contract := styleEditContract()
	contract.Preconditions.DesignHash = "sha256:stale"
	candidate := strings.Replace(baseDesign, `"color":"#222222"`, `"color":"#FF0000"`, 1)
	if _, _, err := ApplyAuthorizedDelta(baseDesign, candidate, contract); !errors.Is(err, ErrBaseRevisionConflict) {
		t.Fatalf("stale base error = %v", err)
	}
}
