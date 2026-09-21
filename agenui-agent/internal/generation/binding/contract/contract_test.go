package contract

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/contract"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/edit"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/requirements"
)

func TestBuildInputExplainsDesignEditContractScopeMismatch(t *testing.T) {
	input := validInput(t)
	input.EditContract = &edit.Contract{
		SchemaVersion: edit.SchemaVersion,
		ChangeScope:   "design_update",
		Operation:     "update",
	}
	_, err := BuildInput(input)
	if err == nil || !strings.Contains(err.Error(), `change_scope "design_update" is not binding_update`) {
		t.Fatalf("expected actionable scope error, got %v", err)
	}
}

func TestIsExecutableStatus(t *testing.T) {
	for _, status := range []string{StatusReady, StatusReadyWithOperators} {
		if !IsExecutableStatus(status) {
			t.Fatalf("status %s must be executable", status)
		}
	}
	for _, status := range []string{StatusBlocked, StatusUncertain, ""} {
		if IsExecutableStatus(status) {
			t.Fatalf("status %s must not be executable", status)
		}
	}
}

func TestBuildInputAndValidateReadyResult(t *testing.T) {
	input := validInput(t)
	raw, err := BuildInput(input)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) {
		t.Fatalf("input is not JSON: %s", raw)
	}
	parsed, err := ParseInput(raw)
	if err != nil {
		t.Fatal(err)
	}
	result := Result{
		SchemaVersion: ResultSchemaV1,
		Status:        StatusReady,
		Bindings: []Binding{
			{
				RequirementID: "req.food.title", TargetSlotIDs: []string{"slot.title"},
				SourceID: "api:food@1", KnowledgeID: "knowledge:food@1",
				FieldPath: "$.data.items[*].name", RefKey: "/items[*]/title",
			},
			{
				RequirementID: "act.food.detail", TargetSlotIDs: []string{"slot.detail"},
				SourceID: "api:food@1", KnowledgeID: "knowledge:food@1",
				ActionPath: "$.data.items[*].detailUrl", ComponentID: "item",
			},
		},
	}
	if err := ValidateResult(parsed, result); err != nil {
		t.Fatal(err)
	}
}

