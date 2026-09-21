package edit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/workspace"
)

const SchemaVersion = "edit_contract.v2"

var (
	ErrEditScopeViolation   = errors.New("EDIT_SCOPE_VIOLATION")
	ErrBaseRevisionConflict = errors.New("BASE_REVISION_CONFLICT")
	ErrEditNoChange         = errors.New("EDIT_NO_CHANGE")
)

// Contract is the Host-frozen, per-Run authorization for modifying an existing
// card. The Main Agent proposes only scope, operation and requested changes;
// every identity, target, protection, impact and precondition is Host-derived.
type Contract struct {
	EditID           string        `json:"edit_id"`
	SchemaVersion    string        `json:"schema_version"`
	BaseGenerationID string        `json:"base_generation_id"`
	BaseCardRevision int64         `json:"base_card_revision"`
	ChangeScope      string        `json:"change_scope"`
	Operation        string        `json:"operation"`
	Intent           string        `json:"intent,omitempty"`
	TargetSet        []Target      `json:"target_set"`
	ProtectedSet     []Protection  `json:"protected_set"`
	ImpactSet        []Impact      `json:"impact_set"`
	Preconditions    Preconditions `json:"preconditions"`
	Acceptance       Acceptance    `json:"acceptance"`
	IdempotencyKey   string        `json:"idempotency_key"`
}

type Target struct {
	Kind             string         `json:"kind"`
	ID               string         `json:"id"`
	ComponentID      string         `json:"component_id"`
	ComponentType    string         `json:"component_type"`
	ContractItemID   string         `json:"contract_item_id,omitempty"`
	ContractActionID string         `json:"contract_action_id,omitempty"`
	Path             string         `json:"path"`
	Value            any            `json:"value,omitempty"`
	Component        map[string]any `json:"component,omitempty"`
	ParentID         string         `json:"parent_id,omitempty"`
	AfterID          string         `json:"after_id,omitempty"`
}

type Protection struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type Impact struct {
	Kind   string `json:"kind"`
	Action string `json:"action"`
}

type Preconditions struct {
	ContentContractHash string `json:"content_contract_hash"`
	DesignHash          string `json:"design_hash"`
	SlotSignatureHash   string `json:"slot_signature_hash"`
	BindingPlanHash     string `json:"binding_plan_hash,omitempty"`
}

type Acceptance struct {
	TargetChanged             bool `json:"target_changed"`
	ProtectedObjectsUnchanged bool `json:"protected_objects_unchanged"`
	RuntimePreviewRequired    bool `json:"runtime_preview_required"`
}

type ResolvedTarget struct {
	DesignRevision string  `json:"design_revision"`
	Query          string  `json:"query"`
	Target         Element `json:"target"`
}

type TargetProposal struct {
	Resolved         ResolvedTarget
	RequestedChanges map[string]any
	Insert           *InsertProposal
}

// InsertProposal contains only structural facts selected by the model. The
// Host validates identity, parent topology and reachability against the frozen
// base Design; it never infers a parent from language or component type.
type InsertProposal struct {
	Component map[string]any
	ParentID  string
	AfterID   string
}

type BuildInput struct {
	TenantID            string
	UserID              string
	SessionID           string
	RunID               string
	BaseGenerationID    string
	BaseCardRevision    int64
	ContentContractID   string
	ContentContractHash string
	BaseDesignHash      string
	SlotSignatureHash   string
	BindingPlanHash     string
	ChangeScope         string
	Operation           string
	Proposals           []TargetProposal
}

