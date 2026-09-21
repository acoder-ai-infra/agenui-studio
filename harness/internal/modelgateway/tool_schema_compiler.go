package modelgateway

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

const (
	ToolSchemaCompilerVersion         = "tool-schema-compiler.v2"
	ToolSchemaDiscriminatorAnnotation = "x-harness-discriminator"
)

// ToolSchemaCompiler lowers a canonical tool input schema into one provider's
// model-facing wire contract. Runtime invocation always validates against the
// untouched canonical schema; compilation must never replace Host validation.
type ToolSchemaCompiler interface {
	Provider() string
	Version() string
	Compile(json.RawMessage) (json.RawMessage, error)
}

// IdentityToolSchemaCompiler validates JSON and preserves canonical schema
// semantics for providers that accept the repository's native JSON Schema.
type IdentityToolSchemaCompiler struct {
	provider string
}

func NewIdentityToolSchemaCompiler(provider string) IdentityToolSchemaCompiler {
	return IdentityToolSchemaCompiler{provider: provider}
}

func (c IdentityToolSchemaCompiler) Provider() string { return c.provider }
func (IdentityToolSchemaCompiler) Version() string    { return ToolSchemaCompilerVersion }

func (c IdentityToolSchemaCompiler) Compile(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil, fmt.Errorf("%s tool schema must be valid JSON", c.provider)
	}
	var schema map[string]any
	if json.Unmarshal(raw, &schema) != nil {
		return nil, fmt.Errorf("%s tool schema must be a JSON object", c.provider)
	}
	if _, exists := schema["x-harness-discriminator"]; !exists {
		return append(json.RawMessage(nil), raw...), nil
	}
	delete(schema, "x-harness-discriminator")
	return json.Marshal(schema)
}

// DiscriminatedObjectUnionToolSchemaCompiler lowers the Harness canonical
// object-union form into the common model-facing JSON Schema subset accepted
// by both OpenAI-compatible and Anthropic tool transports. The untouched
// canonical schema remains authoritative for invocation-time validation.
type DiscriminatedObjectUnionToolSchemaCompiler struct {
	provider string
}

func NewDiscriminatedObjectUnionToolSchemaCompiler(provider string) DiscriminatedObjectUnionToolSchemaCompiler {
	return DiscriminatedObjectUnionToolSchemaCompiler{provider: strings.TrimSpace(provider)}
}

func (c DiscriminatedObjectUnionToolSchemaCompiler) Provider() string { return c.provider }
func (DiscriminatedObjectUnionToolSchemaCompiler) Version() string {
	return ToolSchemaCompilerVersion
}