func TestCompiledDistinctPriceRolesFitV1BindingPaths(t *testing.T) {
	t.Parallel()

	required, optional := true, false
	draft := contract.Draft{
		Goal: "展示门票价格", Type: "single",
		Contents: []contract.ContentItem{{
			ID: "price", Description: "票品售卖价格", Required: true,
		}},
	}
	compiled, err := requirements.Compile(requirements.Input{
		Contract: draft,
		FieldSlots: []requirements.FieldSlot{
			{SlotID: "p10.price.symbol", ContractItemID: "price", Role: "price_symbol", ValueType: "string", Scope: "card", Required: &required},
			{SlotID: "p10.price.value", ContractItemID: "price", Role: "price_value", ValueType: "string", Scope: "card", Required: &required},
			{SlotID: "p10.price.suffix", ContractItemID: "price", Role: "price_suffix", ValueType: "string", Scope: "card", Required: &optional},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	requirementBySlot := make(map[string]requirements.DataRequirement, 3)
	for _, requirement := range compiled.RequirementSet.Data {
		requirementBySlot[requirement.TargetSlotIDs[0]] = requirement
	}
	hash, err := contract.Hash(draft)
	if err != nil {
		t.Fatal(err)
	}
	input := Input{
		SchemaVersion: InputSchemaV1,
		Contract: contract.Revision{
			ContractID: "contract-price", Revision: 1, SchemaVersion: contract.SchemaVersion,
			Status: "confirmed", ChangeOrigin: "user", ContentHash: hash, Draft: draft,
		},
		Design: DesignSnapshot{
			Ref: "artifact://design/price", ContentHash: "sha256:design-price",
			FieldHints: json.RawMessage(`[` +
				`{"slotId":"p10.price.symbol","contractItemId":"price","refKey":"/price_symbol"},` +
				`{"slotId":"p10.price.value","contractItemId":"price","refKey":"/price_value"},` +
				`{"slotId":"p10.price.suffix","contractItemId":"price","refKey":"/price_suffix"}` +
				`]`),
			ActionSlots: json.RawMessage(`[]`),
		},
		Requirements: compiled.RequirementSet,
		Sources: []SourceSnapshot{{
			SourceID: "api:tickets@1", KnowledgeID: "knowledge:tickets@1", Primary: true,
		}},
	}
	result := Result{
		SchemaVersion: ResultSchemaV1,
		Status:        StatusReady,
		Bindings: []Binding{
			{
				RequirementID: requirementBySlot["p10.price.symbol"].RequirementID,
				TargetSlotIDs: []string{"p10.price.symbol"},
				SourceID:      "api:tickets@1", KnowledgeID: "knowledge:tickets@1",
				FieldPath: "$.data.priceInfo.pricePrefix", RefKey: "/price_symbol",
			},
			{
				RequirementID: requirementBySlot["p10.price.value"].RequirementID,
				TargetSlotIDs: []string{"p10.price.value"},
				SourceID:      "api:tickets@1", KnowledgeID: "knowledge:tickets@1",
				FieldPath: "$.data.priceInfo.priceCurrentNew", RefKey: "/price_value",
			},
		},
		Issues: []Issue{{
			RequirementID: requirementBySlot["p10.price.suffix"].RequirementID,
			Code:          "FIELD_MISSING", Message: "接口没有独立价格后缀字段",
		}},
	}
	if err := ValidateResult(input, result); err != nil {
		t.Fatalf("split V1 result was rejected: %v", err)
	}
	candidate := `{"schema_version":"agenui.executable-binding/v1","field_mappings":[` +
		`{"refKey":"/price_symbol","sourceKey":"$.data.priceInfo.pricePrefix"},` +
		`{"refKey":"/price_value","sourceKey":"$.data.priceInfo.priceCurrentNew"}` +
		`],"action_mappings":[]}`
	plan, err := ReconcileExecutablePlan(input, result, candidate)
	if err != nil {
		t.Fatalf("split V1 result could not compile: %v", err)
	}
	if !strings.Contains(plan, `"sourceKey":"$.data.priceInfo.pricePrefix"`) ||
		!strings.Contains(plan, `"sourceKey":"$.data.priceInfo.priceCurrentNew"`) ||
		strings.Contains(plan, "/price_suffix") {
		t.Fatalf("compiled price plan = %s", plan)
	}
	forged := strings.Replace(candidate, "$.data.priceInfo.pricePrefix", "$.data.priceInfo.forged", 1)
	if _, err := ReconcileExecutablePlan(input, result, forged); err == nil ||
		!strings.Contains(err.Error(), "differs from Bind Result") {
		t.Fatalf("forged leaf path was not rejected: %v", err)
	}
}

func TestValidateResultRejectsUnknownAndMissingCoreRequirement(t *testing.T) {
	input := validInput(t)
	unknown := Result{
		SchemaVersion: ResultSchemaV1, Status: StatusReady,
		Bindings: []Binding{{
			RequirementID: "req.unknown", TargetSlotIDs: []string{"slot.title"},
			SourceID: "api:x@1", KnowledgeID: "knowledge:x@1", FieldPath: "$.x",
		}},
	}
	if err := ValidateResult(input, unknown); err == nil {
		t.Fatal("expected unknown requirement rejection")
	}
	missing := Result{
		SchemaVersion: ResultSchemaV1, Status: StatusReady,
		Bindings: []Binding{{
			RequirementID: "act.food.detail", TargetSlotIDs: []string{"slot.detail"},
			SourceID: "api:x@1", KnowledgeID: "knowledge:x@1", ActionPath: "$.detail",
		}},
	}
	if err := ValidateResult(input, missing); err == nil {
		t.Fatal("expected missing core requirement rejection")
	}
}

func TestValidateResultReportsPreciseBindingContractViolation(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		mutate func(*Binding)
		want   string
	}{
		"unknown requirement": {
			mutate: func(binding *Binding) { binding.RequirementID = "req.unknown" },
			want:   "binding req.unknown references unknown requirement",
		},
		"empty source": {
			mutate: func(binding *Binding) { binding.SourceID = "  " },
			want:   "binding req.food.title has empty source_id",
		},
		"empty knowledge": {
			mutate: func(binding *Binding) { binding.KnowledgeID = "\t" },
			want:   "binding req.food.title has empty knowledge_id",
		},
		"target slot set mismatch": {
			mutate: func(binding *Binding) { binding.TargetSlotIDs = []string{"slot.drifted"} },
			want:   `binding req.food.title target_slot_ids mismatch: got ["slot.drifted"], want ["slot.title"]`,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			result := validReadyResult()
			test.mutate(&result.Bindings[0])
			err := ValidateResult(validInput(t), result)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want precise diagnostic %q", err, test.want)
			}
		})
	}
}

