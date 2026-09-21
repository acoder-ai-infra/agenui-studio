// Package contract defines the Content Contract First boundary owned by the
// Bind Agent. It deliberately keeps API fields, JSONPath and operator
// choices out of Contract, Design and Requirement artifacts; those choices
// appear only in the Bind Result and always trace back to a requirement_id.
package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/contract"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/edit"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/requirements"
)

const (
	InputSchemaV1  = "agenui.binder-input/v1"
	ResultSchemaV1 = "agenui.bind-result/v1"

	StatusReady              = "ready"
	StatusReadyWithOperators = "ready_with_operators"
	StatusBlocked            = "blocked"
	StatusUncertain          = "uncertain"

	maxInputBytes  = 4 << 20
	maxResultItems = 4096
)

// IsExecutableStatus is the single delivery boundary shared by persistence,
// presentation and package export. Blocked/uncertain results are valid Binder
// outcomes, but they are never executable deliveries.
func IsExecutableStatus(status string) bool {
	return status == StatusReady || status == StatusReadyWithOperators
}

var (
	ErrInvalidInput       = errors.New("invalid Binder input")
	ErrInvalidResult      = errors.New("invalid Bind Result")
	ErrEditScopeViolation = errors.New("EDIT_SCOPE_VIOLATION")
)

type DesignSnapshot struct {
	Ref         string          `json:"ref"`
	ContentHash string          `json:"content_hash"`
	FieldHints  json.RawMessage `json:"field_hints"`
	ActionSlots json.RawMessage `json:"action_slots"`
}

type SourceSnapshot struct {
	SourceID    string   `json:"source_id"`
	KnowledgeID string   `json:"knowledge_id"`
	Primary     bool     `json:"primary,omitempty"`
	Namespace   string   `json:"namespace,omitempty"`
	Paths       []string `json:"paths,omitempty"`
	ListPath    string   `json:"list_path,omitempty"`
}

type Input struct {
	SchemaVersion string            `json:"schema_version"`
	Contract      contract.Revision `json:"content_contract"`
	Design        DesignSnapshot    `json:"design"`
	Requirements  requirements.Set  `json:"requirements"`
	EditContract  *edit.Contract    `json:"edit_contract,omitempty"`
	Sources       []SourceSnapshot  `json:"sources"`
}

type Binding struct {
	RequirementID    string      `json:"requirement_id"`
	TargetSlotIDs    []string    `json:"target_slot_ids"`
	SourceID         string      `json:"source_id"`
	KnowledgeID      string      `json:"knowledge_id"`
	FieldPath        string      `json:"field_path,omitempty"`
	ActionPath       string      `json:"action_path,omitempty"`
	ActionSourceType string      `json:"action_source_type,omitempty"`
	Transforms       []Transform `json:"transforms,omitempty"`
	RefKey           string      `json:"ref_key,omitempty"`
	ComponentID      string      `json:"component_id,omitempty"`
}

// Transform is an ordered, typed operator invocation applied to the value
// selected by FieldPath. The source path remains authoritative; an operator
// never becomes a second synthetic data source.
type Transform struct {
	OperatorVersionID uint64         `json:"operator_version_id"`
	Params            map[string]any `json:"params,omitempty"`
}

type Issue struct {
	RequirementID string `json:"requirement_id"`
	Code          string `json:"code"`
	Message       string `json:"message"`
}

type Result struct {
	SchemaVersion string    `json:"schema_version"`
	Status        string    `json:"status"`
	Bindings      []Binding `json:"bindings"`
	Issues        []Issue   `json:"issues,omitempty"`
}

func BuildInput(input Input) ([]byte, error) {
	if err := validateInput(input); err != nil {
		return nil, err
	}
	canonical := input
	canonical.Sources = append([]SourceSnapshot(nil), input.Sources...)
	for index := range canonical.Sources {
		canonical.Sources[index].Paths = append([]string(nil), canonical.Sources[index].Paths...)
		slices.Sort(canonical.Sources[index].Paths)
	}
	slices.SortFunc(canonical.Sources, func(left, right SourceSnapshot) int {
		if left.Primary != right.Primary {
			if left.Primary {
				return -1
			}
			return 1
		}
		return strings.Compare(left.SourceID+"\x00"+left.KnowledgeID, right.SourceID+"\x00"+right.KnowledgeID)
	})
	encoded, err := json.Marshal(canonical)
	if err != nil || len(encoded) > maxInputBytes {
		return nil, fmt.Errorf("%w: encode or size", ErrInvalidInput)
	}
	return encoded, nil
}

