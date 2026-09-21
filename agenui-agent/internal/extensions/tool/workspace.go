package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	designcontract "github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/contract"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/edit"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/requirements"
	agenuiextensions "github.com/AGenUI/agenui-studio/agenui-agent/internal/extensions"
	bindingcontract "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/binding/contract"
	catalogadmission "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/catalogadmission"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/workspace"
	"github.com/AGenUI/agenui-studio/agenui-agent/pkg/agenui/renderercatalog"
	"github.com/AGenUI/agenui-studio/agenui-agent/pkg/agenui/schema"
	"github.com/AGenUI/agenui-studio/harness/sdk"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

const (
	WorkspaceProviderID  = "agenui.tools.workspace.v1"
	WorkspaceToolName    = "agenui_workspace"
	WorkspaceToolVersion = "2.2.0"
)

type workspaceSession struct {
	Document           workspace.Document     `json:"document"`
	Revision           string                 `json:"revision"`
	StateVersion       uint64                 `json:"-"`
	DesignHash         string                 `json:"design_hash,omitempty"`
	BindingPlan        map[string]any         `json:"binding_plan,omitempty"`
	BindingInput       *bindingcontract.Input `json:"binding_input,omitempty"`
	BindingCandidate   string                 `json:"binding_candidate,omitempty"`
	BindingResultJSON  string                 `json:"binding_result_json,omitempty"`
	BindingFinal       string                 `json:"binding_final,omitempty"`
	BindingEditQuery   string                 `json:"binding_edit_query,omitempty"`
	BindingEditTargets []string               `json:"binding_edit_targets,omitempty"`
	Committed          bool                   `json:"committed"`
	EditRequired       bool                   `json:"edit_required,omitempty"`
	EditApplied        bool                   `json:"edit_applied,omitempty"`
	RequirementsJSON   string                 `json:"requirements_json,omitempty"`
	RequirementsHash   string                 `json:"requirements_hash,omitempty"`
}

// WorkspaceProvider exposes one general AST tool. It deliberately has no
// component-, business-, or language-specific branches.
type WorkspaceProvider struct {
	tool        *WorkspaceTool
	mu          sync.RWMutex
	sessions    map[string]workspaceSession
	catalogRoot string
	authorizer  interface {
		EditContractSnapshot(tenantID, userID, sessionID string) string
	}
	bindingCommitter interface {
		CommitBinding(context.Context, extension.Context, bindingcontract.Input, string) (resultJSON, plan, final string, err error)
	}
}

func NewWorkspaceProvider(catalogRoot string) (*WorkspaceProvider, error) {
	if strings.TrimSpace(catalogRoot) == "" {
		catalogRoot = renderercatalog.DefaultRoot
	}
	provider := &WorkspaceProvider{
		sessions: make(map[string]workspaceSession), catalogRoot: catalogRoot,
	}
	provider.tool = &WorkspaceTool{provider: provider}
	return provider, nil
}

func (*WorkspaceProvider) ID() string { return WorkspaceProviderID }

func (p *WorkspaceProvider) FunctionTools() []extension.FunctionTool {
	return []extension.FunctionTool{p.tool}
}

func (p *WorkspaceProvider) SetEditAuthorizer(authorizer interface {
	EditContractSnapshot(tenantID, userID, sessionID string) string
}) error {
	if p == nil || authorizer == nil {
		return errors.New("agenui workspace tool: edit authorizer is required")
	}
	p.authorizer = authorizer
	return nil
}

func (p *WorkspaceProvider) SetBindingCommitter(committer interface {
	CommitBinding(context.Context, extension.Context, bindingcontract.Input, string) (resultJSON, plan, final string, err error)
}) error {
	if p == nil || committer == nil {
		return errors.New("agenui workspace tool: binding committer is required")
	}
	p.bindingCommitter = committer
	return nil
}

func (p *WorkspaceProvider) RestoreDesign(tenantID, userID, sessionID, runID, design string) error {
	if p == nil {
		return errors.New("agenui workspace tool: provider is unavailable")
	}
	document, err := workspace.ParseDesignArtifact(design)
	if err != nil {
		return err
	}
	revision, err := document.Revision()
	if err != nil {
		return err
	}
	p.replace(workspaceSessionKey(tenantID, userID, sessionID, runID), workspaceSession{
		Document: document, Revision: revision, DesignHash: edit.HashText(design),
		StateVersion: 1, Committed: false, EditRequired: true,
	})
	return nil
}

func (p *WorkspaceProvider) Reset(tenantID, userID, sessionID, runID string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	delete(p.sessions, workspaceSessionKey(tenantID, userID, sessionID, runID))
	p.mu.Unlock()
}

// CommittedDesignSnapshot returns the Design and Requirement projection
// frozen by the same successful Workspace commit. Callers must not recompile
// the Design through a second admission path.
func (p *WorkspaceProvider) CommittedDesignSnapshot(
	tenantID, userID, sessionID, runID string,
) (design, requirementsJSON, requirementsHash string, ok bool) {
	if p == nil {
		return "", "", "", false
	}
	p.mu.RLock()
	current, exists := p.sessions[workspaceSessionKey(tenantID, userID, sessionID, runID)]
	p.mu.RUnlock()
	if !exists || !current.Committed || current.RequirementsJSON == "" ||
		current.RequirementsHash == "" || current.Document.ValidateStructural() != nil {
		return "", "", "", false
	}
	if err := p.validateCommittedDocument(current.Document); err != nil {
		return "", "", "", false
	}
	design, ok = workspaceDesignArtifact(current)
	if !ok {
		return "", "", "", false
	}
	return design, current.RequirementsJSON, current.RequirementsHash, true
}

func workspaceDesignArtifact(current workspaceSession) (string, bool) {
	artifact, err := workspace.EncodeDesignArtifact(current.Document)
	return artifact, err == nil
}

