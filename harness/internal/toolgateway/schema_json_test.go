package toolgateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestJSONSchemaValidatorRejectsExternalReferences(t *testing.T) {
	validator := NewJSONSchemaValidator(8)
	err := validator.Validate(context.Background(), json.RawMessage(`{"$ref":"https://example.invalid/schema.json"}`), json.RawMessage(`{}`))
	if !IsErrorType(err, ErrorTypeSchemaValidationFailed) {
		t.Fatalf("external reference error = %v", err)
	}
}

func TestSchemaValidationRepairMessageIdentifiesSelectedUnionRequiredFields(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","oneOf":[{"type":"object","required":["action","field_slots","action_slots"],"properties":{"action":{"const":"set_slots"},"field_slots":{"type":"array"},"action_slots":{"type":"array"}}},{"type":"object","required":["action","base_revision"],"properties":{"action":{"const":"commit"},"base_revision":{"type":"string"}}}]}`)
	arguments := json.RawMessage(`{"action":"set_slots","field_slots":[]}`)
	validator := NewJSONSchemaValidator(8)
	err := validator.Validate(context.Background(), schema, arguments)
	if err == nil {
		t.Fatal("expected validation failure")
	}
	message := schemaValidationRepairMessage(schema, arguments, err)
	if !strings.Contains(message, "/action_slots") || strings.Contains(message, "/base_revision") {
		t.Fatalf("repair message = %q", message)
	}
}

func TestJSONSchemaValidatorEnforcesInputBounds(t *testing.T) {
	validator := NewJSONSchemaValidator(8)
	oversizedSchema := json.RawMessage(`{"type":"string","description":"` + strings.Repeat("x", maxJSONSchemaBytes) + `"}`)
	if err := validator.Validate(context.Background(), oversizedSchema, json.RawMessage(`"ok"`)); !IsErrorType(err, ErrorTypeSchemaValidationFailed) {
		t.Fatalf("oversized schema error = %v", err)
	}
	oversizedValue := json.RawMessage(`"` + strings.Repeat("x", maxJSONValueBytes) + `"`)
	if err := validator.Validate(context.Background(), json.RawMessage(`{"type":"string"}`), oversizedValue); !IsErrorType(err, ErrorTypeSchemaValidationFailed) {
		t.Fatalf("oversized value error = %v", err)
	}
}

func TestJSONSchemaValidatorHonorsCancelledContextBeforeWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewJSONSchemaValidator(8).Validate(ctx, json.RawMessage(`{"type":"object"}`), json.RawMessage(`{}`)); err != context.Canceled {
		t.Fatalf("cancelled validation error = %v", err)
	}
}

func TestCoerceArgumentsToSchemaUnwrapsStringifiedArray(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"required":["concepts"],
		"properties":{
			"concepts":{"type":"array","minItems":1,"items":{"type":"string"}},
			"top_k":{"type":"integer","minimum":1,"maximum":50}
		},
		"additionalProperties":false
	}`)
	// The model emitted the array parameter as a JSON-encoded string.
	arguments := json.RawMessage(`{"concepts":"[\"等级体系\", \"成长值\"]","top_k":15}`)

	coerced, changed := coerceArgumentsToSchema(schema, arguments)
	if !changed {
		t.Fatalf("expected arguments to be coerced")
	}
	if err := NewJSONSchemaValidator(8).Validate(context.Background(), schema, coerced); err != nil {
		t.Fatalf("coerced arguments must satisfy the schema: %v", err)
	}
	var decoded struct {
		Concepts []string `json:"concepts"`
		TopK     int      `json:"top_k"`
	}
	if err := json.Unmarshal(coerced, &decoded); err != nil {
		t.Fatalf("decode coerced arguments: %v", err)
	}
	if len(decoded.Concepts) != 2 || decoded.Concepts[0] != "等级体系" || decoded.TopK != 15 {
		t.Fatalf("unexpected coerced arguments: %s", coerced)
	}
}

func TestCoerceArgumentsToSchemaUnwrapsStringifiedArrayInSelectedOneOfBranch(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"oneOf":[
			{"type":"object","required":["action"],"properties":{"action":{"const":"inspect"}},"additionalProperties":false},
			{"type":"object","required":["action","requirement_ids"],"properties":{"action":{"const":"authorize_binding_edit"},"requirement_ids":{"type":"array","minItems":1,"items":{"type":"string"}}},"additionalProperties":false}
		]
	}`)
	arguments := json.RawMessage(`{"action":"authorize_binding_edit","requirement_ids":"[\"data.name\",\"data.price\"]"}`)

	coerced, changed := coerceArgumentsToSchema(schema, arguments)
	if !changed {
		t.Fatal("expected the selected oneOf branch to drive coercion")
	}
	if err := NewJSONSchemaValidator(8).Validate(context.Background(), schema, coerced); err != nil {
		t.Fatalf("coerced arguments must satisfy the selected branch: %v", err)
	}
	var decoded struct {
		Action         string   `json:"action"`
		RequirementIDs []string `json:"requirement_ids"`
	}
	if err := json.Unmarshal(coerced, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Action != "authorize_binding_edit" || len(decoded.RequirementIDs) != 2 {
		t.Fatalf("unexpected coerced arguments: %s", coerced)
	}
}

func TestCoerceArgumentsToSchemaLeavesValidArgumentsUntouched(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"concepts":{"type":"array","items":{"type":"string"}},"project_name":{"type":"string"}}}`)
	arguments := json.RawMessage(`{"concepts":["a","b"],"project_name":"[not-an-array]"}`)

	coerced, changed := coerceArgumentsToSchema(schema, arguments)
	if changed {
		t.Fatalf("well-typed arguments must not be rewritten, got %s", coerced)
	}
}

func TestCoerceArgumentsToSchemaSkipsStringWhoseContentIsNotExpectedType(t *testing.T) {
	// concepts is an array; the string content parses as an object, not an array.
	schema := json.RawMessage(`{"type":"object","properties":{"concepts":{"type":"array","items":{"type":"string"}}}}`)
	arguments := json.RawMessage(`{"concepts":"{\"k\":1}"}`)

	if _, changed := coerceArgumentsToSchema(schema, arguments); changed {
		t.Fatalf("must not unwrap a string whose content does not match the expected type")
	}
}