func ParseInput(raw []byte) (Input, error) {
	if len(raw) == 0 || len(raw) > maxInputBytes {
		return Input{}, ErrInvalidInput
	}
	var input Input
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return Input{}, fmt.Errorf("%w: decode", ErrInvalidInput)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return Input{}, fmt.Errorf("%w: trailing data", ErrInvalidInput)
	}
	canonical, err := BuildInput(input)
	if err != nil {
		return Input{}, err
	}
	if !bytes.Equal(raw, canonical) {
		return Input{}, fmt.Errorf("%w: non-canonical JSON", ErrInvalidInput)
	}
	return input, nil
}

// ProjectCandidateIssues removes diagnostics that cannot describe an
// unresolved requirement in the current admission scope. It deliberately does
// not repair or otherwise normalize retained issues; ValidateResult remains the
// strict authority for their shape, uniqueness and status consistency.
func ProjectCandidateIssues(input Input, result *Result) {
	if result == nil || len(result.Issues) == 0 {
		return
	}

	authorized, delta := authorizedRequirements(input)
	if !delta {
		authorized = make(map[string]struct{}, len(input.Requirements.Data)+len(input.Requirements.Actions))
		for _, requirement := range input.Requirements.Data {
			authorized[requirement.RequirementID] = struct{}{}
		}
		for _, requirement := range input.Requirements.Actions {
			authorized[requirement.RequirementID] = struct{}{}
		}
	}

	bound := make(map[string]struct{}, len(result.Bindings))
	for _, binding := range result.Bindings {
		bound[binding.RequirementID] = struct{}{}
	}
	projected := make([]Issue, 0, len(result.Issues))
	for _, issue := range result.Issues {
		if _, ok := authorized[issue.RequirementID]; !ok {
			continue
		}
		if _, ok := bound[issue.RequirementID]; ok {
			continue
		}
		projected = append(projected, issue)
	}
	result.Issues = projected
}

// NormalizeCandidateStatus repairs only one conservative contradiction in a
// model candidate: a ready verdict paired with an honest issue for a required
// requirement. The Host never invents a binding or an issue. It merely turns
// the publication verdict into blocked so the already validated partial plan
// can survive admission. Missing or malformed issues remain hard failures in
// ValidateResult.
func NormalizeCandidateStatus(input Input, result *Result) {
	if result == nil || (result.Status != StatusReady && result.Status != StatusReadyWithOperators) ||
		len(result.Issues) == 0 {
		return
	}

	required := make(map[string]bool, len(input.Requirements.Data)+len(input.Requirements.Actions))
	for _, requirement := range input.Requirements.Data {
		required[requirement.RequirementID] = dataRequirementMustBind(requirement)
	}
	for _, requirement := range input.Requirements.Actions {
		required[requirement.RequirementID] = actionRequirementMustBind(requirement)
	}
	if authorized, delta := authorizedRequirements(input); delta {
		for id := range required {
			if _, ok := authorized[id]; !ok {
				delete(required, id)
			}
		}
	}
	bound := make(map[string]struct{}, len(result.Bindings))
	for _, binding := range result.Bindings {
		bound[binding.RequirementID] = struct{}{}
	}
	for _, issue := range result.Issues {
		if !required[issue.RequirementID] {
			continue
		}
		if _, exists := bound[issue.RequirementID]; exists {
			continue
		}
		result.Status = StatusBlocked
		return
	}
}

