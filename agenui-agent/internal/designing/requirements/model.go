package requirements

const SchemaVersion = "requirement_set.v1"

// FieldSlot is the validator-approved semantic declaration for one dynamic
// design value. It contains no source, API field, JSONPath, or operator choice.
type FieldSlot struct {
	SlotID         string `json:"slot_id"`
	ContractItemID string `json:"contract_item_id"`
	RefKey         string `json:"ref_key"`
	Role           string `json:"role,omitempty"`
	ValueType      string `json:"value_type"`
	Scope          string `json:"scope"`
	Format         string `json:"format,omitempty"`
	Required       *bool  `json:"required,omitempty"`
}

// ActionSlot is the validator-approved semantic declaration for one clickable
// design element. Binding-specific parameters are intentionally absent.
type ActionSlot struct {
	SlotID           string `json:"slot_id"`
	ContractActionID string `json:"contract_action_id"`
	Role             string `json:"role"`
	Scope            string `json:"scope"`
}

type DataRequirement struct {
	RequirementID   string   `json:"requirement_id"`
	ContractItemID  string   `json:"contract_item_id"`
	Description     string   `json:"description"`
	TargetSlotIDs   []string `json:"target_slot_ids"`
	Level           string   `json:"level"`
	Type            string   `json:"type"`
	Shape           string   `json:"shape"`
	Format          string   `json:"format,omitempty"`
	WhenMissing     string   `json:"when_missing"`
	OperatorAllowed bool     `json:"operator_allowed,omitempty"`
}

type ActionRequirement struct {
	RequirementID    string   `json:"requirement_id"`
	ContractActionID string   `json:"contract_action_id"`
	Description      string   `json:"description"`
	TargetSlotIDs    []string `json:"target_slot_ids"`
	Level            string   `json:"level"`
	NeedConfirmation bool     `json:"need_confirmation"`
	WhenFailed       string   `json:"when_failed"`
}

type Set struct {
	SchemaVersion string              `json:"schema_version"`
	InputHash     string              `json:"input_hash"`
	Data          []DataRequirement   `json:"data_requirements"`
	Actions       []ActionRequirement `json:"action_requirements"`
}

type Report struct {
	InputHash       string   `json:"input_hash"`
	RequiredTotal   int      `json:"required_total"`
	RequiredCovered int      `json:"required_covered"`
	Warnings        []string `json:"warnings,omitempty"`
}

type Result struct {
	RequirementSet Set    `json:"requirement_set"`
	Report         Report `json:"compilation_report"`
}
