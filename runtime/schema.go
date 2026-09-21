package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"unicode/utf8"
)

var supportedSchemaTypes = map[string]struct{}{
	"null": {}, "boolean": {}, "object": {}, "array": {},
	"number": {}, "integer": {}, "string": {},
}

var supportedSchemaKeywords = map[string]struct{}{
	"$schema": {}, "$id": {}, "title": {}, "description": {},
	"type": {}, "properties": {}, "required": {}, "additionalProperties": {},
	"items": {}, "anyOf": {}, "enum": {}, "pattern": {},
	"minLength": {}, "maxLength": {}, "minItems": {}, "maxItems": {},
	"minimum": {}, "maximum": {},
}

func validateSchemaDefinition(raw json.RawMessage) error {
	var schema any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return fmt.Errorf("schema must be valid JSON: %w", err)
	}
	return validateSchemaNodeDefinition(schema)
}

func validateSchemaNodeDefinition(schema any) error {
	if _, ok := schema.(bool); ok {
		return nil
	}
	object, ok := schema.(map[string]any)
	if !ok {
		return fmt.Errorf("schema must be an object or boolean")
	}
	for keyword := range object {
		if _, supported := supportedSchemaKeywords[keyword]; !supported {
			return fmt.Errorf("unsupported schema keyword %q", keyword)
		}
	}
	if value, exists := object["type"]; exists {
		switch typed := value.(type) {
		case string:
			if _, ok := supportedSchemaTypes[typed]; !ok {
				return fmt.Errorf("unsupported schema type %q", typed)
			}
		case []any:
			if len(typed) == 0 {
				return fmt.Errorf("schema type array is empty")
			}
			for _, entry := range typed {
				name, ok := entry.(string)
				if !ok {
					return fmt.Errorf("schema type array must contain strings")
				}
				if _, ok := supportedSchemaTypes[name]; !ok {
					return fmt.Errorf("unsupported schema type %q", name)
				}
			}
		default:
			return fmt.Errorf("schema type must be a string or string array")
		}
	}
	if properties, exists := object["properties"]; exists {
		propertyMap, ok := properties.(map[string]any)
		if !ok {
			return fmt.Errorf("schema properties must be an object")
		}
		for name, child := range propertyMap {
			if name == "" {
				return fmt.Errorf("schema property name is empty")
			}
			if err := validateSchemaNodeDefinition(child); err != nil {
				return fmt.Errorf("property %q: %w", name, err)
			}
		}
	}
	if required, exists := object["required"]; exists {
		values, ok := required.([]any)
		if !ok {
			return fmt.Errorf("schema required must be an array")
		}
		seen := make(map[string]struct{}, len(values))
		for _, value := range values {
			name, ok := value.(string)
			if !ok || name == "" {
				return fmt.Errorf("schema required must contain non-empty strings")
			}
			if _, duplicate := seen[name]; duplicate {
				return fmt.Errorf("schema required contains duplicate %q", name)
			}
			seen[name] = struct{}{}
		}
	}
	if additional, exists := object["additionalProperties"]; exists {
		if _, ok := additional.(bool); !ok {
			return fmt.Errorf("schema additionalProperties must be boolean")
		}
	}
	if enum, exists := object["enum"]; exists {
		values, ok := enum.([]any)
		if !ok || len(values) == 0 {
			return fmt.Errorf("schema enum must be a non-empty array")
		}
	}
	if items, exists := object["items"]; exists {
		if err := validateSchemaNodeDefinition(items); err != nil {
			return fmt.Errorf("items: %w", err)
		}
	}
	if alternatives, exists := object["anyOf"]; exists {
		values, ok := alternatives.([]any)
		if !ok || len(values) == 0 {
			return fmt.Errorf("schema anyOf must be a non-empty array")
		}
		for index, child := range values {
			if err := validateSchemaNodeDefinition(child); err != nil {
				return fmt.Errorf("anyOf[%d]: %w", index, err)
			}
		}
	}
	if pattern, exists := object["pattern"]; exists {
		text, ok := pattern.(string)
		if !ok {
			return fmt.Errorf("schema pattern must be a string")
		}
		if _, err := regexp.Compile(text); err != nil {
			return fmt.Errorf("schema pattern: %w", err)
		}
	}
	for _, keyword := range []string{"minLength", "maxLength", "minItems", "maxItems"} {
		if value, exists := object[keyword]; exists {
			number, ok := value.(float64)
			if !ok || number < 0 || math.Trunc(number) != number {
				return fmt.Errorf("schema %s must be a non-negative integer", keyword)
			}
		}
	}
	for _, keyword := range []string{"minimum", "maximum"} {
		if value, exists := object[keyword]; exists {
			if _, ok := value.(float64); !ok {
				return fmt.Errorf("schema %s must be a number", keyword)
			}
		}
	}
	return nil
}

func validateSchemaValue(raw json.RawMessage, value any) error {
	var schema any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return fmt.Errorf("schema must be valid JSON: %w", err)
	}
	if err := validateSchemaNodeDefinition(schema); err != nil {
		return err
	}
	normalized, err := normalizeJSONValue(value)
	if err != nil {
		return err
	}
	return validateSchemaNode(schema, normalized, "$")
}

