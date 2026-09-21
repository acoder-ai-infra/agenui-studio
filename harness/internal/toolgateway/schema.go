package toolgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	maxJSONSchemaBytes = 1 << 20
	maxJSONValueBytes  = 16 << 20
)

type SchemaValidator interface {
	Validate(ctx context.Context, schema json.RawMessage, data json.RawMessage) error
}

// schemaValidationRepairMessage exposes only schema paths and rule names. It
// never echoes model-supplied values, so the feedback is safe to place in the
// next model turn while the original validation error remains internal.
func schemaValidationRepairMessage(schema, arguments json.RawMessage, validationErr error) string {
	missing := missingRequiredFields(schema, arguments)
	if len(missing) > 0 {
		return "工具参数缺少必填字段：" + strings.Join(missing, "、") + "。请保留已正确参数，仅补齐这些字段后重试一次。"
	}
	var schemaErr *jsonschema.ValidationError
	if errors.As(validationErr, &schemaErr) {
		output := schemaErr.BasicOutput()
		if output != nil {
			locations := make([]string, 0, len(output.Errors))
			for _, item := range output.Errors {
				location := item.InstanceLocation
				if location == "" {
					location = "/"
				}
				keyword := item.KeywordLocation
				if keyword != "" {
					location += " (规则 " + keyword + ")"
				}
				locations = appendUniqueString(locations, location)
				if len(locations) == 4 {
					break
				}
			}
			if len(locations) > 0 {
				return "工具参数不符合 canonical schema，位置：" + strings.Join(locations, "、") + "。请仅修正这些参数后重试一次。"
			}
		}
	}
	return "工具参数不符合 canonical schema。请检查必填字段和字段类型，仅修正参数后重试一次。"
}

