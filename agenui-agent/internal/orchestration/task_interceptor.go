package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/contract"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/edit"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/diagnostics"
	agenuiextensions "github.com/AGenUI/agenui-studio/agenui-agent/internal/extensions"
	stepartifact "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/artifact"
	bindingcontract "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/binding/contract"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/integration/knowrag"
	harness "github.com/AGenUI/agenui-studio/harness/sdk"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

const (
	taskRouteBindingUpdate           = "binding_update"
	missingContentContractCorrection = `{"code":"CONTENT_CONTRACT_REQUIRED","continue_generation":true,"executed":false,"instruction":"Design was not executed. Resubmit agenui_submit_content_contract with schema-valid top-level goal, type, and contents; wait for confirmed before calling agenui_style.","next_action":"submit_content_contract","status":"prerequisite_missing"}`
	missingEditContractCorrection    = `{"code":"EDIT_CONTRACT_REQUIRED","continue_generation":true,"executed":false,"instruction":"Design was not executed. Candidate discovery started an edit flow, but no edit contract was accepted. Correct the target using the returned candidates, submit agenui_submit_edit_contract, and wait for accepted=true before calling agenui_style.","next_action":"submit_edit_contract","status":"prerequisite_missing"}`
)

// BindingOperatorScopeActivator verifies the parent relationship supplied by
// Harness at the Binder tool boundary.
type BindingOperatorScopeActivator interface {
	BindBindingOperatorChild(context.Context, extension.Context) error
}

// BindingParentScopeResolver is the read side of the same active child-to-
// parent link. Supplemental API recall runs inside the Binder child Run, while
// BindingSources belongs to the parent generation. Implementations must resolve
// the exact child Run; selecting a latest Run from the Session is forbidden.
type BindingParentScopeResolver interface {
	ResolveBindingParentScope(context.Context, extension.Context) (extension.Context, error)
}

type TaskInterceptor struct {
	id                      string
	selectionReceiptEnabled bool
	store                   ArtifactContentStore
	bindingOperatorScope    BindingOperatorScopeActivator
	designing               DesigningSnapshotProvider
	workspace               WorkspaceSnapshotProvider
}

type ArtifactContentStore interface {
	Save(
		context.Context,
		harness.Identity,
		string,
		string,
	) (stepartifact.Pointer, error)
	Load(context.Context, harness.Identity, string) (string, error)
	LatestRunID(context.Context, harness.Identity, string) (string, error)
}

type DesigningSnapshotProvider interface {
	DesigningSnapshot(tenantID, userID, sessionID string) (contractJSON, preflightJSON string, ok bool)
	RestoreDesigningSnapshot(context.Context, string, string, string) error
	ResolvedEditSnapshot(tenantID, userID, sessionID string) string
	EditContractSnapshot(tenantID, userID, sessionID string) string
	DesignEditPending(tenantID, userID, sessionID string) bool
	RecordDesignSnapshot(tenantID, userID, sessionID, design string) error
	CurrentDesignSnapshot(tenantID, userID, sessionID string) string
	TakeBindingEditRequest(tenantID, userID, sessionID string) (query, baseRunID string, ok bool)
}

type WorkspaceSnapshotProvider interface {
	RestoreDesign(tenantID, userID, sessionID, runID, design string) error
	CommittedDesignSnapshot(tenantID, userID, sessionID, runID string) (design, requirementsJSON, requirementsHash string, ok bool)
	PrepareBindingInput(context.Context, harness.Identity, bindingcontract.Input) error
	RefreshBindingSources(context.Context, harness.Identity, []bindingcontract.SourceSnapshot) error
	RequireBindingEdit(context.Context, harness.Identity, string) error
	BindingEditTargets(tenantID, userID, sessionID, runID string) ([]string, bool)
	CommittedBindingSnapshot(tenantID, userID, sessionID, runID string) (resultJSON, plan, final string, ok bool)
	Reset(tenantID, userID, sessionID, runID string)
}

