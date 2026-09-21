package requirements

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/contract"
)

type Input struct {
	Contract    contract.Draft `json:"contract"`
	FieldSlots  []FieldSlot    `json:"field_slots"`
	ActionSlots []ActionSlot   `json:"action_slots"`
}

// Compile is deliberately model-free and knowledge-free. It canonicalizes all
// input before deriving stable IDs, ordering, and hashes.
func Compile(source Input) (Result, error) {
	canonicalContract, err := contract.Canonicalize(source.Contract)
	if err != nil {
		return Result{}, err
	}
	actionIDs := make(map[string]struct{}, len(canonicalContract.Actions))
	for _, action := range canonicalContract.Actions {
		actionIDs[action.ID] = struct{}{}
	}
	filteredFieldSlots := make([]FieldSlot, 0, len(source.FieldSlots))
	ignoredActionLabelSlots := make([]string, 0)
	for _, slot := range source.FieldSlots {
		if _, isAction := actionIDs[strings.TrimSpace(slot.ContractItemID)]; isAction {
			ignoredActionLabelSlots = append(ignoredActionLabelSlots, strings.TrimSpace(slot.SlotID))
			continue
		}
		filteredFieldSlots = append(filteredFieldSlots, slot)
	}
	fieldSlots, err := canonicalFieldSlots(filteredFieldSlots)
	if err != nil {
		return Result{}, err
	}
	actionSlots, err := canonicalActionSlots(source.ActionSlots)
	if err != nil {
		return Result{}, err
	}
	canonicalInput := Input{Contract: canonicalContract, FieldSlots: fieldSlots, ActionSlots: actionSlots}
	inputBytes, err := json.Marshal(canonicalInput)
	if err != nil {
		return Result{}, err
	}
	inputSum := sha256.Sum256(inputBytes)
	inputHash := "sha256:" + hex.EncodeToString(inputSum[:])

	contents := make(map[string]contract.ContentItem, len(canonicalContract.Contents))
	for _, item := range canonicalContract.Contents {
		contents[item.ID] = item
	}
	actions := make(map[string]contract.ActionItem, len(canonicalContract.Actions))
	for _, action := range canonicalContract.Actions {
		actions[action.ID] = action
	}
	type dataKey struct{ itemID, valueType, scope, format, role string }
	dataGroups := make(map[dataKey][]FieldSlot)
	for _, slot := range fieldSlots {
		_, exists := contents[slot.ContractItemID]
		if !exists {
			return Result{}, fmt.Errorf("requirement compiler: unknown content %q", slot.ContractItemID)
		}
		key := dataKey{slot.ContractItemID, slot.ValueType, slot.Scope, slot.Format, slot.Role}
		dataGroups[key] = append(dataGroups[key], slot)
	}

	result := Result{RequirementSet: Set{
		SchemaVersion: SchemaVersion, InputHash: inputHash,
	}}
	keys := make([]dataKey, 0, len(dataGroups))
	for key := range dataGroups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return dataKeyString(keys[i]) < dataKeyString(keys[j])
	})
	coveredContents := make(map[string]struct{}, len(keys))
	requiredSlotContents := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		item := contents[key.itemID]
		group := dataGroups[key]
		slotIDs := make([]string, 0, len(group))
		for _, slot := range group {
			slotIDs = append(slotIDs, slot.SlotID)
		}
		slots := uniqueSorted(slotIDs)
		level, missing := "optional", "hide"
		if item.Required && fieldGroupRequired(group) {
			level, missing = "core", "block"
			requiredSlotContents[item.ID] = struct{}{}
		}
		result.RequirementSet.Data = append(result.RequirementSet.Data, DataRequirement{
			RequirementID: stableID("data", dataKeyString(key)), ContractItemID: item.ID,
			Description: item.Description, TargetSlotIDs: slots, Level: level,
			Type: key.valueType, Shape: shapeForSlots(key.scope, group), Format: key.format,
			WhenMissing: missing, OperatorAllowed: key.format != "",
		})
		coveredContents[item.ID] = struct{}{}
	}

	type actionKey struct{ actionID, scope, role string }
	actionGroups := make(map[actionKey][]string)
	for _, slot := range actionSlots {
		if _, exists := actions[slot.ContractActionID]; !exists {
			return Result{}, fmt.Errorf("requirement compiler: unknown action %q", slot.ContractActionID)
		}
		key := actionKey{slot.ContractActionID, slot.Scope, slot.Role}
		actionGroups[key] = append(actionGroups[key], slot.SlotID)
	}
	actionKeys := make([]actionKey, 0, len(actionGroups))
	for key := range actionGroups {
		actionKeys = append(actionKeys, key)
	}
	sort.Slice(actionKeys, func(i, j int) bool {
		return actionKeyString(actionKeys[i]) < actionKeyString(actionKeys[j])
	})
	coveredActions := make(map[string]struct{}, len(actionKeys))
	for _, key := range actionKeys {
		action := actions[key.actionID]
		result.RequirementSet.Actions = append(result.RequirementSet.Actions, ActionRequirement{
			RequirementID: stableID("action", actionKeyString(key)), ContractActionID: action.ID,
			Description: action.Description, TargetSlotIDs: uniqueSorted(actionGroups[key]),
			Level: "core", NeedConfirmation: false, WhenFailed: "block_publish",
		})
		coveredActions[action.ID] = struct{}{}
		result.Report.Warnings = append(result.Report.Warnings, "action_context_pending_binding:"+action.ID)
	}

	result.Report.InputHash = inputHash
	for _, slotID := range uniqueSorted(ignoredActionLabelSlots) {
		result.Report.Warnings = append(result.Report.Warnings, "ignored_action_label_field_slot:"+slotID)
	}
	for _, item := range canonicalContract.Contents {
		if !item.Required {
			continue
		}
		result.Report.RequiredTotal++
		if _, exists := coveredContents[item.ID]; !exists {
			return Result{}, fmt.Errorf("requirement compiler: required content %q has no slot", item.ID)
		}
		if _, exists := requiredSlotContents[item.ID]; !exists {
			return Result{}, fmt.Errorf("requirement compiler: required content %q has no required slot", item.ID)
		}
		result.Report.RequiredCovered++
	}
	for _, action := range canonicalContract.Actions {
		result.Report.RequiredTotal++
		if _, exists := coveredActions[action.ID]; !exists {
			return Result{}, fmt.Errorf("requirement compiler: action %q has no slot", action.ID)
		}
		result.Report.RequiredCovered++
	}
	sort.Strings(result.Report.Warnings)
	if result.RequirementSet.Data == nil {
		result.RequirementSet.Data = []DataRequirement{}
	}
	if result.RequirementSet.Actions == nil {
		result.RequirementSet.Actions = []ActionRequirement{}
	}
	return result, nil
}

