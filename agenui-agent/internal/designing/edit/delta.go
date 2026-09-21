package edit

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/workspace"
)

type DeltaReport struct {
	EditID       string   `json:"edit_id"`
	AppliedPaths []string `json:"applied_paths"`
	BaseHash     string   `json:"base_hash"`
	ResultHash   string   `json:"result_hash"`
	NoOp         bool     `json:"no_op,omitempty"`
}

// ApplyAuthorizedDelta treats Style output as an untrusted full candidate.
// It commits only the paths frozen in the Edit Contract onto the validated
// base Design. Incidental drift elsewhere in a complete model response is
// discarded rather than turning a safe, scoped edit into a failed run.
func ApplyAuthorizedDelta(base, candidate string, contract Contract) (string, DeltaReport, error) {
	if contract.SchemaVersion != SchemaVersion || contract.ChangeScope != "design_update" || len(contract.TargetSet) == 0 {
		return "", DeltaReport{}, fmt.Errorf("%w: invalid edit contract", ErrEditScopeViolation)
	}
	if HashText(base) != contract.Preconditions.DesignHash {
		return "", DeltaReport{}, fmt.Errorf("%w: design hash changed", ErrBaseRevisionConflict)
	}
	baseMessages, err := parseAGenUI(base)
	if err != nil {
		return "", DeltaReport{}, err
	}
	candidateMessages, err := parseAGenUI(candidate)
	if err != nil {
		return "", DeltaReport{}, err
	}
	if len(baseMessages) != len(candidateMessages) {
		return "", DeltaReport{}, fmt.Errorf("%w: message count changed", ErrEditScopeViolation)
	}
	baseComponents, baseUpdate, err := componentIndex(baseMessages)
	if err != nil {
		return "", DeltaReport{}, err
	}
	candidateComponents, candidateUpdate, err := componentIndex(candidateMessages)
	if err != nil {
		return "", DeltaReport{}, err
	}
	if editAlreadySatisfied(baseComponents, candidateComponents, contract) {
		hash := HashText(base)
		return base, DeltaReport{
			EditID: contract.EditID, BaseHash: hash, ResultHash: hash, NoOp: true,
		}, nil
	}
	if !reflect.DeepEqual(withoutComponents(baseUpdate), withoutComponents(candidateUpdate)) {
		return "", DeltaReport{}, fmt.Errorf("%w: updateComponents envelope changed", ErrEditScopeViolation)
	}
	for index := range baseMessages {
		if _, ok := baseMessages[index]["updateComponents"]; ok {
			continue
		}
		if !reflect.DeepEqual(baseMessages[index], candidateMessages[index]) {
			return "", DeltaReport{}, fmt.Errorf("%w: non-component message changed", ErrEditScopeViolation)
		}
	}
	if len(baseComponents) != len(candidateComponents) {
		return "", DeltaReport{}, fmt.Errorf("%w: component set changed", ErrEditScopeViolation)
	}
	allowed := make(map[string]Target, len(contract.TargetSet))
	for _, target := range contract.TargetSet {
		allowed[target.ComponentID+"."+target.Path] = target
	}
	changed := make([]string, 0, len(allowed))
	for id := range baseComponents {
		_, exists := candidateComponents[id]
		if !exists {
			return "", DeltaReport{}, fmt.Errorf("%w: component %s removed", ErrEditScopeViolation, id)
		}
	}
	for qualified, target := range allowed {
		baseValue, baseExists := valueAtPath(baseComponents[target.ComponentID], target.Path)
		candidateValue, candidateExists := valueAtPath(candidateComponents[target.ComponentID], target.Path)
		if !candidateExists || (baseExists && reflect.DeepEqual(baseValue, candidateValue)) {
			continue
		}
		changed = append(changed, qualified)
	}
	if len(changed) == 0 {
		return "", DeltaReport{}, fmt.Errorf("%w: target did not change", ErrEditNoChange)
	}
	for qualified := range allowed {
		if !stringInSlice(changed, qualified) {
			return "", DeltaReport{}, fmt.Errorf("%w: requested path %s did not change", ErrEditNoChange, qualified)
		}
	}
	// Candidate slots are never authoritative for a scoped property edit. A
	// valid typed artifact is required, then only its messages are merged into
	// the Host-frozen base artifact.
	if _, err := workspace.DecodeDesignArtifact(candidate); err != nil {
		return "", DeltaReport{}, fmt.Errorf("%w: typed design artifact is invalid", ErrEditScopeViolation)
	}
	mergedMessages := deepCopyMessages(baseMessages)
	mergedComponents, _, err := componentIndex(mergedMessages)
	if err != nil {
		return "", DeltaReport{}, err
	}
	for _, target := range contract.TargetSet {
		value, exists := valueAtPath(candidateComponents[target.ComponentID], target.Path)
		if !exists {
			return "", DeltaReport{}, fmt.Errorf("%w: candidate omitted %s.%s", ErrEditScopeViolation, target.ComponentID, target.Path)
		}
		baseValue, _ := valueAtPath(baseComponents[target.ComponentID], target.Path)
		if !requestedChangeSatisfied(baseValue, value, target.Value) {
			return "", DeltaReport{}, fmt.Errorf(
				"%w: candidate value for %s.%s does not match requested_changes",
				ErrEditScopeViolation, target.ComponentID, target.Path,
			)
		}
		if err := setValueAtPath(mergedComponents[target.ComponentID], target.Path, value); err != nil {
			return "", DeltaReport{}, err
		}
	}
	encoded, err := json.Marshal(mergedMessages)
	if err != nil {
		return "", DeltaReport{}, err
	}
	merged, err := workspace.ReplaceArtifactMessages(base, string(encoded))
	if err != nil {
		return "", DeltaReport{}, err
	}
	sort.Strings(changed)
	return merged, DeltaReport{
		EditID: contract.EditID, AppliedPaths: changed,
		BaseHash: HashText(base), ResultHash: HashText(merged),
	}, nil
}