// validateCommittedDocument is the one delivery gate for a Design Artifact.
// It validates against the same immutable Renderer Catalog selected by
// admission, including the complete component graph: references must exist,
// the graph must be acyclic, and every component must be reachable from root.
func (p *WorkspaceProvider) validateCommittedDocument(document workspace.Document) error {
	snapshot, err := renderercatalog.LoadCurrent(p.catalogRoot)
	if err != nil {
		return fmt.Errorf("load current renderer catalog: %w", err)
	}
	config, err := snapshot.CatalogConfig()
	if err != nil {
		return fmt.Errorf("load renderer catalog config: %w", err)
	}
	manager, err := schema.NewAGenUISchemaManager(schema.AGenUISchemaManagerConfig{
		Version:  schema.Version09,
		Catalogs: []*schema.CatalogConfig{config},
	})
	if err != nil {
		return fmt.Errorf("build renderer schema manager: %w", err)
	}
	catalog, err := manager.GetSelectedCatalog(map[string]any{
		schema.SupportedCatalogIDsKey: []any{snapshot.CatalogID},
	}, nil, nil)
	if err != nil {
		return fmt.Errorf("select current renderer catalog: %w", err)
	}
	encoded, err := document.MarshalMessages()
	if err != nil {
		return err
	}
	var protocol any
	if err := json.Unmarshal([]byte(encoded), &protocol); err != nil {
		return fmt.Errorf("decode generated protocol: %w", err)
	}
	if err := schema.NewAGenUIValidator(catalog).Validate(protocol, document.RootID, true); err != nil {
		return fmt.Errorf("protocol topology is incomplete: %w", err)
	}
	return nil
}

type WorkspaceTool struct{ provider *WorkspaceProvider }

func (*WorkspaceTool) Name() string { return WorkspaceToolName }

type workspaceCall struct {
	Action         string          `json:"action"`
	BaseRevision   string          `json:"base_revision,omitempty"`
	ComponentIDs   []string        `json:"component_ids,omitempty"`
	BindingResult  json.RawMessage `json:"binding_result,omitempty"`
	SurfaceID      string          `json:"surface_id,omitempty"`
	CatalogID      string          `json:"catalog_id,omitempty"`
	RootID         string          `json:"root_id,omitempty"`
	Components     json.RawMessage `json:"components,omitempty"`
	DataModel      json.RawMessage `json:"data_model,omitempty"`
	FieldSlots     json.RawMessage `json:"field_slots,omitempty"`
	ActionSlots    json.RawMessage `json:"action_slots,omitempty"`
	RuleReceipt    json.RawMessage `json:"design_knowledge_receipt,omitempty"`
	RequirementIDs []string        `json:"requirement_ids,omitempty"`
}

func (t *WorkspaceTool) Invoke(ctx context.Context, call extension.FunctionCall) (*extension.FunctionResult, error) {
	result, err := t.invokeStrict(ctx, call)
	if err == nil || !isModelCorrectableWorkspaceError(err) {
		return result, err
	}
	payload := map[string]any{
		"accepted":  false,
		"retryable": true,
		"error": map[string]any{
			"code":    "WORKSPACE_REJECTED",
			"message": err.Error(),
		},
	}
	for key, value := range t.recoveryState(call) {
		payload[key] = value
	}
	if workspaceCallAction(call) == "commit" {
		feedback := workspaceRepairFeedback(err)
		payload["repair_feedback"] = feedback
		payload["next_actions"] = []string{"put_components", "commit"}
	}
	var conflict *workspace.SlotIdentityConflictError
	if errors.As(err, &conflict) {
		payload["conflict_identity"] = map[string]any{
			"kind": conflict.Kind, "component_id": conflict.ComponentID,
			"contract_id": conflict.ContractID, "property_path": conflict.PropertyPath,
		}
	}
	encoded, encodeErr := json.Marshal(payload)
	if encodeErr != nil {
		return nil, err
	}
	return &extension.FunctionResult{Data: encoded, MimeType: "application/json"}, nil
}

// recoveryState turns a rejected model call into a bounded recovery protocol.
// It contains only document facts and legal next methods; it never interprets
// business intent or silently changes the requested document.
func (t *WorkspaceTool) recoveryState(call extension.FunctionCall) map[string]any {
	if t == nil || t.provider == nil {
		return nil
	}
	key := workspaceContextKey(call.Ctx)
	current, ok := t.provider.snapshot(key)
	if !ok {
		return map[string]any{"next_actions": []string{"begin"}}
	}
	return workspaceStatus(current)
}

func isModelCorrectableWorkspaceError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return strings.HasPrefix(err.Error(), "agenui workspace") ||
		errors.Is(err, workspace.ErrRevisionConflict) || errors.Is(err, workspace.ErrProtectedMutation)
}

// workspaceRepairFeedback turns Catalog-derived protocol failures into a
// bounded correction instruction. These are A2UI structural rules only: they
// never infer business fields, data bindings, or product-specific intent.
func workspaceRepairFeedback(err error) string {
	message := err.Error()
	switch {
	case strings.Contains(message, "not reachable from") || strings.Contains(message, "'children' is a required property"):
		return "Connect every visible component to the declared root through valid child or children references, then resubmit commit. Do not create a second root."
	case strings.Contains(message, ".action:"):
		return "Each Button action must use exactly one valid A2UI Action shape: {event:{name:" + `"..."` + "}} or {functionCall:{call:" + `"..."` + ", args:{...}}}. Do not mix both shapes or add a top-level action name."
	default:
		return "Correct only the reported A2UI protocol fields with put_components, then resubmit commit."
	}
}

func workspaceCallAction(call extension.FunctionCall) string {
	var input workspaceCall
	if err := json.Unmarshal(call.Arguments, &input); err != nil {
		return ""
	}
	return input.Action
}

