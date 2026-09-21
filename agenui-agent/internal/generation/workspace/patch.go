package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Operation is a Host-frozen mutation. Model arguments never reach Apply
// directly: the edit contract has already classified each declaration against
// one immutable base revision.
type Operation struct {
	Op          string         `json:"op"`
	ComponentID string         `json:"component_id"`
	Path        string         `json:"path"`
	Value       any            `json:"value"`
	Component   map[string]any `json:"component,omitempty"`
	ParentID    string         `json:"parent_id,omitempty"`
	AfterID     string         `json:"after_id,omitempty"`
}

type PatchRequest struct {
	BaseRevision string      `json:"base_revision"`
	Operations   []Operation `json:"operations"`
	// EditablePaths is Host-derived and is never accepted from model arguments.
	EditablePaths []string `json:"-"`
}

type PatchResult struct {
	Document     Document `json:"document"`
	Revision     string   `json:"revision"`
	ChangedPaths []string `json:"changed_paths"`
	CreatedIDs   []string `json:"created_ids,omitempty"`
	UpdatedIDs   []string `json:"updated_ids,omitempty"`
}

func Apply(document Document, request PatchRequest) (PatchResult, error) {
	current, err := document.Revision()
	if err != nil {
		return PatchResult{}, err
	}
	if request.BaseRevision == "" || request.BaseRevision != current {
		return PatchResult{}, ErrRevisionConflict
	}
	if len(request.Operations) == 0 {
		return PatchResult{}, errors.New("agenui workspace: authorized operations are required")
	}
	next := cloneDocument(document)
	changed := make([]string, 0, len(request.Operations))
	created := make([]string, 0, len(request.Operations))
	updated := make([]string, 0, len(request.Operations))
	insertions := make([]Operation, 0, len(request.Operations))
	for index, operation := range request.Operations {
		switch operation.Op {
		case "insert_component":
			insertions = append(insertions, operation)
			continue
		case "set_component_property":
		default:
			return PatchResult{}, fmt.Errorf("agenui workspace: operation %d: unsupported operation %q", index, operation.Op)
		}
		component, findErr := findComponent(&next, operation.ComponentID)
		if findErr != nil {
			return PatchResult{}, fmt.Errorf("agenui workspace: operation %d: %w", index, findErr)
		}
		path := "/components/" + escape(operation.ComponentID) + normalizePointer(operation.Path)
		if !editable(path, request.EditablePaths) {
			return PatchResult{}, fmt.Errorf("%w: %s", ErrProtectedMutation, path)
		}
		if setErr := setPointer(component, operation.Path, operation.Value); setErr != nil {
			return PatchResult{}, fmt.Errorf("agenui workspace: operation %d: %w", index, setErr)
		}
		changed = append(changed, path)
		updated = append(updated, operation.ComponentID)
	}
	if len(insertions) > 0 {
		if err := applyInsertions(&next, insertions); err != nil {
			return PatchResult{}, err
		}
		for _, operation := range insertions {
			created = append(created, operation.ComponentID)
			changed = append(changed, "/components/"+escape(operation.ComponentID))
			parent, _ := findComponent(&next, operation.ParentID)
			topology := "/children"
			if _, exists := parent["child"]; exists {
				topology = "/child"
			}
			changed = append(changed, "/components/"+escape(operation.ParentID)+topology)
		}
	}
	if err := next.ValidateStructural(); err != nil {
		return PatchResult{}, err
	}
	revision, err := next.Revision()
	if err != nil {
		return PatchResult{}, err
	}
	return PatchResult{
		Document: next, Revision: revision, ChangedPaths: sortedUnique(changed),
		CreatedIDs: sortedUnique(created), UpdatedIDs: sortedUnique(updated),
	}, nil
}

func applyInsertions(document *Document, operations []Operation) error {
	known := make(map[string]struct{}, len(document.Components)+len(operations))
	for _, component := range document.Components {
		if id, _ := component["id"].(string); id != "" {
			known[id] = struct{}{}
		}
	}
	for index, operation := range operations {
		id := strings.TrimSpace(operation.ComponentID)
		kind, _ := operation.Component["component"].(string)
		componentID, _ := operation.Component["id"].(string)
		if id == "" || strings.TrimSpace(operation.ParentID) == "" ||
			componentID != id || strings.TrimSpace(kind) == "" {
			return fmt.Errorf("agenui workspace: insertion %d requires matching component id, component type and parent_id", index)
		}
		if id == document.RootID {
			return fmt.Errorf("agenui workspace: insertion %d cannot replace the root component", index)
		}
		if _, exists := known[id]; exists {
			return fmt.Errorf("agenui workspace: insertion %d component %q already exists", index, id)
		}
		known[id] = struct{}{}
		document.Components = append(document.Components, cloneComponent(operation.Component))
	}
	pending := append([]Operation(nil), operations...)
	for len(pending) > 0 {
		next := make([]Operation, 0, len(pending))
		progress := false
		for index, operation := range pending {
			parent, err := findComponent(document, operation.ParentID)
			if err != nil {
				return fmt.Errorf("agenui workspace: insertion %d: parent %q does not exist", index, operation.ParentID)
			}
			if operation.AfterID != "" && !hasChildID(parent, operation.AfterID) {
				next = append(next, operation)
				continue
			}
			if err := attachChild(parent, operation.ComponentID, operation.AfterID); err != nil {
				return fmt.Errorf("agenui workspace: insertion %d: %w", index, err)
			}
			progress = true
		}
		if !progress {
			return fmt.Errorf("agenui workspace: insertion after_id dependencies cannot be satisfied")
		}
		pending = next
	}
	if err := validateInsertedReachability(*document, operations); err != nil {
		return err
	}
	return nil
}

