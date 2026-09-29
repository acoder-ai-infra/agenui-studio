package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/edit"
	stepartifact "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/artifact"
	bindingcontract "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/binding/contract"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/workspace"
	harness "github.com/AGenUI/agenui-studio/harness/sdk"
)

func (i *TaskInterceptor) reuseStyleBinding(
	ctx context.Context,
	runID string,
	tenantID string,
	userID string,
	sessionID string,
) (string, bool, error) {
	if i == nil || i.store == nil {
		return "", false, nil
	}
	identity := harness.Identity{
		TenantID: tenantID, UserID: userID,
		SessionID: sessionID, RunID: runID,
	}
	binding, err := i.store.Load(ctx, identity, stepartifact.StepBinding)
	if err != nil {
		if errors.Is(err, stepartifact.ErrNotFound) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("load durable binding artifact: %w", err)
	}
	if !hasBindingDecisions(binding) {
		return "", false, nil
	}
	if _, err = i.store.Load(ctx, identity, stepartifact.StepFinal); err != nil {
		if errors.Is(err, stepartifact.ErrNotFound) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("load materialized style-only artifact: %w", err)
	}
	if bindingUsesTurnLocalSources(binding) {
		// $.merged is materialized per Run. Its source
		// artifacts are intentionally not inherited by a continuation, so an
		// otherwise style-only turn must re-run Binder (and any needed operator)
		// instead of exposing a stale plan.
		return "", false, nil
	}
	return binding, true, nil
}