func (t *WorkspaceTool) invokeStrict(ctx context.Context, call extension.FunctionCall) (*extension.FunctionResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if t == nil || t.provider == nil || call.Name != WorkspaceToolName ||
		(call.Version != "" && call.Version != WorkspaceToolVersion) {
		return nil, errors.New("agenui workspace tool: invalid invocation")
	}
	if call.Ctx.AgentID != agenuiextensions.StyleAgent &&
		call.Ctx.AgentID != agenuiextensions.BinderAgent {
		return nil, errors.New("agenui workspace tool: caller is not allowed")
	}
	var input workspaceCall
	decoder := json.NewDecoder(strings.NewReader(string(call.Arguments)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return nil, fmt.Errorf("agenui workspace tool: decode input: %w", err)
	}
	key := workspaceContextKey(call.Ctx)
	var output any
	switch input.Action {
	case "begin":
		if call.Ctx.AgentID != agenuiextensions.StyleAgent {
			return nil, errors.New("agenui workspace tool: only Style Agent may begin a document")
		}
		dataModel, err := decodeJSONObject(input.DataModel, "data_model")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(input.RootID) != workspace.ProtocolRootID {
			return nil, fmt.Errorf(
				"agenui workspace tool: A2UI v0.9 root_id must be %q",
				workspace.ProtocolRootID,
			)
		}
		document := workspace.Document{
			SchemaVersion: workspace.SchemaVersion,
			SurfaceID:     strings.TrimSpace(input.SurfaceID), CatalogID: strings.TrimSpace(input.CatalogID),
			RootID: workspace.ProtocolRootID, DataModel: dataModel,
		}
		document.NormalizeRuntimeDataModel()
		if err := document.ValidateDraft(); err != nil {
			return nil, err
		}
		revision, err := document.Revision()
		if err != nil {
			return nil, err
		}
		t.provider.mu.Lock()
		if existing, exists := t.provider.sessions[key]; exists && existing.EditRequired {
			t.provider.mu.Unlock()
			return nil, errors.New("agenui workspace tool: begin cannot replace an authorized edit workspace")
		}
		if existing, exists := t.provider.sessions[key]; exists {
			t.provider.mu.Unlock()
			if existing.Revision == revision {
				output = workspaceStatus(existing)
				break
			}
			return nil, workspace.ErrRevisionConflict
		}
		t.provider.sessions[key] = workspaceSession{Document: document, Revision: revision, StateVersion: 1}
		t.provider.mu.Unlock()
		output = workspaceStatus(workspaceSession{Document: document, Revision: revision})
	case "put_components":
		if call.Ctx.AgentID != agenuiextensions.StyleAgent {
			return nil, errors.New("agenui workspace tool: only Style Agent may add components")
		}
		current, ok := t.provider.snapshot(key)
		if !ok || current.Revision != input.BaseRevision {
			return nil, workspace.ErrRevisionConflict
		}
		if current.EditRequired {
			return nil, errors.New("agenui workspace tool: authorized edit workspaces only accept apply_edit_contract")
		}
		components, err := decodeObjectArray(input.Components, "components")
		if err != nil || len(components) == 0 {
			if err != nil {
				return nil, err
			}
			return nil, errors.New("agenui workspace tool: components are required")
		}
		if len(components) > 32 {
			return nil, errors.New("agenui workspace tool: at most 32 components may be added per call")
		}
		if err := upsertWorkspaceComponents(&current.Document, components); err != nil {
			return nil, err
		}
		current.Committed = false
		current.Revision, err = current.Document.Revision()
		if err != nil {
			return nil, err
		}
		if !t.provider.replaceIfRevision(key, input.BaseRevision, current) {
			return nil, workspace.ErrRevisionConflict
		}
		output = workspaceStatus(current)
		output.(map[string]any)["accepted_count"] = len(components)
	case "set_slots":
		if call.Ctx.AgentID != agenuiextensions.StyleAgent {
			return nil, errors.New("agenui workspace tool: only Style Agent may define slots")
		}
		current, ok := t.provider.snapshot(key)
		if !ok || current.Revision != input.BaseRevision {
			return nil, workspace.ErrRevisionConflict
		}
		if current.EditRequired {
			return nil, errors.New("agenui workspace tool: authorized edit workspaces cannot replace slots")
		}
		fields, err := decodeObjectArrayAllowEmpty(input.FieldSlots, "field_slots")
		if err != nil {
			return nil, err
		}
		actions, err := decodeObjectArrayAllowEmpty(input.ActionSlots, "action_slots")
		if err != nil {
			return nil, err
		}
		receipt, err := decodeOptionalJSONObject(input.RuleReceipt, "design_knowledge_receipt")
		if err != nil {
			return nil, err
		}
		current.Document.FieldSlots, current.Document.ActionSlots = fields, actions
		current.Document.DesignKnowledgeReceipt = receipt
		if err := current.Document.NormalizeSlotIdentities(); err != nil {
			return nil, err
		}
		current.Committed = false
		current.RequirementsJSON, current.RequirementsHash = "", ""
		current.Revision, err = current.Document.Revision()
		if err != nil {
			return nil, err
		}
		if !t.provider.replaceIfRevision(key, input.BaseRevision, current) {
			return nil, workspace.ErrRevisionConflict
		}
		output = workspaceStatus(current)
		output.(map[string]any)["field_slots"] = slotIdentityProjection("field", current.Document.FieldSlots)
		output.(map[string]any)["action_slots"] = slotIdentityProjection("action", current.Document.ActionSlots)
	case "set_preview_data":
		if call.Ctx.AgentID != agenuiextensions.StyleAgent {
			return nil, errors.New("agenui workspace tool: only Style Agent may set preview data")
		}
		current, ok := t.provider.snapshot(key)
		if !ok || current.Revision != input.BaseRevision {
			return nil, workspace.ErrRevisionConflict
		}
		if current.EditRequired {
			return nil, errors.New("agenui workspace tool: authorized edit workspaces cannot replace preview data")
		}
		dataModel, err := decodeJSONObject(input.DataModel, "data_model")
		if err != nil {
			return nil, err
		}
		current.Document.DataModel = dataModel
		current.Document.NormalizeRuntimeDataModel()
		current.Committed = false
		current.Revision, err = current.Document.Revision()
		if err != nil {
			return nil, err
		}
		if !t.provider.replaceIfRevision(key, input.BaseRevision, current) {
			return nil, workspace.ErrRevisionConflict
		}
		output = previewStatus(current)
		output.(map[string]any)["revision"] = current.Revision
	case "inspect":
		if call.Ctx.AgentID != agenuiextensions.StyleAgent {
			return nil, errors.New("agenui workspace tool: only Style Agent may inspect components")
		}
		current, ok := t.provider.snapshot(key)
		if !ok {
			return nil, errors.New("agenui workspace tool: workspace is not initialized")
		}
		output = inspectWorkspace(current, input.ComponentIDs)
	case "inspect_binding":
		if call.Ctx.AgentID != agenuiextensions.BinderAgent {
			return nil, errors.New("agenui workspace tool: only Binder Agent may inspect binding slots")
		}
		current, ok := t.provider.snapshot(key)
		if !ok || current.BindingInput == nil {
			return nil, errors.New("agenui workspace tool: frozen binding input is unavailable")
		}
		output = inspectBindingWorkspace(current)
	case "authorize_binding_edit":
		if call.Ctx.AgentID != agenuiextensions.BinderAgent {
			return nil, errors.New("agenui workspace tool: only Binder Agent may authorize a binding edit")
		}
		current, ok := t.provider.snapshot(key)
		if !ok || current.BindingInput == nil || current.Revision != input.BaseRevision {
			return nil, workspace.ErrRevisionConflict
		}
		if strings.TrimSpace(current.BindingEditQuery) == "" {
			return nil, errors.New("agenui workspace tool: this turn is not a binding edit")
		}
		targets, err := validateBindingEditTargets(*current.BindingInput, input.RequirementIDs)
		if err != nil {
			return nil, err
		}
		current.BindingEditTargets = targets
		if !t.provider.replaceIfRevision(key, input.BaseRevision, current) {
			return nil, workspace.ErrRevisionConflict
		}
		output = map[string]any{"revision": current.Revision, "authorized_requirement_ids": targets}
	case "commit_binding":
		if call.Ctx.AgentID != agenuiextensions.BinderAgent {
			return nil, errors.New("agenui workspace tool: only Binder Agent may submit bindings")
		}
		current, ok := t.provider.snapshot(key)
		if !ok || current.BindingInput == nil || current.Revision != input.BaseRevision {
			return nil, workspace.ErrRevisionConflict
		}
		if current.BindingEditQuery != "" && len(current.BindingEditTargets) == 0 {
			return nil, errors.New("agenui workspace tool: authorize_binding_edit is required before submitting this edit")
		}
		candidate, result, err := encodeBindingCandidate(input, *current.BindingInput)
		if err != nil {
			return nil, err
		}
		if err := bindingcontract.ValidateResult(*current.BindingInput, result); err != nil {
			return nil, fmt.Errorf("agenui workspace tool: binding_result does not match the frozen input: %w", err)
		}
		if err := bindingcontract.ValidateSourceFacts(*current.BindingInput, result); err != nil {
			return nil, fmt.Errorf("agenui workspace tool: binding_result uses unavailable source facts: %w", err)
		}
		canonicalPlan, err := bindingcontract.ReconcileExecutablePlan(
			*current.BindingInput, result, candidate,
		)
		if err != nil {
			return nil, fmt.Errorf("agenui workspace tool: mapping candidate does not match binding_result: %w", err)
		}
		candidate, err = bindingcontract.EncodeSubmission(result, canonicalPlan)
		if err != nil {
			return nil, err
		}
		if err := validateBindings(current.Document, bindingMaps(result.Bindings)); err != nil {
			return nil, err
		}
		if t.provider.bindingCommitter == nil {
			return nil, errors.New("agenui workspace tool: binding committer is unavailable")
		}
		// Commit persistence and publish the Workspace projection in one critical
		// section. This prevents a concurrent edit from winning after immutable
		// Binding/Final Artifacts have already been written for an older draft.
		t.provider.mu.Lock()
		latest, exists := t.provider.sessions[key]
		if !exists || latest.Revision != input.BaseRevision || latest.StateVersion != current.StateVersion {
			t.provider.mu.Unlock()
			return nil, workspace.ErrRevisionConflict
		}
		resultJSON, plan, final, err := t.provider.bindingCommitter.CommitBinding(
			ctx, call.Ctx, *current.BindingInput, candidate,
		)
		if err != nil {
			t.provider.mu.Unlock()
			return nil, fmt.Errorf("agenui workspace tool: commit binding: %w", err)
		}
		var committed map[string]any
		if json.Unmarshal([]byte(resultJSON), &committed) != nil {
			t.provider.mu.Unlock()
			return nil, errors.New("agenui workspace tool: committed binding result is invalid")
		}
		committed["workspace_revision"] = current.Revision
		current.BindingPlan = committed
		current.BindingCandidate = plan
		current.BindingResultJSON = resultJSON
		current.BindingFinal = final
		current.StateVersion++
		t.provider.sessions[key] = current
		t.provider.mu.Unlock()
		output = map[string]any{
			"revision": current.Revision, "accepted": true, "status": result.Status,
			"binding_count": len(result.Bindings), "issue_count": len(result.Issues),
		}
	case "apply_edit_contract":
		if call.Ctx.AgentID != agenuiextensions.StyleAgent {
			return nil, errors.New("agenui workspace tool: only Style Agent may apply a design edit")
		}
		current, ok := t.provider.snapshot(key)
		if !ok {
			return nil, errors.New("agenui workspace tool: workspace is not initialized")
		}
		if t.provider.authorizer == nil {
			return nil, errors.New("agenui workspace tool: edit authorization is unavailable")
		}
		var authorization edit.Contract
		raw := t.provider.authorizer.EditContractSnapshot(
			call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID,
		)
		if raw == "" || json.Unmarshal([]byte(raw), &authorization) != nil ||
			authorization.SchemaVersion != edit.SchemaVersion {
			return nil, errors.New("agenui workspace tool: frozen edit contract is missing")
		}
		if current.DesignHash == "" || current.DesignHash != authorization.Preconditions.DesignHash {
			return nil, fmt.Errorf("%w: workspace design changed", edit.ErrBaseRevisionConflict)
		}
		patch, err := edit.AuthorizedPatch(authorization, input.BaseRevision)
		if err != nil {
			return nil, err
		}
		result, err := workspace.Apply(current.Document, patch)
		if err != nil {
			return nil, err
		}
		current.Document, current.Revision, current.Committed = result.Document, result.Revision, false
		current.EditApplied = true
		if !t.provider.replaceIfRevision(key, input.BaseRevision, current) {
			return nil, workspace.ErrRevisionConflict
		}
		output = map[string]any{
			"revision": result.Revision, "changed_paths": result.ChangedPaths,
			"created_component_ids": result.CreatedIDs,
			"updated_component_ids": result.UpdatedIDs,
			"component_count":       len(result.Document.Components),
		}
	case "commit":
		current, ok := t.provider.snapshot(key)
		if !ok || current.Revision != input.BaseRevision {
			return nil, workspace.ErrRevisionConflict
		}
		if current.EditRequired && !current.EditApplied {
			return nil, errors.New("agenui workspace tool: frozen edit contract has not been applied")
		}
		if err := current.Document.ValidateStructural(); err != nil {
			return nil, fmt.Errorf("agenui workspace tool: document is structurally inconsistent: %w", err)
		}
		requirementsJSON, requirementsHash, err := t.compileRequirements(call, current)
		if err != nil {
			return nil, err
		}
		if empty := current.Document.EmptyDataPaths(); len(empty) > 0 {
			return nil, fmt.Errorf(
				"agenui workspace tool: preview data is incomplete at %v; call set_preview_data with readable non-sensitive examples before commit",
				empty,
			)
		}
		messages, err := current.Document.MarshalMessages()
		if err != nil {
			return nil, err
		}
		catalogResult := catalogadmission.Analyze(t.provider.catalogRoot, messages)
		if !catalogResult.Compatible {
			return nil, fmt.Errorf("agenui workspace tool: catalog validation failed: %s", catalogResult.Message)
		}
		current.Document.CatalogID = catalogResult.CatalogID
		if err := t.provider.validateCommittedDocument(current.Document); err != nil {
			return nil, fmt.Errorf("agenui workspace tool: %w", err)
		}
		current.Revision, err = current.Document.Revision()
		if err != nil {
			return nil, err
		}
		current.Committed = true
		current.RequirementsJSON = requirementsJSON
		current.RequirementsHash = requirementsHash
		if artifact, ok := workspaceDesignArtifact(current); ok {
			current.DesignHash = edit.HashText(artifact)
		}
		if !t.provider.replaceIfRevision(key, input.BaseRevision, current) {
			return nil, workspace.ErrRevisionConflict
		}
		output = map[string]any{
			"revision": current.Revision, "catalog_id": catalogResult.CatalogID,
			"component_count": len(current.Document.Components), "committed": true,
			"preview": previewStatus(current), "requirements_hash": requirementsHash,
			"requirements_revision": requirementsHash,
		}
	default:
		return nil, fmt.Errorf("agenui workspace tool: unsupported action %q", input.Action)
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, err
	}
	result := &extension.FunctionResult{Data: encoded, MimeType: "application/json"}
	if input.Action == "commit" {
		outputMap, _ := output.(map[string]any)
		result.Presentation = &extension.ResultPresentation{
			Title:   "卡片版式与内容结构已生成",
			Summary: "界面结构已经校验并提交，可在预览区查看结果",
			Details: []extension.ResultPresentationDetail{
				{Label: "组件数量", Value: fmt.Sprint(outputMap["component_count"])},
				{Label: "目录版本", Value: fmt.Sprint(outputMap["catalog_id"])},
			},
		}
	}
	return result, nil
}

func (t *WorkspaceTool) compileRequirements(
	call extension.FunctionCall,
	current workspaceSession,
) (string, string, error) {
	authority, ok := t.provider.authorizer.(interface {
		DesigningSnapshot(tenantID, userID, sessionID string) (contractJSON, preflightJSON string, ok bool)
	})
	if !ok {
		return "", "", errors.New("agenui workspace tool: frozen content contract provider is unavailable")
	}
	raw, _, exists := authority.DesigningSnapshot(call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID)
	if !exists || strings.TrimSpace(raw) == "" {
		return "", "", errors.New("agenui workspace tool: frozen content contract is unavailable")
	}
	var frozen designcontract.Revision
	if json.Unmarshal([]byte(raw), &frozen) != nil || frozen.SchemaVersion != designcontract.SchemaVersion {
		return "", "", errors.New("agenui workspace tool: frozen content contract is invalid")
	}
	fields, actions, err := requirements.ParseSlots(current.Document.FieldSlots, current.Document.ActionSlots)
	if err != nil {
		return "", "", fmt.Errorf("agenui workspace tool: compile requirements: %w", err)
	}
	compiled, err := requirements.Compile(requirements.Input{
		Contract: frozen.Draft, FieldSlots: fields, ActionSlots: actions,
	})
	if err != nil {
		return "", "", fmt.Errorf("agenui workspace tool: compile requirements: %w", err)
	}
	encoded, err := json.Marshal(compiled)
	if err != nil {
		return "", "", fmt.Errorf("agenui workspace tool: encode requirements: %w", err)
	}
	text := string(encoded)
	return text, edit.HashText(text), nil
}

func workspaceString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := value[key].(string); ok && strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

func slotIdentityProjection(kind string, slots []map[string]any) []map[string]any {
	result := make([]map[string]any, 0, len(slots))
	for _, slot := range slots {
		identity := map[string]any{
			"slot_id":      workspaceString(slot, "slotId", "id"),
			"component_id": workspaceString(slot, "componentId", "component_id"),
		}
		if kind == "action" {
			identity["contract_action_id"] = workspaceString(slot, "contractActionId", "contract_action_id")
		} else {
			identity["contract_item_id"] = workspaceString(slot, "contractItemId", "contract_item_id")
		}
		result = append(result, identity)
	}
	return result
}

func previewStatus(current workspaceSession) map[string]any {
	empty := current.Document.EmptyDataPaths()
	return map[string]any{"ready": len(empty) == 0, "empty_paths": empty}
}

func workspaceStatus(current workspaceSession) map[string]any {
	result := map[string]any{
		"revision":          current.Revision,
		"committed":         current.Committed,
		"component_count":   len(current.Document.Components),
		"field_slot_count":  len(current.Document.FieldSlots),
		"action_slot_count": len(current.Document.ActionSlots),
		"preview":           previewStatus(current),
	}
	switch {
	case current.EditRequired && !current.EditApplied:
		result["next_actions"] = []string{"inspect", "apply_edit_contract"}
	case current.EditRequired && current.EditApplied:
		result["next_actions"] = []string{"commit"}
	case current.Committed:
		result["next_actions"] = []string{}
	case len(current.Document.Components) == 0:
		result["next_actions"] = []string{"put_components"}
	case len(current.Document.FieldSlots) == 0 && len(current.Document.ActionSlots) == 0:
		result["next_actions"] = []string{"set_slots"}
	case len(current.Document.EmptyDataPaths()) > 0:
		result["next_actions"] = []string{"set_preview_data"}
	default:
		result["next_actions"] = []string{"commit"}
	}
	return result
}

func upsertWorkspaceComponents(document *workspace.Document, values []map[string]any) error {
	indices := make(map[string]int, len(document.Components))
	for index, component := range document.Components {
		id, _ := component["id"].(string)
		if id != "" {
			indices[id] = index
		}
	}
	seen := make(map[string]struct{}, len(values))
	for index, raw := range values {
		component, err := normalizeWorkspaceComponent(raw)
		if err != nil {
			return fmt.Errorf("agenui workspace tool: component %d: %w", index, err)
		}
		id, _ := component["id"].(string)
		kind, _ := component["component"].(string)
		if strings.TrimSpace(id) == "" || strings.TrimSpace(kind) == "" {
			return fmt.Errorf("agenui workspace tool: component %d requires string id and string component", index)
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("agenui workspace tool: duplicate component id %q in call", id)
		}
		seen[id] = struct{}{}
		if existing, exists := indices[id]; exists {
			document.Components[existing] = component
		} else {
			document.Components = append(document.Components, component)
		}
	}
	return nil
}

// normalizeWorkspaceComponent accepts the two shapes models commonly produce:
// the canonical flat AGenUI component and an id plus nested component object.
// This is syntax normalization only; Catalog validation still owns component
// admission at commit time.
func normalizeWorkspaceComponent(raw map[string]any) (map[string]any, error) {
	if raw == nil {
		return nil, errors.New("must be an object")
	}
	id, _ := raw["id"].(string)
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("id must be a non-empty string")
	}
	value, exists := raw["component"]
	if !exists {
		return nil, errors.New("component is required")
	}
	switch typed := value.(type) {
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return nil, errors.New("component must be a non-empty string")
		}
		if strings.HasPrefix(text, "{") {
			var nested map[string]any
			if json.Unmarshal([]byte(text), &nested) != nil {
				return nil, errors.New("component contains malformed JSON; use a component name or nested component object")
			}
			return mergeNestedWorkspaceComponent(id, raw, nested)
		}
		return raw, nil
	case map[string]any:
		return mergeNestedWorkspaceComponent(id, raw, typed)
	default:
		return nil, errors.New("component must be a string component name or an object containing a string component name")
	}
}