func ValidateResult(input Input, result Result) error {
	if err := validateInput(input); err != nil {
		return err
	}
	if result.SchemaVersion != ResultSchemaV1 || !validStatus(result.Status) ||
		len(result.Bindings) > maxResultItems || len(result.Issues) > maxResultItems {
		return fmt.Errorf("%w: schema_version/status or item budget", ErrInvalidResult)
	}
	requirementSlots := make(map[string][]string, len(input.Requirements.Data)+len(input.Requirements.Actions))
	requirementKind := make(map[string]string, len(requirementSlots))
	requirementRequired := make(map[string]bool, len(requirementSlots))
	for _, requirement := range input.Requirements.Data {
		requirementSlots[requirement.RequirementID] = requirement.TargetSlotIDs
		requirementKind[requirement.RequirementID] = "data"
		requirementRequired[requirement.RequirementID] = dataRequirementMustBind(requirement)
	}
	for _, requirement := range input.Requirements.Actions {
		requirementSlots[requirement.RequirementID] = requirement.TargetSlotIDs
		requirementKind[requirement.RequirementID] = "action"
		requirementRequired[requirement.RequirementID] = actionRequirementMustBind(requirement)
	}
	authorized, delta := authorizedRequirements(input)
	seen := make(map[string]struct{}, len(result.Bindings))
	allowedSources := make(map[string]struct{}, len(input.Sources))
	for _, source := range input.Sources {
		allowedSources[source.SourceID+"\x00"+source.KnowledgeID] = struct{}{}
	}
	operatorUsed := false
	for _, binding := range result.Bindings {
		slots, exists := requirementSlots[binding.RequirementID]
		if !exists {
			return fmt.Errorf(
				"%w: binding %s references unknown requirement",
				ErrInvalidResult, binding.RequirementID,
			)
		}
		if strings.TrimSpace(binding.SourceID) == "" {
			return fmt.Errorf(
				"%w: binding %s has empty source_id",
				ErrInvalidResult, binding.RequirementID,
			)
		}
		if strings.TrimSpace(binding.KnowledgeID) == "" {
			return fmt.Errorf(
				"%w: binding %s has empty knowledge_id",
				ErrInvalidResult, binding.RequirementID,
			)
		}
		if !sameStrings(slots, binding.TargetSlotIDs) {
			return fmt.Errorf(
				"%w: binding %s target_slot_ids mismatch: got %q, want %q",
				ErrInvalidResult, binding.RequirementID, binding.TargetSlotIDs, slots,
			)
		}
		if _, allowed := allowedSources[binding.SourceID+"\x00"+binding.KnowledgeID]; !allowed {
			return fmt.Errorf(
				"%w: binding %s cites unauthorized source %q knowledge %q",
				ErrInvalidResult, binding.RequirementID, binding.SourceID, binding.KnowledgeID,
			)
		}
		if delta {
			if _, allowed := authorized[binding.RequirementID]; !allowed {
				return ErrEditScopeViolation
			}
		}
		if _, duplicate := seen[binding.RequirementID]; duplicate {
			return fmt.Errorf("%w: requirement %s is bound twice", ErrInvalidResult, binding.RequirementID)
		}
		seen[binding.RequirementID] = struct{}{}
		switch requirementKind[binding.RequirementID] {
		case "data":
			if strings.TrimSpace(binding.FieldPath) == "" || strings.TrimSpace(binding.ActionPath) != "" {
				return fmt.Errorf("%w: data binding %s needs field_path only", ErrInvalidResult, binding.RequirementID)
			}
		case "action":
			if strings.TrimSpace(binding.ActionPath) == "" || strings.TrimSpace(binding.FieldPath) != "" {
				return fmt.Errorf("%w: action binding %s needs action_path only", ErrInvalidResult, binding.RequirementID)
			}
		}
		for _, transform := range binding.Transforms {
			if transform.OperatorVersionID == 0 {
				return fmt.Errorf("%w: binding %s has invalid operator_version_id", ErrInvalidResult, binding.RequirementID)
			}
			if _, err := json.Marshal(transform.Params); err != nil {
				return fmt.Errorf("%w: binding %s has invalid transform params", ErrInvalidResult, binding.RequirementID)
			}
		}
		operatorUsed = operatorUsed || len(binding.Transforms) > 0
	}
	expected := requirementSlots
	if delta {
		expected = make(map[string][]string, len(authorized))
		for id := range authorized {
			expected[id] = requirementSlots[id]
		}
	}
	issueSeen := make(map[string]struct{}, len(result.Issues))
	for _, issue := range result.Issues {
		_, expectedRequirement := expected[issue.RequirementID]
		_, alreadyBound := seen[issue.RequirementID]
		if !expectedRequirement {
			return fmt.Errorf(
				"%w: issue %s references unauthorized requirement",
				ErrInvalidResult, issue.RequirementID,
			)
		}
		if alreadyBound {
			return fmt.Errorf(
				"%w: issue %s duplicates bound requirement",
				ErrInvalidResult, issue.RequirementID,
			)
		}
		if !validIssueCode(issue.Code) {
			return fmt.Errorf(
				"%w: issue %s has invalid code: must match [A-Z0-9_]{1,64}",
				ErrInvalidResult, issue.RequirementID,
			)
		}
		if !validIssueMessage(issue.Message) {
			return fmt.Errorf(
				"%w: issue %s has invalid message: must be valid UTF-8, 1..512 bytes, trimmed, and contain no control characters",
				ErrInvalidResult, issue.RequirementID,
			)
		}
		if _, duplicate := issueSeen[issue.RequirementID]; duplicate {
			return fmt.Errorf(
				"%w: requirement %s has duplicate issues",
				ErrInvalidResult, issue.RequirementID,
			)
		}
		issueSeen[issue.RequirementID] = struct{}{}
	}
	if (result.Status == StatusBlocked || result.Status == StatusUncertain) &&
		len(result.Issues) == 0 {
		return fmt.Errorf(
			"%w: status %s requires at least one unresolved issue",
			ErrInvalidResult, result.Status,
		)
	}
	unresolved := false
	for id := range expected {
		if _, bound := seen[id]; bound {
			continue
		}
		unresolved = true
		if result.Status == StatusReady || result.Status == StatusReadyWithOperators {
			if requirementRequired[id] {
				return fmt.Errorf("%w: requirement %s is not bound", ErrInvalidResult, id)
			}
			if _, explained := issueSeen[id]; !explained {
				return fmt.Errorf("%w: missing issue for %s", ErrInvalidResult, id)
			}
			continue
		}
		if _, explained := issueSeen[id]; !explained {
			return fmt.Errorf("%w: missing issue for %s", ErrInvalidResult, id)
		}
	}
	if (result.Status == StatusBlocked || result.Status == StatusUncertain) && !unresolved {
		return fmt.Errorf(
			"%w: status %s requires an unresolved requirement",
			ErrInvalidResult, result.Status,
		)
	}
	if result.Status == StatusReadyWithOperators && !operatorUsed {
		return fmt.Errorf("%w: operator status without operator", ErrInvalidResult)
	}
	if result.Status == StatusReady && operatorUsed {
		return fmt.Errorf("%w: operator binding requires operator status", ErrInvalidResult)
	}
	return nil
}