// requestedChangeSatisfied is intentionally exact. The model sees the current
// values in the candidate facts and must turn relative language into a concrete
// requested value before the Host freezes the contract.
func requestedChangeSatisfied(_, candidate, requested any) bool {
	return semanticallyEqualStyleValue(candidate, requested)
}

func editAlreadySatisfied(base, candidate map[string]map[string]any, contract Contract) bool {
	for _, target := range contract.TargetSet {
		baseComponent, baseOK := base[target.ComponentID]
		candidateComponent, candidateOK := candidate[target.ComponentID]
		if !baseOK || !candidateOK {
			return false
		}
		baseValue, baseValueOK := valueAtPath(baseComponent, target.Path)
		candidateValue, candidateValueOK := valueAtPath(candidateComponent, target.Path)
		if !baseValueOK || !candidateValueOK ||
			!semanticallyEqualStyleValue(baseValue, candidateValue) ||
			!requestedChangeAlreadySatisfied(target.Path, baseValue, target.Value) {
			return false
		}
	}
	return true
}

func requestedChangeAlreadySatisfied(_ string, current, requested any) bool {
	return semanticallyEqualStyleValue(current, requested)
}

func semanticallyEqualStyleValue(left, right any) bool {
	return reflect.DeepEqual(left, right)
}

func parseAGenUI(raw string) ([]map[string]any, error) {
	block, decodeErr := workspace.ArtifactMessagesJSON(raw)
	var messages []map[string]any
	if decodeErr != nil || json.Unmarshal([]byte(block), &messages) != nil {
		return nil, fmt.Errorf("%w: invalid AGenUI candidate", ErrEditScopeViolation)
	}
	return messages, nil
}

func componentIndex(messages []map[string]any) (map[string]map[string]any, map[string]any, error) {
	for _, message := range messages {
		update, ok := message["updateComponents"].(map[string]any)
		if !ok {
			continue
		}
		values, ok := update["components"].([]any)
		if !ok {
			return nil, nil, fmt.Errorf("%w: components are missing", ErrEditScopeViolation)
		}
		result := make(map[string]map[string]any, len(values))
		for _, value := range values {
			component, ok := value.(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("%w: invalid component", ErrEditScopeViolation)
			}
			id, _ := component["id"].(string)
			if id == "" || result[id] != nil {
				return nil, nil, fmt.Errorf("%w: invalid component id", ErrEditScopeViolation)
			}
			result[id] = component
		}
		return result, update, nil
	}
	return nil, nil, fmt.Errorf("%w: updateComponents is missing", ErrEditScopeViolation)
}

func withoutComponents(update map[string]any) map[string]any {
	result := make(map[string]any, len(update)-1)
	for key, value := range update {
		if key != "components" {
			result[key] = value
		}
	}
	return result
}

func diffPaths(prefix string, left, right any) []string {
	if semanticallyEqualStyleValue(left, right) {
		return nil
	}
	leftMap, leftOK := left.(map[string]any)
	rightMap, rightOK := right.(map[string]any)
	if !leftOK || !rightOK {
		return []string{strings.TrimPrefix(prefix, ".")}
	}
	keys := make(map[string]struct{}, len(leftMap)+len(rightMap))
	for key := range leftMap {
		keys[key] = struct{}{}
	}
	for key := range rightMap {
		keys[key] = struct{}{}
	}
	var result []string
	for key := range keys {
		result = append(result, diffPaths(prefix+"."+key, leftMap[key], rightMap[key])...)
	}
	sort.Strings(result)
	return result
}

func deepCopyMessages(input []map[string]any) []map[string]any {
	encoded, _ := json.Marshal(input)
	var output []map[string]any
	_ = json.Unmarshal(encoded, &output)
	return output
}

func valueAtPath(component map[string]any, path string) (any, bool) {
	var current any = component
	for _, part := range strings.Split(path, ".") {
		values, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = values[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func setValueAtPath(component map[string]any, path string, value any) error {
	parts := strings.Split(path, ".")
	current := component
	for _, part := range parts[:len(parts)-1] {
		value, exists := current[part]
		if !exists {
			next := make(map[string]any)
			current[part] = next
			current = next
			continue
		}
		next, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: path %s does not exist", ErrEditScopeViolation, path)
		}
		current = next
	}
	current[parts[len(parts)-1]] = value
	return nil
}
