package edit

import (
	"strings"
	"testing"
)

func TestBuildContractFreezesResolvedDesignEdit(t *testing.T) {
	resolved := ResolvedTarget{
		DesignRevision: "design_abc",
		Query:          "把标题改成红色",
		Target: Element{
			ElementID: "food.title.primary", ComponentID: "title",
			ComponentType: "Text", ContractItemID: "food.title",
			EditablePaths: []string{"text", "styles.color"},
		},
	}
	got, err := BuildContract(BuildInput{
		TenantID: "tenant-1", UserID: "user-1", SessionID: "session-1",
		RunID: "run-2", BaseGenerationID: "gen-1", BaseCardRevision: 1,
		ContentContractID: "contract-1", ContentContractHash: "sha256:contract",
		BaseDesignHash: "sha256:design", SlotSignatureHash: "sha256:slots",
		ChangeScope: "design_update", Operation: "update",
		Proposals: []TargetProposal{{Resolved: resolved, RequestedChanges: map[string]any{"styles.color": "#FF0000"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != SchemaVersion || got.EditID == "" || got.IdempotencyKey == "" {
		t.Fatalf("identity fields missing: %#v", got)
	}
	if len(got.TargetSet) != 1 || got.TargetSet[0].ID != "food.title.primary" ||
		got.TargetSet[0].ComponentID != "title" || got.TargetSet[0].Path != "styles.color" {
		t.Fatalf("target_set = %#v", got.TargetSet)
	}
	if len(got.ProtectedSet) != 2 || len(got.ImpactSet) != 2 ||
		got.Preconditions.ContentContractHash != "sha256:contract" ||
		got.Preconditions.DesignHash != "sha256:design" ||
		got.Preconditions.SlotSignatureHash != "sha256:slots" {
		t.Fatalf("host-derived guardrails missing: %#v", got)
	}
}

func TestBuildContractRejectsPathOutsideTargetIndex(t *testing.T) {
	base := BuildInput{
		TenantID: "tenant-1", UserID: "user-1", SessionID: "session-1",
		RunID: "run-2", BaseGenerationID: "gen-1", BaseCardRevision: 1,
		ContentContractID: "contract-1", ContentContractHash: "sha256:contract",
		BaseDesignHash: "sha256:design", SlotSignatureHash: "sha256:slots",
		ChangeScope: "design_update", Operation: "update",
		Proposals: []TargetProposal{{Resolved: ResolvedTarget{DesignRevision: "design_abc", Query: "把标题改成红色", Target: Element{
			ElementID: "food.title.primary", ComponentID: "title", ComponentType: "Text",
			EditablePaths: []string{"text", "styles.color"},
		}}}},
	}
	base.Proposals[0].RequestedChanges = map[string]any{"styles.opacity": "0.8"}
	if _, err := BuildContract(base); err == nil || !strings.Contains(err.Error(), "not an editable property") {
		t.Fatalf("unindexed path error = %v", err)
	}
	base.Proposals[0].RequestedChanges = map[string]any{"styles.color": "#FF0000"}
	base.Proposals[0].Resolved.DesignRevision = ""
	if _, err := BuildContract(base); err == nil || !strings.Contains(err.Error(), "resolved") {
		t.Fatalf("unresolved target error = %v", err)
	}
}

func TestBuildContractAcceptsIndexedPathsWithoutKeywordRecognition(t *testing.T) {
	for _, test := range []struct {
		name  string
		query string
		path  string
		value string
	}{
		{name: "resolved relative font size", query: "标题调小点", path: "styles.font-size", value: "16px"},
		{name: "visible text property", query: "标题改成欢迎回来", path: "text", value: "欢迎回来"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := BuildContract(BuildInput{
				TenantID: "tenant-1", UserID: "user-1", SessionID: "session-1",
				RunID: "run-2", BaseGenerationID: "gen-1", BaseCardRevision: 1,
				ContentContractID: "contract-1", ContentContractHash: "sha256:contract",
				BaseDesignHash: "sha256:design", SlotSignatureHash: "sha256:slots",
				ChangeScope: "design_update", Operation: "update",
				Proposals: []TargetProposal{{RequestedChanges: map[string]any{test.path: test.value}, Resolved: ResolvedTarget{
					DesignRevision: "design_abc", Query: test.query,
					Target: Element{
						ElementID: "food.title.primary", ComponentID: "title", ComponentType: "Text",
						ContractItemID: "food.title", EditablePaths: []string{test.path},
					},
				}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.TargetSet) != 1 || got.TargetSet[0].Path != test.path || got.TargetSet[0].Value != test.value {
				t.Fatalf("contract = %#v", got)
			}
		})
	}
}

func TestEffectiveRequestedChangesDropsCurrentFactsOnly(t *testing.T) {
	target := Element{CurrentStyles: map[string]any{
		"color":  "#003366",
		"border": map[string]any{"width": 1},
	}}
	got := EffectiveRequestedChanges(target, map[string]any{
		"styles.color":        "#003366",
		"styles.fontSize":     28,
		"styles.border.width": 1,
	})
	if len(got) != 1 || got["styles.fontSize"] != 28 {
		t.Fatalf("effective changes = %#v", got)
	}
}

func TestBuildContractFreezesMultipleTargetsWithIndependentValues(t *testing.T) {
	makeTarget := func(id string) ResolvedTarget {
		return ResolvedTarget{
			DesignRevision: "design_abc", Query: id,
			Target: Element{ElementID: id, ComponentID: id, ComponentType: "Text", EditablePaths: []string{"styles.color", "styles.font-weight"}},
		}
	}
	got, err := BuildContract(BuildInput{
		TenantID: "tenant", UserID: "user", SessionID: "session", RunID: "run",
		BaseGenerationID: "base", BaseCardRevision: 1, ContentContractID: "contract",
		ContentContractHash: "sha256:contract", BaseDesignHash: "sha256:design", SlotSignatureHash: "sha256:slots",
		ChangeScope: "design_update", Operation: "update",
		Proposals: []TargetProposal{
			{Resolved: makeTarget("reserveText"), RequestedChanges: map[string]any{"styles.color": "#FFFFFF", "styles.font-weight": 700}},
			{Resolved: makeTarget("moreText"), RequestedChanges: map[string]any{"styles.color": "#EEEEEE", "styles.font-weight": 600}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.TargetSet) != 4 {
		t.Fatalf("target_set = %#v", got.TargetSet)
	}
	values := map[string]any{}
	for _, target := range got.TargetSet {
		values[target.ComponentID+"."+target.Path] = target.Value
	}
	if values["reserveText.styles.color"] != "#FFFFFF" || values["moreText.styles.color"] != "#EEEEEE" {
		t.Fatalf("independent values lost: %#v", values)
	}
}

func TestBuildContractFreezesMixedDeclarativeUpsert(t *testing.T) {
	got, err := BuildContract(BuildInput{
		TenantID: "tenant", UserID: "user", SessionID: "session", RunID: "run",
		BaseGenerationID: "base", BaseCardRevision: 1, ContentContractID: "contract",
		ContentContractHash: "sha256:contract", BaseDesignHash: "sha256:design", SlotSignatureHash: "sha256:slots",
		ChangeScope: "design_update", Operation: "upsert",
		Proposals: []TargetProposal{
			{Resolved: ResolvedTarget{DesignRevision: "design", Query: "title", Target: Element{
				ElementID: "title", ComponentID: "title", ComponentType: "Text", EditablePaths: []string{"styles.color"},
			}}, RequestedChanges: map[string]any{"styles.color": "#ff0000"}},
			{Insert: &InsertProposal{
				Component: map[string]any{"id": "badge", "component": "Text", "text": "新品"},
				ParentID:  "root", AfterID: "title",
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Operation != "upsert" || len(got.TargetSet) != 2 {
		t.Fatalf("contract = %#v", got)
	}
	var inserted Target
	for _, target := range got.TargetSet {
		if target.Kind == "design_component_insert" {
			inserted = target
		}
	}
	if inserted.ComponentID != "badge" || inserted.ParentID != "root" || inserted.AfterID != "title" ||
		inserted.Component["component"] != "Text" {
		t.Fatalf("insert target = %#v", inserted)
	}
	if len(got.ImpactSet) != 3 || got.ImpactSet[0].Kind != "component:root" {
		t.Fatalf("impact set = %#v", got.ImpactSet)
	}
}