func canonicalFieldSlots(source []FieldSlot) ([]FieldSlot, error) {
	result := append([]FieldSlot(nil), source...)
	seen := make(map[string]struct{}, len(result))
	for index := range result {
		slot := &result[index]
		slot.SlotID = strings.TrimSpace(slot.SlotID)
		slot.ContractItemID = strings.TrimSpace(slot.ContractItemID)
		slot.RefKey = strings.TrimSpace(slot.RefKey)
		slot.Role = normalizeFieldRole(slot.Role)
		slot.ValueType = strings.ToLower(strings.TrimSpace(slot.ValueType))
		if slot.ValueType == "" {
			slot.ValueType = "string"
		}
		slot.Scope = strings.ToLower(strings.TrimSpace(slot.Scope))
		if slot.Scope == "" {
			slot.Scope = "surface"
		}
		slot.Format = strings.TrimSpace(slot.Format)
		if slot.SlotID == "" || slot.ContractItemID == "" {
			return nil, errors.New("requirement compiler: field slot id and contract item are required")
		}
		if _, exists := seen[slot.SlotID]; exists {
			return nil, fmt.Errorf("requirement compiler: duplicate slot %q", slot.SlotID)
		}
		seen[slot.SlotID] = struct{}{}
	}
	sort.Slice(result, func(i, j int) bool { return fieldSlotKey(result[i]) < fieldSlotKey(result[j]) })
	return result, nil
}

