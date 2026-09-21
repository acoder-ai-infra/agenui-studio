package workspace

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func sampleDocument() Document {
	return Document{
		SchemaVersion: SchemaVersion,
		SurfaceID:     "default",
		CatalogID:     "https://example.test/catalog.json",
		RootID:        "root",
		Components: []map[string]any{
			{"id": "root", "component": "Column", "children": []any{"title", "cta"}},
			{"id": "title", "component": "Text", "text": map[string]any{"path": "/title"}},
			{"id": "cta", "component": "Button", "child": "cta_label"},
			{"id": "cta_label", "component": "Text", "text": map[string]any{"path": "/cta"}},
		},
		DataModel: map[string]any{"title": "旧标题", "cta": "确认"},
		FieldSlots: []map[string]any{
			{"id": "title.text", "componentId": "title", "refKey": "/title"},
		},
	}
}

func TestMessagesUseCanonicalNestedV09Protocol(t *testing.T) {
	document := sampleDocument()
	encoded, err := document.MarshalMessages()
	if err != nil {
		t.Fatal(err)
	}
	var messages []map[string]any
	if err := json.Unmarshal([]byte(encoded), &messages); err != nil {
		t.Fatal(err)
	}
	if _, exists := messages[0]["type"]; exists {
		t.Fatalf("flat type protocol leaked into canonical messages: %s", encoded)
	}
	create, ok := messages[0]["createSurface"].(map[string]any)
	if !ok || create["surfaceId"] != "default" {
		t.Fatalf("createSurface = %#v", messages[0])
	}
	update, ok := messages[1]["updateComponents"].(map[string]any)
	if !ok || len(update["components"].([]any)) != 4 {
		t.Fatalf("updateComponents = %#v", messages[1])
	}
}

func TestDocumentRejectsNonProtocolRootID(t *testing.T) {
	document := sampleDocument()
	document.RootID = "product-card"
	document.Components[0]["id"] = "product-card"

	if err := document.ValidateDraft(); err == nil || !strings.Contains(err.Error(), `root_id must be "root"`) {
		t.Fatalf("ValidateDraft() error = %v", err)
	}
	if err := document.ValidateStructural(); err == nil || !strings.Contains(err.Error(), `root_id must be "root"`) {
		t.Fatalf("ValidateStructural() error = %v", err)
	}
}

func TestNormalizeRuntimeDataModelConvertsJSONSchemaWithoutInventingFields(t *testing.T) {
	document := sampleDocument()
	document.DataModel = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title":     map[string]any{"type": "string"},
			"rating":    map[string]any{"type": "number"},
			"available": map[string]any{"type": "boolean"},
			"items":     map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
		},
	}
	if !document.NormalizeRuntimeDataModel() {
		t.Fatal("JSON Schema shaped model was not normalized")
	}
	want := map[string]any{"title": "", "rating": 0, "available": false, "items": []any{}}
	if !reflect.DeepEqual(document.DataModel, want) {
		t.Fatalf("data model = %#v, want %#v", document.DataModel, want)
	}
	if document.NormalizeRuntimeDataModel() {
		t.Fatal("runtime value model must not be normalized twice")
	}
}