func validIssueCode(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, current := range value {
		if (current < 'A' || current > 'Z') &&
			(current < '0' || current > '9') && current != '_' {
			return false
		}
	}
	return true
}

func validIssueMessage(value string) bool {
	if !utf8.ValidString(value) || len(value) == 0 || len(value) > 512 ||
		strings.TrimSpace(value) != value {
		return false
	}
	for _, current := range value {
		if unicode.IsControl(current) {
			return false
		}
	}
	return true
}

func validateInput(input Input) error {
	if input.SchemaVersion != InputSchemaV1 {
		return fmt.Errorf("%w: schema_version must be %q", ErrInvalidInput, InputSchemaV1)
	}
	if input.Contract.SchemaVersion != contract.SchemaVersion || input.Contract.ContractID == "" ||
		input.Contract.Revision < 1 || input.Contract.Status != "confirmed" {
		return fmt.Errorf("%w: content contract identity/status", ErrInvalidInput)
	}
	if input.Design.Ref == "" || !validHash(input.Design.ContentHash) {
		return fmt.Errorf("%w: design ref/content_hash", ErrInvalidInput)
	}
	if input.Requirements.SchemaVersion != requirements.SchemaVersion ||
		input.Requirements.InputHash == "" {
		return fmt.Errorf("%w: requirements identity", ErrInvalidInput)
	}
	if !json.Valid(input.Design.FieldHints) || !json.Valid(input.Design.ActionSlots) {
		return fmt.Errorf("%w: design sidecars", ErrInvalidInput)
	}
	hash, err := contract.Hash(input.Contract.Draft)
	if err != nil || hash != input.Contract.ContentHash {
		return fmt.Errorf("%w: content contract hash", ErrInvalidInput)
	}
	contents := make(map[string]struct{}, len(input.Contract.Contents))
	actions := make(map[string]struct{}, len(input.Contract.Actions))
	for _, item := range input.Contract.Contents {
		contents[item.ID] = struct{}{}
	}
	for _, action := range input.Contract.Actions {
		actions[action.ID] = struct{}{}
	}
	ids := make(map[string]struct{}, len(input.Requirements.Data)+len(input.Requirements.Actions))
	for _, requirement := range input.Requirements.Data {
		if requirement.RequirementID == "" || len(requirement.TargetSlotIDs) == 0 {
			return fmt.Errorf("%w: data requirement id/target slots", ErrInvalidInput)
		}
		if _, ok := contents[requirement.ContractItemID]; !ok {
			return fmt.Errorf("%w: data requirement %q references unknown content %q", ErrInvalidInput, requirement.RequirementID, requirement.ContractItemID)
		}
		if _, duplicate := ids[requirement.RequirementID]; duplicate {
			return fmt.Errorf("%w: duplicate requirement %q", ErrInvalidInput, requirement.RequirementID)
		}
		ids[requirement.RequirementID] = struct{}{}
	}
	for _, requirement := range input.Requirements.Actions {
		if requirement.RequirementID == "" || len(requirement.TargetSlotIDs) == 0 {
			return fmt.Errorf("%w: action requirement id/target slots", ErrInvalidInput)
		}
		if _, ok := actions[requirement.ContractActionID]; !ok {
			return fmt.Errorf("%w: action requirement %q references unknown action %q", ErrInvalidInput, requirement.RequirementID, requirement.ContractActionID)
		}
		if _, duplicate := ids[requirement.RequirementID]; duplicate {
			return fmt.Errorf("%w: duplicate requirement %q", ErrInvalidInput, requirement.RequirementID)
		}
		ids[requirement.RequirementID] = struct{}{}
	}
	primary := 0
	seenSources := make(map[string]struct{}, len(input.Sources))
	for _, source := range input.Sources {
		if source.SourceID == "" || source.KnowledgeID == "" {
			return fmt.Errorf("%w: source identity", ErrInvalidInput)
		}
		key := source.SourceID + "\x00" + source.KnowledgeID
		if _, duplicate := seenSources[key]; duplicate {
			return fmt.Errorf("%w: duplicate source %q", ErrInvalidInput, source.SourceID)
		}
		seenSources[key] = struct{}{}
		seenPaths := make(map[string]struct{}, len(source.Paths))
		for _, path := range source.Paths {
			if strings.TrimSpace(path) != path || path == "" {
				return fmt.Errorf("%w: source %q has invalid path", ErrInvalidInput, source.SourceID)
			}
			if _, duplicate := seenPaths[path]; duplicate {
				return fmt.Errorf("%w: source %q has duplicate path %q", ErrInvalidInput, source.SourceID, path)
			}
			seenPaths[path] = struct{}{}
		}
		if source.ListPath != "" {
			if strings.TrimSpace(source.ListPath) != source.ListPath {
				return fmt.Errorf("%w: source %q has invalid list_path", ErrInvalidInput, source.SourceID)
			}
			if _, exists := seenPaths[source.ListPath]; !exists {
				return fmt.Errorf("%w: source %q list_path is not an authorized path", ErrInvalidInput, source.SourceID)
			}
		}
		if source.Primary {
			primary++
		}
	}
	if len(input.Sources) > 0 && primary != 1 {
		return fmt.Errorf("%w: sources require exactly one primary", ErrInvalidInput)
	}
	if input.EditContract != nil {
		if input.EditContract.SchemaVersion != edit.SchemaVersion {
			return fmt.Errorf("%w: edit contract schema_version", ErrInvalidInput)
		}
		if input.EditContract.ChangeScope != "binding_update" {
			return fmt.Errorf("%w: edit contract change_scope %q is not binding_update", ErrInvalidInput, input.EditContract.ChangeScope)
		}
		if input.EditContract.Operation != "update" {
			return fmt.Errorf("%w: edit contract operation %q is not update", ErrInvalidInput, input.EditContract.Operation)
		}
		if input.EditContract.Preconditions.ContentContractHash != input.Contract.ContentHash {
			return fmt.Errorf("%w: edit contract content contract hash", ErrInvalidInput)
		}
		if input.EditContract.Preconditions.DesignHash != input.Design.ContentHash {
			return fmt.Errorf("%w: edit contract design hash", ErrInvalidInput)
		}
		allowed, delta := authorizedRequirements(input)
		if !delta || len(allowed) == 0 {
			return fmt.Errorf("%w: edit contract has no binding requirement targets", ErrInvalidInput)
		}
		for id := range allowed {
			if _, exists := ids[id]; !exists {
				return fmt.Errorf("%w: edit contract references unknown requirement %q", ErrInvalidInput, id)
			}
		}
	}
	return nil
}

func authorizedRequirements(input Input) (map[string]struct{}, bool) {
	result := make(map[string]struct{})
	if input.EditContract == nil || input.EditContract.ChangeScope != "binding_update" {
		return result, false
	}
	for _, target := range input.EditContract.TargetSet {
		if target.Kind == "data_requirement" || target.Kind == "action_requirement" {
			result[target.ID] = struct{}{}
		}
	}
	return result, true
}

func validStatus(value string) bool {
	return IsExecutableStatus(value) || value == StatusBlocked || value == StatusUncertain
}

func dataRequirementMustBind(requirement requirements.DataRequirement) bool {
	return strings.EqualFold(strings.TrimSpace(requirement.Level), "core") ||
		strings.EqualFold(strings.TrimSpace(requirement.WhenMissing), "block")
}

func actionRequirementMustBind(requirement requirements.ActionRequirement) bool {
	return strings.EqualFold(strings.TrimSpace(requirement.Level), "core") ||
		strings.EqualFold(strings.TrimSpace(requirement.WhenFailed), "block_publish")
}

func validHash(value string) bool {
	return strings.HasPrefix(value, "sha256:") && len(value) > len("sha256:")
}

func sameStrings(left, right []string) bool {
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(left, right)
}
