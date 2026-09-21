package orchestration

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/contract"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/edit"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/requirements"
	bindingcontract "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/binding/contract"
)

func TestUsableBindingUpdateEditContractRequiresExactCurrentAuthorization(t *testing.T) {
	draft := contract.Draft{
		Goal: "展示商品", Type: "list",
		Contents: []contract.ContentItem{{ID: "ticket_title", Description: "商品标题", Required: true}},
	}
	hash, err := contract.Hash(draft)
	if err != nil {
		t.Fatal(err)
	}
	design := typedDesignArtifact(t, `[{"updateComponents":{"components":[{"id":"root","component":"Column"}]}}]`, `[]`, `[]`)
	styleAuthorization := edit.Contract{
		SchemaVersion: edit.SchemaVersion, ChangeScope: "design_update", Operation: "update",
		Preconditions: edit.Preconditions{
			ContentContractHash: hash,
			DesignHash:          edit.HashText(design),
		},
	}
	baseline := `{"schema_version":"agenui.executable-binding/v1","field_mappings":[],"action_mappings":[]}`
	expected := styleAuthorization
	expected.ChangeScope = "binding_update"
	expected.Preconditions.BindingPlanHash = edit.HashText(baseline)
	expected.TargetSet = []edit.Target{{Kind: "data_requirement", ID: "req.ticket.title"}}
	expected.Intent = "改标题"
	raw, err := json.Marshal(styleAuthorization)
	if err != nil {
		t.Fatal(err)
	}
	if isUsableBindingUpdateEditContract(string(raw), expected) {
		t.Fatal("design_update authorization must not be reused by Binder")
	}
	raw, err = json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	if !isUsableBindingUpdateEditContract(string(raw), expected) {
		t.Fatal("matching binding_update authorization should be reusable")
	}
	changed := expected
	changed.Intent = "改价格"
	if isUsableBindingUpdateEditContract(string(raw), changed) {
		t.Fatal("binding_update authorization must be tied to the exact query and target")
	}
}

func TestBuildBinderContractPromptFreezesRequirementsAndSourceReceipts(t *testing.T) {
	draft := contract.Draft{
		Goal: "展示美食", Type: "list",
		Contents: []contract.ContentItem{{ID: "food.title", Description: "标题", Required: true}},
	}
	hash, _ := contract.Hash(draft)
	contractJSON, _ := json.Marshal(contract.Revision{
		ContractID: "contract-food", Revision: 1, SchemaVersion: contract.SchemaVersion,
		Status: "confirmed", ChangeOrigin: "user", ContentHash: hash, Draft: draft,
	})
	design := typedDesignArtifact(t,
		`[{"updateComponents":{"components":[{"id":"root","component":"Column"}]}}]`,
		`[{"slotId":"slot.title","contractItemId":"food.title","refKey":"/items[*]/title"}]`, `[]`)
	requirementsJSON, _ := json.Marshal(requirements.Result{RequirementSet: requirements.Set{
		SchemaVersion: requirements.SchemaVersion, InputHash: "sha256:req",
		Data: []requirements.DataRequirement{{
			RequirementID: "req.food.title", ContractItemID: "food.title", Description: "标题",
			TargetSlotIDs: []string{"slot.title"}, Level: "core", Type: "string", Shape: "list_item", WhenMissing: "block",
		}}, Actions: []requirements.ActionRequirement{},
	}})
	api := `{"query":"美食","total":1,"results":[{"data_source_id":"/food/search","api_version":"v2","knowledge_revision":"kr-7","content_hash":"sha256:abc"}]}`
	prompt, input, err := buildBinderContractPrompt("binder evidence", string(contractJSON), design, string(requirementsJSON), "", api)
	if err != nil {
		t.Fatal(err)
	}
	if input.Requirements.Data[0].RequirementID != "req.food.title" ||
		input.Sources[0].SourceID != "/food/search@v2" ||
		input.Sources[0].KnowledgeID != "kr-7@sha256:abc" {
		t.Fatalf("unexpected input: %+v", input)
	}
	for _, want := range []string{
		"agenui_workspace.inspect_binding",
		"agenui_workspace.commit_binding",
		"binder evidence",
		"不要读取或重写完整 AGenUI DSL",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q: %s", want, prompt)
		}
	}
	for _, forbidden := range []string{"req.food.title", "/food/search@v2", "## 权威绑定输入"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("prompt must not duplicate Workspace projection %q: %s", forbidden, prompt)
		}
	}
}

