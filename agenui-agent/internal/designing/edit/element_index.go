package edit

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	ErrTargetNotFound  = errors.New("design edit: target not found")
	ErrTargetAmbiguous = errors.New("design edit: target is ambiguous")
)

// Element is a server-derived searchable view of one real AGenUI component.
// It is never supplied by the management UI and cannot name a nonexistent ID.
type Element struct {
	ElementID        string         `json:"element_id"`
	ComponentID      string         `json:"component_id"`
	ComponentType    string         `json:"component_type"`
	ParentID         string         `json:"parent_id,omitempty"`
	ChildIDs         []string       `json:"child_ids,omitempty"`
	Relation         string         `json:"relation,omitempty"`
	SemanticRole     string         `json:"semantic_role,omitempty"`
	ContractItemID   string         `json:"contract_item_id,omitempty"`
	ContractActionID string         `json:"contract_action_id,omitempty"`
	Descriptions     []string       `json:"descriptions,omitempty"`
	Properties       map[string]any `json:"properties,omitempty"`
	CurrentStyles    map[string]any `json:"styles,omitempty"`
	EditablePaths    []string       `json:"editable_paths"`
}

// Index contains only components extracted from the validated current Design.
type Index struct {
	Revision string    `json:"revision"`
	Elements []Element `json:"elements"`
}

type fieldHint struct {
	SlotID         string `json:"slotId"`
	ComponentID    string `json:"componentId"`
	Role           string `json:"role"`
	Description    string `json:"description"`
	ContractItemID string `json:"contractItemId"`
}

type actionSlot struct {
	SlotID           string `json:"slotId"`
	ComponentID      string `json:"componentId"`
	Role             string `json:"role"`
	Description      string `json:"description"`
	ContractActionID string `json:"contractActionId"`
}