func mergeNestedWorkspaceComponent(id string, outer, nested map[string]any) (map[string]any, error) {
	kind, _ := nested["component"].(string)
	if strings.TrimSpace(kind) == "" {
		return nil, errors.New("nested component.component must be a non-empty string")
	}
	result := make(map[string]any, len(outer)+len(nested))
	for key, value := range nested {
		result[key] = value
	}
	for key, value := range outer {
		if key != "component" {
			result[key] = value
		}
	}
	result["id"] = id
	return result, nil
}

func decodeJSONObject(raw json.RawMessage, field string) (map[string]any, error) {
	decoded, err := unwrapJSONString(raw)
	if err != nil {
		return nil, fmt.Errorf("agenui workspace tool: decode %s: %w", field, err)
	}
	var result map[string]any
	if len(decoded) == 0 || json.Unmarshal(decoded, &result) != nil || result == nil {
		return nil, fmt.Errorf("agenui workspace tool: %s must be an object", field)
	}
	return result, nil
}

func decodeOptionalJSONObject(raw json.RawMessage, field string) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	return decodeJSONObject(raw, field)
}

func decodeObjectArray(raw json.RawMessage, field string) ([]map[string]any, error) {
	result, err := decodeObjectArrayAllowEmpty(raw, field)
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("agenui workspace tool: %s must not be empty", field)
	}
	return result, nil
}