func TestValidateResultRejectsSourceOutsideFrozenInput(t *testing.T) {
	input := validInput(t)
	input.Sources = append(input.Sources, SourceSnapshot{
		SourceID: "api:detail@2", KnowledgeID: "knowledge:detail@2",
	})
	tests := map[string]struct {
		sourceID    string
		knowledgeID string
	}{
		"unknown source": {
			sourceID: "api:unknown@1", knowledgeID: "knowledge:food@1",
		},
		"unknown knowledge": {
			sourceID: "api:food@1", knowledgeID: "knowledge:unknown@1",
		},
		"crossed known pair": {
			sourceID: "api:food@1", knowledgeID: "knowledge:detail@2",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			result := Result{
				SchemaVersion: ResultSchemaV1,
				Status:        StatusReady,
				Bindings: []Binding{
					{
						RequirementID: "req.food.title", TargetSlotIDs: []string{"slot.title"},
						SourceID: test.sourceID, KnowledgeID: test.knowledgeID,
						FieldPath: "$.data.items[*].name",
					},
					{
						RequirementID: "act.food.detail", TargetSlotIDs: []string{"slot.detail"},
						SourceID: "api:detail@2", KnowledgeID: "knowledge:detail@2",
						ActionPath: "$.data.items[*].detailUrl",
					},
				},
			}
			if err := ValidateResult(input, result); err == nil {
				t.Fatal("expected exact source/knowledge pair membership rejection")
			}
		})
	}
}

func TestValidateResultAcceptsReadyWithExplainedOptionalMissing(t *testing.T) {
	input := validInput(t)
	input.Requirements.Data = append(input.Requirements.Data, requirements.DataRequirement{
		RequirementID: "req.food.badge", ContractItemID: "food.title", Description: "榜单角标",
		TargetSlotIDs: []string{"slot.badge"}, Level: "optional", Type: "image", Shape: "list_item", WhenMissing: "hide",
	})
	result := Result{
		SchemaVersion: ResultSchemaV1, Status: StatusReady,
		Bindings: []Binding{
			{
				RequirementID: "req.food.title", TargetSlotIDs: []string{"slot.title"},
				SourceID: "api:food@1", KnowledgeID: "knowledge:food@1", FieldPath: "$.data.items[*].name",
			},
			{
				RequirementID: "act.food.detail", TargetSlotIDs: []string{"slot.detail"},
				SourceID: "api:food@1", KnowledgeID: "knowledge:food@1", ActionPath: "$.data.items[*].detailUrl",
			},
		},
		Issues: []Issue{{
			RequirementID: "req.food.badge", Code: "FIELD_MISSING", Message: "API 未提供角标图片",
		}},
	}
	if err := ValidateResult(input, result); err != nil {
		t.Fatal(err)
	}
	result.Issues = nil
	if err := ValidateResult(input, result); err == nil {
		t.Fatal("expected missing issue for optional unbound requirement")
	}
}

