package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/edit"
	stepartifact "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/artifact"
	bindingcontract "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/binding/contract"
	"github.com/AGenUI/agenui-studio/harness/sdk"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

type binderContractAdmission struct {
	Result     bindingcontract.Result
	ResultJSON string
	Completion Completion
}

// CommitBinding is the single Host callback used by agenui_workspace. It
// validates protected baselines, materializes deterministically, and persists
// the immutable Binding and Final artifacts before the tool reports success.
func (i *TaskInterceptor) CommitBinding(
	ctx context.Context,
	call extension.Context,
	input bindingcontract.Input,
	candidate string,
) (resultJSON, plan, final string, err error) {
	if i == nil || i.store == nil {
		return "", "", "", errors.New("binding commit storage is unavailable")
	}
	rootRunID := strings.TrimSpace(call.RootRunID)
	if rootRunID == "" {
		rootRunID = strings.TrimSpace(call.ParentRunID)
	}
	if rootRunID == "" {
		rootRunID = call.RunID
	}
	identity := harness.Identity{
		TenantID: call.TenantID, UserID: call.UserID,
		SessionID: call.SessionID, RunID: rootRunID,
	}
	admitted, err := i.consumeWorkspaceBinding(ctx, identity, input, candidate)
	if err != nil {
		return "", "", "", err
	}
	plan, err = bindingcontract.EncodeSubmission(admitted.Result, admitted.Completion.Plan)
	if err != nil {
		return "", "", "", err
	}
	if _, err = i.store.Save(ctx, identity, stepartifact.StepBinding, plan); err != nil {
		return "", "", "", fmt.Errorf("persist committed binding: %w", err)
	}
	if !bindingcontract.IsExecutableStatus(admitted.Result.Status) {
		return admitted.ResultJSON, plan, "", nil
	}
	if _, err = i.store.Save(ctx, identity, stepartifact.StepFinal, admitted.Completion.Result); err != nil {
		return "", "", "", fmt.Errorf("persist finalized delivery: %w", err)
	}
	return admitted.ResultJSON, plan, admitted.Completion.Result, nil
}

// consumeWorkspaceBinding consumes the candidate already admitted by
// agenui_workspace. It verifies immutable edit baselines unavailable inside
// the model-facing tool, then
// materializes without re-running Workspace admission or repairing the model.
func (i *TaskInterceptor) consumeWorkspaceBinding(
	ctx context.Context, identity harness.Identity, input bindingcontract.Input, candidate string,
) (admission binderContractAdmission, err error) {
	result, candidatePlan, err := parseBinderContractResult(candidate)
	if err != nil {
		return admission, err
	}
	search, err := loadBinderContractWorkingSearch(ctx, i.store, identity)
	if err != nil {
		if !errors.Is(err, stepartifact.ErrNotFound) || len(result.Bindings) > 0 {
			return admission, fmt.Errorf("load authoritative Binder sources: %w", err)
		}
		search = ""
	}
	plan := candidatePlan
	if input.EditContract != nil {
		baseRunID, baseRevision, baseline, loadErr := i.bindingUpdateBaseline(
			ctx, identity, input.EditContract.BaseGenerationID,
		)
		if loadErr != nil {
			return admission, loadErr
		}
		if input.EditContract.BaseGenerationID != baseRunID || input.EditContract.BaseCardRevision != baseRevision ||
			input.EditContract.Preconditions.BindingPlanHash == "" ||
			edit.HashText(baseline) != input.EditContract.Preconditions.BindingPlanHash {
			return admission, errors.New("binding_update baseline hash mismatch")
		}
		plan, err = bindingcontract.MergeExecutablePlanDelta(input, result, baseline, candidatePlan)
		if err != nil {
			return admission, err
		}
	}
	template, err := i.store.Load(ctx, identity, stepartifact.StepTemplate)
	if err != nil {
		return admission, fmt.Errorf("load authoritative Binder template: %w", err)
	}
	if edit.HashText(template) != input.Design.ContentHash {
		return admission, errors.New("authoritative Binder template differs from frozen Design")
	}
	completion, err := materializedCompletion(template, search, plan)
	if err != nil {
		return admission, err
	}
	if input.EditContract != nil {
		if err := bindingcontract.ValidateProtectedExecutablePlan(input, plan, completion.Plan); err != nil {
			return admission, fmt.Errorf("binding_update changed protected mappings: %w", err)
		}
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return admission, err
	}
	return binderContractAdmission{Result: result, ResultJSON: string(resultJSON), Completion: completion}, nil
}