func decodeObjectArrayAllowEmpty(raw json.RawMessage, field string) ([]map[string]any, error) {
	decoded, err := unwrapJSONString(raw)
	if err != nil {
		return nil, fmt.Errorf("agenui workspace tool: decode %s: %w", field, err)
	}
	var result []map[string]any
	if len(decoded) == 0 || json.Unmarshal(decoded, &result) != nil || result == nil {
		return nil, fmt.Errorf("agenui workspace tool: %s must be an array", field)
	}
	return result, nil
}

// PrepareBindingInput freezes the Host-owned projection that the Binder may
// inspect. It contains semantic slots, requirements and authorized sources,
// never the complete component tree.
func (p *WorkspaceProvider) PrepareBindingInput(
	ctx context.Context,
	identity harness.Identity,
	input bindingcontract.Input,
) error {
	if p == nil {
		return errors.New("agenui workspace tool: provider is unavailable")
	}
	key := workspaceSessionKey(identity.TenantID, identity.UserID, identity.SessionID, identity.RunID)
	current, ok := p.snapshot(key)
	if !ok {
		return errors.New("agenui workspace tool: workspace is not initialized")
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("agenui workspace tool: encode frozen binding input: %w", err)
	}
	var frozen bindingcontract.Input
	if err := json.Unmarshal(raw, &frozen); err != nil {
		return fmt.Errorf("agenui workspace tool: freeze binding input: %w", err)
	}
	current.BindingInput = &frozen
	current.BindingCandidate = ""
	current.BindingResultJSON = ""
	current.BindingFinal = ""
	current.BindingPlan = nil
	if !p.replaceIfRevision(key, current.Revision, current) {
		return workspace.ErrRevisionConflict
	}
	return nil
}