func canonicalActionSlots(source []ActionSlot) ([]ActionSlot, error) {
	result := append([]ActionSlot(nil), source...)
	seen := make(map[string]struct{}, len(result))
	for index := range result {
		slot := &result[index]
		slot.SlotID = strings.TrimSpace(slot.SlotID)
		slot.ContractActionID = strings.TrimSpace(slot.ContractActionID)
		slot.Role = strings.TrimSpace(slot.Role)
		if slot.Role == "" {
			slot.Role = "action"
		}
		slot.Scope = strings.ToLower(strings.TrimSpace(slot.Scope))
		if slot.Scope == "" {
			slot.Scope = "surface"
		}
		if slot.SlotID == "" || slot.ContractActionID == "" {
			return nil, errors.New("requirement compiler: action slot id and contract action are required")
		}
		if _, exists := seen[slot.SlotID]; exists {
			return nil, fmt.Errorf("requirement compiler: duplicate slot %q", slot.SlotID)
		}
		seen[slot.SlotID] = struct{}{}
	}
	sort.Slice(result, func(i, j int) bool { return actionSlotKey(result[i]) < actionSlotKey(result[j]) })
	return result, nil
}

func dataKeyString(key struct{ itemID, valueType, scope, format, role string }) string {
	return strings.Join([]string{key.itemID, key.valueType, key.scope, key.format, key.role}, "|")
}

func fieldGroupRequired(slots []FieldSlot) bool {
	for _, slot := range slots {
		// Programmatic v1 callers did not carry per-slot requiredness. Preserve
		// their contract-level behavior while runtime Style output always supplies
		// the explicit boolean enforced by the template validator.
		if slot.Required == nil || *slot.Required {
			return true
		}
	}
	return false
}

func actionKeyString(key struct{ actionID, scope, role string }) string {
	return strings.Join([]string{key.actionID, key.scope, key.role}, "|")
}

func fieldSlotKey(slot FieldSlot) string {
	return strings.Join([]string{slot.ContractItemID, slot.SlotID, slot.Role, slot.ValueType, slot.Scope, slot.Format}, "|")
}

func normalizeFieldRole(value string) string {
	value = strings.TrimSpace(value)
	normalized := make([]byte, 0, len(value)+4)
	appendSeparator := func() {
		if len(normalized) > 0 && normalized[len(normalized)-1] != '_' {
			normalized = append(normalized, '_')
		}
	}
	for index := 0; index < len(value); index++ {
		current := value[index]
		if current == '-' || current == ' ' {
			appendSeparator()
			continue
		}
		if index > 0 && asciiIdentifierByte(value[index-1]) &&
			asciiIdentifierByte(current) && englishIdentifierBoundary(value, index) {
			appendSeparator()
		}
		if asciiUpper(current) {
			current += 'a' - 'A'
		}
		normalized = append(normalized, current)
	}
	return strings.Trim(string(normalized), "_")
}

func englishIdentifierBoundary(value string, index int) bool {
	if index <= 0 || index >= len(value) {
		return true
	}
	left, right := value[index-1], value[index]
	if !asciiIdentifierByte(left) || !asciiIdentifierByte(right) {
		return true
	}
	if asciiDigit(left) != asciiDigit(right) {
		return true
	}
	if asciiLower(left) && asciiUpper(right) {
		return true
	}
	return asciiUpper(left) && asciiUpper(right) && index+1 < len(value) &&
		asciiLower(value[index+1])
}

func asciiIdentifierByte(value byte) bool {
	return asciiLower(value) || asciiUpper(value) || asciiDigit(value)
}

func asciiLower(value byte) bool { return value >= 'a' && value <= 'z' }
func asciiUpper(value byte) bool { return value >= 'A' && value <= 'Z' }
func asciiDigit(value byte) bool { return value >= '0' && value <= '9' }

func actionSlotKey(slot ActionSlot) string {
	return strings.Join([]string{slot.ContractActionID, slot.SlotID, slot.Scope, slot.Role}, "|")
}

func shapeForSlots(scope string, slots []FieldSlot) string {
	if strings.Contains(scope, "[*]") {
		return "list_item"
	}
	for _, slot := range slots {
		if strings.Contains(slot.RefKey, "[*]") {
			return "list_item"
		}
	}
	return "single"
}

func stableID(prefix, semanticKey string) string {
	sum := sha256.Sum256([]byte(semanticKey))
	return prefix + "." + hex.EncodeToString(sum[:6])
}

func uniqueSorted(source []string) []string {
	seen := make(map[string]struct{}, len(source))
	for _, value := range source {
		seen[value] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
