// Package materializer turns an admitted Design and Binding Plan into the
// final AGenUI artifact. Admission belongs to Workspace; this package never
// guesses, repairs, drops, or reinterprets model decisions.
package materializer

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	bindingcontract "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/binding/contract"
)

type Output struct {
	Result string
	Plan   string
}

// Materialize deterministically adds runtime action references to an already
// admitted plan. Field mappings pass through byte-for-byte in canonical form.
func Materialize(design, plan string) (Output, error) {
	var messages []any
	if err := json.Unmarshal([]byte(design), &messages); err != nil {
		return Output{}, fmt.Errorf("materialize design: %w", err)
	}
	dataModel := findDataModel(messages)
	if dataModel == nil {
		return Output{}, errors.New("materialize design: updateDataModel.value is required")
	}
	components := finalComponents(messages)
	executable, err := bindingcontract.ParseExecutablePlan(plan)
	if err != nil {
		return Output{}, err
	}
	fields, actions := executable.FieldMappings, executable.ActionMappings
	for _, mapping := range actions {
		componentID := stringValue(mapping["componentId"])
		component := findComponent(components, componentID)
		if component == nil {
			return Output{}, fmt.Errorf("materialize action: component %q is missing", componentID)
		}
		kind := stringValue(mapping["actionSourceType"])
		var sourcePath string
		switch kind {
		case "url":
			sourcePath = stringValue(mapping["urlPath"])
		case "event":
			sourcePath = stringValue(mapping["eventPath"])
		default:
			return Output{}, fmt.Errorf("materialize action %q: unsupported source type %q", componentID, kind)
		}
		if sourcePath == "" {
			return Output{}, fmt.Errorf("materialize action %q: source path is required", componentID)
		}
		ref := "/__actions/" + safeKey(componentID) + "/payload"
		setPointerIfMissing(dataModel, ref, map[string]any{})
		fields = append(fields, map[string]any{
			"refKey": ref, "sourceKey": sourcePath, "confidence": 1,
			"reason": "deterministic action materialization",
		})
		if kind == "url" {
			component["action"] = map[string]any{"functionCall": map[string]any{
				"call": "openUrl", "args": map[string]any{"url": map[string]any{"path": ref}},
				"returnType": "void",
			}}
		} else {
			eventName := stringValue(mapping["eventName"])
			if eventName == "" {
				return Output{}, fmt.Errorf("materialize action %q: frozen event name is required", componentID)
			}
			component["action"] = map[string]any{"event": map[string]any{
				"name": eventName, "context": map[string]any{"payload": map[string]any{"path": ref}},
			}}
		}
	}
	encoded, err := json.Marshal(messages)
	if err != nil {
		return Output{}, fmt.Errorf("materialize result: %w", err)
	}
	planJSON, err := bindingcontract.EncodeExecutablePlan(fields, actions)
	if err != nil {
		return Output{}, err
	}
	return Output{Result: string(encoded), Plan: planJSON}, nil
}

func findDataModel(messages []any) map[string]any {
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		update, _ := message["updateDataModel"].(map[string]any)
		value, _ := update["value"].(map[string]any)
		if value != nil {
			return value
		}
	}
	return nil
}

func finalComponents(messages []any) []map[string]any {
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		update, _ := message["updateComponents"].(map[string]any)
		values, _ := update["components"].([]any)
		if len(values) > 0 {
			return objectSlice(values)
		}
	}
	return nil
}

func findComponent(components []map[string]any, id string) map[string]any {
	for _, component := range components {
		if stringValue(component["id"]) == id || stringValue(component["child"]) == id {
			return component
		}
		children, _ := component["children"].([]any)
		if found := findComponent(objectSlice(children), id); found != nil {
			return found
		}
	}
	return nil
}

func objectSlice(values []any) []map[string]any {
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if item, ok := value.(map[string]any); ok {
			result = append(result, item)
		}
	}
	return result
}

func setPointerIfMissing(root map[string]any, pointer string, value any) {
	segments := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	current := root
	for index, segment := range segments {
		segment = strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")
		if index == len(segments)-1 {
			if _, exists := current[segment]; !exists {
				current[segment] = value
			}
			return
		}
		next, _ := current[segment].(map[string]any)
		if next == nil {
			next = map[string]any{}
			current[segment] = next
		}
		current = next
	}
}

func stringValue(value any) string { text, _ := value.(string); return strings.TrimSpace(text) }

func safeKey(value string) string {
	var result strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '_' || character == '-' {
			result.WriteRune(character)
		} else {
			result.WriteByte('_')
		}
	}
	if result.Len() == 0 {
		return "action"
	}
	return result.String()
}

// StableMapKeys is exported only for deterministic extension implementations.
func StableMapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