func BuildContract(input BuildInput) (Contract, error) {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.UserID) == "" ||
		strings.TrimSpace(input.SessionID) == "" || strings.TrimSpace(input.RunID) == "" {
		return Contract{}, errors.New("edit contract: invocation identity is required")
	}
	if strings.TrimSpace(input.BaseGenerationID) == "" || input.BaseCardRevision < 1 ||
		strings.TrimSpace(input.ContentContractID) == "" ||
		!validHash(input.ContentContractHash) || !validHash(input.BaseDesignHash) ||
		!validHash(input.SlotSignatureHash) {
		return Contract{}, errors.New("edit contract: base revision and protected hashes are required")
	}
	if input.ChangeScope != "design_update" || (input.Operation != "update" && input.Operation != "upsert") {
		return Contract{}, errors.New("edit contract: only design_update with update or upsert is supported")
	}
	if len(input.Proposals) == 0 {
		return Contract{}, errors.New("edit contract: targets are required")
	}
	targets := make([]Target, 0)
	seen := make(map[string]struct{})
	for _, proposal := range input.Proposals {
		if proposal.Insert != nil {
			component := cloneMap(proposal.Insert.Component)
			componentID, _ := component["id"].(string)
			componentType, _ := component["component"].(string)
			parentID := strings.TrimSpace(proposal.Insert.ParentID)
			if strings.TrimSpace(componentID) == "" || strings.TrimSpace(componentType) == "" || parentID == "" {
				return Contract{}, errors.New("edit contract: inserted component requires id, component and parent_id")
			}
			identity := "insert\x00" + componentID
			if _, duplicate := seen[identity]; duplicate {
				return Contract{}, fmt.Errorf("edit contract: duplicate inserted component %q", componentID)
			}
			seen[identity] = struct{}{}
			targets = append(targets, Target{
				Kind: "design_component_insert", ID: componentID,
				ComponentID: componentID, ComponentType: componentType,
				Component: component, ParentID: parentID,
				AfterID: strings.TrimSpace(proposal.Insert.AfterID),
			})
			continue
		}
		resolved := proposal.Resolved
		if strings.TrimSpace(resolved.DesignRevision) == "" || strings.TrimSpace(resolved.Query) == "" ||
			strings.TrimSpace(resolved.Target.ElementID) == "" || strings.TrimSpace(resolved.Target.ComponentID) == "" {
			return Contract{}, errors.New("edit contract: every target must be resolved")
		}
		if len(proposal.RequestedChanges) == 0 {
			return Contract{}, fmt.Errorf("edit contract: requested_changes is required for component %q", resolved.Target.ComponentID)
		}
		paths := make([]string, 0, len(proposal.RequestedChanges))
		for path := range proposal.RequestedChanges {
			paths = append(paths, strings.TrimSpace(path))
		}
		sort.Strings(paths)
		for _, path := range paths {
			if !validEditablePath(path) || !stringInSlice(resolved.Target.EditablePaths, path) {
				return Contract{}, fmt.Errorf("edit contract: path %q is not an editable property of component %q", path, resolved.Target.ComponentID)
			}
			value := proposal.RequestedChanges[path]
			if text, ok := value.(string); ok && strings.TrimSpace(text) == "" {
				return Contract{}, fmt.Errorf("edit contract: value for path %q is empty", path)
			}
			identity := resolved.Target.ComponentID + "\x00" + path
			if _, duplicate := seen[identity]; duplicate {
				return Contract{}, fmt.Errorf("edit contract: duplicate target %s.%s", resolved.Target.ComponentID, path)
			}
			seen[identity] = struct{}{}
			targets = append(targets, Target{Kind: "design_slot", ID: resolved.Target.ElementID,
				ComponentID: resolved.Target.ComponentID, ComponentType: resolved.Target.ComponentType,
				ContractItemID: resolved.Target.ContractItemID, ContractActionID: resolved.Target.ContractActionID,
				Path: path, Value: value})
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		return targets[i].Kind+"\x00"+targets[i].ComponentID+"\x00"+targets[i].Path <
			targets[j].Kind+"\x00"+targets[j].ComponentID+"\x00"+targets[j].Path
	})
	seedTargets := make([]string, 0, len(targets))
	for _, target := range targets {
		value, err := json.Marshal(struct {
			Value     any            `json:"value,omitempty"`
			Component map[string]any `json:"component,omitempty"`
			ParentID  string         `json:"parent_id,omitempty"`
			AfterID   string         `json:"after_id,omitempty"`
		}{target.Value, target.Component, target.ParentID, target.AfterID})
		if err != nil {
			return Contract{}, fmt.Errorf("edit contract: encode value for %s.%s: %w", target.ComponentID, target.Path, err)
		}
		seedTargets = append(seedTargets, target.Kind+"\x00"+target.ComponentID+"\x00"+target.Path+"\x00"+string(value))
	}
	seed := strings.Join([]string{
		input.TenantID, input.UserID, input.SessionID, input.RunID,
		input.BaseGenerationID, input.BaseDesignHash, strings.Join(seedTargets, "\x01"),
	}, "\x00")
	sum := sha256.Sum256([]byte(seed))
	editID := "edit_" + hex.EncodeToString(sum[:8])
	return Contract{
		EditID: editID, SchemaVersion: SchemaVersion,
		BaseGenerationID: input.BaseGenerationID, BaseCardRevision: input.BaseCardRevision,
		ChangeScope: input.ChangeScope, Operation: input.Operation,
		TargetSet: targets,
		ProtectedSet: []Protection{
			{Kind: "content_contract", ID: input.ContentContractID},
			{Kind: "all_other_design_slots", ID: "*"},
		},
		ImpactSet: append(insertImpacts(targets), []Impact{
			{Kind: "design", Action: "regenerate_partial"},
			{Kind: "runtime_preview", Action: "regenerate"},
		}...),
		Preconditions: Preconditions{
			ContentContractHash: input.ContentContractHash, DesignHash: input.BaseDesignHash,
			SlotSignatureHash: input.SlotSignatureHash, BindingPlanHash: input.BindingPlanHash,
		},
		Acceptance: Acceptance{
			TargetChanged: true, ProtectedObjectsUnchanged: true, RuntimePreviewRequired: true,
		},
		IdempotencyKey: input.SessionID + ":" + input.BaseGenerationID + ":" + editID,
	}, nil
}