// RefreshBindingSources projects a governed Search receipt into the already
// frozen Binder view. Search runs inside the Binder child after requirements
// and slots were prepared, so immutable source receipts are the only part of
// the view that changes during that child turn.
func (p *WorkspaceProvider) RefreshBindingSources(
	ctx context.Context,
	identity harness.Identity,
	sources []bindingcontract.SourceSnapshot,
) error {
	if p == nil {
		return errors.New("agenui workspace tool: provider is unavailable")
	}
	key := workspaceSessionKey(identity.TenantID, identity.UserID, identity.SessionID, identity.RunID)
	current, ok := p.snapshot(key)
	if !ok || current.BindingInput == nil {
		return errors.New("agenui workspace tool: frozen binding input is unavailable")
	}
	raw, err := json.Marshal(sources)
	if err != nil {
		return fmt.Errorf("agenui workspace tool: encode authorized binding sources: %w", err)
	}
	var frozen []bindingcontract.SourceSnapshot
	if err := json.Unmarshal(raw, &frozen); err != nil {
		return fmt.Errorf("agenui workspace tool: freeze authorized binding sources: %w", err)
	}
	current.BindingInput.Sources = frozen
	current.BindingCandidate = ""
	current.BindingResultJSON = ""
	current.BindingFinal = ""
	current.BindingPlan = nil
	if !p.replaceIfRevision(key, current.Revision, current) {
		return workspace.ErrRevisionConflict
	}
	return nil
}