func normalizeJSONValue(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var normalized any
	_ = decoder.Decode(&normalized) // json.Marshal above guarantees valid JSON.
	return normalized, nil
}

func validateSchemaNode(schema, value any, path string) error {
	if allowed, ok := schema.(bool); ok {
		if allowed {
			return nil
		}
		return fmt.Errorf("%s is rejected by schema", path)
	}
	object := schema.(map[string]any)
	if alternatives, exists := object["anyOf"].([]any); exists {
		for _, child := range alternatives {
			if validateSchemaNode(child, value, path) == nil {
				return nil
			}
		}
		return fmt.Errorf("%s does not match anyOf", path)
	}
	if expected, exists := object["type"]; exists && !matchesSchemaType(expected, value) {
		return fmt.Errorf("%s expected %v, got %s", path, expected, jsonValueType(value))
	}
	if enum, exists := object["enum"].([]any); exists {
		matched := false
		for _, candidate := range enum {
			if jsonValuesEqual(candidate, value) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s is not in enum", path)
		}
	}
	switch typed := value.(type) {
	case map[string]any:
		if err := validateObjectSchema(object, typed, path); err != nil {
			return err
		}
	case []any:
		if err := validateArraySchema(object, typed, path); err != nil {
			return err
		}
	case string:
		if err := validateStringSchema(object, typed, path); err != nil {
			return err
		}
	case json.Number:
		if err := validateNumberSchema(object, typed, path); err != nil {
			return err
		}
	}
	return nil
}

func matchesSchemaType(expected any, value any) bool {
	if name, ok := expected.(string); ok {
		return matchesOneSchemaType(name, value)
	}
	if values, ok := expected.([]any); ok {
		for _, entry := range values {
			if name, ok := entry.(string); ok && matchesOneSchemaType(name, value) {
				return true
			}
		}
	}
	return false
}

func matchesOneSchemaType(expected string, value any) bool {
	switch expected {
	case "null":
		return value == nil
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := value.(json.Number)
		return ok
	case "integer":
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		parsed, err := number.Float64()
		return err == nil && math.Trunc(parsed) == parsed
	default:
		return false
	}
}

func jsonValueType(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case json.Number:
		return "number"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func validateObjectSchema(schema map[string]any, value map[string]any, path string) error {
	if required, ok := schema["required"].([]any); ok {
		for _, entry := range required {
			name, ok := entry.(string)
			if !ok {
				return fmt.Errorf("schema required must contain strings")
			}
			if _, exists := value[name]; !exists {
				return fmt.Errorf("%s.%s is required", path, name)
			}
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	additional, hasAdditional := schema["additionalProperties"].(bool)
	for name, child := range value {
		propertySchema, exists := properties[name]
		if !exists {
			if hasAdditional && !additional {
				return fmt.Errorf("%s.%s is not allowed", path, name)
			}
			continue
		}
		if err := validateSchemaNode(propertySchema, child, path+"."+name); err != nil {
			return err
		}
	}
	return nil
}

func validateArraySchema(schema map[string]any, value []any, path string) error {
	if minimum, ok := schemaInteger(schema, "minItems"); ok && len(value) < minimum {
		return fmt.Errorf("%s has fewer than %d items", path, minimum)
	}
	if maximum, ok := schemaInteger(schema, "maxItems"); ok && len(value) > maximum {
		return fmt.Errorf("%s has more than %d items", path, maximum)
	}
	if itemSchema, exists := schema["items"]; exists {
		for index, item := range value {
			if err := validateSchemaNode(itemSchema, item, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateStringSchema(schema map[string]any, value, path string) error {
	length := utf8.RuneCountInString(value)
	if minimum, ok := schemaInteger(schema, "minLength"); ok && length < minimum {
		return fmt.Errorf("%s is shorter than %d", path, minimum)
	}
	if maximum, ok := schemaInteger(schema, "maxLength"); ok && length > maximum {
		return fmt.Errorf("%s is longer than %d", path, maximum)
	}
	if pattern, ok := schema["pattern"].(string); ok {
		matched, err := regexp.MatchString(pattern, value)
		if err != nil || !matched {
			return fmt.Errorf("%s does not match pattern", path)
		}
	}
	return nil
}

func validateNumberSchema(schema map[string]any, value json.Number, path string) error {
	number, err := value.Float64()
	if err != nil {
		return fmt.Errorf("%s is not a valid number", path)
	}
	if minimum, ok := schemaNumber(schema, "minimum"); ok && number < minimum {
		return fmt.Errorf("%s is less than %s", path, strconv.FormatFloat(minimum, 'f', -1, 64))
	}
	if maximum, ok := schemaNumber(schema, "maximum"); ok && number > maximum {
		return fmt.Errorf("%s is greater than %s", path, strconv.FormatFloat(maximum, 'f', -1, 64))
	}
	return nil
}

func schemaInteger(schema map[string]any, key string) (int, bool) {
	value, ok := schema[key].(float64)
	return int(value), ok && value >= 0 && math.Trunc(value) == value
}

func schemaNumber(schema map[string]any, key string) (float64, bool) {
	value, ok := schema[key].(float64)
	return value, ok
}

func jsonValuesEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}
