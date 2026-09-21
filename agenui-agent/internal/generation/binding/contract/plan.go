package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// ReconcileExecutablePlan turns the Bind Result into the only executable
// field/action mapping accepted by Workspace. The Bind Result owns
// requirement, slot, source and path authority. Candidate mapping detail may
// contribute only the action kind (url/event). Candidate mappings not
// declared by the Result are discarded before canonicalization; missing or
// drifted mappings for declared bindings are rejected.
func ReconcileExecutablePlan(input Input, result Result, candidate string) (string, error) {
	return reconcileExecutablePlan(input, result, candidate, true, false, false)
}

func reconcileExecutablePlan(
	input Input,
	result Result,
	candidate string,
	allowEmptyFieldSynthesis bool,
	rejectExtraFields bool,
	rejectExtraActions bool,
) (string, error) {
	if err := ValidateResult(input, result); err != nil {
		return "", err
	}
	fieldCandidates, err := planMappings(candidate, true)
	if err != nil {
		return "", err
	}
	actionCandidates, err := planMappings(candidate, false)
	if err != nil {
		return "", err
	}

	fieldSlots, err := planFieldSlots(input.Design.FieldHints)
	if err != nil {
		return "", err
	}
	actionSlots, err := planActionSlots(input.Design.ActionSlots)
	if err != nil {
		return "", err
	}

	expectedFields := make([]planExpectedField, 0, len(result.Bindings))
	expectedActions := make([]planExpectedAction, 0, len(result.Bindings))
	for _, binding := range result.Bindings {
		if strings.TrimSpace(binding.FieldPath) != "" {
			for _, slotID := range binding.TargetSlotIDs {
				hint, ok := fieldSlots[slotID]
				if !ok || hint.RefKey == "" {
					return "", fmt.Errorf("%w: field slot %q has no frozen refKey", ErrInvalidResult, slotID)
				}
				expectedFields = append(expectedFields, planExpectedField{
					RefKey: hint.RefKey, SourceKey: binding.FieldPath,
					BindingID: binding.RequirementID,
				})
			}
			if refKey := strings.TrimSpace(binding.RefKey); refKey != "" &&
				!bindingOwnsFieldRef(binding, fieldSlots, refKey) {
				return "", fmt.Errorf(
					"%w: binding %s ref_key %q is not one of its frozen target slots",
					ErrInvalidResult, binding.RequirementID, refKey,
				)
			}
			continue
		}
		for _, slotID := range binding.TargetSlotIDs {
			slot, ok := actionSlots[slotID]
			if !ok || slot.ComponentID == "" {
				return "", fmt.Errorf("%w: action slot %q has no frozen componentId", ErrInvalidResult, slotID)
			}
			expectedActions = append(expectedActions, planExpectedAction{
				ComponentID: slot.ComponentID, SourcePath: binding.ActionPath,
				EventName:  slot.ContractActionID,
				BindingIDs: []string{binding.RequirementID},
			})
		}
		if componentID := strings.TrimSpace(binding.ComponentID); componentID != "" &&
			!bindingOwnsActionComponent(binding, actionSlots, componentID) {
			return "", fmt.Errorf(
				"%w: binding %s component_id %q is not one of its frozen target slots",
				ErrInvalidResult, binding.RequirementID, componentID,
			)
		}
	}

	fields, err := reconcileFields(
		expectedFields,
		fieldCandidates,
		allowEmptyFieldSynthesis,
		rejectExtraFields,
	)
	if err != nil {
		return "", err
	}
	actions, err := reconcileActions(
		expectedActions, actionCandidates, allowEmptyFieldSynthesis, rejectExtraActions,
	)
	if err != nil {
		return "", err
	}
	return EncodeExecutablePlan(fields, actions)
}