func TestCanonicalizeSlotIDsAllowsRepeatedComponentLocalNames(t *testing.T) {
	document := sampleDocument()
	document.Components = append(document.Components,
		map[string]any{"id": "subtitle", "component": "Text", "text": "副标题"},
	)
	document.FieldSlots = []map[string]any{
		{"componentId": "title", "slotId": "text", "refKey": "/title"},
		{"componentId": "subtitle", "slotId": "text", "refKey": "/subtitle"},
	}
	document.ActionSlots = []map[string]any{
		{"componentId": "cta", "slotId": "action"},
	}
	document.CanonicalizeSlotIDs()
	if got := slotID(document.FieldSlots[0]); got != "title.text" {
		t.Fatalf("first slot = %q", got)
	}
	if got := slotID(document.FieldSlots[1]); got != "subtitle.text" {
		t.Fatalf("second slot = %q", got)
	}
	if got := slotID(document.ActionSlots[0]); got != "cta.action" {
		t.Fatalf("action slot = %q", got)
	}
	if err := document.ValidateStructural(); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeSlotIdentitiesDistinguishesActionsOnOneComponent(t *testing.T) {
	document := Document{ActionSlots: []map[string]any{
		{"componentId": "actionButton", "contractActionId": "purchase_ticket", "role": "action", "scope": "item"},
		{"componentId": "actionButton", "contractActionId": "reserve_ticket", "role": "action", "scope": "item"},
	}}
	if err := document.NormalizeSlotIdentities(); err != nil {
		t.Fatal(err)
	}
	if len(document.ActionSlots) != 2 {
		t.Fatalf("action slots = %#v", document.ActionSlots)
	}
	first := slotID(document.ActionSlots[0])
	second := slotID(document.ActionSlots[1])
	if first == "" || second == "" || first == second {
		t.Fatalf("Host identities are not unique: %q %q", first, second)
	}
}

func TestValidateStructuralRequiresExplicitAbsoluteFieldRef(t *testing.T) {
	document := sampleDocument()
	document.FieldSlots[0]["refKey"] = "text"
	if err := document.ValidateStructural(); err == nil || !strings.Contains(err.Error(), "absolute data-model refKey") {
		t.Fatalf("field ref error = %v", err)
	}
}

func TestValidateStructuralRejectsMissingFormatStringPreviewPath(t *testing.T) {
	document := sampleDocument()
	document.Components[1]["text"] = map[string]any{
		"call": "formatString",
		"args": map[string]any{"value": "评分 ${rating} · ${title}"},
	}
	if err := document.ValidateStructural(); err == nil || !strings.Contains(err.Error(), `missing preview path "rating"`) {
		t.Fatalf("formatString validation error = %v", err)
	}
}

func TestValidateStructuralAcceptsFormatStringPreviewPathInsideList(t *testing.T) {
	document := sampleDocument()
	document.DataModel = map[string]any{"items": []any{map[string]any{"rating": 4.8, "title": "示例"}}}
	document.Components[1]["text"] = map[string]any{
		"call": "formatString",
		"args": map[string]any{"value": "评分 ${rating}"},
	}
	if err := document.ValidateStructural(); err != nil {
		t.Fatalf("formatString list validation error = %v", err)
	}
}

func TestNormalizeSlotIdentitiesCollapsesExactDuplicatesAndRejectsConflicts(t *testing.T) {
	exact := map[string]any{
		"componentId": "title", "contractItemId": "ticket_title", "refKey": "/items[*]/title",
		"role": "title", "valueType": "string", "scope": "item",
	}
	document := Document{FieldSlots: []map[string]any{cloneMap(exact), cloneMap(exact)}}
	if err := document.NormalizeSlotIdentities(); err != nil {
		t.Fatal(err)
	}
	if len(document.FieldSlots) != 1 {
		t.Fatalf("exact duplicates were not collapsed: %#v", document.FieldSlots)
	}
	conflict := cloneMap(exact)
	conflict["description"] = "conflicting declaration"
	document = Document{FieldSlots: []map[string]any{cloneMap(exact), conflict}}
	if err := document.NormalizeSlotIdentities(); err == nil || !strings.Contains(err.Error(), "conflicting field slot identity") {
		t.Fatalf("conflicting identity error = %v", err)
	}
}

func TestValidateStructuralRejectsSlotForUnknownComponent(t *testing.T) {
	document := Document{
		SchemaVersion: SchemaVersion,
		SurfaceID:     "default",
		CatalogID:     "catalog:test",
		RootID:        "root",
		Components: []map[string]any{
			{"id": "root", "component": "Column", "children": []any{"title"}},
			{"id": "title", "component": "Text", "text": "标题"},
		},
		DataModel:  map[string]any{"title": "示例标题"},
		FieldSlots: []map[string]any{{"slotId": "title.text", "componentId": "title_text"}},
	}

	err := document.ValidateStructural()
	if err == nil || !strings.Contains(err.Error(), `references unknown component "title_text"`) {
		t.Fatalf("ValidateStructural error = %v", err)
	}
}

func TestApplyUsesRevisionAndPreservesUnrelatedSubtrees(t *testing.T) {
	document := sampleDocument()
	base, err := document.Revision()
	if err != nil {
		t.Fatal(err)
	}
	originalRoot := cloneMap(document.Components[0])
	result, err := Apply(document, PatchRequest{
		BaseRevision: base,
		EditablePaths: []string{
			"/components/cta_label/styles/color",
		},
		Operations: []Operation{{
			Op: "set_component_property", ComponentID: "cta_label",
			Path: "/styles/color", Value: "#ffffff",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(result.Document, document) || result.Revision == base {
		t.Fatal("patch did not produce a new revision")
	}
	if !reflect.DeepEqual(result.Document.Components[0], originalRoot) {
		t.Fatalf("unrelated root changed: %#v", result.Document.Components[0])
	}
	if _, exists := document.Components[3]["styles"]; exists {
		t.Fatal("input document was mutated")
	}
	if !reflect.DeepEqual(result.ChangedPaths, []string{"/components/cta_label/styles/color"}) {
		t.Fatalf("changed paths = %#v", result.ChangedPaths)
	}
}

func TestApplyRejectsStaleRevisionAndProtectedMutation(t *testing.T) {
	document := sampleDocument()
	_, err := Apply(document, PatchRequest{
		BaseRevision: "sha256:stale",
		Operations:   []Operation{{Op: "set_data", Path: "/title", Value: "新标题"}},
	})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale revision error = %v", err)
	}
	base, _ := document.Revision()
	_, err = Apply(document, PatchRequest{
		BaseRevision: base,
		EditablePaths: []string{
			"/components/cta_label/styles",
		},
		Operations: []Operation{{
			Op: "set_component_property", ComponentID: "title",
			Path: "/styles/color", Value: "red",
		}},
	})
	if !errors.Is(err, ErrProtectedMutation) {
		t.Fatalf("protected mutation error = %v", err)
	}
}

func TestApplyAtomicallyUpdatesAndInsertsDeclaredComponents(t *testing.T) {
	document := sampleDocument()
	base, _ := document.Revision()
	result, err := Apply(document, PatchRequest{
		BaseRevision:  base,
		EditablePaths: []string{"/components/cta_label/styles/color"},
		Operations: []Operation{
			{Op: "set_component_property", ComponentID: "cta_label", Path: "/styles/color", Value: "#ffffff"},
			{Op: "insert_component", ComponentID: "badge", ParentID: "root", AfterID: "title",
				Component: map[string]any{"id": "badge", "component": "Text", "text": "新品"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.CreatedIDs, []string{"badge"}) ||
		!reflect.DeepEqual(result.UpdatedIDs, []string{"cta_label"}) {
		t.Fatalf("classification = created %#v updated %#v", result.CreatedIDs, result.UpdatedIDs)
	}
	root := result.Document.Components[0]
	children, _ := root["children"].([]any)
	if !reflect.DeepEqual(children, []any{"title", "badge", "cta"}) {
		t.Fatalf("root children = %#v", children)
	}
	if len(document.Components) == len(result.Document.Components) {
		t.Fatal("input document was mutated or insertion was lost")
	}
}

func TestApplyRejectsInvalidInsertionWithoutPartialUpdate(t *testing.T) {
	document := sampleDocument()
	base, _ := document.Revision()
	_, err := Apply(document, PatchRequest{
		BaseRevision:  base,
		EditablePaths: []string{"/components/cta_label/styles/color"},
		Operations: []Operation{
			{Op: "set_component_property", ComponentID: "cta_label", Path: "/styles/color", Value: "#ffffff"},
			{Op: "insert_component", ComponentID: "badge", ParentID: "missing",
				Component: map[string]any{"id": "badge", "component": "Text", "text": "新品"}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), `parent "missing" does not exist`) {
		t.Fatalf("invalid parent error = %v", err)
	}
	if _, exists := document.Components[3]["styles"]; exists || len(document.Components) != 4 {
		t.Fatalf("rejected batch mutated input: %#v", document)
	}
}

func TestParseDesignArtifactRestoresTypedWorkspaceDocument(t *testing.T) {
	raw, err := EncodeDesignArtifact(Document{
		SchemaVersion: SchemaVersion, SurfaceID: "default", CatalogID: "catalog:test", RootID: "root",
		Components: []map[string]any{{"id": "root", "component": "Column", "children": []any{"title"}}, {"id": "title", "component": "Text", "text": map[string]any{"path": "/title"}}},
		DataModel:  map[string]any{"title": "标题"}, FieldSlots: []map[string]any{{"slotId": "title.field.1234", "componentId": "title", "refKey": "/title"}},
		ActionSlots: []map[string]any{}, DesignKnowledgeReceipt: map[string]any{"revision_id": "rules-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	document, err := ParseDesignArtifact(raw)
	if err != nil {
		t.Fatal(err)
	}
	if document.RootID != "root" || document.DataModel["title"] != "标题" ||
		len(document.FieldSlots) != 1 || document.DesignKnowledgeReceipt["revision_id"] != "rules-1" {
		t.Fatalf("document = %#v", document)
	}
}