func hasChildID(parent map[string]any, childID string) bool {
	for _, current := range componentChildIDs(parent) {
		if current == childID {
			return true
		}
	}
	return false
}

func attachChild(parent map[string]any, childID, afterID string) error {
	if raw, exists := parent["children"]; exists {
		children, ok := raw.([]any)
		if !ok {
			return errors.New("parent children is not an ordered component-id array")
		}
		result := make([]any, 0, len(children)+1)
		for _, child := range children {
			if id, _ := child.(string); id != childID {
				result = append(result, child)
			}
		}
		if afterID == "" {
			result = append(result, childID)
		} else {
			inserted := false
			ordered := make([]any, 0, len(result)+1)
			for _, child := range result {
				ordered = append(ordered, child)
				if id, _ := child.(string); id == afterID {
					ordered = append(ordered, childID)
					inserted = true
				}
			}
			if !inserted {
				return fmt.Errorf("after_id %q is not a child of parent", afterID)
			}
			result = ordered
		}
		parent["children"] = result
		return nil
	}
	if raw, exists := parent["child"]; exists {
		if afterID != "" {
			return errors.New("after_id is invalid for a single-child parent")
		}
		current, _ := raw.(string)
		if current != "" && current != childID {
			return fmt.Errorf("single-child parent already owns %q", current)
		}
		parent["child"] = childID
		return nil
	}
	return errors.New("parent exposes neither child nor ordered children topology")
}

func validateInsertedReachability(document Document, operations []Operation) error {
	components := make(map[string]map[string]any, len(document.Components))
	parents := make(map[string][]string)
	for _, component := range document.Components {
		if id, _ := component["id"].(string); id != "" {
			components[id] = component
			for _, childID := range componentChildIDs(component) {
				parents[childID] = append(parents[childID], id)
			}
		}
	}
	for _, operation := range operations {
		owners := sortedUnique(parents[operation.ComponentID])
		if len(owners) != 1 || owners[0] != operation.ParentID {
			return fmt.Errorf(
				"agenui workspace: inserted component %q must have exactly parent %q, got %v",
				operation.ComponentID, operation.ParentID, owners,
			)
		}
	}
	visited, active := make(map[string]bool), make(map[string]bool)
	var visit func(string) error
	visit = func(id string) error {
		if active[id] {
			return fmt.Errorf("agenui workspace: component topology contains a cycle at %q", id)
		}
		if visited[id] {
			return nil
		}
		component := components[id]
		if component == nil {
			return fmt.Errorf("agenui workspace: component reference %q does not exist", id)
		}
		active[id] = true
		for _, childID := range componentChildIDs(component) {
			if err := visit(childID); err != nil {
				return err
			}
		}
		active[id], visited[id] = false, true
		return nil
	}
	if err := visit(document.RootID); err != nil {
		return err
	}
	for _, operation := range operations {
		if !visited[operation.ComponentID] {
			return fmt.Errorf("agenui workspace: inserted component %q is not reachable from root", operation.ComponentID)
		}
	}
	return nil
}

func componentChildIDs(component map[string]any) []string {
	var result []string
	if child, _ := component["child"].(string); child != "" {
		result = append(result, child)
	}
	switch children := component["children"].(type) {
	case []any:
		for _, child := range children {
			if id, _ := child.(string); id != "" {
				result = append(result, id)
			}
		}
	case map[string]any:
		if id, _ := children["componentId"].(string); id != "" {
			result = append(result, id)
		}
	}
	return result
}

func cloneComponent(input map[string]any) map[string]any {
	raw, _ := json.Marshal(input)
	var output map[string]any
	_ = json.Unmarshal(raw, &output)
	return output
}

func editable(path string, allowed []string) bool {
	for _, candidate := range allowed {
		if path == candidate {
			return true
		}
	}
	return false
}

func findComponent(document *Document, id string) (map[string]any, error) {
	for _, component := range document.Components {
		if component["id"] == id {
			return component, nil
		}
	}
	return nil, fmt.Errorf("component %q not found", id)
}

func normalizePointer(path string) string {
	if strings.HasPrefix(path, "/") {
		return path
	}
	return "/" + path
}

func pointerParts(path string) ([]string, error) {
	path = normalizePointer(path)
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for index := range parts {
		parts[index] = strings.ReplaceAll(strings.ReplaceAll(parts[index], "~1", "/"), "~0", "~")
		if parts[index] == "" || parts[index] == "__proto__" || parts[index] == "prototype" || parts[index] == "constructor" {
			return nil, errors.New("property path contains an invalid segment")
		}
	}
	return parts, nil
}

func setPointer(root map[string]any, path string, value any) error {
	parts, err := pointerParts(path)
	if err != nil {
		return err
	}
	current := root
	for _, part := range parts[:len(parts)-1] {
		next, exists := current[part]
		if !exists {
			child := make(map[string]any)
			current[part] = child
			current = child
			continue
		}
		child, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("path segment %q is not an object", part)
		}
		current = child
	}
	current[parts[len(parts)-1]] = cloneValue(value)
	return nil
}

func cloneValue(value any) any {
	raw, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var output any
	if err := json.Unmarshal(raw, &output); err != nil {
		return value
	}
	return output
}

func cloneDocument(input Document) Document {
	raw, _ := json.Marshal(input)
	var output Document
	_ = json.Unmarshal(raw, &output)
	return output
}

func escape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}
