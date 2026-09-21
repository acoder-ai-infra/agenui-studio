package contract

import "testing"

func TestReconcileExecutablePlanRejectsUnknownTarget(t *testing.T) {
	input := validInput(t)
	result := validResult(input)
	candidate := `{"schema_version":"agenui.executable-binding/v1","field_mappings":[{"refKey":"/unknown","sourceKey":"$.title"}],"action_mappings":[]}`
	if _, err := ReconcileExecutablePlan(input, result, candidate); err == nil {
		t.Fatal("expected unknown target rejection")
	}
}

func validResult(input Input) Result {
	result := Result{SchemaVersion: ResultSchemaV1, Status: StatusReady}
	for _, requirement := range input.Requirements.Data {
		result.Bindings = append(result.Bindings, Binding{
			RequirementID: requirement.RequirementID, TargetSlotIDs: requirement.TargetSlotIDs,
			SourceID: input.Sources[0].SourceID, KnowledgeID: input.Sources[0].KnowledgeID,
			FieldPath: "$.title",
		})
	}
	for _, requirement := range input.Requirements.Actions {
		result.Bindings = append(result.Bindings, Binding{
			RequirementID: requirement.RequirementID, TargetSlotIDs: requirement.TargetSlotIDs,
			SourceID: input.Sources[0].SourceID, KnowledgeID: input.Sources[0].KnowledgeID,
			ActionPath: "$.action", ActionSourceType: "event",
		})
	}
	return result
}

func validReadyResult() Result {
	return Result{
		SchemaVersion: ResultSchemaV1,
		Status:        StatusReady,
		Bindings: []Binding{
			{RequirementID: "req.food.title", TargetSlotIDs: []string{"slot.title"}, SourceID: "api:food@1", KnowledgeID: "knowledge:food@1", FieldPath: "$.title"},
			{RequirementID: "act.food.detail", TargetSlotIDs: []string{"slot.detail"}, SourceID: "api:food@1", KnowledgeID: "knowledge:food@1", ActionPath: "$.action", ActionSourceType: "event"},
		},
	}
}
