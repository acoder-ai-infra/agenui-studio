package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/contract"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/edit"
	agenuiextensions "github.com/AGenUI/agenui-studio/agenui-agent/internal/extensions"
	stepartifact "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/artifact"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/catalogadmission"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/workspace"
	"github.com/AGenUI/agenui-studio/agenui-agent/pkg/agenui/renderercatalog"
	"github.com/AGenUI/agenui-studio/harness/sdk"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

const (
	DesigningProviderID       = "agenui.tools.designing.v2"
	SubmitContractName        = "agenui_submit_content_contract"
	PreflightCapabilitiesName = "agenui_preflight_capabilities"
	ResolveDesignEditName     = "agenui_resolve_design_edit"
	SubmitEditContractName    = "agenui_submit_edit_contract"
	PrepareBindingEditName    = "agenui_prepare_binding_edit"
	DesigningToolVersion      = "2.0.0"
)

type capabilityEvidenceStore interface {
	CapabilityPreflightEvidence(tenantID, sessionID, traceID string) (string, bool)
}

type DesigningProvider struct {
	contract     *SubmitContentContract
	preflight    *PreflightCapabilities
	edit         *ResolveDesignEdit
	editContract *SubmitEditContract
	bindingEdit  *PrepareBindingEdit
	nextSteps    *PublishNextSteps
	sessions     *designingSessions
	artifacts    designingArtifactLoader
	catalogRoot  string
}

type designingArtifactLoader interface {
	Load(context.Context, harness.Identity, string) (string, error)
	LatestRunID(context.Context, harness.Identity, string) (string, error)
	Save(context.Context, harness.Identity, string, string) (stepartifact.Pointer, error)
}

type designingSessions struct {
	mu           sync.RWMutex
	contracts    map[string]contract.Revision
	preflight    map[string]string
	designs      map[string]string
	resolved     map[string]string
	edits        map[string]edit.Contract
	pendingEdits map[string]bool
	bindingEdits map[string]bindingEditRequest
}

type bindingEditRequest struct {
	Query     string
	BaseRunID string
}

func NewDesigningProvider(evidence capabilityEvidenceStore, catalogRoots ...string) (*DesigningProvider, error) {
	catalogRoot := renderercatalog.DefaultRoot
	if len(catalogRoots) > 0 && strings.TrimSpace(catalogRoots[0]) != "" {
		catalogRoot = catalogRoots[0]
	}
	sessions := &designingSessions{
		contracts:    make(map[string]contract.Revision),
		preflight:    make(map[string]string),
		designs:      make(map[string]string),
		resolved:     make(map[string]string),
		edits:        make(map[string]edit.Contract),
		pendingEdits: make(map[string]bool),
		bindingEdits: make(map[string]bindingEditRequest),
	}
	preflight := &PreflightCapabilities{sessions: sessions, evidence: evidence}
	provider := &DesigningProvider{
		contract:     &SubmitContentContract{sessions: sessions},
		preflight:    preflight,
		edit:         &ResolveDesignEdit{sessions: sessions},
		editContract: &SubmitEditContract{sessions: sessions},
		bindingEdit:  &PrepareBindingEdit{sessions: sessions},
		nextSteps:    &PublishNextSteps{},
		sessions:     sessions, catalogRoot: catalogRoot,
	}
	provider.contract.provider = provider
	provider.preflight.provider = provider
	provider.edit.provider = provider
	provider.editContract.provider = provider
	provider.bindingEdit.provider = provider
	return provider, nil
}

func (p *DesigningProvider) editableStylePaths() []string {
	if p == nil {
		return nil
	}
	paths, err := catalogadmission.EditableStylePaths(p.catalogRoot)
	if err != nil {
		return nil
	}
	return paths
}

func (*DesigningProvider) ID() string { return DesigningProviderID }

func (p *DesigningProvider) FunctionTools() []extension.FunctionTool {
	return []extension.FunctionTool{p.contract, p.preflight, p.edit, p.editContract, p.bindingEdit, p.nextSteps}
}