// BuildIndex parses an already validator-approved AGenUI message array and its
// semantic sidecars into a stable, searchable component index.
func BuildIndex(revision string, agenuiJSON, fieldHintsJSON, actionSlotsJSON string, catalogEditablePaths ...string) (Index, error) {
	if strings.TrimSpace(revision) == "" {
		return Index{}, errors.New("design edit: revision is required")
	}
	var messages []map[string]any
	if err := json.Unmarshal([]byte(agenuiJSON), &messages); err != nil {
		return Index{}, fmt.Errorf("design edit: decode AGenUI: %w", err)
	}
	components := make(map[string]map[string]any)
	for _, message := range messages {
		update, _ := message["updateComponents"].(map[string]any)
		values, _ := update["components"].([]any)
		for _, value := range values {
			component, _ := value.(map[string]any)
			id, _ := component["id"].(string)
			kind, _ := component["component"].(string)
			if id == "" || kind == "" {
				continue
			}
			components[id] = component
		}
	}
	parentByChild := make(map[string]string)
	for parentID, component := range components {
		if childID, ok := component["child"].(string); ok && childID != "" {
			parentByChild[childID] = parentID
		}
	}
	// A direct child edge is more specific than a container's children list.
	// Build it first so map iteration cannot detach a Button label from its
	// owning action and weaken the model-selected action_label candidate.
	for parentID, component := range components {
		if childIDs, ok := component["children"].([]any); ok {
			for _, child := range childIDs {
				if childID, ok := child.(string); ok && childID != "" {
					if _, exists := parentByChild[childID]; !exists {
						parentByChild[childID] = parentID
					}
				}
			}
		}
	}
	var hints []fieldHint
	if strings.TrimSpace(fieldHintsJSON) != "" && json.Unmarshal([]byte(fieldHintsJSON), &hints) != nil {
		return Index{}, errors.New("design edit: invalid field hints")
	}
	var actions []actionSlot
	if strings.TrimSpace(actionSlotsJSON) != "" && json.Unmarshal([]byte(actionSlotsJSON), &actions) != nil {
		return Index{}, errors.New("design edit: invalid action slots")
	}
	hintsByComponent := make(map[string]fieldHint, len(hints))
	for _, hint := range hints {
		if _, exists := components[hint.ComponentID]; !exists {
			return Index{}, fmt.Errorf("design edit: field hint references unknown component %q", hint.ComponentID)
		}
		hintsByComponent[hint.ComponentID] = hint
	}
	actionsByComponent := make(map[string]actionSlot, len(actions))
	for _, action := range actions {
		if _, exists := components[action.ComponentID]; !exists {
			return Index{}, fmt.Errorf("design edit: action slot references unknown component %q", action.ComponentID)
		}
		actionsByComponent[action.ComponentID] = action
	}
	result := Index{Revision: revision}
	ids := make([]string, 0, len(components))
	for id := range components {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		component := components[id]
		kind, _ := component["component"].(string)
		element := Element{
			ElementID: id, ComponentID: id, ComponentType: kind,
			ParentID: parentByChild[id], ChildIDs: componentChildren(component),
			Properties: componentProperties(component),
		}
		element.EditablePaths = mergeEditablePaths(componentEditablePaths(component), catalogEditablePaths)
		if styles, ok := component["styles"].(map[string]any); ok {
			element.CurrentStyles = styles
		}
		if hint, exists := hintsByComponent[id]; exists {
			element.SemanticRole = hint.Role
			element.ContractItemID = hint.ContractItemID
			if hint.SlotID != "" {
				element.ElementID = hint.SlotID
			}
			if hint.Description != "" {
				element.Descriptions = append(element.Descriptions, hint.Description)
			}
		}
		if action, exists := actionsByComponent[id]; exists {
			element.SemanticRole = action.Role
			element.ContractActionID = action.ContractActionID
			element.Relation = "action_control"
			if action.SlotID != "" {
				element.ElementID = action.SlotID
			}
			if action.Description != "" {
				element.Descriptions = append(element.Descriptions, action.Description)
			}
		}
		if element.ContractActionID == "" {
			for parentID := parentByChild[id]; parentID != ""; parentID = parentByChild[parentID] {
				action, exists := actionsByComponent[parentID]
				if !exists {
					continue
				}
				element.ContractActionID = action.ContractActionID
				element.SemanticRole = action.Role
				element.Relation = "action_label"
				if action.Description != "" {
					element.Descriptions = append(element.Descriptions, action.Description)
				}
				break
			}
		}
		result.Elements = append(result.Elements, element)
	}
	return result, nil
}

func componentChildren(component map[string]any) []string {
	result := make([]string, 0)
	if childID, ok := component["child"].(string); ok && childID != "" {
		result = append(result, childID)
	}
	if childIDs, ok := component["children"].([]any); ok {
		for _, child := range childIDs {
			if childID, ok := child.(string); ok && childID != "" {
				result = append(result, childID)
			}
		}
	}
	return result
}

func mergeEditablePaths(current, catalog []string) []string {
	seen := make(map[string]struct{}, len(current)+len(catalog))
	for _, path := range append(current, catalog...) {
		if validEditablePath(path) {
			seen[path] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for path := range seen {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func componentEditablePaths(component map[string]any) []string {
	var paths []string
	for key, value := range component {
		switch key {
		case "id", "component", "child", "children":
			continue
		}
		collectEditablePaths(key, value, &paths)
	}
	sort.Strings(paths)
	return paths
}

func collectEditablePaths(prefix string, value any, paths *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		if len(typed) == 0 {
			*paths = append(*paths, prefix)
			return
		}
		for key, child := range typed {
			collectEditablePaths(prefix+"."+key, child, paths)
		}
	default:
		*paths = append(*paths, prefix)
	}
}

func componentProperties(component map[string]any) map[string]any {
	result := make(map[string]any)
	for key, value := range component {
		switch key {
		case "id", "component", "child", "children", "styles":
			continue
		default:
			result[key] = value
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
