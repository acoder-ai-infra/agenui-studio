package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/edit"
	agenuiextensions "github.com/AGenUI/agenui-studio/agenui-agent/internal/extensions"
	stepartifact "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/artifact"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/workspace"
	"github.com/AGenUI/agenui-studio/harness/sdk"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// SubmitEditContract atomically freezes every model-selected target in one
// immutable per-Run contract. IDs and paths are checked against the current
// Design and Catalog; semantic selection remains the model's responsibility.
type SubmitEditContract struct {
	sessions *designingSessions
	provider *DesigningProvider
}

// editContractProposalError marks a rejection that the model can repair by
// inspecting the current candidates and submitting a corrected proposal. It
// deliberately excludes persistence and artifact failures: those are Host
// failures and must remain hard tool errors instead of inviting blind retries.
type editContractProposalError struct{ cause error }

func (e *editContractProposalError) Error() string { return e.cause.Error() }
func (e *editContractProposalError) Unwrap() error { return e.cause }

func rejectEditContractProposal(err error) error {
	return &editContractProposalError{cause: err}
}

func (*SubmitEditContract) Name() string { return SubmitEditContractName }

func (t *SubmitEditContract) Invoke(ctx context.Context, call extension.FunctionCall) (*extension.FunctionResult, error) {
	result, err := t.invokeStrict(ctx, call)
	if err == nil {
		return result, err
	}
	var rejection *editContractProposalError
	if !errors.As(err, &rejection) {
		return result, err
	}
	encoded, encodeErr := json.Marshal(map[string]any{
		"accepted": false, "retryable": true,
		"error":       map[string]any{"code": "EDIT_CONTRACT_REJECTED", "message": rejection.Error()},
		"next_action": "inspect candidates and submit one corrected atomic targets array",
	})
	if encodeErr != nil {
		return nil, err
	}
	return &extension.FunctionResult{Data: encoded, MimeType: "application/json"}, nil
}