func (p *DesigningProvider) SetPersistence(artifacts designingArtifactLoader) error {
	if p == nil || artifacts == nil {
		return errors.New("designing tools: artifact store is required")
	}
	p.artifacts = artifacts
	p.preflight.provider = p
	p.edit.provider = p
	p.editContract.provider = p
	p.bindingEdit.provider = p
	p.contract.provider = p
	return nil
}

// TakeBindingEditRequest consumes the exact user request prepared by the Main
// Agent. The orchestration layer uses it as a typed route fact; models never
// need to manufacture an internal task-description marker.
func (p *DesigningProvider) TakeBindingEditRequest(tenantID, userID, sessionID string) (string, string, bool) {
	if p == nil || p.sessions == nil {
		return "", "", false
	}
	key := sessionKey(tenantID, userID, sessionID)
	p.sessions.mu.Lock()
	defer p.sessions.mu.Unlock()
	request, ok := p.sessions.bindingEdits[key]
	delete(p.sessions.bindingEdits, key)
	return request.Query, request.BaseRunID, ok &&
		strings.TrimSpace(request.Query) != "" && strings.TrimSpace(request.BaseRunID) != ""
}

func (p *DesigningProvider) RecordDesignSnapshot(tenantID, userID, sessionID, design string) error {
	if p == nil || p.sessions == nil || strings.TrimSpace(design) == "" {
		return errors.New("design snapshot: content is required")
	}
	key := sessionKey(tenantID, userID, sessionID)
	p.sessions.mu.Lock()
	p.sessions.designs[key] = design
	delete(p.sessions.resolved, key)
	delete(p.sessions.edits, key)
	delete(p.sessions.pendingEdits, key)
	delete(p.sessions.bindingEdits, key)
	p.sessions.mu.Unlock()
	return nil
}

func (p *DesigningProvider) ResolvedEditSnapshot(tenantID, userID, sessionID string) string {
	if p == nil || p.sessions == nil {
		return ""
	}
	key := sessionKey(tenantID, userID, sessionID)
	p.sessions.mu.RLock()
	defer p.sessions.mu.RUnlock()
	return p.sessions.resolved[key]
}

func (p *DesigningProvider) DesignEditPending(tenantID, userID, sessionID string) bool {
	if p == nil || p.sessions == nil {
		return false
	}
	key := sessionKey(tenantID, userID, sessionID)
	p.sessions.mu.RLock()
	defer p.sessions.mu.RUnlock()
	return p.sessions.pendingEdits[key]
}