func NewTaskInterceptor() *TaskInterceptor {
	return &TaskInterceptor{id: "agenui.task_interceptor.v1"}
}

// NewSearchReceiptInterceptor exposes the short model-visible Search receipt
// under a separate Extension ID. The implementation is registered as an
// available Agent-scoped provider so isolated evaluation catalogs can bind it;
// serving Agents remain bound only to agenui.task_interceptor.v1.
func NewSearchReceiptInterceptor() *TaskInterceptor {
	return &TaskInterceptor{
		id:                      "agenui.search_receipt.v1",
		selectionReceiptEnabled: true,
	}
}

func NewTaskInterceptorWithStore(store ArtifactContentStore) *TaskInterceptor {
	interceptor := NewTaskInterceptor()
	interceptor.store = store
	return interceptor
}

func (i *TaskInterceptor) SetStore(store ArtifactContentStore) {
	if i != nil {
		i.store = store
	}
}

func (i *TaskInterceptor) SetWorkspaceSnapshotProvider(provider WorkspaceSnapshotProvider) {
	if i != nil {
		i.workspace = provider
	}
}

func (i *TaskInterceptor) SetDesigningSnapshotProvider(provider DesigningSnapshotProvider) {
	if i != nil {
		i.designing = provider
	}
}

func taskRootRunID(ctx extension.Context) string {
	if root := strings.TrimSpace(ctx.RootRunID); root != "" {
		return root
	}
	return strings.TrimSpace(ctx.RunID)
}

func (i *TaskInterceptor) SetBindingOperatorScopeActivator(
	activator BindingOperatorScopeActivator,
) {
	if i != nil {
		i.bindingOperatorScope = activator
	}
}

func (i *TaskInterceptor) ID() string {
	if i == nil || strings.TrimSpace(i.id) == "" {
		return "agenui.task_interceptor.v1"
	}
	return i.id
}

// persistBinderSearchArtifact freezes the Binder's search snapshot as the Run's
// search Artifact. The materializer loads it by pointer from generation.Refs, so
// skipping this write makes the binding continuation unresolvable: the Binder's
// real field mappings are already durable, but resolveDesigning sees search as
// ErrNotFound and either drops the whole plan for the design-only fallback or
// fails outright with "incomplete binding continuation".
func (i *TaskInterceptor) persistBinderSearchArtifact(
	ctx context.Context,
	call extension.ToolCallInfo,
	full string,
	compacted string,
) {
	if i == nil || i.store == nil {
		return
	}
	rootRunID := strings.TrimSpace(call.Ctx.RootRunID)
	if rootRunID == "" {
		rootRunID = call.Ctx.RunID
	}
	if strings.TrimSpace(rootRunID) == "" {
		return
	}
	if _, saveErr := i.store.Save(ctx, harness.Identity{
		TenantID:  call.Ctx.TenantID,
		UserID:    call.Ctx.UserID,
		SessionID: call.Ctx.SessionID,
		RunID:     rootRunID,
	}, stepartifact.StepSearch, firstNonEmptyString(full, compacted)); saveErr != nil {
		log.Printf(
			"[agenui-route] stage=binder_search_artifact status=failed tenant=%s user=%s session=%s run=%s child_run=%s agent=%s cause=%q",
			call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID,
			rootRunID, call.Ctx.RunID, call.Ctx.AgentID,
			diagnostics.ErrorChain(saveErr),
		)
	}
}