func missingRequiredFields(schema, arguments json.RawMessage) []string {
	var schemaDoc map[string]any
	var args map[string]any
	if json.Unmarshal(schema, &schemaDoc) != nil || json.Unmarshal(arguments, &args) != nil {
		return nil
	}
	required := stringValues(schemaDoc["required"])
	if branches, ok := schemaDoc["oneOf"].([]any); ok {
		for _, rawBranch := range branches {
			branch, _ := rawBranch.(map[string]any)
			properties, _ := branch["properties"].(map[string]any)
			if branchMatchesArguments(properties, args) {
				required = append(required, stringValues(branch["required"])...)
				break
			}
		}
	}
	missing := make([]string, 0, len(required))
	seen := map[string]struct{}{}
	for _, name := range required {
		if _, exists := args[name]; exists {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		missing = append(missing, "/"+escapeJSONPointerToken(name))
	}
	sort.Strings(missing)
	return missing
}

func branchMatchesArguments(properties map[string]any, arguments map[string]any) bool {
	for name, raw := range properties {
		property, _ := raw.(map[string]any)
		constant, hasConstant := property["const"]
		if !hasConstant {
			continue
		}
		if value, exists := arguments[name]; !exists || fmt.Sprint(value) != fmt.Sprint(constant) {
			return false
		}
		return true
	}
	return false
}

func stringValues(value any) []string {
	raw, _ := value.([]any)
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func escapeJSONPointerToken(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

type BasicSchemaValidator struct{}

type JSONSchemaValidator struct {
	mu         sync.Mutex
	compiled   map[string]*jsonschema.Schema
	order      []string
	maxEntries int
}

func NewJSONSchemaValidator(maxEntries int) *JSONSchemaValidator {
	if maxEntries <= 0 {
		maxEntries = 1024
	}
	return &JSONSchemaValidator{compiled: make(map[string]*jsonschema.Schema), maxEntries: maxEntries}
}

func (v *JSONSchemaValidator) Validate(ctx context.Context, schema json.RawMessage, data json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(bytes.TrimSpace(schema)) == 0 {
		return nil
	}
	if len(schema) > maxJSONSchemaBytes {
		return NewToolError(ErrorTypeSchemaValidationFailed, "JSON schema exceeds size limit", false, nil)
	}
	if len(data) > maxJSONValueBytes {
		return NewToolError(ErrorTypeSchemaValidationFailed, "JSON value exceeds validation size limit", false, nil)
	}
	compiled, err := v.compile(schema)
	if err != nil {
		return NewToolError(ErrorTypeSchemaValidationFailed, "invalid JSON schema", false, err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return NewToolError(ErrorTypeSchemaValidationFailed, "invalid JSON value", false, err)
	}
	if err := compiled.Validate(value); err != nil {
		return NewToolError(ErrorTypeSchemaValidationFailed, "JSON value does not match schema", false, err)
	}
	return nil
}

func (v *JSONSchemaValidator) compile(raw json.RawMessage) (*jsonschema.Schema, error) {
	key := hashString(string(raw))
	v.mu.Lock()
	defer v.mu.Unlock()
	if compiled, ok := v.compiled[key]; ok {
		return compiled, nil
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(rejectExternalSchemaLoader{})
	location := "urn:harness:tool-schema:" + key
	if err := compiler.AddResource(location, document); err != nil {
		return nil, err
	}
	compiled, err := compiler.Compile(location)
	if err != nil {
		return nil, err
	}
	v.compiled[key] = compiled
	v.order = append(v.order, key)
	for len(v.order) > v.maxEntries {
		oldest := v.order[0]
		v.order = v.order[1:]
		delete(v.compiled, oldest)
	}
	return compiled, nil
}

type rejectExternalSchemaLoader struct{}

func (rejectExternalSchemaLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external JSON schema reference is not allowed: %s", url)
}

type basicSchema struct {
	Type                 string                 `json:"type"`
	Required             []string               `json:"required"`
	Properties           map[string]basicSchema `json:"properties"`
	AdditionalProperties *bool                  `json:"additionalProperties"`
}

// coerceArgumentsToSchema repairs a common tool-call defect where a model emits
// a non-string parameter (array/object/number/integer/boolean) as a JSON-encoded
// string, e.g. {"concepts":"[\"a\",\"b\"]"} instead of {"concepts":["a","b"]}.
// For each top-level property whose schema declares a single non-string type,
// when the supplied value is a JSON string whose contents parse into a value of
// that expected type, the string is unwrapped. Every other value is left
// untouched, and on any structural mismatch the original arguments are returned
// unchanged so that normal schema validation still surfaces the real error.
func coerceArgumentsToSchema(schema, arguments json.RawMessage) (json.RawMessage, bool) {
	if len(bytes.TrimSpace(schema)) == 0 || len(bytes.TrimSpace(arguments)) == 0 {
		return arguments, false
	}
	var schemaDoc struct {
		Properties map[string]json.RawMessage `json:"properties"`
		OneOf      []json.RawMessage          `json:"oneOf"`
	}
	if err := json.Unmarshal(schema, &schemaDoc); err != nil {
		return arguments, false
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &args); err != nil || len(args) == 0 {
		return arguments, false
	}
	properties := make(map[string]json.RawMessage, len(schemaDoc.Properties))
	for name, property := range schemaDoc.Properties {
		properties[name] = property
	}
	for name, property := range selectedOneOfProperties(schemaDoc.OneOf, arguments) {
		properties[name] = property
	}
	if len(properties) == 0 {
		return arguments, false
	}
	changed := false
	for name, raw := range args {
		propSchema, ok := properties[name]
		if !ok {
			continue
		}
		expected := schemaPropertyType(propSchema)
		if expected == "" || expected == "string" {
			continue
		}
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil {
			continue // value is not a JSON string; nothing to unwrap
		}
		inner := json.RawMessage(bytes.TrimSpace([]byte(encoded)))
		if !json.Valid(inner) || !jsonValueMatchesType(inner, expected) {
			continue
		}
		args[name] = inner
		changed = true
	}
	if !changed {
		return arguments, false
	}
	repaired, err := json.Marshal(args)
	if err != nil {
		return arguments, false
	}
	return repaired, true
}

// selectedOneOfProperties returns the properties of the union branch selected
// by a const discriminator already present in the arguments. This keeps
// coercion aligned with canonical oneOf schemas such as
// {action:"authorize_binding_edit"} without weakening validation for any
// other branch.
func selectedOneOfProperties(branches []json.RawMessage, arguments json.RawMessage) map[string]json.RawMessage {
	if len(branches) == 0 {
		return nil
	}
	var args map[string]any
	if json.Unmarshal(arguments, &args) != nil {
		return nil
	}
	for _, rawBranch := range branches {
		var branch map[string]any
		if json.Unmarshal(rawBranch, &branch) != nil {
			continue
		}
		properties, _ := branch["properties"].(map[string]any)
		if !oneOfBranchMatchesArguments(properties, args) {
			continue
		}
		var typed struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if json.Unmarshal(rawBranch, &typed) == nil {
			return typed.Properties
		}
	}
	return nil
}

func oneOfBranchMatchesArguments(properties map[string]any, arguments map[string]any) bool {
	for name, raw := range properties {
		property, _ := raw.(map[string]any)
		constant, hasConstant := property["const"]
		if !hasConstant {
			continue
		}
		value, exists := arguments[name]
		return exists && fmt.Sprint(value) == fmt.Sprint(constant)
	}
	return false
}

// schemaPropertyType returns the single JSON Schema type declared by a property,
// or "" when the type is absent or a union (which is left untouched).
func schemaPropertyType(raw json.RawMessage) string {
	var doc struct {
		Type json.RawMessage `json:"type"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Type) == 0 {
		return ""
	}
	var single string
	if err := json.Unmarshal(doc.Type, &single); err == nil {
		return single
	}
	return ""
}

// jsonValueMatchesType reports whether raw decodes to a JSON value of the given
// schema type. Numbers are decoded with UseNumber so integers stay exact.
func jsonValueMatchesType(raw json.RawMessage, expected string) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	switch expected {
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		_, ok := value.(json.Number)
		return ok
	case "integer":
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		_, err := number.Int64()
		return err == nil
	default:
		return false
	}
}

func (v BasicSchemaValidator) Validate(_ context.Context, schema json.RawMessage, data json.RawMessage) error {
	if len(bytes.TrimSpace(schema)) == 0 {
		return nil
	}
	parsed, err := parseBasicSchema(schema)
	if err != nil {
		return NewToolError(ErrorTypeSchemaValidationFailed, "invalid input schema", false, err)
	}
	value, err := decodeJSON(data)
	if err != nil {
		return NewToolError(ErrorTypeSchemaValidationFailed, "invalid JSON arguments", false, err)
	}
	if err := validateValue(parsed, value, "$"); err != nil {
		return NewToolError(ErrorTypeSchemaValidationFailed, err.Error(), false, nil)
	}
	return nil
}

func parseBasicSchema(data json.RawMessage) (basicSchema, error) {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return basicSchema{}, err
	}
	if err := validateBasicSchemaObject(raw, "$"); err != nil {
		return basicSchema{}, err
	}
	var parsed basicSchema
	if err := json.Unmarshal(data, &parsed); err != nil {
		return basicSchema{}, err
	}
	return parsed, nil
}

func validateBasicSchemaObject(value any, path string) error {
	object, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%s must be a schema object", path)
	}
	for key, field := range object {
		switch key {
		case "type":
			typeName, ok := field.(string)
			if !ok || !isSupportedBasicSchemaType(typeName) {
				return fmt.Errorf("%s.type is unsupported", path)
			}
		case "required":
			values, ok := field.([]any)
			if !ok {
				return fmt.Errorf("%s.required must be an array", path)
			}
			for _, item := range values {
				if _, ok := item.(string); !ok {
					return fmt.Errorf("%s.required must contain strings", path)
				}
			}
		case "properties":
			properties, ok := field.(map[string]any)
			if !ok {
				return fmt.Errorf("%s.properties must be an object", path)
			}
			for name, property := range properties {
				if err := validateBasicSchemaObject(property, path+".properties."+name); err != nil {
					return err
				}
			}
		case "additionalProperties":
			if _, ok := field.(bool); !ok {
				return fmt.Errorf("%s.additionalProperties must be boolean", path)
			}
		case "title", "description", "$comment":
			if _, ok := field.(string); !ok {
				return fmt.Errorf("%s.%s must be string", path, key)
			}
		case "deprecated", "readOnly", "writeOnly":
			if _, ok := field.(bool); !ok {
				return fmt.Errorf("%s.%s must be boolean", path, key)
			}
		case "default", "examples":
			// Annotation-only fields do not change validation semantics.
		default:
			return fmt.Errorf("%s.%s is unsupported by BasicSchemaValidator", path, key)
		}
	}
	return nil
}

func isSupportedBasicSchemaType(value string) bool {
	switch value {
	case "object", "array", "string", "boolean", "number", "integer":
		return true
	default:
		return false
	}
}

func decodeJSON(data json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values are not allowed")
		}
		return nil, err
	}
	return value, nil
}

func validateValue(schema basicSchema, value any, path string) error {
	if schema.Type != "" {
		if err := validateType(schema.Type, value, path); err != nil {
			return err
		}
	}
	if schema.Type == "object" || len(schema.Properties) > 0 || len(schema.Required) > 0 || schema.AdditionalProperties != nil {
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be object", path)
		}
		for _, field := range schema.Required {
			if _, ok := object[field]; !ok {
				return fmt.Errorf("%s.%s is required", path, field)
			}
		}
		if schema.AdditionalProperties != nil && !*schema.AdditionalProperties {
			for field := range object {
				if _, ok := schema.Properties[field]; !ok {
					return fmt.Errorf("%s.%s is not allowed", path, field)
				}
			}
		}
		for field, propertySchema := range schema.Properties {
			fieldValue, ok := object[field]
			if !ok {
				continue
			}
			if err := validateValue(propertySchema, fieldValue, path+"."+field); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateType(schemaType string, value any, path string) error {
	switch schemaType {
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("%s must be object", path)
		}
	case "array":
		if _, ok := value.([]any); !ok {
			return fmt.Errorf("%s must be array", path)
		}
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s must be string", path)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be boolean", path)
		}
	case "number":
		if _, ok := value.(json.Number); !ok {
			return fmt.Errorf("%s must be number", path)
		}
	case "integer":
		number, ok := value.(json.Number)
		if !ok {
			return fmt.Errorf("%s must be integer", path)
		}
		if _, err := strconv.ParseInt(number.String(), 10, 64); err != nil {
			return fmt.Errorf("%s must be integer", path)
		}
	default:
		return fmt.Errorf("%s uses unsupported type %q", path, schemaType)
	}
	return nil
}