func TestBinderContractSourcesPreservesOrdinarySearchReceiptBehavior(t *testing.T) {
	t.Parallel()

	raw := `{"query":"美食","total":1,"results":[` +
		`{"data_source_id":"/food/search","api_version":"v2","knowledge_revision":"kr-7","content_hash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}` +
		`]}`
	sources, err := binderContractSources(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []bindingcontract.SourceSnapshot{
		{
			SourceID: "/food/search@v2", KnowledgeID: "kr-7@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Primary: true, Namespace: "source_0",
		},
	}
	if len(sources) != len(want) {
		t.Fatalf("sources = %+v, want %+v", sources, want)
	}
	for index := range want {
		if !reflect.DeepEqual(sources[index], want[index]) {
			t.Fatalf("source %d = %+v, want %+v", index, sources[index], want[index])
		}
	}
}

func TestBinderContractSourcesAcceptsBoundedModelSelectedCandidates(t *testing.T) {
	t.Parallel()

	raw := `{"query":"商品目录","total":2,"results":[` +
		`{"data_source_id":"/ticket/search","api_version":"v2","knowledge_revision":"kr-7","content_hash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},` +
		`{"data_source_id":"/scenic/search","api_version":"v3","knowledge_revision":"kr-9","content_hash":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}` +
		`]}`
	sources, err := binderContractSources(raw)
	if err != nil || len(sources) != 2 || !sources[0].Primary || sources[1].Primary {
		t.Fatalf("selected sources=%+v error=%v", sources, err)
	}
}

func TestParseAndValidateBinderContractOutput(t *testing.T) {
	input := bindingcontract.Input{
		SchemaVersion: bindingcontract.InputSchemaV1,
		Requirements: requirements.Set{
			SchemaVersion: requirements.SchemaVersion, InputHash: "sha256:req",
			Data: []requirements.DataRequirement{{RequirementID: "req.title", ContractItemID: "title", TargetSlotIDs: []string{"slot.title"}, Level: "core", Type: "string", Shape: "scalar", WhenMissing: "block"}},
		},
	}
	// Result validation is exercised by the contract package. This test owns
	// the orchestration wire boundary: exactly one typed submission is accepted.
	raw := `{"schema_version":"agenui.binding-submission/v1","result":{"schema_version":"agenui.bind-result/v1","status":"ready","bindings":[],"issues":[]},"plan":{"schema_version":"agenui.executable-binding/v1","field_mappings":[],"action_mappings":[]}}`
	result, block, err := parseBinderContractResult(raw)
	if err != nil || block == "" || result.SchemaVersion != bindingcontract.ResultSchemaV1 {
		t.Fatalf("result=%+v block=%q err=%v input=%+v", result, block, err, input)
	}
	if _, _, err := parseBinderContractResult(raw + raw); err == nil {
		t.Fatal("expected concatenated submission rejection")
	}
}

// TestCanonicalizeBinderContractResultSourceIDsMapsUnambiguousReceiptEcho locks the
// deterministic normalization of a real model echo drift: the model copies
// data_source_id / result_id from the search receipt instead of the composite
// source_id / knowledge_id in the frozen input. When the echo resolves to
// exactly one authorized source it is rewritten to that source's composite
// identity; ambiguous or unknown echoes are left untouched so ValidateResult
// still rejects them fail-closed.
func TestCanonicalizeBinderContractResultSourceIDsMapsUnambiguousReceiptEcho(t *testing.T) {
	t.Parallel()

	sources := []bindingcontract.SourceSnapshot{{
		SourceID:    "/ws/tools/general/OnTheWayGoodFoodTool@unversioned",
		KnowledgeID: "snapshot-7bb3bead23e2b3df@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Primary:     true, Namespace: "source_0",
	}}
	result := bindingcontract.Result{
		SchemaVersion: bindingcontract.ResultSchemaV1,
		Status:        bindingcontract.StatusBlocked,
		Bindings: []bindingcontract.Binding{{
			RequirementID: "data.x", TargetSlotIDs: []string{"slot.x"},
			SourceID:    "/ws/tools/general/OnTheWayGoodFoodTool",
			KnowledgeID: "api-7bb3bead23e2b3df",
			FieldPath:   "$.data.data.voCard.poiList[*].mainImg",
		}},
	}
	canonicalizeBinderContractResultSourceIDs(sources, &result)
	if result.Bindings[0].SourceID != sources[0].SourceID ||
		result.Bindings[0].KnowledgeID != sources[0].KnowledgeID {
		t.Fatalf("unambiguous echo must canonicalize: %+v", result.Bindings[0])
	}

	// An echo that does not resolve to any frozen source must stay unchanged.
	foreign := bindingcontract.Result{
		SchemaVersion: bindingcontract.ResultSchemaV1,
		Status:        bindingcontract.StatusBlocked,
		Bindings: []bindingcontract.Binding{{
			RequirementID: "data.x", TargetSlotIDs: []string{"slot.x"},
			SourceID: "/ws/tools/general/SomeOtherTool", KnowledgeID: "api-ffff",
			FieldPath: "$.data.other",
		}},
	}
	canonicalizeBinderContractResultSourceIDs(sources, &foreign)
	if foreign.Bindings[0].SourceID != "/ws/tools/general/SomeOtherTool" {
		t.Fatalf("foreign echo must not be adopted: %+v", foreign.Bindings[0])
	}

	// Two authorized sources sharing the same data_source_id prefix are
	// ambiguous: the echo must stay unchanged and fail validation later.
	ambiguous := append([]bindingcontract.SourceSnapshot(nil), sources[0], bindingcontract.SourceSnapshot{
		SourceID:    "/ws/tools/general/OnTheWayGoodFoodTool@v2",
		KnowledgeID: "rev-2@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Namespace:   "source_1",
	})
	echo := bindingcontract.Result{
		SchemaVersion: bindingcontract.ResultSchemaV1,
		Status:        bindingcontract.StatusBlocked,
		Bindings: []bindingcontract.Binding{{
			RequirementID: "data.x", TargetSlotIDs: []string{"slot.x"},
			SourceID: "/ws/tools/general/OnTheWayGoodFoodTool", KnowledgeID: "api-7bb3bead23e2b3df",
			FieldPath: "$.data.data.voCard.poiList[*].mainImg",
		}},
	}
	canonicalizeBinderContractResultSourceIDs(ambiguous, &echo)
	if echo.Bindings[0].SourceID != "/ws/tools/general/OnTheWayGoodFoodTool" {
		t.Fatalf("ambiguous echo must not be adopted: %+v", echo.Bindings[0])
	}
}

// TestCanonicalizeBinderContractResultSourceIDsMapsRealWorldEchoShapes locks the
// two echo shapes observed in the live baseline run that killed binder runs:
// the bare API path against a host-observed entity ID, and the api-<digest>
// knowledge echo against the snapshot-<digest> knowledge ID.
func TestCanonicalizeBinderContractResultSourceIDsMapsRealWorldEchoShapes(t *testing.T) {
	t.Parallel()

	sources := []bindingcontract.SourceSnapshot{{
		SourceID:    "api:host-observed:/ws/tools/general/DestinationEntertainmentTool@host-observed@sha256:" + strings.Repeat("a", 64),
		KnowledgeID: "snapshot-bc2ec2cc94ee2bbd@sha256:" + strings.Repeat("a", 64),
		Primary:     true, Namespace: "source_0",
	}}
	result := bindingcontract.Result{
		SchemaVersion: bindingcontract.ResultSchemaV1,
		Status:        bindingcontract.StatusBlocked,
		Bindings: []bindingcontract.Binding{
			{
				RequirementID: "data.path", TargetSlotIDs: []string{"slot.a"},
				SourceID:    "/ws/tools/general/DestinationEntertainmentTool",
				KnowledgeID: "api-bc2ec2cc94ee2bbd",
				FieldPath:   "$.data.data.voCard.poiList[*].mainImg",
			},
			{
				RequirementID: "data.digest", TargetSlotIDs: []string{"slot.b"},
				SourceID:    "some-freeform-name",
				KnowledgeID: "api-bc2ec2cc94ee2bbd",
				FieldPath:   "$.data.data.voCard.poiList[*].poiName.text",
			},
		},
	}
	canonicalizeBinderContractResultSourceIDs(sources, &result)
	for index, binding := range result.Bindings {
		if binding.SourceID != sources[0].SourceID || binding.KnowledgeID != sources[0].KnowledgeID {
			t.Fatalf("real-world echo %d must canonicalize: %+v", index, binding)
		}
	}

	// A short or foreign digest still must not be adopted.
	foreign := bindingcontract.Result{
		SchemaVersion: bindingcontract.ResultSchemaV1,
		Status:        bindingcontract.StatusBlocked,
		Bindings: []bindingcontract.Binding{{
			RequirementID: "data.x", TargetSlotIDs: []string{"slot.x"},
			SourceID: "other", KnowledgeID: "api-ffff",
			FieldPath: "$.data.other",
		}},
	}
	canonicalizeBinderContractResultSourceIDs(sources, &foreign)
	if foreign.Bindings[0].SourceID != "other" {
		t.Fatalf("foreign digest echo must not be adopted: %+v", foreign.Bindings[0])
	}
}

// TestDedupeBinderContractResultBindingsOnlyCollapsesExactRepeats prevents
// normalization from silently choosing one of two materially different
// bindings. Every binding field, including the chosen source path and legacy
// target, must agree before a model repeat is safe to collapse.
func TestDedupeBinderContractResultBindingsOnlyCollapsesExactRepeats(t *testing.T) {
	t.Parallel()

	source := "api:host-observed:/ws/tools/general/RouteWeatherCardTool@host-observed@sha256:" + strings.Repeat("a", 64)
	knowledge := "snapshot-a8da3a7f@sha256:" + strings.Repeat("a", 64)
	result := bindingcontract.Result{
		SchemaVersion: bindingcontract.ResultSchemaV1,
		Status:        bindingcontract.StatusBlocked,
		Bindings: []bindingcontract.Binding{
			{
				RequirementID: "data.44456e3f9576",
				// target_slot_ids is a set in the contract. A repeated
				// binding may serialize the same slots in a different order
				// without becoming a materially different choice.
				TargetSlotIDs: []string{"slot.item.province", "slot.item.city"},
				SourceID:      source, KnowledgeID: knowledge,
				FieldPath: "$.data.data.voCard.weathers[*].city_desc",
				RefKey:    "/items[*]/city",
			},
			{
				RequirementID: "data.44456e3f9576",
				TargetSlotIDs: []string{"slot.item.city", "slot.item.province"},
				SourceID:      source, KnowledgeID: knowledge,
				FieldPath: "$.data.data.voCard.weathers[*].city_desc",
				RefKey:    "/items[*]/city",
			},
			{
				RequirementID: "data.other",
				TargetSlotIDs: []string{"slot.other"},
				SourceID:      source, KnowledgeID: knowledge,
				FieldPath: "$.data.data.voCard.weathers[*].temperature",
			},
		},
	}
	dedupeBinderContractResultBindings(&result)
	if len(result.Bindings) != 2 {
		t.Fatalf("exact repeat must collapse: %+v", result.Bindings)
	}
	if result.Bindings[0].FieldPath != "$.data.data.voCard.weathers[*].city_desc" ||
		result.Bindings[1].RequirementID != "data.other" {
		t.Fatalf("exact repeat changed the binding order: %+v", result.Bindings)
	}

	// A repeat that changes any field is ambiguous, not a collapse candidate:
	// leave it for ValidateResult to reject fail-closed instead of first-wins.
	ambiguous := bindingcontract.Result{
		SchemaVersion: bindingcontract.ResultSchemaV1,
		Status:        bindingcontract.StatusBlocked,
		Bindings: []bindingcontract.Binding{
			{
				RequirementID: "data.x", TargetSlotIDs: []string{"slot.x"},
				SourceID: source, KnowledgeID: knowledge, FieldPath: "$.a", RefKey: "/x",
			},
			{
				RequirementID: "data.x", TargetSlotIDs: []string{"slot.x"},
				SourceID: source, KnowledgeID: knowledge, FieldPath: "$.b", RefKey: "/x",
			},
		},
	}
	dedupeBinderContractResultBindings(&ambiguous)
	if len(ambiguous.Bindings) != 2 {
		t.Fatalf("different-path repeat must stay for fail-closed validation: %+v", ambiguous.Bindings)
	}

	duplicateSlot := result.Bindings[0]
	duplicateSlot.TargetSlotIDs = []string{"slot.item.city", "slot.item.city"}
	differentSlotMultiset := duplicateSlot
	differentSlotMultiset.TargetSlotIDs = []string{"slot.item.city", "slot.item.province"}
	malformedSlots := bindingcontract.Result{
		SchemaVersion: bindingcontract.ResultSchemaV1,
		Status:        bindingcontract.StatusBlocked,
		Bindings:      []bindingcontract.Binding{duplicateSlot, differentSlotMultiset},
	}
	dedupeBinderContractResultBindings(&malformedSlots)
	if len(malformedSlots.Bindings) != 2 {
		t.Fatalf("different slot multiplicity must stay for strict validation: %+v", malformedSlots.Bindings)
	}
}

// TestBinderContractResultDiagEmitsDeterministicHistogram locks the grep contract
// the evaluation harness consumes from the [agenui-binding-diag] bind_result
// line: stable key=value tokens and a sorted issue-code histogram.
func TestBinderContractResultDiagEmitsDeterministicHistogram(t *testing.T) {
	t.Parallel()

	diag := binderContractResultDiag(bindingcontract.Result{
		SchemaVersion: bindingcontract.ResultSchemaV1,
		Status:        bindingcontract.StatusBlocked,
		Bindings: []bindingcontract.Binding{
			{RequirementID: "data.a", Transforms: []bindingcontract.Transform{{OperatorVersionID: 42}}},
			{RequirementID: "data.b"},
		},
		Issues: []bindingcontract.Issue{
			{RequirementID: "data.c", Code: "FIELD_MISSING"},
			{RequirementID: "data.d", Code: "field_unsafe_shape"},
			{RequirementID: "data.e", Code: "FIELD_MISSING"},
		},
	})
	want := "status=blocked bindings=2 operator_bindings=1 issues=3 " +
		"issue_codes=FIELD_MISSING:2,FIELD_UNSAFE_SHAPE:1"
	if diag != want {
		t.Fatalf("diag = %q, want %q", diag, want)
	}
	if empty := binderContractResultDiag(bindingcontract.Result{Status: bindingcontract.StatusReady}); !strings.Contains(empty, "issue_codes=-") {
		t.Fatalf("empty histogram = %q", empty)
	}
}

func TestBuildBindingUpdateEditContractAuthorizesOneRequirement(t *testing.T) {
	draft := contract.Draft{
		Goal: "展示美食", Type: "list",
		Contents: []contract.ContentItem{
			{ID: "food.title", Description: "标题", Required: true},
			{ID: "food.price", Description: "价格", Required: true},
		},
	}
	hash, _ := contract.Hash(draft)
	contractJSON, _ := json.Marshal(contract.Revision{
		ContractID: "contract-food", Revision: 1, SchemaVersion: contract.SchemaVersion,
		Status: "confirmed", ChangeOrigin: "user", ContentHash: hash, Draft: draft,
	})
	design := typedDesignArtifact(t,
		`[{"updateComponents":{"components":[{"id":"root","component":"Column"}]}}]`,
		`[{"slotId":"slot.title","contractItemId":"food.title","refKey":"/items[*]/title"},{"slotId":"slot.price","contractItemId":"food.price","refKey":"/items[*]/price"}]`, `[]`)
	requirementsJSON, _ := json.Marshal(requirements.Result{RequirementSet: requirements.Set{
		SchemaVersion: requirements.SchemaVersion, InputHash: "sha256:req",
		Data: []requirements.DataRequirement{
			{RequirementID: "req.food.title", ContractItemID: "food.title", Description: "美食标题", TargetSlotIDs: []string{"slot.title"}, Level: "core", Type: "string", Shape: "list_item", WhenMissing: "block"},
			{RequirementID: "req.food.price", ContractItemID: "food.price", Description: "美食价格", TargetSlotIDs: []string{"slot.price"}, Level: "core", Type: "number", Shape: "list_item", WhenMissing: "block"},
		},
	}})
	baseline := `{"schema_version":"agenui.executable-binding/v1","field_mappings":[{"refKey":"/items[*]/title","sourceKey":"$.data.items[*].title"},{"refKey":"/items[*]/price","sourceKey":"$.data.items[*].price"}],"action_mappings":[]}`
	editContract, err := buildBindingUpdateEditContract(
		"run-edit", "generation-base", 7,
		"标题改绑到 name 字段", string(contractJSON), design,
		string(requirementsJSON), baseline, []string{"req.food.title"}, "tenant-a", "user-a", "session-a",
	)
	if err != nil {
		t.Fatal(err)
	}
	if editContract.ChangeScope != "binding_update" ||
		editContract.BaseGenerationID != "generation-base" ||
		editContract.BaseCardRevision != 7 ||
		len(editContract.TargetSet) != 1 ||
		editContract.TargetSet[0].Kind != "data_requirement" ||
		editContract.TargetSet[0].ID != "req.food.title" {
		t.Fatalf("unexpected edit contract: %+v", editContract)
	}
	if editContract.Preconditions.BindingPlanHash != edit.HashText(baseline) {
		t.Fatalf("binding plan hash = %q", editContract.Preconditions.BindingPlanHash)
	}
	editJSON, _ := json.Marshal(editContract)
	api := `{"query":"美食","total":1,"results":[{"data_source_id":"/food/search","api_version":"v2","knowledge_revision":"kr-7","content_hash":"sha256:abc"}]}`
	_, input, err := buildBinderContractPrompt(
		"legacy binder evidence", string(contractJSON), design,
		string(requirementsJSON), string(editJSON), api,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindingcontract.ValidateResult(input, bindingcontract.Result{
		SchemaVersion: bindingcontract.ResultSchemaV1,
		Status:        bindingcontract.StatusReady,
		Bindings: []bindingcontract.Binding{{
			RequirementID: "req.food.title", TargetSlotIDs: []string{"slot.title"},
			SourceID: "/food/search@v2", KnowledgeID: "kr-7@sha256:abc",
			FieldPath: "$.data.items[*].name", RefKey: "/items[*]/title",
		}},
	}); err != nil {
		t.Fatalf("binding_update result should be scoped to title only: %v", err)
	}
}