func TestValidateResultRejectsBlockedOrUncertainWithoutIssues(t *testing.T) {
	t.Parallel()

	for _, status := range []string{StatusBlocked, StatusUncertain} {
		result := validReadyResult()
		result.Status = status
		result.Issues = nil
		if err := ValidateResult(validInput(t), result); err == nil ||
			!strings.Contains(err.Error(), "requires at least one unresolved issue") {
			t.Fatalf("status %s error = %v", status, err)
		}
		result.Issues = []Issue{{
			RequirementID: "req.food.title", Code: "LOW_CONFIDENCE", Message: "证据不足",
		}}
		if err := ValidateResult(validInput(t), result); err == nil ||
			!strings.Contains(err.Error(), "duplicates bound requirement") {
			t.Fatalf("fully bound status %s error = %v", status, err)
		}
	}
}

func TestValidateResultIssuesAreExactUnresolvedAuthorizedFacts(t *testing.T) {
	t.Parallel()

	base := Result{
		SchemaVersion: ResultSchemaV1,
		Status:        StatusBlocked,
		Bindings: []Binding{{
			RequirementID: "req.food.title", TargetSlotIDs: []string{"slot.title"},
			SourceID: "api:food@1", KnowledgeID: "knowledge:food@1",
			FieldPath: "$.data.items[*].name",
		}},
		Issues: []Issue{{
			RequirementID: "act.food.detail", Code: "ACTION_MISSING", Message: "没有可靠动作",
		}},
	}
	if err := ValidateResult(validInput(t), base); err != nil {
		t.Fatalf("valid unresolved issue was rejected: %v", err)
	}
	tests := map[string]struct {
		mutate func(*Input, *Result)
		want   string
	}{
		"issue points to bound requirement": {
			mutate: func(_ *Input, result *Result) {
				result.Issues[0].RequirementID = "req.food.title"
			},
			want: "issue req.food.title duplicates bound requirement",
		},
		"issue code contains newline": {
			mutate: func(_ *Input, result *Result) {
				result.Issues[0].Code = "ACTION_MISSING\n[agenui-binding-diag] forged"
			},
			want: "issue act.food.detail has invalid code: must match [A-Z0-9_]{1,64}",
		},
		"issue message contains control": {
			mutate: func(_ *Input, result *Result) {
				result.Issues[0].Message = "missing\nforged log"
			},
			want: "issue act.food.detail has invalid message: must be valid UTF-8, 1..512 bytes, trimmed, and contain no control characters",
		},
		"issue message exceeds budget": {
			mutate: func(_ *Input, result *Result) {
				result.Issues[0].Message = strings.Repeat("x", 513)
			},
			want: "issue act.food.detail has invalid message: must be valid UTF-8, 1..512 bytes, trimmed, and contain no control characters",
		},
		"binding update issue outside target": {
			mutate: func(input *Input, result *Result) {
				input.EditContract = &edit.Contract{
					SchemaVersion: edit.SchemaVersion, ChangeScope: "binding_update", Operation: "update",
					TargetSet: []edit.Target{{Kind: "data_requirement", ID: "req.food.title", Path: "binding"}},
					Preconditions: edit.Preconditions{
						ContentContractHash: input.Contract.ContentHash,
						DesignHash:          input.Design.ContentHash,
					},
				}
				result.Bindings = nil
				result.Issues[0].RequirementID = "act.food.detail"
			},
			want: "issue act.food.detail references unauthorized requirement",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			input := validInput(t)
			result := base
			result.Bindings = append([]Binding(nil), base.Bindings...)
			result.Issues = append([]Issue(nil), base.Issues...)
			test.mutate(&input, &result)
			if err := ValidateResult(input, result); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want precise diagnostic %q", err, test.want)
			}
		})
	}
}