func insertImpacts(targets []Target) []Impact {
	seen := make(map[string]struct{})
	var result []Impact
	for _, target := range targets {
		if target.Kind != "design_component_insert" || target.ParentID == "" {
			continue
		}
		key := "component:" + target.ParentID
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, Impact{Kind: key, Action: "update_topology"})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Kind < result[j].Kind })
	return result
}

// EffectiveRequestedChanges removes only proposals that are already true in
// the resolved target's current component facts. This is value comparison, not
// language interpretation: the model still decides what the user meant, while
// the Host avoids turning an explicit "keep this value" constraint into an
// impossible must-change target.
func EffectiveRequestedChanges(target Element, proposed map[string]any) map[string]any {
	result := make(map[string]any, len(proposed))
	for path, requested := range proposed {
		segments := strings.Split(strings.TrimSpace(path), ".")
		var current any = target.Properties
		if segments[0] == "styles" {
			current = target.CurrentStyles
			segments = segments[1:]
		}
		found := true
		for _, segment := range segments {
			values, ok := current.(map[string]any)
			if !ok {
				found = false
				break
			}
			current, found = values[segment]
			if !found {
				break
			}
		}
		if found && semanticallyEqualStyleValue(current, requested) {
			continue
		}
		result[path] = requested
	}
	return result
}

func validEditablePath(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	for _, segment := range strings.Split(path, ".") {
		segment = strings.TrimSpace(segment)
		if segment == "" || segment == "__proto__" || segment == "prototype" || segment == "constructor" {
			return false
		}
	}
	return true
}

func HashText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func SlotSignatureHash(design string) (string, error) {
	fields, actions, err := workspace.ArtifactSlotsJSON(design)
	if err != nil {
		return "", errors.New("edit contract: typed design slots are missing")
	}
	return HashText(fields + "\x00" + actions), nil
}

func validHash(value string) bool {
	return strings.HasPrefix(value, "sha256:") && len(value) > len("sha256:")
}

func cloneMap(input map[string]any) map[string]any {
	result := make(map[string]any, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func stringInSlice(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