func (p *DesigningProvider) EditContractSnapshot(tenantID, userID, sessionID string) string {
	if p == nil || p.sessions == nil {
		return ""
	}
	key := sessionKey(tenantID, userID, sessionID)
	p.sessions.mu.RLock()
	defer p.sessions.mu.RUnlock()
	value, ok := p.sessions.edits[key]
	if !ok {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func (p *DesigningProvider) CurrentDesignSnapshot(tenantID, userID, sessionID string) string {
	if p == nil || p.sessions == nil {
		return ""
	}
	key := sessionKey(tenantID, userID, sessionID)
	p.sessions.mu.RLock()
	defer p.sessions.mu.RUnlock()
	return p.sessions.designs[key]
}

// EnsureDesignEditContract is the Host-side safety net for existing-card
// continuations. It can recover a previously persisted contract and expose
// real targets, but it deliberately does not infer a target, property path, or
// value. The Main Agent selects an exact target and one of its editable paths;
// the Host then freezes the target and protection sets.
func (p *DesigningProvider) EnsureDesignEditContract(
	ctx context.Context,
	identity harness.Identity,
	query string,
) (editContractJSON, resolvedJSON string, err error) {
	if p == nil || p.sessions == nil || p.artifacts == nil {
		return "", "", errors.New("design edit contract: persistent recovery is unavailable")
	}
	key := sessionKey(identity.TenantID, identity.UserID, identity.SessionID)
	p.sessions.mu.RLock()
	existing, hasExisting := p.sessions.edits[key]
	baseDesign := p.sessions.designs[key]
	_, contractOK := p.sessions.contracts[key]
	p.sessions.mu.RUnlock()
	if hasExisting {
		encoded, marshalErr := json.Marshal(existing)
		if marshalErr != nil {
			return "", "", marshalErr
		}
		return string(encoded), p.ResolvedEditSnapshot(identity.TenantID, identity.UserID, identity.SessionID), nil
	}
	if baseDesign == "" || !contractOK {
		if restoreErr := p.restoreSession(ctx, extension.Context{
			TenantID: identity.TenantID, UserID: identity.UserID, SessionID: identity.SessionID,
		}); restoreErr != nil {
			return "", "", restoreErr
		}
		p.sessions.mu.RLock()
		existing, hasExisting = p.sessions.edits[key]
		baseDesign = p.sessions.designs[key]
		_, contractOK = p.sessions.contracts[key]
		p.sessions.mu.RUnlock()
	}
	if hasExisting {
		encoded, marshalErr := json.Marshal(existing)
		if marshalErr != nil {
			return "", "", marshalErr
		}
		return string(encoded), p.ResolvedEditSnapshot(identity.TenantID, identity.UserID, identity.SessionID), nil
	}
	if baseDesign == "" || !contractOK {
		return "", "", errors.New("design edit contract: base design and content contract are required")
	}
	if resolved := p.ResolvedEditSnapshot(identity.TenantID, identity.UserID, identity.SessionID); strings.TrimSpace(resolved) != "" {
		return "", resolved, errors.New("design edit contract: resolved target requires an explicit requested_changes proposal")
	}
	sum := sha256.Sum256([]byte(baseDesign))
	messages, fields, actions, partsErr := designIndexParts(baseDesign)
	if partsErr != nil {
		return "", "", partsErr
	}
	stylePaths := p.editableStylePaths()
	index, indexErr := edit.BuildIndex(
		"design_"+hex.EncodeToString(sum[:8]),
		messages, fields, actions, stylePaths...,
	)
	if indexErr != nil {
		return "", "", indexErr
	}
	response := map[string]any{
		"query": strings.TrimSpace(query), "design_revision": index.Revision,
		"status": "candidates", "candidates": edit.Candidates(index),
	}
	encodedResolution, marshalErr := json.Marshal(response)
	if marshalErr != nil {
		return "", "", marshalErr
	}
	return "", string(encodedResolution), errors.New("design edit contract: model must select all target IDs and submit one atomic targets proposal")
}

// DesigningSnapshot exposes only host-validated session artifacts to the task
// boundary. It never exposes model scratch state.
func (p *DesigningProvider) DesigningSnapshot(tenantID, userID, sessionID string) (string, string, bool) {
	if p == nil || p.sessions == nil {
		return "", "", false
	}
	key := sessionKey(tenantID, userID, sessionID)
	p.sessions.mu.RLock()
	defer p.sessions.mu.RUnlock()
	revision, ok := p.sessions.contracts[key]
	if !ok {
		return "", "", false
	}
	encoded, err := json.Marshal(revision)
	if err != nil {
		return "", "", false
	}
	return string(encoded), p.sessions.preflight[key], true
}

func (p *DesigningProvider) RestoreDesigningSnapshot(
	ctx context.Context,
	tenantID, userID, sessionID string,
) error {
	return p.restoreSession(ctx, extension.Context{
		TenantID: tenantID, UserID: userID, SessionID: sessionID,
	})
}

type SubmitContentContract struct {
	sessions *designingSessions
	provider *DesigningProvider
}

func (*SubmitContentContract) Name() string { return SubmitContractName }

func (t *SubmitContentContract) Invoke(ctx context.Context, call extension.FunctionCall) (*extension.FunctionResult, error) {
	if call.Ctx.AgentID != agenuiextensions.MainAgent || call.Name != SubmitContractName {
		return nil, errors.New("submit content contract: caller is not allowed")
	}
	var draft contract.Draft
	decoder := json.NewDecoder(strings.NewReader(string(call.Arguments)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&draft); err != nil {
		return nil, err
	}
	canonical, err := contract.Canonicalize(draft)
	if err != nil {
		return nil, err
	}
	hash, err := contract.Hash(canonical)
	if err != nil {
		return nil, err
	}
	idSum := sha256.Sum256([]byte(call.Ctx.TenantID + "\x00" + call.Ctx.UserID + "\x00" + call.Ctx.SessionID + "\x00content-contract"))
	contractID := "contract_" + hex.EncodeToString(idSum[:8])
	revisionNumber := int64(1)
	key := sessionKey(call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID)
	if t.provider != nil {
		t.sessions.mu.RLock()
		_, exists := t.sessions.contracts[key]
		t.sessions.mu.RUnlock()
		if !exists {
			_ = t.provider.restoreSession(ctx, call.Ctx)
		}
	}
	t.sessions.mu.Lock()
	defer t.sessions.mu.Unlock()
	if previous, ok := t.sessions.contracts[key]; ok && previous.ContractID == contractID {
		if previous.ContentHash == hash {
			revisionNumber = previous.Revision
		} else {
			revisionNumber = previous.Revision + 1
		}
	}
	revision := contract.Revision{
		ContractID: contractID, Revision: revisionNumber,
		SchemaVersion: contract.SchemaVersion, Status: "confirmed",
		ChangeOrigin: "user", ContentHash: hash, Draft: canonical,
	}
	if revisionNumber > 1 {
		revision.BaseRevision = revisionNumber - 1
	}
	encoded, err := json.Marshal(revision)
	if err != nil {
		return nil, err
	}
	if t.provider != nil && t.provider.artifacts != nil {
		if _, err := t.provider.artifacts.Save(ctx, harness.Identity{
			TenantID: call.Ctx.TenantID, UserID: call.Ctx.UserID,
			SessionID: call.Ctx.SessionID, RunID: call.Ctx.RunID,
		}, stepartifact.StepContract, string(encoded)); err != nil {
			return nil, err
		}
	}
	// The durable Artifact is the source of truth. Publish the in-memory
	// projection only after persistence succeeds so callers can never observe a
	// contract revision that recovery cannot load.
	t.sessions.contracts[key] = revision
	delete(t.sessions.preflight, key)
	delete(t.sessions.resolved, key)
	delete(t.sessions.edits, key)
	delete(t.sessions.pendingEdits, key)
	deliveryMode := revision.DeliveryMode
	if deliveryMode == "" {
		deliveryMode = contract.DeliveryModeExecutable
	}
	response, _ := json.Marshal(map[string]any{
		"contract_id": revision.ContractID, "revision": revision.Revision,
		"content_hash": revision.ContentHash, "status": revision.Status,
		"delivery_mode": deliveryMode,
	})
	return &extension.FunctionResult{
		Data: response, MimeType: "application/json",
		Presentation: contentContractPresentation(canonical),
	}, nil
}

// PreflightCapabilities returns an explicit unknown snapshot until the shared
// capability knowledge provider is configured. It never fabricates available
// data, actions, or operators.
type PreflightCapabilities struct {
	sessions *designingSessions
	provider *DesigningProvider
	evidence capabilityEvidenceStore
}

func (*PreflightCapabilities) Name() string { return PreflightCapabilitiesName }

func (t *PreflightCapabilities) Invoke(ctx context.Context, call extension.FunctionCall) (*extension.FunctionResult, error) {
	if call.Ctx.AgentID != agenuiextensions.StyleAgent || call.Name != PreflightCapabilitiesName {
		return nil, errors.New("capability preflight: caller is not allowed")
	}
	key := sessionKey(call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID)
	t.sessions.mu.RLock()
	revision, ok := t.sessions.contracts[key]
	t.sessions.mu.RUnlock()
	var submitted struct {
		ContractSource string          `json:"contract_source"`
		Contract       *contract.Draft `json:"contract"`
	}
	if err := json.Unmarshal(call.Arguments, &submitted); err != nil {
		return nil, errors.New("capability preflight: input is invalid")
	}
	if submitted.ContractSource != "frozen" && submitted.ContractSource != "provided" {
		return nil, errors.New("capability preflight: contract_source must be frozen or provided")
	}
	if submitted.ContractSource == "provided" && submitted.Contract == nil {
		return nil, errors.New("capability preflight: provided contract is missing")
	}
	if submitted.ContractSource == "frozen" && submitted.Contract != nil {
		return nil, errors.New("capability preflight: frozen source must not include contract")
	}
	if !ok {
		if t.provider != nil {
			_ = t.provider.restoreSession(ctx, call.Ctx)
		}
		t.sessions.mu.RLock()
		revision, ok = t.sessions.contracts[key]
		t.sessions.mu.RUnlock()
	}
	if submitted.Contract != nil {
		canonical, err := contract.Canonicalize(*submitted.Contract)
		if err != nil {
			return nil, err
		}
		hash, err := contract.Hash(canonical)
		if err != nil {
			return nil, err
		}
		if ok && hash != revision.ContentHash {
			return nil, errors.New("capability preflight: submitted contract does not match frozen revision")
		}
		if !ok {
			revision = contract.Revision{SchemaVersion: contract.SchemaVersion, Status: "evaluation", ContentHash: hash, Draft: canonical}
			ok = true
		}
	}
	if !ok {
		return nil, errors.New("capability preflight: content contract is missing")
	}
	if t.evidence != nil {
		rawEvidence, found := t.evidence.CapabilityPreflightEvidence(
			call.Ctx.TenantID, call.Ctx.SessionID, call.Ctx.TraceID,
		)
		if !found {
			return nil, errors.New("capability preflight: KnowRAG search was not executed in this trace")
		}
		return t.storeKnowledgeSnapshot(call, revision, rawEvidence)
	}
	if t.provider != nil && t.provider.artifacts != nil {
		rootRunID := strings.TrimSpace(call.Ctx.RootRunID)
		if rootRunID == "" {
			rootRunID = call.Ctx.RunID
		}
		rawEvidence, loadErr := t.provider.artifacts.Load(ctx, harness.Identity{
			TenantID: call.Ctx.TenantID, UserID: call.Ctx.UserID,
			SessionID: call.Ctx.SessionID, RunID: rootRunID,
		}, stepartifact.StepCapabilityEvidence)
		if loadErr != nil {
			if errors.Is(loadErr, stepartifact.ErrNotFound) {
				return nil, errors.New("capability preflight: API search was not executed for this Run")
			}
			return nil, fmt.Errorf("capability preflight: load API evidence: %w", loadErr)
		}
		return t.storeKnowledgeSnapshot(call, revision, rawEvidence)
	}
	items := make([]map[string]any, 0, len(revision.Contents)+len(revision.Actions))
	for _, item := range revision.Contents {
		items = append(items, map[string]any{
			"contract_item_id": item.ID, "item_type": "content", "status": "unknown",
			"knowledge": []any{}, "may_need_operator": false,
			"note": "能力知识 Provider 尚未配置，未伪造可用结论",
		})
	}
	for _, action := range revision.Actions {
		items = append(items, map[string]any{
			"contract_item_id": action.ID, "item_type": "action", "status": "unknown",
			"knowledge": []any{}, "may_need_operator": false,
			"note": "能力知识 Provider 尚未配置，未伪造可用结论",
		})
	}
	encoded, err := json.Marshal(map[string]any{"items": items, "overall": "needs_attention"})
	if err != nil {
		return nil, err
	}
	t.sessions.mu.Lock()
	t.sessions.preflight[key] = string(encoded)
	t.sessions.mu.Unlock()
	return &extension.FunctionResult{
		Data: encoded, MimeType: "application/json",
		Presentation: capabilityPreflightPresentation(items, "needs_attention"),
	}, nil
}

func (t *PreflightCapabilities) storeKnowledgeSnapshot(
	call extension.FunctionCall,
	revision contract.Revision,
	rawEvidence string,
) (*extension.FunctionResult, error) {
	providerUnknown := capabilityProviderUnknown(rawEvidence)
	_, count, err := capabilityEvidence(rawEvidence)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(revision.Contents)+len(revision.Actions))
	appendItem := func(id, itemType string) {
		status := "unknown"
		knowledge := []any{}
		note := "KnowRAG 返回了候选能力；语义是否满足该 Contract 项由 Style 模型基于原始需求判断"
		if providerUnknown {
			note = "能力知识服务本次未返回有效结果，当前能力状态未知；未伪造可用或不可用结论"
		} else if count == 0 {
			status = "unavailable"
			note = "KnowRAG 未检索到匹配的开发者能力"
		} else {
			knowledge = []any{map[string]any{
				"source": "agenui-knowrag", "tool": "search_developer_apis",
				"candidate_count": count,
			}}
		}
		items = append(items, map[string]any{
			"contract_item_id": id, "item_type": itemType, "status": status,
			"knowledge": knowledge, "may_need_operator": false, "note": note,
		})
	}
	for _, item := range revision.Contents {
		appendItem(item.ID, "content")
	}
	for _, action := range revision.Actions {
		appendItem(action.ID, "action")
	}
	overall := "needs_attention"
	if count == 0 && !providerUnknown {
		overall = "blocked"
	}
	encoded, err := json.Marshal(map[string]any{
		"items": items, "overall": overall,
	})
	if err != nil {
		return nil, err
	}
	key := sessionKey(call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID)
	t.sessions.mu.Lock()
	t.sessions.preflight[key] = string(encoded)
	t.sessions.mu.Unlock()
	return &extension.FunctionResult{
		Data: encoded, MimeType: "application/json",
		Presentation: capabilityPreflightPresentation(items, overall),
	}, nil
}

func contentContractPresentation(draft contract.Draft) *extension.ResultPresentation {
	cardType := "单体卡"
	if draft.Type == "list" {
		cardType = "列表卡"
	}
	content := make([]string, 0, len(draft.Contents))
	for _, item := range draft.Contents {
		content = append(content, item.Description)
	}
	details := []extension.ResultPresentationDetail{
		{Label: "卡片类型", Value: cardType},
		{Label: "内容字段", Value: strings.Join(content, "、")},
	}
	if len(draft.Actions) > 0 {
		actions := make([]string, 0, len(draft.Actions))
		for _, item := range draft.Actions {
			actions = append(actions, item.Description)
		}
		details = append(details, extension.ResultPresentationDetail{Label: "交互动作", Value: strings.Join(actions, "、")})
	}
	return &extension.ResultPresentation{
		Title: "卡片内容契约已生成", Summary: draft.Goal, Details: details,
	}
}

func capabilityPreflightPresentation(items []map[string]any, overall string) *extension.ResultPresentation {
	counts := map[string]int{"available": 0, "unknown": 0, "unavailable": 0}
	for _, item := range items {
		if status, _ := item["status"].(string); status != "" {
			counts[status]++
		}
	}
	summary := "能力状态需要进一步确认"
	if overall == "ready" {
		summary = "所需字段和动作均已找到候选能力"
	} else if overall == "blocked" {
		summary = "存在尚未找到候选能力的字段或动作"
	}
	return &extension.ResultPresentation{
		Title:   "字段和能力知识检索已完成",
		Summary: summary,
		Details: []extension.ResultPresentationDetail{
			{Label: "检索范围", Value: fmt.Sprintf("%d 项内容与动作", len(items))},
			{Label: "能力状态", Value: fmt.Sprintf("可用 %d · 待确认 %d · 未找到 %d", counts["available"], counts["unknown"], counts["unavailable"])},
		},
	}
}

func capabilityProviderUnknown(raw string) bool {
	var value map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &value) != nil {
		return false
	}
	status, _ := value["preflight_status"].(string)
	return status == "unknown"
}

func capabilityEvidence(raw string) (string, int, error) {
	normalized := strings.TrimSpace(raw)
	for range 4 {
		if normalized == "" || normalized[0] != '"' {
			break
		}
		var unwrapped string
		if json.Unmarshal([]byte(normalized), &unwrapped) != nil {
			break
		}
		normalized = strings.TrimSpace(unwrapped)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(normalized), &value); err != nil {
		return "", 0, errors.New("capability preflight: KnowRAG result is invalid")
	}
	count := 0
	for _, key := range []string{"results", "apis"} {
		if values, ok := value[key].([]any); ok && len(values) > count {
			count = len(values)
		}
	}
	if total, ok := value["total"].(float64); ok && int(total) > count {
		count = int(total)
	}
	return strings.ToLower(normalized), count, nil
}

func sessionKey(tenantID, userID, sessionID string) string {
	return tenantID + "\x00" + userID + "\x00" + sessionID
}

type ResolveDesignEdit struct {
	sessions *designingSessions
	provider *DesigningProvider
}

func (*ResolveDesignEdit) Name() string { return ResolveDesignEditName }

func (t *ResolveDesignEdit) Invoke(ctx context.Context, call extension.FunctionCall) (*extension.FunctionResult, error) {
	if call.Ctx.AgentID != agenuiextensions.MainAgent || call.Name != ResolveDesignEditName {
		return nil, errors.New("resolve design edit: caller is not allowed")
	}
	var input struct {
		Query    string               `json:"query"`
		TargetID string               `json:"target_id"`
		Cursor   string               `json:"cursor"`
		PageSize int                  `json:"page_size"`
		Filters  edit.CandidateFilter `json:"filters"`
	}
	if err := json.Unmarshal(call.Arguments, &input); err != nil || strings.TrimSpace(input.Query) == "" {
		return nil, errors.New("resolve design edit: query is required")
	}
	key := sessionKey(call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID)
	t.sessions.mu.RLock()
	design := t.sessions.designs[key]
	t.sessions.mu.RUnlock()
	if design == "" {
		if t.provider == nil {
			return nil, errors.New("resolve design edit: persistent recovery is unavailable")
		}
		if restoreErr := t.provider.restoreSession(ctx, call.Ctx); restoreErr != nil {
			if errors.Is(restoreErr, stepartifact.ErrNotFound) {
				encoded, err := json.Marshal(map[string]any{
					"status":              "no_base_design",
					"query":               strings.TrimSpace(input.Query),
					"reason":              "no completed design is available for this session",
					"next_action":         "submit_content_contract",
					"route":               "new_generation",
					"continue_generation": true,
					"instruction":         "Treat the accumulated requirement and visual input as a new card: submit the content contract, then delegate to agenui_style. Do not stop with a routing explanation.",
				})
				if err != nil {
					return nil, err
				}
				return &extension.FunctionResult{Data: encoded, MimeType: "application/json"}, nil
			}
			return nil, fmt.Errorf("resolve design edit: restore current design: %w", restoreErr)
		}
		t.sessions.mu.RLock()
		design = t.sessions.designs[key]
		t.sessions.mu.RUnlock()
		if design == "" {
			return nil, errors.New("resolve design edit: current design is missing")
		}
	}
	sum := sha256.Sum256([]byte(design))
	messages, fields, actions, partsErr := designIndexParts(design)
	if partsErr != nil {
		return nil, partsErr
	}
	stylePaths := t.provider.editableStylePaths()
	index, err := edit.BuildIndex(
		"design_"+hex.EncodeToString(sum[:8]),
		messages, fields, actions, stylePaths...,
	)
	if err != nil {
		return nil, err
	}
	t.sessions.mu.Lock()
	t.sessions.pendingEdits[key] = true
	t.sessions.mu.Unlock()
	response := map[string]any{
		"query": strings.TrimSpace(input.Query), "design_revision": index.Revision,
	}
	if strings.TrimSpace(input.TargetID) == "" {
		page, pageErr := edit.PageCandidates(index, input.Filters, input.Cursor, input.PageSize)
		if pageErr != nil {
			status, code := "invalid_cursor", "CANDIDATE_CURSOR_INVALID"
			if errors.Is(pageErr, edit.ErrCandidateCursorStale) {
				status, code = "stale_cursor", "CANDIDATE_CURSOR_STALE"
			}
			response["status"] = status
			response["retryable"] = true
			response["error"] = map[string]any{"code": code, "message": pageErr.Error()}
			response["next_action"] = "restart candidate discovery without cursor"
		} else {
			response["status"] = "candidates"
			addCandidatePage(response, page)
		}
	} else {
		matches := edit.ExactMatches(index, input.TargetID)
		switch len(matches) {
		case 0:
			response["status"] = "not_found"
			response["retryable"] = true
			response["error"] = map[string]any{
				"code": "TARGET_NOT_FOUND", "message": fmt.Sprintf("target_id %q does not exist in the current design", input.TargetID),
			}
			response["next_action"] = "select an exact element_id or component_id from candidates"
			page, _ := edit.PageCandidates(index, input.Filters, "", input.PageSize)
			addCandidatePage(response, page)
		case 1:
			response["status"] = "resolved"
			response["target"] = matches[0]
		default:
			response["status"] = "ambiguous"
			response["retryable"] = true
			response["error"] = map[string]any{
				"code": "TARGET_AMBIGUOUS", "message": fmt.Sprintf("target_id %q matches multiple current design elements", input.TargetID),
			}
			response["next_action"] = "select one unambiguous element_id or component_id"
			response["candidates"] = edit.SummarizeCandidates(matches)
			response["matched_count"] = len(matches)
			response["has_more"] = false
		}
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}
	if response["status"] == "resolved" {
		t.sessions.mu.Lock()
		t.sessions.resolved[key] = string(encoded)
		delete(t.sessions.edits, key)
		t.sessions.mu.Unlock()
	}
	return &extension.FunctionResult{Data: encoded, MimeType: "application/json"}, nil
}

func addCandidatePage(response map[string]any, page edit.CandidatePage) {
	response["candidates"] = page.Candidates
	response["matched_count"] = page.MatchedCount
	response["has_more"] = page.HasMore
	if page.NextCursor != "" {
		response["next_cursor"] = page.NextCursor
	}
}

func designIndexParts(raw string) (messages, fields, actions string, err error) {
	messages, err = workspace.ArtifactMessagesJSON(raw)
	if err != nil {
		return "", "", "", err
	}
	fields, actions, err = workspace.ArtifactSlotsJSON(raw)
	return messages, fields, actions, err
}

func (p *DesigningProvider) restoreSession(ctx context.Context, invocation extension.Context) error {
	if p == nil || p.artifacts == nil {
		return errors.New("designing tools: persistent recovery is unavailable")
	}
	identity := harness.Identity{
		TenantID: invocation.TenantID, UserID: invocation.UserID,
		SessionID: invocation.SessionID,
	}
	runID, err := p.artifacts.LatestRunID(ctx, identity, stepartifact.StepDesign)
	if err != nil {
		return err
	}
	identity.RunID = runID
	contractJSON, err := p.artifacts.Load(ctx, identity, stepartifact.StepContract)
	if err != nil {
		return err
	}
	design, err := p.artifacts.Load(ctx, identity, stepartifact.StepDesign)
	if err != nil {
		return err
	}
	preflight, err := p.artifacts.Load(ctx, identity, stepartifact.StepPreflight)
	if err != nil && !errors.Is(err, stepartifact.ErrNotFound) {
		return err
	}
	var revision contract.Revision
	if err := json.Unmarshal([]byte(contractJSON), &revision); err != nil {
		return err
	}
	if _, err := contract.Canonicalize(revision.Draft); err != nil {
		return err
	}
	key := sessionKey(invocation.TenantID, invocation.UserID, invocation.SessionID)
	p.sessions.mu.Lock()
	p.sessions.contracts[key] = revision
	p.sessions.designs[key] = design
	if strings.TrimSpace(preflight) != "" {
		p.sessions.preflight[key] = preflight
	} else {
		delete(p.sessions.preflight, key)
	}
	delete(p.sessions.resolved, key)
	delete(p.sessions.edits, key)
	delete(p.sessions.pendingEdits, key)
	delete(p.sessions.bindingEdits, key)
	p.sessions.mu.Unlock()
	return nil
}