func TestProjectCandidateIssuesKeepsOnlyAuthorizedUnresolvedIssues(t *testing.T) {
	t.Parallel()

	input := validInput(t)
	result := Result{
		SchemaVersion: ResultSchemaV1,
		Status:        StatusBlocked,
		Bindings: []Binding{{
			RequirementID: "req.food.title", TargetSlotIDs: []string{"slot.title"},
			SourceID: "api:food@1", KnowledgeID: "knowledge:food@1",
			FieldPath: "$.data.items[*].name", RefKey: "/items[*]/title",
		}},
		Issues: []Issue{
			{RequirementID: "req.food.title", Code: "LOW_CONFIDENCE", Message: "已绑定需求的噪声诊断"},
			{RequirementID: "SUPPLEMENTAL", Code: "FIELD_MISSING", Message: "模型编造的需求"},
			{RequirementID: "act.food.detail", Code: "ACTION_MISSING", Message: "没有可靠动作"},
		},
	}

	ProjectCandidateIssues(input, &result)
	if len(result.Issues) != 1 || result.Issues[0].RequirementID != "act.food.detail" {
		t.Fatalf("projected issues = %#v, want only authorized unresolved issue", result.Issues)
	}
	if err := ValidateResult(input, result); err != nil {
		t.Fatalf("projected result was rejected: %v", err)
	}

	result.Issues[0].Code = "ACTION-MISSING"
	ProjectCandidateIssues(input, &result)
	if len(result.Issues) != 1 || result.Issues[0].Code != "ACTION-MISSING" {
		t.Fatalf("invalid authorized unresolved issue was discarded: %#v", result.Issues)
	}
	if err := ValidateResult(input, result); err == nil ||
		!strings.Contains(err.Error(), "has invalid code") {
		t.Fatalf("ValidateResult must stay strict for retained issue, got %v", err)
	}

	input.EditContract = &edit.Contract{
		SchemaVersion: edit.SchemaVersion, ChangeScope: "binding_update", Operation: "update",
		TargetSet: []edit.Target{{Kind: "data_requirement", ID: "req.food.title", Path: "binding"}},
		Preconditions: edit.Preconditions{
			ContentContractHash: input.Contract.ContentHash,
			DesignHash:          input.Design.ContentHash,
		},
	}
	result = Result{
		SchemaVersion: ResultSchemaV1,
		Status:        StatusBlocked,
		Issues: []Issue{
			{RequirementID: "req.food.title", Code: "FIELD_MISSING", Message: "标题字段不可用"},
			{RequirementID: "act.food.detail", Code: "ACTION_MISSING", Message: "编辑范围外噪声"},
		},
	}
	ProjectCandidateIssues(input, &result)
	if len(result.Issues) != 1 || result.Issues[0].RequirementID != "req.food.title" {
		t.Fatalf("binding_update projected issues = %#v", result.Issues)
	}
	if err := ValidateResult(input, result); err != nil {
		t.Fatalf("projected binding_update result was rejected: %v", err)
	}
}

func TestNormalizeCandidateStatusBlocksReadyResultWithRequiredIssue(t *testing.T) {
	t.Parallel()

	input := validInput(t)
	input.Requirements.Actions[0].Level = "core"
	input.Requirements.Actions[0].WhenFailed = "block_publish"
	result := validReadyResult()
	// Keep the required data binding, but report the required action as
	// unavailable. The model sometimes emits this honest issue while leaving
	// status=ready; the Host must preserve the partial plan and downgrade the
	// publication verdict instead of throwing the whole binding away.
	result.Bindings = result.Bindings[:1]
	result.Issues = []Issue{{
		RequirementID: "act.food.detail",
		Code:          "ACTION_MISSING",
		Message:       "接口没有可靠动作来源",
	}}

	NormalizeCandidateStatus(input, &result)
	if result.Status != StatusBlocked {
		t.Fatalf("required unresolved issue did not downgrade ready status: %q", result.Status)
	}
	if err := ValidateResult(input, result); err != nil {
		t.Fatalf("downgraded partial result was rejected: %v", err)
	}

	optional := input
	optional.Requirements.Actions[0].Level = "optional"
	optional.Requirements.Actions[0].WhenFailed = "hide"
	result.Status = StatusReady
	NormalizeCandidateStatus(optional, &result)
	if result.Status != StatusReady {
		t.Fatalf("optional issue unexpectedly blocked publication: %q", result.Status)
	}
}

