package orchestration

import (
	"encoding/json"
	"sort"
	"strings"
)

// binderRefUsage is a structural projection of one binding target. It contains
// no semantic ranking or business aliases.
type binderRefUsage struct {
	RefKey        string
	ComponentType string
	ComponentID   string
	UsagePath     string
}

func binderString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func binderObject(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		return typed
	case string:
		var object map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(typed)), &object) == nil {
			return object
		}
	}
	return nil
}

func binderComponentType(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		if valueType := binderString(typed["type"]); valueType != "" {
			return valueType
		}
		keys := sortedKeys(typed)
		if len(keys) > 0 {
			return keys[0]
		}
	}
	return ""
}

func binderValueType(value any) string {
	switch value.(type) {
	case string:
		return "string"
	case json.Number, float32, float64:
		return "number"
	case bool:
		return "boolean"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return "unknown"
	}
}

func binderSourceScope(path string) string {
	index := strings.LastIndex(path, "[*]")
	if index < 0 {
		return ""
	}
	return path[:index+3]
}

func binderDataModel(agenui string) any {
	if strings.TrimSpace(agenui) == "" {
		return nil
	}
	var messages []any
	if json.Unmarshal([]byte(agenui), &messages) != nil {
		var single map[string]any
		if json.Unmarshal([]byte(agenui), &single) != nil {
			return nil
		}
		messages = []any{single}
	}
	for _, message := range messages {
		object, _ := message.(map[string]any)
		update, _ := object["updateDataModel"].(map[string]any)
		if value, exists := update["value"]; exists {
			return value
		}
	}
	return nil
}

func binderRefKeys(dataModel any, examples map[string]string) []string {
	seen := make(map[string]struct{})
	collectBinderRefKeys(dataModel, "", seen, examples)
	keys := make([]string, 0, len(seen))
	for key := range seen {
		if key != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func collectBinderRefKeys(value any, path string, seen map[string]struct{}, examples map[string]string) {
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range sortedKeys(typed) {
			collectBinderRefKeys(typed[key], path+"/"+key, seen, examples)
		}
	case []any:
		arrayPath := path + "[*]"
		if len(typed) == 0 {
			seen[arrayPath] = struct{}{}
			return
		}
		for _, item := range typed {
			collectBinderRefKeys(item, arrayPath, seen, examples)
		}
	default:
		if path != "" {
			seen[path] = struct{}{}
		}
	}
}

func binderRefUsages(agenui string, refKeys []string) []binderRefUsage {
	if len(refKeys) == 0 || strings.TrimSpace(agenui) == "" {
		return nil
	}
	var messages []any
	if json.Unmarshal([]byte(agenui), &messages) != nil {
		return nil
	}
	var usages []binderRefUsage
	seen := make(map[string]struct{})
	for _, message := range messages {
		object, _ := message.(map[string]any)
		update, _ := object["updateComponents"].(map[string]any)
		components, _ := update["components"].([]any)
		for _, componentValue := range components {
			component, _ := componentValue.(map[string]any)
			componentID := binderString(component["id"])
			componentType := binderComponentType(component["component"])
			paths := make(map[string]struct{})
			collectBinderPaths(component, paths)
			for path := range paths {
				for _, refKey := range refKeys {
					if !binderRefMatchesPath(refKey, path) {
						continue
					}
					key := strings.Join([]string{refKey, componentType, componentID, path}, "\x00")
					if _, exists := seen[key]; exists {
						continue
					}
					seen[key] = struct{}{}
					usages = append(usages, binderRefUsage{RefKey: refKey, ComponentType: componentType, ComponentID: componentID, UsagePath: path})
				}
			}
		}
	}
	sort.Slice(usages, func(i, j int) bool {
		return usages[i].RefKey+"\x00"+usages[i].ComponentID+"\x00"+usages[i].UsagePath < usages[j].RefKey+"\x00"+usages[j].ComponentID+"\x00"+usages[j].UsagePath
	})
	return usages
}

func collectBinderPaths(value any, paths map[string]struct{}) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "path" {
				if path := binderString(child); path != "" {
					paths[path] = struct{}{}
				}
			}
			collectBinderPaths(child, paths)
		}
	case []any:
		for _, child := range typed {
			collectBinderPaths(child, paths)
		}
	}
}

func binderRefMatchesPath(refKey, path string) bool {
	refNormalized := strings.TrimPrefix(strings.ReplaceAll(refKey, "[*]", ""), "/")
	pathNormalized := strings.TrimPrefix(strings.ReplaceAll(path, "[*]", ""), "/")
	pathNormalized = strings.ReplaceAll(pathNormalized, ".", "/")
	return refNormalized == pathNormalized || strings.HasSuffix(refNormalized, "/"+pathNormalized) || strings.HasSuffix(pathNormalized, "/"+refNormalized)
}
