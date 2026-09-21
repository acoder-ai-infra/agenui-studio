package requirements

import (
	"encoding/json"
	"errors"
	"strings"
)

type fieldSlotWire struct {
	SlotID         string `json:"slotId"`
	ContractItemID string `json:"contractItemId"`
	RefKey         string `json:"refKey"`
	Role           string `json:"role"`
	ValueType      string `json:"valueType"`
	Scope          string `json:"scope"`
	Format         string `json:"format"`
	Required       *bool  `json:"required"`
}

type actionSlotWire struct {
	SlotID           string `json:"slotId"`
	ContractActionID string `json:"contractActionId"`
	Role             string `json:"role"`
	Scope            string `json:"scope"`
}

// ParseSlots projects Workspace-owned typed slot arrays into the deterministic
// Requirement compiler input. Workspace has already frozen slot identity; this
// layer does not parse or rewrite a text protocol.
func ParseSlots(fieldSlots, actionSlots []map[string]any) ([]FieldSlot, []ActionSlot, error) {
	fieldRaw, err := json.Marshal(fieldSlots)
	if err != nil {
		return nil, nil, errors.New("requirement parser: field slots cannot be encoded")
	}
	var fieldWire []fieldSlotWire
	if err := json.Unmarshal(fieldRaw, &fieldWire); err != nil {
		return nil, nil, errors.New("requirement parser: field slots are invalid")
	}
	fields := make([]FieldSlot, 0, len(fieldWire))
	for _, slot := range fieldWire {
		if strings.TrimSpace(slot.ContractItemID) == "" {
			continue
		}
		fields = append(fields, FieldSlot{
			SlotID: slot.SlotID, ContractItemID: slot.ContractItemID,
			RefKey: slot.RefKey, Role: slot.Role, ValueType: slot.ValueType,
			Scope: slot.Scope, Format: slot.Format, Required: slot.Required,
		})
	}
	actionRaw, err := json.Marshal(actionSlots)
	if err != nil {
		return nil, nil, errors.New("requirement parser: action slots cannot be encoded")
	}
	var actionWire []actionSlotWire
	if err := json.Unmarshal(actionRaw, &actionWire); err != nil {
		return nil, nil, errors.New("requirement parser: action slots are invalid")
	}
	actions := make([]ActionSlot, 0, len(actionWire))
	for _, slot := range actionWire {
		if strings.TrimSpace(slot.ContractActionID) == "" {
			continue
		}
		actions = append(actions, ActionSlot{
			SlotID: slot.SlotID, ContractActionID: slot.ContractActionID,
			Role: slot.Role, Scope: slot.Scope,
		})
	}
	return fields, actions, nil
}