func (i *TaskInterceptor) Intercept(
	ctx context.Context,
	call extension.ToolCallInfo,
	next extension.ToolCallNext,
) (extension.ToolCallOutcome, error) {
	rootRunID := taskRootRunID(call.Ctx)
	// Binder's ordinary Workspace/search tools use their own frozen child Run
	// identity. Parent resolution is required only at the operator boundary,
	// where the operator provider asks BindingOperatorScope for the exact root
	// artifact Run. Ordinary Binder calls must not depend on an application-level
	// copy of the Harness Run lifecycle.
	if IsBinderPrimarySearchCall(
		call,
		BinderAgentID,
		BinderAgentVersion,
	) {
		return i.interceptBinderSearch(ctx, call, next)
	}
	if searchTool(call.Name) && i != nil {
		arguments := call.Arguments
		outcome, err := next(ctx, arguments)
		if err != nil {
			return extension.ToolCallOutcome{}, err
		}
		query, topK := searchRequest(arguments)
		process := knowrag.ProcessToolResult
		if i.selectionReceiptEnabled {
			process = knowrag.ProcessToolResultWithSelectionReceipt
		}
		full, compacted, err := process(
			outcome.Result, query, call.Ctx.TenantID, call.Ctx.UserID, topK,
		)
		if err != nil {
			// Style preflight is semantic guidance, not the authoritative Binder
			// lookup. If KnowRAG returns an empty or malformed payload, retain an
			// explicit unknown receipt so the Style Agent can continue designing
			// without fabricating either availability or unavailability. Binder
			// calls are handled by the strict branches above and still fail closed.
			if call.Ctx.AgentID != agenuiextensions.StyleAgent {
				return extension.ToolCallOutcome{}, err
			}
			full, compacted = stylePreflightUnknownReceipt(query)
			log.Printf(
				"[agenui-route] stage=capability_knowledge status=degraded tenant=%s user=%s session=%s run=%s trace=%s agent=%s cause=%q",
				call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID,
				call.Ctx.RunID, call.Ctx.TraceID, call.Ctx.AgentID,
				diagnostics.ErrorChain(err),
			)
		}
		rootRunID := strings.TrimSpace(call.Ctx.RootRunID)
		if rootRunID == "" {
			rootRunID = call.Ctx.RunID
		}
		// The public tool is named search_developer_apis. Keep the persistence
		// decision tied to the Style Agent role instead of one transport alias:
		// Binder searches have already returned through the strict branch above.
		if i.store != nil && call.Ctx.AgentID == agenuiextensions.StyleAgent {
			if _, saveErr := i.store.Save(ctx, harness.Identity{
				TenantID: call.Ctx.TenantID, UserID: call.Ctx.UserID,
				SessionID: call.Ctx.SessionID, RunID: rootRunID,
			}, stepartifact.StepCapabilityEvidence, full); saveErr != nil {
				return extension.ToolCallOutcome{}, fmt.Errorf("persist capability evidence: %w", saveErr)
			}
		}
		if call.Ctx.AgentID == BinderAgentID {
			i.persistBinderSearchArtifact(ctx, call, full, compacted)
		}
		return extension.ToolCallOutcome{Result: compacted}, nil
	}

	step, _, ok := taskStep(call)
	if !ok || i == nil {
		return next(ctx, call.Arguments)
	}

	preparedBindingEdit := ""
	preparedBindingBaseRunID := ""
	preparedBindingBaseDesign := ""
	route := ""
	if step == "bind_apis" && i.designing != nil {
		if query, baseRunID, prepared := i.designing.TakeBindingEditRequest(
			call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID,
		); prepared {
			preparedBindingEdit = query
			preparedBindingBaseRunID = baseRunID
			route = taskRouteBindingUpdate
		}
	}
	if preparedBindingBaseRunID != "" {
		var materializeErr error
		preparedBindingBaseDesign, materializeErr = i.materializeBindingEditBase(
			ctx,
			harness.Identity{
				TenantID: call.Ctx.TenantID, UserID: call.Ctx.UserID,
				SessionID: call.Ctx.SessionID, RunID: rootRunID,
			},
			preparedBindingBaseRunID,
		)
		if materializeErr != nil {
			return extension.ToolCallOutcome{}, materializeErr
		}
	}
	arguments := call.Arguments
	designingContract := ""
	designingTarget := ""
	designingEditContract := ""
	designingBaseDesign := ""
	finalArtifactContent := ""
	var binderContractInput *bindingcontract.Input
	userIntent := ""
	if preparedBindingEdit != "" {
		userIntent = preparedBindingEdit
	}
	if step == "bind_apis" && preparedBindingEdit == "" && i.designing != nil {
		contractJSON, _, exists := i.designing.DesigningSnapshot(
			call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID,
		)
		if !exists {
			if restoreErr := i.designing.RestoreDesigningSnapshot(
				ctx, call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID,
			); restoreErr != nil {
				return extension.ToolCallOutcome{}, fmt.Errorf("binding: restore delivery mode: %w", restoreErr)
			}
			contractJSON, _, exists = i.designing.DesigningSnapshot(
				call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID,
			)
		}
		if !exists {
			return extension.ToolCallOutcome{}, errors.New("binding: frozen content contract is missing")
		}
		deliveryMode, deliveryErr := contentContractDeliveryMode(contractJSON)
		if deliveryErr != nil {
			return extension.ToolCallOutcome{}, deliveryErr
		}
		if deliveryMode == contract.DeliveryModeDesignPreview {
			return extension.ToolCallOutcome{Result: `{"status":"skipped","delivery_mode":"design_preview","executable":false,"reason":"design preview does not authorize data or action binding"}`}, nil
		}
	}
	if step == "search_apis" {
	} else if step == "bind_apis" {
		if i.workspace != nil && i.designing != nil {
			design := preparedBindingBaseDesign
			if design == "" {
				design = i.designing.CurrentDesignSnapshot(
					call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID,
				)
			}
			if design == "" {
				_ = i.designing.RestoreDesigningSnapshot(
					ctx, call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID,
				)
				design = i.designing.CurrentDesignSnapshot(
					call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID,
				)
			}
			if design != "" {
				if restoreErr := i.workspace.RestoreDesign(
					call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID, rootRunID, design,
				); restoreErr != nil {
					return extension.ToolCallOutcome{}, fmt.Errorf("binding: restore AGenUI workspace: %w", restoreErr)
				}
			}
		}
		var enrichmentErr error
		arguments, binderContractInput, enrichmentErr = i.enrichBindingArguments(
			ctx,
			rootRunID,
			call.Ctx.TenantID,
			call.Ctx.UserID,
			call.Ctx.SessionID,
			call.Ctx.TraceID,
			taskSubagent(call),
			preparedBindingBaseRunID,
			route,
			userIntent,
			arguments,
		)
		if enrichmentErr != nil {
			return extension.ToolCallOutcome{}, enrichmentErr
		}
		if i.workspace != nil && binderContractInput != nil {
			identity := harness.Identity{
				TenantID: call.Ctx.TenantID, UserID: call.Ctx.UserID,
				SessionID: call.Ctx.SessionID, RunID: rootRunID,
			}
			if prepareErr := i.workspace.PrepareBindingInput(ctx, identity, *binderContractInput); prepareErr != nil {
				return extension.ToolCallOutcome{}, fmt.Errorf("binding: prepare AGenUI workspace: %w", prepareErr)
			}
			if route == taskRouteBindingUpdate {
				if editErr := i.workspace.RequireBindingEdit(ctx, identity, userIntent); editErr != nil {
					return extension.ToolCallOutcome{}, fmt.Errorf("binding: prepare edit request: %w", editErr)
				}
			}
		}
	} else if step == "design" {
		if i.designing == nil {
			return extension.ToolCallOutcome{}, errors.New("design: snapshot provider is missing")
		}
		var exists bool
		designingContract, _, exists = i.designing.DesigningSnapshot(
			call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID,
		)
		if !exists {
			restoreErr := i.designing.RestoreDesigningSnapshot(
				ctx, call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID,
			)
			if restoreErr != nil {
				if errors.Is(restoreErr, stepartifact.ErrNotFound) &&
					call.Ctx.AgentID == agenuiextensions.MainAgent {
					// Design remains strictly gated by an immutable Contract, but
					// authoritative absence is a model-correctable ordering error,
					// not a runtime/storage failure. Return a static envelope so the
					// Root can submit schema-valid goal/type/contents and try again;
					// do not invoke Style, emit progress, or persist any artifact.
					return extension.ToolCallOutcome{Result: missingContentContractCorrection}, nil
				}
				return extension.ToolCallOutcome{}, fmt.Errorf(
					"design: restore frozen content contract: %w", restoreErr,
				)
			}
			designingContract, _, exists = i.designing.DesigningSnapshot(
				call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID,
			)
		}
		if !exists || strings.TrimSpace(designingContract) == "" {
			return extension.ToolCallOutcome{}, errors.New("design: frozen content contract is missing")
		}
		arguments = enrichDesignArguments(arguments, designingContract)
		currentDesign := i.designing.CurrentDesignSnapshot(call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID)
		designingEditContract = i.designing.EditContractSnapshot(call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID)
		if currentDesign != "" && strings.TrimSpace(designingEditContract) == "" &&
			i.designing.DesignEditPending(call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID) {
			return extension.ToolCallOutcome{Result: missingEditContractCorrection}, nil
		}
		styleEdit := currentDesign != "" && strings.TrimSpace(designingEditContract) != ""
		if i.workspace != nil && !styleEdit {
			i.workspace.Reset(call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID, rootRunID)
		}
		if styleEdit {
			designingBaseDesign = currentDesign
			if i.workspace != nil {
				if restoreErr := i.workspace.RestoreDesign(
					call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID, rootRunID, currentDesign,
				); restoreErr != nil {
					return extension.ToolCallOutcome{}, fmt.Errorf("design: restore AGenUI workspace: %w", restoreErr)
				}
			}
			if strings.TrimSpace(designingEditContract) == "" {
				designingEditContract = i.designing.EditContractSnapshot(call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID)
			}
			if strings.TrimSpace(designingEditContract) == "" {
				return extension.ToolCallOutcome{}, errors.New("design: submit an edit contract before editing an existing card")
			}
			var authorization edit.Contract
			if decodeErr := json.Unmarshal([]byte(designingEditContract), &authorization); decodeErr != nil ||
				authorization.SchemaVersion != edit.SchemaVersion {
				return extension.ToolCallOutcome{}, errors.New("design: persisted edit contract is invalid")
			}
			var frozen contract.Revision
			if decodeErr := json.Unmarshal([]byte(designingContract), &frozen); decodeErr != nil ||
				authorization.Preconditions.ContentContractHash != frozen.ContentHash {
				return extension.ToolCallOutcome{}, fmt.Errorf("%w: content contract changed", edit.ErrBaseRevisionConflict)
			}
			arguments = enrichEditContractArguments(arguments, designingEditContract)
		}
		if designingTarget == "" {
			designingTarget = i.designing.ResolvedEditSnapshot(call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID)
		}
		if target := designingTarget; target != "" {
			designingTarget = target
			arguments = enrichDesignTargetArguments(arguments, target)
		}
		if i.store != nil {
			if _, saveErr := i.store.Save(ctx, harness.Identity{
				TenantID: call.Ctx.TenantID, UserID: call.Ctx.UserID,
				SessionID: call.Ctx.SessionID, RunID: call.Ctx.RunID,
			}, stepartifact.StepContract, designingContract); saveErr != nil {
				return extension.ToolCallOutcome{}, saveErr
			}
		}
	}
	if step == "bind_apis" {
		if reused, ok, reuseErr := i.reuseStyleBinding(
			ctx,
			call.Ctx.RunID,
			call.Ctx.TenantID,
			call.Ctx.UserID,
			call.Ctx.SessionID,
		); reuseErr != nil {
			return extension.ToolCallOutcome{}, reuseErr
		} else if ok {
			return extension.ToolCallOutcome{Result: reused}, nil
		}
	}
	var outcome extension.ToolCallOutcome
	var err error
	outcome, err = next(ctx, arguments)
	if err != nil {
		return extension.ToolCallOutcome{}, err
	}
	var committedRequirementsJSON string
	if step == "design" && i.workspace != nil {
		committed, requirementsJSON, _, exists := i.workspace.CommittedDesignSnapshot(
			call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID, rootRunID,
		)
		if !exists {
			err = errors.New("design: Style Agent did not atomically commit the AGenUI workspace and requirements")
			return extension.ToolCallOutcome{}, err
		}
		outcome.Result = committed
		committedRequirementsJSON = requirementsJSON
	}
	searchFullForRun := ""
	if step == "design" {
		// Workspace commit is the single edit admission boundary. Re-applying
		// the same authorization here used to reject already committed designs
		// and made the parent task a second policy engine.
		_, preflightJSON, snapshotOK := i.designing.DesigningSnapshot(
			call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID,
		)
		if !snapshotOK || strings.TrimSpace(preflightJSON) == "" {
			return extension.ToolCallOutcome{}, errors.New("design: capability preflight was not executed")
		}
		if i.store != nil {
			identity := harness.Identity{TenantID: call.Ctx.TenantID, UserID: call.Ctx.UserID, SessionID: call.Ctx.SessionID, RunID: call.Ctx.RunID}
			if _, saveErr := i.store.Save(ctx, identity, stepartifact.StepPreflight, preflightJSON); saveErr != nil {
				return extension.ToolCallOutcome{}, saveErr
			}
			if _, saveErr := i.store.Save(ctx, identity, stepartifact.StepRequirements, committedRequirementsJSON); saveErr != nil {
				return extension.ToolCallOutcome{}, saveErr
			}
		}
	}
	if step == "search_apis" {
		full := ""
		if i.store != nil {
			loaded, loadErr := i.store.Load(ctx, harness.Identity{
				TenantID: call.Ctx.TenantID, UserID: call.Ctx.UserID,
				SessionID: call.Ctx.SessionID, RunID: call.Ctx.RunID,
			}, stepartifact.StepSearch)
			if loadErr == nil {
				full = loaded
			} else if !errors.Is(loadErr, stepartifact.ErrNotFound) {
				return extension.ToolCallOutcome{}, loadErr
			}
		}
		if full == "" {
			full = normalizedJSONText(outcome.Result)
		}
		compacted := compactSearchResult(outcome.Result)
		if compacted != "" {
			outcome.Result = compacted
		}
		if full != "" {
			searchFullForRun = full
		}
	}
	if step == "bind_apis" {
		if binderContractInput != nil {
			// Rebuild the frozen input after the child has selected sources.
			refreshedArguments, refreshedInput, refreshErr :=
				i.enrichBindingArguments(
					ctx,
					rootRunID,
					call.Ctx.TenantID,
					call.Ctx.UserID,
					call.Ctx.SessionID,
					call.Ctx.TraceID,
					taskSubagent(call),
					preparedBindingBaseRunID,
					route,
					userIntent,
					call.Arguments,
				)
			if refreshErr != nil {
				refreshErr = fmt.Errorf(
					"refresh Binder inputs after child: %w", refreshErr,
				)
				return extension.ToolCallOutcome{}, refreshErr
			}
			if refreshedInput == nil {
				refreshErr = errors.New(
					"refresh Binder inputs after child returned no frozen input",
				)
				return extension.ToolCallOutcome{}, refreshErr
			}
			arguments = refreshedArguments
			binderContractInput = refreshedInput
			if i.workspace == nil {
				return extension.ToolCallOutcome{}, errors.New("Binder requires AGenUI workspace")
			}
			resultJSON, plan, final, committed := i.workspace.CommittedBindingSnapshot(
				call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID, rootRunID,
			)
			if !committed {
				return extension.ToolCallOutcome{}, errors.New("Binder must commit the binding with agenui_workspace")
			}
			var admittedResult bindingcontract.Result
			if decodeErr := json.Unmarshal([]byte(resultJSON), &admittedResult); decodeErr != nil {
				return extension.ToolCallOutcome{}, fmt.Errorf("binding: decode committed result: %w", decodeErr)
			}
			log.Printf(
				"[agenui-binding-diag] stage=bind_result tenant=%s session=%s run=%s %s",
				call.Ctx.TenantID, call.Ctx.SessionID, call.Ctx.RunID,
				binderContractResultDiag(admittedResult),
			)
			outcome.Result = plan
			finalArtifactContent = final
		} else {
			identity := harness.Identity{
				TenantID: call.Ctx.TenantID, UserID: call.Ctx.UserID,
				SessionID: call.Ctx.SessionID, RunID: call.Ctx.RunID,
			}
			templateResult, bindingInputErr := i.store.Load(ctx, identity, stepartifact.StepTemplate)
			if bindingInputErr != nil {
				return extension.ToolCallOutcome{}, bindingInputErr
			}
			apiResult, bindingInputErr := loadBindingSearch(ctx, i.store, identity)
			if bindingInputErr != nil {
				return extension.ToolCallOutcome{}, bindingInputErr
			}
			completion, completionErr := materializedCompletion(templateResult, apiResult, outcome.Result)
			if completionErr != nil {
				return extension.ToolCallOutcome{}, completionErr
			}
			outcome.Result = completion.Plan
			finalArtifactContent = completion.Result
		}
	}

	durableContent := outcome.Result
	durableStep := stepartifact.StepBinding
	switch step {
	case "search_apis":
		durableStep = stepartifact.StepSearch
		durableContent = firstNonEmptyString(
			searchFullForRun,
			outcome.Result,
		)
	case "design":
		durableStep = stepartifact.StepDesign
	case "bind_apis":
		durableStep = stepartifact.StepBinding
	}
	if i.store != nil {
		identity := harness.Identity{
			TenantID:  call.Ctx.TenantID,
			UserID:    call.Ctx.UserID,
			SessionID: call.Ctx.SessionID,
			RunID:     call.Ctx.RunID,
		}
		if _, saveErr := i.store.Save(
			ctx,
			identity,
			durableStep,
			durableContent,
		); saveErr != nil {
			return extension.ToolCallOutcome{}, saveErr
		}
		// The validated Design is also the immutable template input for the
		// optional search/binding continuation run. Keeping the alias durable
		// lets the existing Binder hand-off stay unchanged.
		if step == "design" {
			if _, saveErr := i.store.Save(
				ctx, identity, stepartifact.StepTemplate, durableContent,
			); saveErr != nil {
				return extension.ToolCallOutcome{}, saveErr
			}
			if _, reuseErr := i.materializeStyleOnlyEdit(
				ctx, identity, designingBaseDesign, durableContent, designingEditContract,
			); reuseErr != nil {
				return extension.ToolCallOutcome{}, reuseErr
			}
		}
		// The native Harness Chat entrypoint owns the Run lifecycle and therefore
		// does not pass through the former AGenUI generation wrapper/committer.
		// Once Workspace admission has produced a deterministic materialization,
		// persist it in the same Run as the canonical downloadable/renderable
		// artifact. This is an artifact fact, not a second Run state machine.
		if finalArtifactContent != "" {
			if _, saveErr := i.store.Save(
				ctx, identity, stepartifact.StepFinal, finalArtifactContent,
			); saveErr != nil {
				return extension.ToolCallOutcome{}, saveErr
			}
		}
	}
	if step == "design" {
		if snapshotErr := i.designing.RecordDesignSnapshot(
			call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID, outcome.Result,
		); snapshotErr != nil {
			return extension.ToolCallOutcome{}, snapshotErr
		}
	}
	return outcome, nil
}

func contentContractDeliveryMode(raw string) (string, error) {
	var revision contract.Revision
	if json.Unmarshal([]byte(raw), &revision) != nil || revision.SchemaVersion != contract.SchemaVersion {
		return "", errors.New("binding: persisted content contract is invalid")
	}
	if revision.DeliveryMode == "" {
		return contract.DeliveryModeExecutable, nil
	}
	if revision.DeliveryMode != contract.DeliveryModeExecutable && revision.DeliveryMode != contract.DeliveryModeDesignPreview {
		return "", errors.New("binding: persisted content contract has invalid delivery mode")
	}
	return revision.DeliveryMode, nil
}