// MergeExecutablePlanDelta applies one binding_update authorization to a durable
// baseline Plan. The model is allowed to describe only the authorized
// requirement delta; every non-target field and action is copied from the
// Host-owned baseline. A blocked delta therefore removes only its target's old
// mapping and cannot erase unrelated bindings.
func MergeExecutablePlanDelta(
	input Input,
	result Result,
	baseline string,
	candidate string,
) (string, error) {
	if input.EditContract == nil || input.EditContract.ChangeScope != "binding_update" {
		return "", fmt.Errorf("%w: binding delta requires binding_update authorization", ErrInvalidInput)
	}
	delta, err := ReconcileExecutablePlan(input, result, candidate)
	if err != nil {
		return "", err
	}
	baselineFields, err := planMappings(baseline, true)
	if err != nil {
		return "", fmt.Errorf("%w: baseline field mappings: %v", ErrInvalidInput, err)
	}
	baselineActions, err := planMappings(baseline, false)
	if err != nil {
		return "", fmt.Errorf("%w: baseline action mappings: %v", ErrInvalidInput, err)
	}
	deltaFields, err := planMappings(delta, true)
	if err != nil {
		return "", err
	}
	deltaActions, err := planMappings(delta, false)
	if err != nil {
		return "", err
	}

	targetFields, targetActions, err := planDeltaTargetKeys(input)
	if err != nil {
		return "", err
	}

	fields := make([]map[string]any, 0, len(baselineFields)+len(deltaFields))
	for _, mapping := range baselineFields {
		if _, targeted := targetFields[planString(mapping["refKey"])]; targeted {
			continue
		}
		fields = append(fields, mapping)
	}
	fields = append(fields, deltaFields...)
	actions := make([]map[string]any, 0, len(baselineActions)+len(deltaActions))
	for _, mapping := range baselineActions {
		if _, targeted := targetActions[planString(mapping["componentId"])]; targeted {
			continue
		}
		actions = append(actions, mapping)
	}
	actions = append(actions, deltaActions...)
	return EncodeExecutablePlan(fields, actions)
}

// ValidateProtectedExecutablePlan proves that deterministic finalization changed
// only the target authorized by a binding_update Edit Contract. The Result
// validates the target delta separately; every protected baseline field and
// action must remain byte-equivalent as canonical JSON, including transform,
// kind and path. This is a Host-state invariant and is not model-correctable.
func ValidateProtectedExecutablePlan(input Input, before, after string) error {
	if input.EditContract == nil || input.EditContract.ChangeScope != "binding_update" {
		return nil
	}
	targetFields, targetActions, err := planDeltaTargetKeys(input)
	if err != nil {
		return err
	}
	beforeFields, err := planMappings(before, true)
	if err != nil {
		return err
	}
	afterFields, err := planMappings(after, true)
	if err != nil {
		return err
	}
	if err := compareProtectedMappings(
		"field", "refKey", targetFields, beforeFields, afterFields,
	); err != nil {
		return err
	}
	beforeActions, err := planMappings(before, false)
	if err != nil {
		return err
	}
	afterActions, err := planMappings(after, false)
	if err != nil {
		return err
	}
	return compareProtectedMappings(
		"action", "componentId", targetActions, beforeActions, afterActions,
	)
}

func planDeltaTargetKeys(input Input) (map[string]struct{}, map[string]struct{}, error) {
	fieldSlots, err := planFieldSlots(input.Design.FieldHints)
	if err != nil {
		return nil, nil, err
	}
	actionSlots, err := planActionSlots(input.Design.ActionSlots)
	if err != nil {
		return nil, nil, err
	}
	authorized, _ := authorizedRequirements(input)
	targetFields := make(map[string]struct{})
	targetActions := make(map[string]struct{})
	actionOwners := make(map[string][]string)
	for _, requirement := range input.Requirements.Data {
		if _, ok := authorized[requirement.RequirementID]; !ok {
			continue
		}
		for _, slotID := range requirement.TargetSlotIDs {
			slot, ok := fieldSlots[slotID]
			if !ok {
				return nil, nil, fmt.Errorf("%w: binding delta field slot %q is missing", ErrInvalidInput, slotID)
			}
			targetFields[slot.RefKey] = struct{}{}
		}
	}
	for _, requirement := range input.Requirements.Actions {
		_, isAuthorized := authorized[requirement.RequirementID]
		for _, slotID := range requirement.TargetSlotIDs {
			slot, ok := actionSlots[slotID]
			if !ok {
				if isAuthorized {
					return nil, nil, fmt.Errorf("%w: binding delta action slot %q is missing", ErrInvalidInput, slotID)
				}
				continue
			}
			actionOwners[slot.ComponentID] = appendUniqueStrings(
				actionOwners[slot.ComponentID], requirement.RequirementID,
			)
			if isAuthorized {
				targetActions[slot.ComponentID] = struct{}{}
			}
		}
	}
	if len(targetFields)+len(targetActions) == 0 {
		return nil, nil, fmt.Errorf("%w: binding delta has no frozen targets", ErrInvalidInput)
	}
	if err := validateDeltaActionOwners(targetActions, actionOwners, authorized); err != nil {
		return nil, nil, err
	}
	return targetFields, targetActions, nil
}

