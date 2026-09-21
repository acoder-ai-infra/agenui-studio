package runtime

import (
	"encoding/json"
	"math"
	"testing"
)

func TestSchemaDefinitionValidation(t *testing.T) {
	for _, raw := range []string{
		`true`, `false`, `{}`, `{"type":"string"}`, `{"type":["string","null"]}`,
		`{"type":"object","properties":{"name":{"type":"string"}}}`,
		`{"type":"array","items":{"type":"number"}}`,
		`{"anyOf":[{"type":"string"},{"type":"number"}]}`,
		`{"type":"string","pattern":"^[a-z]+$"}`,
	} {
		if err := validateSchemaDefinition(schema(raw)); err != nil {
			t.Fatalf("valid schema %s: %v", raw, err)
		}
	}
	for _, raw := range []string{
		``, `{`, `1`, `{"type":"unknown"}`, `{"type":[]}`, `{"type":[1]}`,
		`{"type":["unknown"]}`, `{"type":1}`, `{"properties":[]}`,
		`{"properties":{"":{"type":"string"}}}`, `{"properties":{"x":1}}`,
		`{"items":1}`, `{"anyOf":[]}`, `{"anyOf":{}}`, `{"anyOf":[1]}`,
		`{"pattern":1}`, `{"pattern":"["}`, `{"unknownKeyword":true}`,
		`{"required":"name"}`, `{"required":[1]}`, `{"required":[""]}`,
		`{"required":["name","name"]}`, `{"additionalProperties":{}}`,
		`{"enum":"x"}`, `{"enum":[]}`,
		`{"minLength":-1}`, `{"maxLength":1.5}`, `{"minItems":"1"}`,
		`{"maxItems":-1}`, `{"minimum":"0"}`, `{"maximum":true}`,
	} {
		if err := validateSchemaDefinition(schema(raw)); err == nil {
			t.Fatalf("invalid schema %s succeeded", raw)
		}
	}
}

func TestSchemaValueTypesAndAnyOf(t *testing.T) {
	valid := []struct {
		schema string
		value  any
	}{
		{`{"type":"null"}`, nil}, {`{"type":"boolean"}`, true},
		{`{"type":"object"}`, map[string]any{}}, {`{"type":"array"}`, []any{}},
		{`{"type":"string"}`, "x"}, {`{"type":"number"}`, 1.5},
		{`{"type":"integer"}`, 2}, {`{"type":["string","null"]}`, nil},
		{`{"anyOf":[{"type":"string"},{"type":"number"}]}`, 2},
		{`{"enum":["a","b"]}`, "a"}, {`true`, map[string]any{"x": 1}},
	}
	for _, test := range valid {
		if err := validateSchemaValue(schema(test.schema), test.value); err != nil {
			t.Fatalf("schema=%s value=%#v: %v", test.schema, test.value, err)
		}
	}
	invalid := []struct {
		schema string
		value  any
	}{
		{`false`, "x"}, {`{"type":"null"}`, false}, {`{"type":"boolean"}`, 1},
		{`{"type":"object"}`, []any{}}, {`{"type":"array"}`, map[string]any{}},
		{`{"type":"string"}`, 1}, {`{"type":"number"}`, "1"},
		{`{"type":"integer"}`, 1.5}, {`{"type":["string","null"]}`, true},
		{`{"anyOf":[{"type":"string"},{"type":"number"}]}`, true},
		{`{"enum":["a","b"]}`, "c"},
	}
	for _, test := range invalid {
		if err := validateSchemaValue(schema(test.schema), test.value); err == nil {
			t.Fatalf("schema=%s value=%#v succeeded", test.schema, test.value)
		}
	}
	if _, err := normalizeJSONValue(math.NaN()); err == nil {
		t.Fatal("NaN normalized")
	}
}

func TestObjectSchemaValidation(t *testing.T) {
	raw := schema(`{"type":"object","required":["name"],"properties":{"name":{"type":"string"},"age":{"type":"integer"}},"additionalProperties":false}`)
	if err := validateSchemaValue(raw, map[string]any{"name": "a", "age": 2}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{
		map[string]any{"age": 2}, map[string]any{"name": "a", "extra": true}, map[string]any{"name": "a", "age": "2"},
	} {
		if err := validateSchemaValue(raw, value); err == nil {
			t.Fatalf("value=%#v succeeded", value)
		}
	}
	if err := validateObjectSchema(map[string]any{"required": []any{1.0}}, map[string]any{}, "$"); err == nil {
		t.Fatal("non-string required succeeded")
	}
}

func TestArrayStringAndNumberSchemaValidation(t *testing.T) {
	arraySchema := schema(`{"type":"array","minItems":1,"maxItems":2,"items":{"type":"integer"}}`)
	if err := validateSchemaValue(arraySchema, []any{1, 2}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{[]any{}, []any{1, 2, 3}, []any{1, "x"}} {
		if err := validateSchemaValue(arraySchema, value); err == nil {
			t.Fatalf("array=%#v succeeded", value)
		}
	}
	stringSchema := schema(`{"type":"string","minLength":2,"maxLength":4,"pattern":"^[a-z]+$"}`)
	if err := validateSchemaValue(stringSchema, "abcd"); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"a", "abcde", "AB"} {
		if err := validateSchemaValue(stringSchema, value); err == nil {
			t.Fatalf("string=%q succeeded", value)
		}
	}
	numberSchema := schema(`{"type":"number","minimum":1,"maximum":3}`)
	if err := validateSchemaValue(numberSchema, 2); err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{0, 4} {
		if err := validateSchemaValue(numberSchema, value); err == nil {
			t.Fatalf("number=%v succeeded", value)
		}
	}
	if err := validateNumberSchema(map[string]any{}, json.Number("bad"), "$"); err == nil {
		t.Fatal("bad number succeeded")
	}
}

func TestSchemaHelperEdges(t *testing.T) {
	if err := validateSchemaValue(schema(`{`), 1); err == nil {
		t.Fatal("invalid runtime schema succeeded")
	}
	if err := validateSchemaValue(schema(`{"type":"bad"}`), 1); err == nil {
		t.Fatal("unsupported runtime schema succeeded")
	}
	if err := validateSchemaValue(schema(`{"type":"number"}`), math.NaN()); err == nil {
		t.Fatal("NaN runtime value succeeded")
	}
	if _, ok := schemaInteger(map[string]any{"n": -1.0}, "n"); ok {
		t.Fatal("negative schema integer accepted")
	}
	if _, ok := schemaInteger(map[string]any{"n": 1.5}, "n"); ok {
		t.Fatal("fractional schema integer accepted")
	}
	if _, ok := schemaInteger(map[string]any{}, "n"); ok {
		t.Fatal("missing schema integer accepted")
	}
	if _, ok := schemaNumber(map[string]any{}, "n"); ok {
		t.Fatal("missing schema number accepted")
	}
	if jsonValueType(make(chan int)) == "" {
		t.Fatal("unknown type empty")
	}
	for _, value := range []any{nil, true, map[string]any{}, []any{}, "x", json.Number("1")} {
		if jsonValueType(value) == "" {
			t.Fatalf("empty type for %#v", value)
		}
	}
	if matchesOneSchemaType("integer", json.Number("bad")) || matchesOneSchemaType("unknown", nil) {
		t.Fatal("invalid schema type matched")
	}
	if err := validateObjectSchema(map[string]any{}, map[string]any{"extra": true}, "$"); err != nil {
		t.Fatal(err)
	}
	if jsonValuesEqual(make(chan int), make(chan int)) {
		t.Fatal("unserializable values equal")
	}
}