func (p *WorkspaceProvider) RequireBindingEdit(ctx context.Context, identity harness.Identity, query string) error {
	if p == nil || strings.TrimSpace(query) == "" {
		return errors.New("agenui workspace tool: binding edit query is required")
	}
	key := workspaceSessionKey(identity.TenantID, identity.UserID, identity.SessionID, identity.RunID)
	current, ok := p.snapshot(key)
	if !ok {
		return errors.New("agenui workspace tool: workspace is not initialized")
	}
	current.BindingEditQuery = strings.TrimSpace(query)
	current.BindingEditTargets = nil
	if !p.replaceIfRevision(key, current.Revision, current) {
		return workspace.ErrRevisionConflict
	}
	return nil
}

func (p *WorkspaceProvider) BindingEditTargets(tenantID, userID, sessionID, runID string) ([]string, bool) {
	current, ok := p.snapshot(workspaceSessionKey(tenantID, userID, sessionID, runID))
	if !ok || len(current.BindingEditTargets) == 0 {
		return nil, false
	}
	return append([]string(nil), current.BindingEditTargets...), true
}

func (p *WorkspaceProvider) CommittedBindingSnapshot(tenantID, userID, sessionID, runID string) (resultJSON, plan, final string, ok bool) {
	current, ok := p.snapshot(workspaceSessionKey(tenantID, userID, sessionID, runID))
	if !ok || strings.TrimSpace(current.BindingCandidate) == "" ||
		strings.TrimSpace(current.BindingResultJSON) == "" {
		return "", "", "", false
	}
	if err := p.validateCommittedDocument(current.Document); err != nil {
		return "", "", "", false
	}
	return current.BindingResultJSON, current.BindingCandidate, current.BindingFinal, true
}