func validateDeltaActionOwners(
	targets map[string]struct{},
	owners map[string][]string,
	authorized map[string]struct{},
) error {
	keys := make([]string, 0, len(targets))
	for key := range targets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		for _, owner := range owners[key] {
			if _, ok := authorized[owner]; ok {
				continue
			}
			return fmt.Errorf(
				"%w: binding delta action componentId %q is also owned by protected requirement %q",
				ErrEditScopeViolation, key, owner,
			)
		}
	}
	return nil
}

func compareProtectedMappings(
	kind, keyName string,
	targets map[string]struct{},
	before, after []map[string]any,
) error {
	project := func(values []map[string]any) (map[string]string, error) {
		result := make(map[string]string)
		for _, value := range values {
			key := planString(value[keyName])
			if key == "" {
				return nil, fmt.Errorf("%w: protected %s mapping lacks %s", ErrInvalidResult, kind, keyName)
			}
			if _, targeted := targets[key]; targeted {
				continue
			}
			if _, duplicate := result[key]; duplicate {
				return nil, fmt.Errorf("%w: duplicate protected %s mapping %q", ErrInvalidResult, kind, key)
			}
			canonical, err := json.Marshal(value)
			if err != nil {
				return nil, fmt.Errorf("%w: encode protected %s mapping %q", ErrInvalidResult, kind, key)
			}
			result[key] = string(canonical)
		}
		return result, nil
	}
	want, err := project(before)
	if err != nil {
		return err
	}
	got, err := project(after)
	if err != nil {
		return err
	}
	if len(want) != len(got) {
		return fmt.Errorf("%w: protected %s mappings changed", ErrInvalidResult, kind)
	}
	for key, expected := range want {
		if got[key] != expected {
			return fmt.Errorf("%w: protected %s mapping %q changed", ErrInvalidResult, kind, key)
		}
	}
	return nil
}

type planFieldSlot struct {
	SlotID string `json:"slotId"`
	RefKey string `json:"refKey"`
}

type planActionSlot struct {
	SlotID           string `json:"slotId"`
	ComponentID      string `json:"componentId"`
	ContractActionID string `json:"contractActionId"`
}

type planExpectedField struct {
	RefKey    string
	SourceKey string
	BindingID string
}

type planExpectedAction struct {
	ComponentID string
	SourcePath  string
	EventName   string
	BindingIDs  []string
}

func planFieldSlots(raw json.RawMessage) (map[string]planFieldSlot, error) {
	var values []planFieldSlot
	if err := decodeJSON(raw, &values); err != nil {
		return nil, fmt.Errorf("%w: decode frozen field hints: %v", ErrInvalidInput, err)
	}
	result := make(map[string]planFieldSlot, len(values))
	for _, value := range values {
		value.SlotID = strings.TrimSpace(value.SlotID)
		value.RefKey = strings.TrimSpace(value.RefKey)
		if value.SlotID == "" || value.RefKey == "" {
			return nil, fmt.Errorf("%w: frozen field hint lacks slotId/refKey", ErrInvalidInput)
		}
		if _, exists := result[value.SlotID]; exists {
			return nil, fmt.Errorf("%w: duplicate frozen field slot %q", ErrInvalidInput, value.SlotID)
		}
		result[value.SlotID] = value
	}
	return result, nil
}

func planActionSlots(raw json.RawMessage) (map[string]planActionSlot, error) {
	var values []planActionSlot
	if err := decodeJSON(raw, &values); err != nil {
		return nil, fmt.Errorf("%w: decode frozen action slots: %v", ErrInvalidInput, err)
	}
	result := make(map[string]planActionSlot, len(values))
	for _, value := range values {
		value.SlotID = strings.TrimSpace(value.SlotID)
		value.ComponentID = strings.TrimSpace(value.ComponentID)
		value.ContractActionID = strings.TrimSpace(value.ContractActionID)
		if value.SlotID == "" || value.ComponentID == "" || value.ContractActionID == "" {
			return nil, fmt.Errorf("%w: frozen action slot lacks slotId/componentId/contractActionId", ErrInvalidInput)
		}
		if _, exists := result[value.SlotID]; exists {
			return nil, fmt.Errorf("%w: duplicate frozen action slot %q", ErrInvalidInput, value.SlotID)
		}
		result[value.SlotID] = value
	}
	return result, nil
}

func decodeJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}

func bindingOwnsFieldRef(binding Binding, slots map[string]planFieldSlot, refKey string) bool {
	for _, slotID := range binding.TargetSlotIDs {
		if slots[slotID].RefKey == refKey {
			return true
		}
	}
	return false
}

func bindingOwnsActionComponent(binding Binding, slots map[string]planActionSlot, componentID string) bool {
	for _, slotID := range binding.TargetSlotIDs {
		if slots[slotID].ComponentID == componentID {
			return true
		}
	}
	return false
}

func reconcileFields(
	expected []planExpectedField,
	candidates []map[string]any,
	allowEmptySynthesis bool,
	rejectExtras bool,
) ([]map[string]any, error) {
	expectedByRef := make(map[string]planExpectedField, len(expected))
	expectedOrder := make([]planExpectedField, 0, len(expected))
	for _, item := range expected {
		if existing, duplicate := expectedByRef[item.RefKey]; duplicate {
			if existing.SourceKey == item.SourceKey && existing.BindingID == item.BindingID {
				continue
			}
			return nil, fmt.Errorf("%w: two target slots resolve to duplicate refKey %q", ErrInvalidResult, item.RefKey)
		}
		expectedByRef[item.RefKey] = item
		expectedOrder = append(expectedOrder, item)
	}

	boundCandidates := make(map[string]map[string]any, len(candidates))
	boundFingerprints := make(map[string]string, len(candidates))
	bindingCandidateCounts := make(map[string]int, len(expectedOrder))
	for _, candidate := range candidates {
		refKey := planString(candidate["refKey"])
		sourceKey := planString(candidate["sourceKey"])
		if sourceKey == "" {
			continue
		}
		expectedItem, declared := expectedByRef[refKey]
		if !declared {
			if rejectExtras {
				return nil, fmt.Errorf("%w: extra field mapping is not declared by Bind Result", ErrInvalidResult)
			}
			continue
		}
		canonical, fingerprint, err := canonicalFieldCandidate(
			refKey, sourceKey, candidate,
		)
		if err != nil {
			return nil, err
		}
		if existing, exists := boundFingerprints[refKey]; exists {
			if existing != fingerprint {
				return nil, fmt.Errorf(
					"%w: duplicate field mapping for %q has conflicting sourceKey or transform",
					ErrInvalidResult, refKey,
				)
			}
			// The executable field identity is refKey + sourceKey + transform.
			// componentId is not part of the field mapping wire contract and is
			// deliberately omitted, so equivalent model fan-out cannot expand the
			// frozen Design slot's authorization.
			continue
		}
		if sourceKey != expectedItem.SourceKey {
			return nil, fmt.Errorf(
				"%w: field mapping %q sourceKey %q differs from Bind Result %q",
				ErrInvalidResult, refKey, sourceKey, expectedItem.SourceKey,
			)
		}
		boundCandidates[refKey] = canonical
		boundFingerprints[refKey] = fingerprint
		bindingCandidateCounts[expectedItem.BindingID]++
	}
	if len(boundCandidates) < len(expectedOrder) {
		if !allowEmptySynthesis {
			return nil, fmt.Errorf("%w: missing field mapping: got %d want %d", ErrInvalidResult, len(boundCandidates), len(expectedOrder))
		}
		// An entirely empty compatibility block is safe to synthesize because
		// Result owns every direct source path. For a partial block, only fan out
		// within a binding that supplied at least one compatible mapping; never
		// fill an unrelated omitted requirement.
		if len(boundCandidates) != 0 {
			for _, item := range expectedOrder {
				if _, exists := boundCandidates[item.RefKey]; exists {
					continue
				}
				if bindingCandidateCounts[item.BindingID] == 0 {
					return nil, fmt.Errorf("%w: missing field mapping: got %d want %d", ErrInvalidResult, len(boundCandidates), len(expectedOrder))
				}
			}
		}
	}

	result := make([]map[string]any, 0, len(expectedOrder))
	for _, item := range expectedOrder {
		candidate, exists := boundCandidates[item.RefKey]
		if !exists {
			if !allowEmptySynthesis {
				return nil, fmt.Errorf("%w: missing field mapping for %q", ErrInvalidResult, item.RefKey)
			}
			candidate = map[string]any{
				"refKey": item.RefKey, "sourceKey": item.SourceKey,
			}
			result = append(result, candidate)
			continue
		}
		if sourceKey := planString(candidate["sourceKey"]); sourceKey != item.SourceKey {
			return nil, fmt.Errorf(
				"%w: field mapping %q sourceKey %q differs from Bind Result %q",
				ErrInvalidResult, item.RefKey, sourceKey, item.SourceKey,
			)
		}
		result = append(result, candidate)
	}
	return result, nil
}