// materializeStyleOnlyEdit gives a design-only continuation its own immutable
// Run artifacts. The previous Binding/Search facts are reused only when the
// frozen edit contract targets design, the binding contract is unchanged, and
// the plan has no turn-local merged source.
func (i *TaskInterceptor) materializeStyleOnlyEdit(
	ctx context.Context,
	identity harness.Identity,
	previousDesign string,
	currentDesign string,
	editContractJSON string,
) (bool, error) {
	if i == nil || i.store == nil ||
		!editContractPreservesBinding(editContractJSON) ||
		templateBindingContract(previousDesign) == "" ||
		templateBindingContract(previousDesign) != templateBindingContract(currentDesign) {
		return false, nil
	}
	var authorization struct {
		BaseGenerationID string `json:"base_generation_id"`
	}
	if json.Unmarshal([]byte(editContractJSON), &authorization) != nil ||
		strings.TrimSpace(authorization.BaseGenerationID) == "" ||
		authorization.BaseGenerationID == identity.RunID {
		return false, nil
	}
	base := identity
	base.RunID = authorization.BaseGenerationID
	binding, err := i.store.Load(ctx, base, stepartifact.StepBinding)
	if err != nil {
		if errors.Is(err, stepartifact.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("load style-only base binding: %w", err)
	}
	if !hasBindingDecisions(binding) || bindingUsesTurnLocalSources(binding) {
		return false, nil
	}
	search := ""
	if loaded, loadErr := i.store.Load(ctx, base, stepartifact.StepSearch); loadErr == nil {
		search = loaded
	} else if !errors.Is(loadErr, stepartifact.ErrNotFound) {
		return false, fmt.Errorf("load style-only base sources: %w", loadErr)
	}
	if _, err = i.store.Save(ctx, identity, stepartifact.StepBinding, binding); err != nil {
		return false, fmt.Errorf("persist reused style-only binding: %w", err)
	}
	if search != "" {
		if _, err = i.store.Save(ctx, identity, stepartifact.StepSearch, search); err != nil {
			return false, fmt.Errorf("persist reused style-only sources: %w", err)
		}
	}
	completion, err := materializedCompletion(currentDesign, search, binding)
	if err != nil {
		return false, fmt.Errorf("materialize style-only edit: %w", err)
	}
	if _, err = i.store.Save(ctx, identity, stepartifact.StepFinal, completion.Result); err != nil {
		return false, fmt.Errorf("persist style-only final artifact: %w", err)
	}
	return true, nil
}

func bindingUsesTurnLocalSources(binding string) bool {
	submission, err := bindingcontract.ParseSubmission(binding)
	if err != nil {
		return false
	}
	mappings := append(
		append([]map[string]any(nil), submission.Plan.FieldMappings...),
		submission.Plan.ActionMappings...,
	)
	for _, mapping := range mappings {
		for _, key := range []string{
			"sourceKey", "urlPath", "eventPath", "trackKey", "trackPath",
		} {
			path := strings.TrimSpace(binderString(mapping[key]))
			if path == "$.merged" || strings.HasPrefix(path, "$.merged.") ||
				strings.HasPrefix(path, "$.merged[") {
				return true
			}
		}
	}
	return false
}

func editContractPreservesBinding(raw string) bool {
	var contract struct {
		SchemaVersion string `json:"schema_version"`
		ChangeScope   string `json:"change_scope"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &contract) != nil {
		return false
	}
	return contract.SchemaVersion == edit.SchemaVersion && contract.ChangeScope == "design_update"
}

func templateBindingContract(template string) string {
	agenui, _ := workspace.ArtifactMessagesJSON(template)
	fieldSlots, actionSlotJSON, _ := workspace.ArtifactSlotsJSON(template)
	refKeys := binderRefKeys(binderDataModel(agenui), make(map[string]string))
	usages := binderRefUsages(agenui, refKeys)
	usageSignatures := make([]string, 0, len(usages))
	for _, usage := range usages {
		usageSignatures = append(usageSignatures, strings.Join([]string{
			usage.RefKey,
			usage.ComponentType,
			usage.ComponentID,
			usage.UsagePath,
		}, "\x1f"))
	}
	sort.Strings(usageSignatures)

	fieldRefs := contractSignatures(
		fieldSlots,
		[]string{
			"refKey",
			"componentId",
			"role",
			"description",
			"assetType",
			"required",
		},
	)
	actionSlots := contractSignatures(
		actionSlotJSON,
		[]string{
			"componentId",
			"componentType",
			"role",
			"scopeRefKey",
			"labelRefKey",
			"description",
		},
	)
	if len(refKeys) == 0 && len(usageSignatures) == 0 &&
		len(fieldRefs) == 0 && len(actionSlots) == 0 {
		return ""
	}
	return "--data-model--\n" + strings.Join(refKeys, "\n") +
		"\n--usages--\n" + strings.Join(usageSignatures, "\n") +
		"\n--field-slots--\n" + strings.Join(fieldRefs, "\n") +
		"\n--actions--\n" + strings.Join(actionSlots, "\n")
}

func contractSignatures(
	raw string,
	keys []string,
) []string {
	var items []map[string]any
	if raw == "" || json.Unmarshal([]byte(raw), &items) != nil {
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		parts := make([]string, len(keys))
		for index, key := range keys {
			parts[index] = orchestrationContractValue(item[key])
		}
		signature := strings.Join(parts, "\x1f")
		if strings.Trim(signature, "\x1f") != "" {
			result = append(result, signature)
		}
	}
	sort.Strings(result)
	return result
}

func hasBindingDecisions(binding string) bool {
	submission, err := bindingcontract.ParseSubmission(binding)
	return err == nil && (len(submission.Result.Bindings) > 0 ||
		len(submission.Plan.FieldMappings) > 0 || len(submission.Plan.ActionMappings) > 0)
}

func orchestrationContractValue(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	if value == nil {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func searchRequest(arguments json.RawMessage) (string, int) {
	var request struct {
		Query string `json:"query"`
		TopK  int    `json:"top_k"`
	}
	if json.Unmarshal(arguments, &request) != nil {
		return "", 1
	}
	if request.TopK == 0 {
		request.TopK = 1
	}
	return strings.TrimSpace(request.Query), request.TopK
}

func appendTaskDescription(arguments json.RawMessage, suffix string) json.RawMessage {
	var decoded map[string]json.RawMessage
	if json.Unmarshal(arguments, &decoded) != nil {
		return arguments
	}
	var description string
	if raw := decoded["description"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &description)
	}
	encodedDescription, err := json.Marshal(strings.TrimSpace(description) + suffix)
	if err != nil {
		return arguments
	}
	decoded["description"] = encodedDescription
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return arguments
	}
	return encoded
}

func (i *TaskInterceptor) enrichBindingArguments(
	ctx context.Context,
	runID string,
	tenantID string,
	userID string,
	sessionID string,
	traceID string,
	subagent string,
	baseRunID string,
	route string,
	userIntent string,
	arguments json.RawMessage,
) (json.RawMessage, *bindingcontract.Input, error) {
	identity := harness.Identity{
		TenantID: tenantID, UserID: userID,
		SessionID: sessionID, RunID: runID,
	}
	apiResult, templateResult := "", ""
	isBinder := subagent == BinderAgentID
	if i.store != nil {
		if apiResult == "" {
			loaded, loadErr := i.store.Load(ctx, identity, stepartifact.StepSearch)
			if loadErr == nil {
				apiResult = loaded
			} else if !isBinder || !errors.Is(loadErr, stepartifact.ErrNotFound) {
				return arguments, nil, fmt.Errorf(
					"load durable search artifact for binding: %w",
					loadErr,
				)
			}
		}
		if templateResult == "" && !isBinder {
			loaded, loadErr := i.store.Load(
				ctx,
				identity,
				stepartifact.StepTemplate,
			)
			if loadErr != nil {
				return arguments, nil, fmt.Errorf(
					"load durable template artifact for binding: %w",
					loadErr,
				)
			}
			templateResult = loaded
		}
	}
	if !isBinder && (templateResult == "" || apiResult == "") {
		if i.store == nil {
			// Legacy/in-memory tests and standalone Binder calls may not have a
			// Composition Root artifact store. Preserve their original task input;
			// the durable production path above remains fail closed.
			return arguments, nil, nil
		}
		return arguments, nil, errors.New("binding prerequisites are incomplete")
	}
	var decoded map[string]json.RawMessage
	if json.Unmarshal(arguments, &decoded) != nil {
		return arguments, nil, nil
	}
	var description string
	if raw := decoded["description"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &description)
	}
	var frozen *bindingcontract.Input
	// Binder always consumes the frozen contract projection. Route is optional:
	// it only distinguishes a binding_update authorization from an initial
	// binding. No private route marker is required: invalid model mappings must
	// remain visible to the owning tool instead of being silently removed.
	if isBinder {
		if i.store == nil {
			return arguments, nil, errors.New("Binder requires durable artifacts")
		}
		identity := harness.Identity{
			TenantID: tenantID, UserID: userID,
			SessionID: sessionID, RunID: runID,
		}
		contractJSON, loadErr := i.store.Load(ctx, identity, stepartifact.StepContract)
		if loadErr != nil {
			return arguments, nil, fmt.Errorf("load Binder contract: %w", loadErr)
		}
		design, loadErr := i.store.Load(ctx, identity, stepartifact.StepDesign)
		if loadErr != nil {
			return arguments, nil, fmt.Errorf("load Binder design: %w", loadErr)
		}
		requirementJSON, loadErr := i.store.Load(ctx, identity, stepartifact.StepRequirements)
		if loadErr != nil {
			return arguments, nil, fmt.Errorf("load Binder requirements: %w", loadErr)
		}
		editJSON := ""
		if loaded, editErr := i.store.Load(ctx, identity, stepartifact.StepEditContract); editErr == nil {
			editJSON = loaded
		} else if !errors.Is(editErr, stepartifact.ErrNotFound) {
			return arguments, nil, fmt.Errorf("load Binder edit contract: %w", editErr)
		}
		// Edit contracts are capability-scoped authorizations. A design_update
		// contract inherited from the previous design turn must never be passed
		// to Binder: Binder only accepts binding_update authorizations. Initial
		// binding and source reselection do not need an edit authorization at all.
		if route != taskRouteBindingUpdate {
			editJSON = ""
		} else {
			baseRunID, baseRevision, baselineBinding, bindingErr := i.bindingUpdateBaseline(
				ctx, identity, baseRunID,
			)
			if bindingErr != nil {
				return arguments, nil, bindingErr
			}
			targetIDs, selected := i.workspace.BindingEditTargets(tenantID, userID, sessionID, runID)
			if selected {
				generated, buildErr := buildBindingUpdateEditContract(
					runID,
					baseRunID,
					baseRevision,
					userIntent,
					contractJSON,
					design,
					requirementJSON,
					baselineBinding,
					targetIDs,
					tenantID,
					userID,
					sessionID,
				)
				if buildErr != nil {
					return arguments, nil, buildErr
				}
				encoded, marshalErr := json.Marshal(generated)
				if marshalErr != nil {
					return arguments, nil, marshalErr
				}
				canonicalEditJSON := string(encoded)
				if !isUsableBindingUpdateEditContract(editJSON, generated) {
					if _, saveErr := i.store.Save(ctx, identity, stepartifact.StepEditContract, canonicalEditJSON); saveErr != nil {
						return arguments, nil, fmt.Errorf("persist Binder edit contract: %w", saveErr)
					}
				}
				editJSON = canonicalEditJSON
			} else {
				// Exact Requirement selection is model-owned. The first child
				// invocation receives the edit request through Workspace and freezes
				// its selection before candidate admission.
				editJSON = ""
			}
		}
		binderPrompt, binderInput, buildErr := buildBinderContractPrompt(
			description, contractJSON, design, requirementJSON, editJSON, apiResult,
		)
		if buildErr != nil {
			return arguments, nil, buildErr
		}
		description = binderPrompt
		frozen = &binderInput
	}
	encodedDescription, err := json.Marshal(description)
	if err != nil {
		return arguments, nil, err
	}
	decoded["description"] = encodedDescription
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return arguments, nil, err
	}
	return encoded, frozen, nil
}

// materializeBindingEditBase freezes the explicitly prepared completed
// generation into the current root Run. A continuation must not guess a base
// by looking at whichever Session artifact happens to be newest after the
// route has been prepared, and Binder must not mutate artifacts owned by an
// earlier Run.
func (i *TaskInterceptor) materializeBindingEditBase(
	ctx context.Context,
	current harness.Identity,
	baseRunID string,
) (string, error) {
	if i == nil || i.store == nil {
		return "", errors.New("binding_update requires durable artifacts")
	}
	baseRunID = strings.TrimSpace(baseRunID)
	if baseRunID == "" || strings.TrimSpace(current.RunID) == "" {
		return "", errors.New("binding_update requires explicit base and current runs")
	}
	base := current
	base.RunID = baseRunID
	var design string
	for _, step := range []string{
		stepartifact.StepContract,
		stepartifact.StepPreflight,
		stepartifact.StepTemplate,
		stepartifact.StepDesign,
		stepartifact.StepRequirements,
	} {
		content, err := i.store.Load(ctx, base, step)
		if err != nil {
			return "", fmt.Errorf("load binding_update base %s: %w", step, err)
		}
		if step == stepartifact.StepDesign {
			design = content
		}
		if current.RunID == base.RunID {
			continue
		}
		if _, err = i.store.Save(ctx, current, step, content); err != nil {
			return "", fmt.Errorf("persist binding_update %s: %w", step, err)
		}
	}
	return design, nil
}

func (i *TaskInterceptor) bindingUpdateBaseline(
	ctx context.Context,
	identity harness.Identity,
	baseRunID string,
) (string, int64, string, error) {
	if i == nil || i.store == nil {
		return "", 0, "", errors.New(
			"binding_update requires durable artifacts",
		)
	}
	baseRunID = strings.TrimSpace(baseRunID)
	if baseRunID == "" {
		return "", 0, "", errors.New(
			"binding_update requires a frozen base run",
		)
	}
	if baseRunID == identity.RunID {
		return "", 0, "", errors.New(
			"binding_update base run must precede the current run",
		)
	}
	baseIdentity := identity
	baseIdentity.RunID = baseRunID
	baseline, err := i.store.Load(ctx, baseIdentity, stepartifact.StepBinding)
	if err != nil {
		return "", 0, "", fmt.Errorf(
			"load binding_update baseline: %w", err,
		)
	}
	if strings.TrimSpace(baseline) == "" {
		return "", 0, "", errors.New(
			"binding_update baseline is empty",
		)
	}
	return baseRunID, 1, baseline, nil
}