func TestBindingUpdateOnlyAcceptsAuthorizedRequirementDelta(t *testing.T) {
	input := validInput(t)
	input.EditContract = &edit.Contract{
		EditID: "edit_binding_1", SchemaVersion: edit.SchemaVersion,
		BaseGenerationID: "generation-1", BaseCardRevision: 2,
		ChangeScope: "binding_update", Operation: "update",
		TargetSet:    []edit.Target{{Kind: "data_requirement", ID: "req.food.title", Path: "binding"}},
		ProtectedSet: []edit.Protection{{Kind: "all_other_bindings", ID: "*"}},
		Preconditions: edit.Preconditions{
			ContentContractHash: input.Contract.ContentHash,
			DesignHash:          input.Design.ContentHash,
			SlotSignatureHash:   "sha256:slots",
			BindingPlanHash:     "sha256:binding",
		},
		IdempotencyKey: "session:generation:edit",
	}
	result := Result{
		SchemaVersion: ResultSchemaV1, Status: StatusReady,
		Bindings: []Binding{{
			RequirementID: "act.food.detail", TargetSlotIDs: []string{"slot.detail"},
			SourceID: "api:x@1", KnowledgeID: "knowledge:x@1", ActionPath: "$.detail",
		}},
	}
	if err := ValidateResult(input, result); err == nil {
		t.Fatal("expected edit scope violation")
	}
	result.Bindings = []Binding{{
		RequirementID: "req.food.title", TargetSlotIDs: []string{"slot.title"},
		SourceID: "api:food@1", KnowledgeID: "knowledge:food@1", FieldPath: "$.name",
	}}
	if err := ValidateResult(input, result); err != nil {
		t.Fatal(err)
	}
}

func validInput(t *testing.T) Input {
	t.Helper()
	draft := contract.Draft{
		Goal: "展示美食", Type: "list",
		Contents: []contract.ContentItem{{ID: "food.title", Description: "标题", Required: true}},
		Actions:  []contract.ActionItem{{ID: "food.detail", Description: "查看详情"}},
	}
	hash, err := contract.Hash(draft)
	if err != nil {
		t.Fatal(err)
	}
	return Input{
		SchemaVersion: InputSchemaV1,
		Contract: contract.Revision{
			ContractID: "contract-food", Revision: 1, SchemaVersion: contract.SchemaVersion,
			Status: "confirmed", ChangeOrigin: "user", ContentHash: hash, Draft: draft,
		},
		Design: DesignSnapshot{
			Ref: "artifact://design/1", ContentHash: "sha256:design",
			FieldHints:  json.RawMessage(`[{"slotId":"slot.title","contractItemId":"food.title","refKey":"/items[*]/title"}]`),
			ActionSlots: json.RawMessage(`[{"slotId":"slot.detail","contractActionId":"food.detail","componentId":"item"}]`),
		},
		Requirements: requirements.Set{
			SchemaVersion: requirements.SchemaVersion, InputHash: "sha256:req",
			Data: []requirements.DataRequirement{{
				RequirementID: "req.food.title", ContractItemID: "food.title", Description: "标题",
				TargetSlotIDs: []string{"slot.title"}, Level: "core", Type: "string", Shape: "list_item", WhenMissing: "block",
			}},
			Actions: []requirements.ActionRequirement{{
				RequirementID: "act.food.detail", ContractActionID: "food.detail", Description: "详情",
				TargetSlotIDs: []string{"slot.detail"}, Level: "important", WhenFailed: "hide",
			}},
		},
		Sources: []SourceSnapshot{{
			SourceID: "api:food@1", KnowledgeID: "knowledge:food@1", Primary: true,
		}},
	}
}