func (t *SubmitEditContract) invokeStrict(ctx context.Context, call extension.FunctionCall) (*extension.FunctionResult, error) {
	if call.Ctx.AgentID != agenuiextensions.MainAgent || call.Name != SubmitEditContractName {
		return nil, errors.New("submit edit contract: caller is not allowed")
	}
	var proposed struct {
		ChangeScope string `json:"change_scope"`
		Operation   string `json:"operation"`
		Targets     []struct {
			TargetID         string         `json:"target_id"`
			RequestedChanges map[string]any `json:"requested_changes"`
			Component        map[string]any `json:"component"`
			ParentID         string         `json:"parent_id"`
			AfterID          string         `json:"after_id"`
		} `json:"targets"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(call.Arguments)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&proposed); err != nil {
		return nil, rejectEditContractProposal(fmt.Errorf("submit edit contract: invalid proposal: %w", err))
	}
	if t.provider == nil || t.provider.artifacts == nil {
		return nil, errors.New("submit edit contract: durable persistence is unavailable")
	}
	key := sessionKey(call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID)
	t.sessions.mu.RLock()
	baseDesign := t.sessions.designs[key]
	contentContract, contractOK := t.sessions.contracts[key]
	t.sessions.mu.RUnlock()
	if baseDesign == "" || !contractOK {
		return nil, errors.New("submit edit contract: base design and content contract are required")
	}
	if len(proposed.Targets) == 0 {
		return nil, rejectEditContractProposal(errors.New("submit edit contract: targets are required"))
	}
	messages, fields, actions, err := designIndexParts(baseDesign)
	if err != nil {
		return nil, err
	}
	stylePaths := t.provider.editableStylePaths()
	index, err := edit.BuildIndex("design_"+strings.TrimPrefix(edit.HashText(baseDesign), "sha256:")[:16], messages, fields, actions, stylePaths...)
	if err != nil {
		return nil, err
	}
	baseDocument, err := workspace.ParseDesignArtifact(baseDesign)
	if err != nil {
		return nil, err
	}
	proposals := make([]edit.TargetProposal, 0, len(proposed.Targets))
	createdIDs, updatedIDs, unchangedIDs := make([]string, 0), make([]string, 0), make([]string, 0)
	for _, submitted := range proposed.Targets {
		component, componentErr := normalizeEditComponent(submitted.Component)
		if componentErr != nil {
			return nil, rejectEditContractProposal(fmt.Errorf("submit edit contract: target %q: %w", submitted.TargetID, componentErr))
		}
		lookupID := strings.TrimSpace(submitted.TargetID)
		if lookupID == "" && component != nil {
			lookupID, _ = component["id"].(string)
		}
		selected, selectErr := edit.Select(index, lookupID)
		if selectErr != nil && !errors.Is(selectErr, edit.ErrTargetNotFound) {
			return nil, rejectEditContractProposal(fmt.Errorf("submit edit contract: invalid target_id %q: %w", lookupID, selectErr))
		}
		if errors.Is(selectErr, edit.ErrTargetNotFound) {
			if component == nil {
				return nil, rejectEditContractProposal(fmt.Errorf(
					"submit edit contract: target %q does not exist; provide component plus parent_id to declare an insertion",
					lookupID,
				))
			}
			componentID, _ := component["id"].(string)
			if lookupID != "" && lookupID != componentID {
				return nil, rejectEditContractProposal(fmt.Errorf(
					"submit edit contract: target_id %q does not match declared component id %q", lookupID, componentID,
				))
			}
			if strings.TrimSpace(submitted.ParentID) == "" {
				return nil, rejectEditContractProposal(fmt.Errorf(
					"submit edit contract: new component %q requires parent_id", componentID,
				))
			}
			proposals = append(proposals, edit.TargetProposal{Insert: &edit.InsertProposal{
				Component: component, ParentID: submitted.ParentID, AfterID: submitted.AfterID,
			}})
			createdIDs = append(createdIDs, componentID)
			continue
		}
		if component != nil {
			componentID, _ := component["id"].(string)
			componentType, _ := component["component"].(string)
			if componentID != selected.ComponentID || componentType != selected.ComponentType {
				return nil, rejectEditContractProposal(fmt.Errorf(
					"submit edit contract: declaration identity %q/%q does not match existing target %q/%q",
					componentID, componentType, selected.ComponentID, selected.ComponentType,
				))
			}
		}
		declaredChanges := componentDeclaredChanges(component)
		changes, mergeErr := mergeRequestedChanges(submitted.RequestedChanges, declaredChanges)
		if mergeErr != nil {
			return nil, rejectEditContractProposal(fmt.Errorf("submit edit contract: target %q: %w", lookupID, mergeErr))
		}
		changes = edit.EffectiveRequestedChanges(selected, changes)
		if len(changes) == 0 {
			unchangedIDs = append(unchangedIDs, selected.ComponentID)
			continue
		}
		updatedIDs = append(updatedIDs, selected.ComponentID)
		proposals = append(proposals, edit.TargetProposal{Resolved: edit.ResolvedTarget{
			DesignRevision: index.Revision, Query: lookupID, Target: selected,
		}, RequestedChanges: changes})
	}
	if len(proposals) == 0 {
		return nil, rejectEditContractProposal(errors.New("submit edit contract: every proposed value already matches the current design"))
	}
	baseIdentity := harness.Identity{
		TenantID: call.Ctx.TenantID, UserID: call.Ctx.UserID,
		SessionID: call.Ctx.SessionID,
	}
	baseRunID, err := t.provider.artifacts.LatestRunID(
		ctx, baseIdentity, stepartifact.StepDesign,
	)
	if err != nil {
		return nil, fmt.Errorf("submit edit contract: load base run: %w", err)
	}
	baseIdentity.RunID = baseRunID
	baseBinding, bindingErr := t.provider.artifacts.Load(ctx, baseIdentity, stepartifact.StepBinding)
	if bindingErr != nil && !errors.Is(bindingErr, stepartifact.ErrNotFound) {
		return nil, fmt.Errorf("submit edit contract: load base binding: %w", bindingErr)
	}
	slotHash, err := edit.SlotSignatureHash(baseDesign)
	if err != nil {
		return nil, err
	}
	contractValue, err := edit.BuildContract(edit.BuildInput{
		TenantID: call.Ctx.TenantID, UserID: call.Ctx.UserID,
		SessionID: call.Ctx.SessionID, RunID: call.Ctx.RunID,
		BaseGenerationID: baseRunID, BaseCardRevision: contentContract.Revision,
		ContentContractID:   contentContract.ContractID,
		ContentContractHash: contentContract.ContentHash,
		BaseDesignHash:      edit.HashText(baseDesign), SlotSignatureHash: slotHash,
		BindingPlanHash: edit.HashText(baseBinding),
		ChangeScope:     proposed.ChangeScope, Operation: proposed.Operation,
		Proposals: proposals,
	})
	if err != nil {
		return nil, rejectEditContractProposal(err)
	}
	baseRevision, err := baseDocument.Revision()
	if err != nil {
		return nil, err
	}
	patch, err := edit.AuthorizedPatch(contractValue, baseRevision)
	if err != nil {
		return nil, rejectEditContractProposal(err)
	}
	if _, err := workspace.Apply(baseDocument, patch); err != nil {
		return nil, rejectEditContractProposal(fmt.Errorf("submit edit contract: declared batch is not applicable: %w", err))
	}
	encoded, err := json.Marshal(contractValue)
	if err != nil {
		return nil, err
	}
	identity := harness.Identity{
		TenantID: call.Ctx.TenantID, UserID: call.Ctx.UserID,
		SessionID: call.Ctx.SessionID, RunID: call.Ctx.RunID,
	}
	pointer, err := t.provider.artifacts.Save(ctx, identity, stepartifact.StepEditContract, string(encoded))
	if err != nil {
		return nil, fmt.Errorf("submit edit contract: persist: %w", err)
	}
	t.sessions.mu.Lock()
	t.sessions.edits[key] = contractValue
	delete(t.sessions.pendingEdits, key)
	t.sessions.mu.Unlock()
	response, _ := json.Marshal(map[string]any{
		"accepted": true,
		"edit_id":  contractValue.EditID, "schema_version": contractValue.SchemaVersion,
		"edit_contract_ref": pointer.Ref, "base_generation_id": contractValue.BaseGenerationID,
		"target_set": contractValue.TargetSet, "protected_set": contractValue.ProtectedSet,
		"impact_set": contractValue.ImpactSet, "preconditions": contractValue.Preconditions,
		"created_component_ids":   sortedStrings(createdIDs),
		"updated_component_ids":   sortedStrings(updatedIDs),
		"unchanged_component_ids": sortedStrings(unchangedIDs),
	})
	return &extension.FunctionResult{Data: response, MimeType: "application/json"}, nil
}

func normalizeEditComponent(input map[string]any) (map[string]any, error) {
	if input == nil {
		return nil, nil
	}
	component := cloneJSONMap(input)
	id, _ := component["id"].(string)
	kind, _ := component["component"].(string)
	if strings.TrimSpace(id) == "" || strings.TrimSpace(kind) == "" {
		return nil, errors.New("component declaration requires non-empty string id and component")
	}
	return component, nil
}

// componentDeclaredChanges turns a partial desired component declaration into
// exact property leaves. Identity and topology are structural facts, never
// editable property guesses. Catalog-derived editable_paths remain the final
// authority in edit.BuildContract.
func componentDeclaredChanges(component map[string]any) map[string]any {
	result := make(map[string]any)
	if component == nil {
		return result
	}
	keys := make([]string, 0, len(component))
	for key := range component {
		if key != "id" && key != "component" && key != "child" && key != "children" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		flattenDeclaredChange(key, component[key], result)
	}
	return result
}

func flattenDeclaredChange(path string, value any, output map[string]any) {
	object, ok := value.(map[string]any)
	if !ok || len(object) == 0 {
		output[path] = value
		return
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		flattenDeclaredChange(path+"."+key, object[key], output)
	}
}

func mergeRequestedChanges(explicit, declared map[string]any) (map[string]any, error) {
	result := make(map[string]any, len(explicit)+len(declared))
	for path, value := range declared {
		result[path] = value
	}
	for path, value := range explicit {
		if current, exists := result[path]; exists && !reflect.DeepEqual(current, value) {
			return nil, fmt.Errorf("requested_changes conflicts with component declaration at %q", path)
		}
		result[path] = value
	}
	return result, nil
}

func cloneJSONMap(input map[string]any) map[string]any {
	raw, _ := json.Marshal(input)
	var output map[string]any
	_ = json.Unmarshal(raw, &output)
	return output
}

func sortedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			seen[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