func canonicalFieldCandidate(
	refKey string,
	sourceKey string,
	candidate map[string]any,
) (map[string]any, string, error) {
	canonical := map[string]any{
		"refKey": refKey, "sourceKey": sourceKey,
	}
	if _, exists := candidate["transform"]; exists {
		return nil, "", fmt.Errorf("%w: field mapping %q must not contain inline transform; use binding.transforms", ErrInvalidResult, refKey)
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, "", fmt.Errorf(
			"%w: canonicalize field mapping %q: %v",
			ErrInvalidResult, refKey, err,
		)
	}
	return canonical, string(encoded), nil
}

func reconcileActions(
	expected []planExpectedAction,
	candidates []map[string]any,
	allowSynthesis bool,
	rejectExtras bool,
) ([]map[string]any, error) {
	expectedByComponent := make(map[string]planExpectedAction, len(expected))
	expectedOrder := make([]planExpectedAction, 0, len(expected))
	for _, item := range expected {
		if existing, duplicate := expectedByComponent[item.ComponentID]; duplicate {
			if existing.SourcePath != item.SourcePath || existing.EventName != item.EventName {
				return nil, fmt.Errorf("%w: two target slots resolve to duplicate componentId %q with different paths", ErrInvalidResult, item.ComponentID)
			}
			existing.BindingIDs = appendUniqueStrings(existing.BindingIDs, item.BindingIDs...)
			expectedByComponent[item.ComponentID] = existing
			for index := range expectedOrder {
				if expectedOrder[index].ComponentID == item.ComponentID {
					expectedOrder[index] = existing
					break
				}
			}
			continue
		}
		expectedByComponent[item.ComponentID] = item
		expectedOrder = append(expectedOrder, item)
	}

	boundCandidates := make(map[string]map[string]any, len(candidates))
	boundFingerprints := make(map[string]string, len(candidates))
	bindingCandidateCounts := make(map[string]int, len(expected))
	bindingKinds := make(map[string]string, len(expected))
	for _, candidate := range candidates {
		componentID := firstPlanString(candidate, "componentId", "component_id")
		kind := firstPlanString(candidate, "actionSourceType", "action_source_type")
		urlPath := firstPlanString(candidate, "urlPath", "url_path")
		eventPath := firstPlanString(candidate, "eventPath", "event_path")
		// Bind Result uses action_path while the historical executable plan used
		// urlPath/eventPath. Normalize that transport difference at this single
		// boundary; requirement, component and source-path authority still comes
		// from the frozen Result and is checked below.
		if actionPath := planString(candidate["action_path"]); actionPath != "" {
			switch kind {
			case "url":
				if urlPath == "" {
					urlPath = actionPath
				}
			case "event":
				if eventPath == "" {
					eventPath = actionPath
				}
			}
		}
		if kind == "" && urlPath == "" && eventPath == "" {
			continue
		}
		if _, declared := expectedByComponent[componentID]; !declared {
			if rejectExtras {
				return nil, fmt.Errorf("%w: extra action mapping is not declared by Bind Result", ErrInvalidResult)
			}
			continue
		}
		expectedItem := expectedByComponent[componentID]
		matched := kind == "url" && urlPath == expectedItem.SourcePath && eventPath == "" ||
			kind == "event" && eventPath == expectedItem.SourcePath && urlPath == ""
		if !matched {
			return nil, fmt.Errorf(
				"%w: action path for %q differs from Bind Result %q",
				ErrInvalidResult, componentID, expectedItem.SourcePath,
			)
		}
		fingerprint := kind + "\x00" + expectedItem.SourcePath
		if existing, exists := boundFingerprints[componentID]; exists {
			if existing != fingerprint {
				return nil, fmt.Errorf("%w: duplicate action mapping for %q conflicts", ErrInvalidResult, componentID)
			}
			continue
		}
		canonicalCandidate := map[string]any{
			"componentId": componentID, "actionSourceType": kind,
		}
		if kind == "url" {
			canonicalCandidate["urlPath"] = urlPath
		} else if kind == "event" {
			canonicalCandidate["eventPath"] = eventPath
			canonicalCandidate["eventName"] = expectedItem.EventName
		}
		boundCandidates[componentID] = canonicalCandidate
		boundFingerprints[componentID] = fingerprint
		for _, bindingID := range expectedItem.BindingIDs {
			bindingCandidateCounts[bindingID]++
			if existing := bindingKinds[bindingID]; existing != "" && existing != kind {
				return nil, fmt.Errorf("%w: target slots for binding %q have conflicting action kinds", ErrInvalidResult, bindingID)
			}
			bindingKinds[bindingID] = kind
		}
	}
	if len(boundCandidates) < len(expectedOrder) {
		if !allowSynthesis {
			return nil, fmt.Errorf("%w: missing action mapping: got %d want %d", ErrInvalidResult, len(boundCandidates), len(expectedOrder))
		}
		for _, item := range expectedOrder {
			if _, exists := boundCandidates[item.ComponentID]; exists {
				continue
			}
			covered := false
			for _, bindingID := range item.BindingIDs {
				covered = covered || bindingCandidateCounts[bindingID] > 0
			}
			if !covered {
				return nil, fmt.Errorf("%w: missing action mapping: got %d want %d", ErrInvalidResult, len(boundCandidates), len(expectedOrder))
			}
		}
	}

	result := make([]map[string]any, 0, len(expectedOrder))
	for _, item := range expectedOrder {
		candidate, exists := boundCandidates[item.ComponentID]
		if !exists {
			if !allowSynthesis {
				return nil, fmt.Errorf("%w: missing action mapping for %q", ErrInvalidResult, item.ComponentID)
			}
			kind := ""
			for _, bindingID := range item.BindingIDs {
				if bindingKinds[bindingID] != "" {
					kind = bindingKinds[bindingID]
					break
				}
			}
			if kind == "" {
				return nil, fmt.Errorf("%w: missing action kind for %q", ErrInvalidResult, item.ComponentID)
			}
			candidate = map[string]any{"actionSourceType": kind}
			if kind == "url" {
				candidate["urlPath"] = item.SourcePath
			} else {
				candidate["eventPath"] = item.SourcePath
				candidate["eventName"] = item.EventName
			}
		}
		kind := planString(candidate["actionSourceType"])
		urlPath := planString(candidate["urlPath"])
		eventPath := planString(candidate["eventPath"])
		matched := kind == "url" && urlPath == item.SourcePath && eventPath == "" ||
			kind == "event" && eventPath == item.SourcePath && urlPath == ""
		if !matched {
			return nil, fmt.Errorf(
				"%w: action path for %q differs from Bind Result %q",
				ErrInvalidResult, item.ComponentID, item.SourcePath,
			)
		}
		canonical := map[string]any{
			"componentId": item.ComponentID, "actionSourceType": kind,
		}
		if kind == "url" {
			canonical["urlPath"] = item.SourcePath
		} else {
			canonical["eventPath"] = item.SourcePath
			canonical["eventName"] = item.EventName
		}
		result = append(result, canonical)
	}
	return result, nil
}

func firstPlanString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if result := planString(value[key]); result != "" {
			return result
		}
	}
	return ""
}

func appendUniqueStrings(values []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(additions))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	for _, value := range additions {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	return values
}

func planMappings(value string, fields bool) ([]map[string]any, error) {
	plan, err := ParseExecutablePlan(value)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidResult, err)
	}
	if fields {
		return plan.FieldMappings, nil
	}
	return plan.ActionMappings, nil
}

func planString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}