func (c DiscriminatedObjectUnionToolSchemaCompiler) Compile(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil, fmt.Errorf("%s tool schema must be valid JSON", c.provider)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil || schema["type"] != "object" {
		return nil, fmt.Errorf("%s tool schema must declare type object", c.provider)
	}
	if _, hasAnyOf := schema["anyOf"]; hasAnyOf {
		return nil, fmt.Errorf("%s tool schema top-level anyOf is unsupported", c.provider)
	}
	if _, hasAllOf := schema["allOf"]; hasAllOf {
		return nil, fmt.Errorf("%s tool schema top-level allOf is unsupported", c.provider)
	}

	discriminator, annotated := schema[ToolSchemaDiscriminatorAnnotation].(string)
	discriminator = strings.TrimSpace(discriminator)
	branchesValue, hasOneOf := schema["oneOf"]
	if !hasOneOf {
		if !annotated {
			return append(json.RawMessage(nil), raw...), nil
		}
		delete(schema, ToolSchemaDiscriminatorAnnotation)
		return json.Marshal(schema)
	}
	if discriminator == "" {
		return nil, fmt.Errorf("%s tool schema top-level oneOf requires %s", c.provider, ToolSchemaDiscriminatorAnnotation)
	}
	branches, ok := branchesValue.([]any)
	if !ok || len(branches) == 0 {
		return nil, fmt.Errorf("%s tool schema top-level oneOf must contain object branches", c.provider)
	}

	properties := map[string]any{}
	requiredCounts := map[string]int{}
	propertyActions := map[string][]string{}
	requiredActions := map[string][]string{}
	operationContracts := make([]string, 0, len(branches))
	seenVariants := map[string]struct{}{}
	additionalPropertiesFalse := true
	for _, value := range branches {
		branch, ok := value.(map[string]any)
		if !ok || branch["type"] != "object" {
			return nil, fmt.Errorf("%s tool schema top-level oneOf branch must declare type object", c.provider)
		}
		branchProperties, ok := branch["properties"].(map[string]any)
		if !ok {
			branchProperties = map[string]any{}
		}
		action := toolSchemaBranchDiscriminator(branchProperties, discriminator)
		if action == "" {
			return nil, fmt.Errorf("%s tool schema oneOf branch must declare string %s.const", c.provider, discriminator)
		}
		if _, duplicate := seenVariants[action]; duplicate {
			return nil, fmt.Errorf("%s tool schema has duplicate %s discriminator value %q", c.provider, discriminator, action)
		}
		seenVariants[action] = struct{}{}
		for name, propertySchema := range branchProperties {
			if existing, exists := properties[name]; exists {
				properties[name] = mergeToolPropertySchemas(existing, propertySchema)
			} else {
				properties[name] = propertySchema
			}
			propertyActions[name] = appendUniqueToolSchemaValue(propertyActions[name], action)
		}
		branchRequired := toolSchemaStringSlice(branch["required"])
		for _, required := range branchRequired {
			requiredCounts[required]++
			requiredActions[required] = appendUniqueToolSchemaValue(requiredActions[required], action)
		}
		operationContracts = append(operationContracts, toolSchemaOperationContract(discriminator, action, branchRequired))
		if branch["additionalProperties"] != false {
			additionalPropertiesFalse = false
		}
	}
	for name, propertySchema := range properties {
		property, ok := propertySchema.(map[string]any)
		if !ok || name == discriminator {
			continue
		}
		usage := "Used when " + discriminator + " is one of: " + strings.Join(propertyActions[name], ", ") + "."
		if actions := requiredActions[name]; len(actions) > 0 {
			usage += " Required when " + discriminator + " is one of: " + strings.Join(actions, ", ") + "."
		}
		if description, _ := property["description"].(string); description != "" {
			property["description"] = usage + " " + description
		} else {
			property["description"] = usage
		}
	}

	required := make([]string, 0, len(requiredCounts))
	for name, count := range requiredCounts {
		if count == len(branches) {
			required = append(required, name)
		}
	}
	sort.Strings(required)
	delete(schema, ToolSchemaDiscriminatorAnnotation)
	delete(schema, "oneOf")
	schema["properties"] = properties
	contractDescription := "Discriminated operation requirements: " + strings.Join(operationContracts, "; ") + "."
	if description, _ := schema["description"].(string); description != "" {
		schema["description"] = description + " " + contractDescription
	} else {
		schema["description"] = contractDescription
	}
	if len(required) > 0 {
		schema["required"] = required
	} else {
		delete(schema, "required")
	}
	if additionalPropertiesFalse {
		schema["additionalProperties"] = false
	}
	return json.Marshal(schema)
}

func toolSchemaBranchDiscriminator(properties map[string]any, discriminator string) string {
	actionSchema, _ := properties[discriminator].(map[string]any)
	action, _ := actionSchema["const"].(string)
	return strings.TrimSpace(action)
}

func toolSchemaOperationContract(discriminator, action string, required []string) string {
	fields := make([]string, 0, len(required))
	for _, field := range required {
		if field != discriminator {
			fields = append(fields, field)
		}
	}
	sort.Strings(fields)
	if len(fields) == 0 {
		return discriminator + "=" + action + " requires no additional fields"
	}
	return discriminator + "=" + action + " requires " + strings.Join(fields, ", ")
}

func appendUniqueToolSchemaValue(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func mergeToolPropertySchemas(existing, next any) any {
	if reflect.DeepEqual(existing, next) {
		return existing
	}
	if existingMap, ok := existing.(map[string]any); ok {
		if nextMap, ok := next.(map[string]any); ok {
			if existingConst, exists := existingMap["const"]; exists && len(existingMap) == 1 {
				if nextConst, exists := nextMap["const"]; exists && len(nextMap) == 1 {
					return map[string]any{"enum": []any{existingConst, nextConst}}
				}
			}
			if enum, exists := existingMap["enum"].([]any); exists && len(existingMap) == 1 {
				if nextConst, exists := nextMap["const"]; exists && len(nextMap) == 1 {
					for _, value := range enum {
						if reflect.DeepEqual(value, nextConst) {
							return existing
						}
					}
					return map[string]any{"enum": append(append([]any(nil), enum...), nextConst)}
				}
			}
		}
		if variants, exists := existingMap["anyOf"].([]any); exists && len(existingMap) == 1 {
			for _, variant := range variants {
				if reflect.DeepEqual(variant, next) {
					return existing
				}
			}
			return map[string]any{"anyOf": append(append([]any(nil), variants...), next)}
		}
	}
	return map[string]any{"anyOf": []any{existing, next}}
}

func toolSchemaStringSlice(value any) []string {
	values, _ := value.([]any)
	result := make([]string, 0, len(values))
	for _, item := range values {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}