func encodeBindingCandidate(input workspaceCall, frozen bindingcontract.Input) (string, bindingcontract.Result, error) {
	if len(input.BindingResult) == 0 {
		return "", bindingcontract.Result{}, errors.New("agenui workspace tool: binding_result is required")
	}
	bindingResult, err := unwrapJSONString(input.BindingResult)
	if err != nil {
		return "", bindingcontract.Result{}, fmt.Errorf("agenui workspace tool: decode binding_result string: %w", err)
	}
	var result bindingcontract.Result
	decoder := json.NewDecoder(strings.NewReader(string(bindingResult)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || result.SchemaVersion != bindingcontract.ResultSchemaV1 {
		return "", bindingcontract.Result{}, errors.New("agenui workspace tool: binding_result is invalid")
	}
	if result.Status != bindingcontract.StatusReady &&
		result.Status != bindingcontract.StatusReadyWithOperators &&
		result.Status != bindingcontract.StatusBlocked &&
		result.Status != bindingcontract.StatusUncertain {
		return "", bindingcontract.Result{}, errors.New("agenui workspace tool: binding status is invalid")
	}
	fields := []map[string]any{}
	actions, err := deriveActionMappings(frozen, result)
	if err != nil {
		return "", bindingcontract.Result{}, err
	}
	plan, err := bindingcontract.EncodeExecutablePlan(fields, actions)
	if err != nil {
		return "", bindingcontract.Result{}, err
	}
	return plan, result, nil
}

func deriveActionMappings(frozen bindingcontract.Input, result bindingcontract.Result) ([]map[string]any, error) {
	var slots []map[string]any
	if err := json.Unmarshal(frozen.Design.ActionSlots, &slots); err != nil {
		return nil, errors.New("agenui workspace tool: frozen action slots are invalid")
	}
	components := make(map[string]string, len(slots))
	for _, slot := range slots {
		components[stringValue(slot["slotId"])] = stringValue(slot["componentId"])
	}
	var mappings []map[string]any
	for _, binding := range result.Bindings {
		if binding.ActionPath == "" {
			continue
		}
		for _, slotID := range binding.TargetSlotIDs {
			componentID := components[slotID]
			if componentID == "" {
				return nil, fmt.Errorf("agenui workspace tool: action slot %q has no frozen componentId", slotID)
			}
			mapping := map[string]any{
				"componentId":      componentID,
				"actionSourceType": binding.ActionSourceType,
			}
			switch binding.ActionSourceType {
			case "url":
				mapping["urlPath"] = binding.ActionPath
			case "event":
				mapping["eventPath"] = binding.ActionPath
			default:
				return nil, fmt.Errorf("agenui workspace tool: action binding %q requires action_source_type url/event", binding.RequirementID)
			}
			mappings = append(mappings, mapping)
		}
	}
	return mappings, nil
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func unwrapJSONString(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || raw[0] != '"' {
		return raw, nil
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return nil, err
	}
	return json.RawMessage(encoded), nil
}

func bindingMaps(bindings []bindingcontract.Binding) []map[string]any {
	result := make([]map[string]any, 0, len(bindings))
	for _, binding := range bindings {
		result = append(result, map[string]any{"target_slot_ids": binding.TargetSlotIDs})
	}
	return result
}

func (p *WorkspaceProvider) CommittedBindingPlan(tenantID, userID, sessionID, runID string) (string, bool) {
	current, ok := p.snapshot(workspaceSessionKey(tenantID, userID, sessionID, runID))
	if !ok || current.BindingPlan == nil {
		return "", false
	}
	raw, err := json.Marshal(current.BindingPlan)
	return string(raw), err == nil
}

func (p *WorkspaceProvider) snapshot(key string) (workspaceSession, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	current, ok := p.sessions[key]
	return current, ok
}

func (p *WorkspaceProvider) replace(key string, current workspaceSession) {
	p.mu.Lock()
	if current.StateVersion == 0 {
		current.StateVersion = 1
	}
	p.sessions[key] = current
	p.mu.Unlock()
}

func (p *WorkspaceProvider) replaceIfRevision(
	key, expectedRevision string,
	current workspaceSession,
) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	existing, ok := p.sessions[key]
	if !ok || existing.Revision != expectedRevision || existing.StateVersion != current.StateVersion {
		return false
	}
	current.StateVersion++
	p.sessions[key] = current
	return true
}

func workspaceContextKey(ctx extension.Context) string {
	runID := strings.TrimSpace(ctx.RootRunID)
	if runID == "" {
		runID = strings.TrimSpace(ctx.RunID)
	}
	return workspaceSessionKey(ctx.TenantID, ctx.UserID, ctx.SessionID, runID)
}

func workspaceSessionKey(tenantID, userID, sessionID, runID string) string {
	return tenantID + "\x00" + userID + "\x00" + sessionID + "\x00" + runID
}

func validateBindings(document workspace.Document, bindings []map[string]any) error {
	validSlots := make(map[string]struct{}, len(document.FieldSlots)+len(document.ActionSlots))
	for _, slot := range append(append([]map[string]any(nil), document.FieldSlots...), document.ActionSlots...) {
		id, _ := slot["slotId"].(string)
		if id == "" {
			id, _ = slot["id"].(string)
		}
		if id != "" {
			validSlots[id] = struct{}{}
		}
	}
	for index, binding := range bindings {
		targets := bindingTargetSlots(binding)
		if len(targets) == 0 {
			return fmt.Errorf("agenui workspace tool: binding %d has no target slots", index)
		}
		for _, slotID := range targets {
			if _, exists := validSlots[slotID]; !exists {
				return fmt.Errorf("agenui workspace tool: binding %d references unknown slot %q", index, slotID)
			}
		}
	}
	return nil
}

func bindingTargetSlots(binding map[string]any) []string {
	if slotID, _ := binding["slot_id"].(string); slotID != "" {
		return []string{slotID}
	}
	var result []string
	switch values := binding["target_slot_ids"].(type) {
	case []any:
		for _, value := range values {
			if slotID, _ := value.(string); slotID != "" {
				result = append(result, slotID)
			}
		}
	case []string:
		result = append(result, values...)
	}
	return result
}

func inspectWorkspace(current workspaceSession, componentIDs []string) map[string]any {
	result := workspaceStatus(current)
	result["surface_id"] = current.Document.SurfaceID
	result["catalog_id"] = current.Document.CatalogID
	result["root_id"] = current.Document.RootID
	if len(componentIDs) == 0 {
		return result
	}
	wanted := make(map[string]struct{}, len(componentIDs))
	for _, id := range componentIDs {
		wanted[id] = struct{}{}
	}
	components := make([]map[string]any, 0)
	for _, component := range current.Document.Components {
		id, _ := component["id"].(string)
		if _, exists := wanted[id]; exists {
			components = append(components, component)
		}
	}
	result["components"] = components
	return result
}

func inspectBindingWorkspace(current workspaceSession) map[string]any {
	result := map[string]any{
		"revision":     current.Revision,
		"requirements": current.BindingInput.Requirements,
		"sources":      current.BindingInput.Sources,
		"field_slots":  current.Document.FieldSlots,
		"action_slots": current.Document.ActionSlots,
	}
	if current.BindingEditQuery != "" {
		result["edit_request"] = current.BindingEditQuery
		result["edit_requires_authorization"] = true
	}
	return result
}

func validateBindingEditTargets(input bindingcontract.Input, values []string) ([]string, error) {
	allowed := make(map[string]struct{}, len(input.Requirements.Data)+len(input.Requirements.Actions))
	for _, item := range input.Requirements.Data {
		allowed[item.RequirementID] = struct{}{}
	}
	for _, item := range input.Requirements.Actions {
		allowed[item.RequirementID] = struct{}{}
	}
	targets := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if _, ok := allowed[value]; !ok {
			return nil, fmt.Errorf("agenui workspace tool: unknown binding requirement %q", value)
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		targets = append(targets, value)
	}
	if len(targets) == 0 {
		return nil, errors.New("agenui workspace tool: at least one requirement_id is required")
	}
	sort.Strings(targets)
	return targets, nil
}

var _ extension.ToolProvider = (*WorkspaceProvider)(nil)
