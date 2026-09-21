package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentregistry"
	"github.com/AGenUI/agenui-studio/harness/internal/authcontext"
	"github.com/AGenUI/agenui-studio/harness/internal/httptooldef"
	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/modeladmin"
	"github.com/AGenUI/agenui-studio/harness/internal/skill"
	"github.com/AGenUI/agenui-studio/harness/internal/toolconfig"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

const managementScope = "agent.config.read"

func managementPrincipal(w http.ResponseWriter, r *http.Request, write bool) (authcontext.Principal, bool) {
	principal, ok := authcontext.FromContext(r.Context())
	required := managementScope
	if write {
		required = "agent.config.write"
	}
	if !ok || !principal.HasScope(required) {
		writeError(w, http.StatusForbidden, "MANAGEMENT_FORBIDDEN", "agent configuration management permission is required")
		return authcontext.Principal{}, false
	}
	return principal, true
}

func requireManagementService(w http.ResponseWriter, service any) bool {
	if isNilManagementService(service) {
		writeError(w, http.StatusNotImplemented, "MANAGEMENT_UNAVAILABLE", "management storage is not configured")
		return false
	}
	return true
}

func requireMCPOAuthService(w http.ResponseWriter, service any) bool {
	if isNilManagementService(service) {
		writeError(w, http.StatusNotImplemented, "MCP_OAUTH_UNAVAILABLE", "mcp oauth authorization manager is not configured")
		return false
	}
	return true
}

func isNilManagementService(service any) bool {
	if service == nil {
		return true
	}
	value := reflect.ValueOf(service)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (d *Deps) handleListAgentConfigs(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, false)
	if !ok || !requireManagementService(w, d.AgentConfigControl) {
		return
	}
	drafts, err := d.AgentConfigControl.ListDrafts(r.Context(), p.TenantID)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	managedIDs := make(map[string]struct{}, len(drafts))
	for _, draft := range drafts {
		managedIDs[draft.AgentID] = struct{}{}
	}
	var staticConfigs []agentregistry.AgentConfig
	if d.AgentConfigSource != nil && includeStaticAgentConfigs(r) {
		configs, listErr := d.AgentConfigSource.ListAgentConfigs(r.Context())
		if listErr != nil {
			writeManagementError(w, listErr)
			return
		}
		for _, config := range configs {
			if _, managed := managedIDs[config.AgentID]; !managed {
				claimed, claimedErr := d.AgentConfigControl.AgentIDManaged(r.Context(), config.AgentID)
				if claimedErr != nil {
					writeManagementError(w, claimedErr)
					return
				}
				if claimed {
					continue
				}
				staticConfigs = append(staticConfigs, config)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": drafts, "static_items": staticConfigs,
		"tenant_id": p.TenantID, "environment": d.ConfigEnvironment,
		"tenant_switch_enabled": d.LocalTenantSwitch,
	})
}

func includeStaticAgentConfigs(r *http.Request) bool {
	value := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("include_static")))
	return value == "1" || value == "true"
}

func (d *Deps) handleGetAgentConfigDraft(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, false)
	if !ok || !requireManagementService(w, d.AgentConfigControl) {
		return
	}
	draft, err := d.AgentConfigControl.GetDraft(r.Context(), p.TenantID, r.PathValue("id"))
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, draft)
}

func (d *Deps) handleSaveAgentConfigDraft(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, true)
	if !ok || !requireManagementService(w, d.AgentConfigControl) {
		return
	}
	var body struct {
		Config           agentregistry.AgentConfig `json:"config"`
		ExpectedRevision int64                     `json:"expected_revision"`
	}
	if err := decodeManagementJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	if body.Config.AgentID != r.PathValue("id") {
		writeError(w, http.StatusBadRequest, "AGENT_ID_MISMATCH", "path and config agent_id must match")
		return
	}
	draft, err := d.AgentConfigControl.SaveDraft(r.Context(), agentregistry.SaveAgentConfigDraftRequest{
		TenantID: p.TenantID, Config: body.Config, ExpectedRevision: body.ExpectedRevision, Actor: p.UserID,
	})
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, draft)
}

func (d *Deps) handleListAgentConfigVersions(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, false)
	if !ok || !requireManagementService(w, d.AgentConfigControl) {
		return
	}
	versions, err := d.AgentConfigControl.ListVersions(r.Context(), p.TenantID, r.PathValue("id"))
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": versions})
}

func (d *Deps) handleGetAgentConfigRelease(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, false)
	if !ok || !requireManagementService(w, d.AgentConfigControl) {
		return
	}
	release, err := d.AgentConfigControl.GetRelease(r.Context(), p.TenantID, d.ConfigEnvironment, r.PathValue("id"))
	if errors.Is(err, agentregistry.ErrAgentConfigControlNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"release": nil})
		return
	}
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"release": release})
}

func (d *Deps) handlePublishAgentConfig(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, true)
	if !ok || !requireManagementService(w, d.AgentConfigControl) {
		return
	}
	var body struct {
		Environment             agentregistry.ConfigEnvironment `json:"environment"`
		ExpectedDraftRevision   int64                           `json:"expected_draft_revision"`
		ExpectedReleaseRevision int64                           `json:"expected_release_revision"`
		Reason                  string                          `json:"reason"`
	}
	if err := decodeManagementJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	if !d.matchesConfigEnvironment(body.Environment) {
		writeError(w, http.StatusBadRequest, "ENVIRONMENT_MISMATCH", "release environment must match the running service")
		return
	}
	body.Environment = d.ConfigEnvironment
	draft, err := d.AgentConfigControl.GetDraft(r.Context(), p.TenantID, r.PathValue("id"))
	if err != nil {
		writeManagementError(w, err)
		return
	}
	if draft.Revision != body.ExpectedDraftRevision {
		writeManagementError(w, agentregistry.ErrAgentConfigControlConflict)
		return
	}
	if err := d.ensureSubAgentReleases(r.Context(), p.TenantID, body.Environment, p.UserID, draft.Config); err != nil {
		writeManagementError(w, err)
		return
	}
	version, release, err := d.AgentConfigControl.PublishDraft(r.Context(), agentregistry.PublishAgentConfigDraftRequest{
		TenantID: p.TenantID, AgentID: r.PathValue("id"), Environment: body.Environment,
		ExpectedDraftRevision: body.ExpectedDraftRevision, ExpectedReleaseRevision: body.ExpectedReleaseRevision,
		Actor: p.UserID, Reason: body.Reason,
	})
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": version, "release": release})
}

func (d *Deps) ensureSubAgentReleases(ctx context.Context, tenantID string, environment agentregistry.ConfigEnvironment, actor string, root agentregistry.AgentConfig) error {
	if d.AgentConfigControl == nil || len(root.SubAgents) == 0 {
		return nil
	}
	staticConfigs, err := d.staticAgentConfigs(ctx)
	if err != nil {
		return err
	}
	return d.ensureSubAgentReleasesFromConfig(ctx, tenantID, environment, actor, root, staticConfigs, map[string]bool{root.AgentID: true})
}

func (d *Deps) ensureSubAgentReleasesFromConfig(ctx context.Context, tenantID string, environment agentregistry.ConfigEnvironment, actor string, cfg agentregistry.AgentConfig, staticConfigs map[string]agentregistry.AgentConfig, stack map[string]bool) error {
	for _, childID := range cfg.SubAgents {
		if childID == "" {
			continue
		}
		if stack[childID] {
			return fmt.Errorf("%w: sub_agents contains a cycle at %s", agentregistry.ErrAgentConfigControlInvalid, childID)
		}
		if _, err := d.AgentConfigControl.GetRelease(ctx, tenantID, environment, childID); err == nil {
			continue
		} else if !errors.Is(err, agentregistry.ErrAgentConfigControlNotFound) {
			return err
		}

		draft, err := d.AgentConfigControl.GetDraft(ctx, tenantID, childID)
		if errors.Is(err, agentregistry.ErrAgentConfigControlNotFound) {
			staticConfig, ok := staticConfigs[childID]
			if !ok {
				return fmt.Errorf("%w: sub-agent %s has no release, draft, or static config to publish", agentregistry.ErrAgentConfigControlInvalid, childID)
			}
			draft, err = d.AgentConfigControl.SaveDraft(ctx, agentregistry.SaveAgentConfigDraftRequest{
				TenantID: tenantID, Config: staticConfig, ExpectedRevision: 0, Actor: actor,
			})
		}
		if err != nil {
			return err
		}

		stack[childID] = true
		if err := d.ensureSubAgentReleasesFromConfig(ctx, tenantID, environment, actor, draft.Config, staticConfigs, stack); err != nil {
			return err
		}
		delete(stack, childID)

		if _, _, err := d.AgentConfigControl.PublishDraft(ctx, agentregistry.PublishAgentConfigDraftRequest{
			TenantID: tenantID, AgentID: childID, Environment: environment,
			ExpectedDraftRevision: draft.Revision, ExpectedReleaseRevision: 0,
			Actor: actor, Reason: "auto publish sub-agent for parent release",
		}); err != nil {
			return err
		}
	}
	return nil
}

func (d *Deps) staticAgentConfigs(ctx context.Context) (map[string]agentregistry.AgentConfig, error) {
	if d.AgentConfigSource == nil {
		return nil, nil
	}
	configs, err := d.AgentConfigSource.ListAgentConfigs(ctx)
	if err != nil {
		return nil, err
	}
	index := make(map[string]agentregistry.AgentConfig, len(configs))
	for _, cfg := range configs {
		if cfg.AgentID != "" {
			index[cfg.AgentID] = cfg
		}
	}
	return index, nil
}

func (d *Deps) handlePromoteAgentConfig(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, true)
	if !ok || !requireManagementService(w, d.AgentConfigControl) {
		return
	}
	var body struct {
		Version                 string                          `json:"version"`
		Environment             agentregistry.ConfigEnvironment `json:"environment"`
		ExpectedReleaseRevision int64                           `json:"expected_release_revision"`
		Reason                  string                          `json:"reason"`
	}
	if err := decodeManagementJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	if !d.matchesConfigEnvironment(body.Environment) {
		writeError(w, http.StatusBadRequest, "ENVIRONMENT_MISMATCH", "release environment must match the running service")
		return
	}
	body.Environment = d.ConfigEnvironment
	release, err := d.AgentConfigControl.PromoteVersion(r.Context(), agentregistry.PromoteAgentConfigVersionRequest{
		TenantID: p.TenantID, AgentID: r.PathValue("id"), Version: body.Version, Environment: body.Environment,
		ExpectedReleaseRevision: body.ExpectedReleaseRevision, Actor: p.UserID, Reason: body.Reason,
	})
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, release)
}

func (d *Deps) handleListPromptVersions(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, false)
	if !ok || !requireManagementService(w, d.Prompts) {
		return
	}
	ref := strings.TrimSpace(r.URL.Query().Get("prompt_ref"))
	if !promptRefVisibleToTenant(ref, p.TenantID, r.PathValue("id")) {
		writeError(w, http.StatusForbidden, "PROMPT_FORBIDDEN", "prompt reference does not belong to this tenant and agent")
		return
	}
	versions, err := d.Prompts.ListVersions(r.Context(), ref)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	latest := ""
	if len(versions) > 0 {
		latest = versions[0].Version
	}
	writeJSON(w, http.StatusOK, map[string]any{"prompt_ref": ref, "latest_version": latest, "items": versions})
}

func (d *Deps) handleCreatePromptVersion(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, true)
	if !ok || !requireManagementService(w, d.Prompts) {
		return
	}
	var body struct {
		PromptRef string   `json:"prompt_ref"`
		Version   string   `json:"version"`
		Content   string   `json:"content"`
		Variables []string `json:"variables"`
	}
	if err := decodeManagementJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	managedRef := tenantManagedPromptRef(p.TenantID, r.PathValue("id"))
	prompt := agentregistry.PromptVersion{
		Ref: managedRef, Version: body.Version, Content: body.Content,
		Variables: body.Variables, CreatedBy: p.UserID,
	}
	if err := d.Prompts.Create(r.Context(), prompt); err != nil {
		writeManagementError(w, err)
		return
	}
	stored, err := d.Prompts.Get(r.Context(), agentregistry.PromptKey{Ref: managedRef, Version: body.Version})
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"prompt_ref": managedRef, "source_prompt_ref": strings.TrimSpace(body.PromptRef), "prompt": stored,
	})
}

func tenantManagedPromptRef(tenantID, agentID string) string {
	tenantDigest := sha256.Sum256([]byte(strings.TrimSpace(tenantID)))
	agentDigest := sha256.Sum256([]byte(strings.TrimSpace(agentID)))
	return fmt.Sprintf("prompt://tenant-%x/agent-%x/system", tenantDigest[:8], agentDigest[:8])
}

func promptRefVisibleToTenant(ref, tenantID, agentID string) bool {
	parsed, err := url.Parse(ref)
	if err != nil || parsed.Scheme != "prompt" || parsed.Host == "" {
		return false
	}
	if !strings.HasPrefix(parsed.Host, "tenant-") {
		return true
	}
	return ref == tenantManagedPromptRef(tenantID, agentID)
}

func (d *Deps) matchesConfigEnvironment(requested agentregistry.ConfigEnvironment) bool {
	return d.ConfigEnvironment != "" && (requested == "" || requested == d.ConfigEnvironment)
}

func (d *Deps) handleListAgentRelations(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, false)
	if !ok || !requireManagementService(w, d.AgentConfigControl) {
		return
	}
	drafts, err := d.AgentConfigControl.ListDrafts(r.Context(), p.TenantID)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	type edge struct {
		Parent string `json:"parent"`
		Child  string `json:"child"`
	}
	var edges []edge
	for _, draft := range drafts {
		for _, child := range draft.Config.SubAgents {
			edges = append(edges, edge{Parent: draft.AgentID, Child: child})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": edges})
}

func (d *Deps) handleListMCPServers(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, false)
	if !ok || !requireManagementService(w, d.ManagedMCP) {
		return
	}
	items, err := d.ManagedMCP.List(r.Context(), p.TenantID)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (d *Deps) handleCreateMCPServer(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, true)
	if !ok || !requireManagementService(w, d.ManagedMCP) {
		return
	}
	var body struct {
		Definition       mcp.ServerDefinition `json:"definition"`
		ExpectedRevision int64                `json:"expected_revision"`
	}
	if err := decodeManagementJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	if body.ExpectedRevision != 0 {
		writeError(w, http.StatusBadRequest, "INVALID_MCP_REVISION", "new MCP server expected_revision must be 0")
		return
	}
	items, err := d.ManagedMCP.List(r.Context(), p.TenantID)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	definition := body.Definition
	definition.Version = "v1"
	for attempts := 0; attempts < 16; attempts++ {
		definition.ID = mcp.NextGeneratedServerID(items)
		server, saveErr := d.ManagedMCP.Save(r.Context(), p.TenantID, p.UserID, definition, 0)
		if saveErr == nil {
			writeJSON(w, http.StatusOK, server)
			return
		}
		if !errors.Is(saveErr, mcp.ErrManagedConflict) {
			writeManagementError(w, saveErr)
			return
		}
		items, err = d.ManagedMCP.List(r.Context(), p.TenantID)
		if err != nil {
			writeManagementError(w, err)
			return
		}
	}
	writeManagementError(w, mcp.ErrManagedConflict)
}

func (d *Deps) handleSaveMCPServer(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, true)
	if !ok || !requireManagementService(w, d.ManagedMCP) {
		return
	}
	var body struct {
		Definition       mcp.ServerDefinition `json:"definition"`
		ExpectedRevision int64                `json:"expected_revision"`
	}
	if err := decodeManagementJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	serverID := r.PathValue("id")
	body.Definition.ID = serverID
	if body.ExpectedRevision > 0 {
		current, err := d.ManagedMCP.GetManaged(r.Context(), p.TenantID, serverID)
		if err != nil {
			writeManagementError(w, err)
			return
		}
		body.Definition.Version = mcp.NextManagedVersion(current.Definition.Version)
	} else {
		body.Definition.Version = "v1"
	}
	server, err := d.ManagedMCP.Save(r.Context(), p.TenantID, p.UserID, body.Definition, body.ExpectedRevision)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, server)
}

func (d *Deps) handleListModelProviders(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, false)
	if !ok || !requireManagementService(w, d.ManagedModelProviders) {
		return
	}
	items, err := d.ManagedModelProviders.List(r.Context(), p.TenantID)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (d *Deps) handleSaveModelProvider(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, true)
	if !ok || !requireManagementService(w, d.ManagedModelProviders) {
		return
	}
	var body struct {
		Definition       modeladmin.ProviderDefinition `json:"definition"`
		ExpectedRevision int64                         `json:"expected_revision"`
	}
	if err := decodeManagementJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	if body.Definition.ID != r.PathValue("id") {
		writeError(w, http.StatusBadRequest, "MODEL_PROVIDER_ID_MISMATCH", "path and definition id must match")
		return
	}
	body.Definition.TenantID = p.TenantID
	body.Definition.Scope = modeladmin.ScopeTenant
	// Dry-run before persisting so a broken config never reaches live traffic
	// (immediate-effect resolver). Skipped when no validator is wired.
	if d.ValidateModelProvider != nil {
		if err := d.ValidateModelProvider(r.Context(), body.Definition); err != nil {
			writeError(w, http.StatusBadRequest, "MODEL_PROVIDER_INVALID", strings.TrimSpace(err.Error()))
			return
		}
	}
	provider, err := d.ManagedModelProviders.Save(r.Context(), p.TenantID, p.UserID, body.Definition, body.ExpectedRevision)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, provider)
}

func (d *Deps) handleDeleteModelProvider(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, true)
	if !ok || !requireManagementService(w, d.ManagedModelProviders) {
		return
	}
	id := r.PathValue("id")
	current, err := d.ManagedModelProviders.GetManaged(r.Context(), p.TenantID, id)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	if err := d.ManagedModelProviders.Delete(r.Context(), p.TenantID, id, current.Revision); err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
}

func (d *Deps) handleDebugMCPTools(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, true)
	if !ok || !requireManagementService(w, d.ManagedMCP) || !requireManagementService(w, d.MCPDebug) {
		return
	}
	var body struct{}
	if err := decodeManagementJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_DEBUG_REQUEST", err.Error())
		return
	}
	serverID := r.PathValue("id")
	if _, err := d.ManagedMCP.GetManaged(r.Context(), p.TenantID, serverID); err != nil {
		writeManagementError(w, err)
		return
	}
	start := time.Now()
	snapshot, err := d.MCPDebug.ResolveSnapshot(r.Context(), mcp.ManagementDebugPrincipal(p.TenantID, p.UserID), serverID)
	if err != nil {
		if recordErr := d.appendMCPDebugRecord(r.Context(), p, mcp.DebugRecord{
			ServerID: serverID, Operation: mcp.DebugOperationListTools, RequestJSON: safeDebugJSON(map[string]any{"operation": mcp.DebugOperationListTools}),
			ResponseJSON: safeDebugJSON(map[string]any{"error": strings.TrimSpace(err.Error())}), ErrorCode: managementErrorCode(err), ErrorMessage: strings.TrimSpace(err.Error()), LatencyMS: time.Since(start).Milliseconds(),
		}); recordErr != nil {
			writeManagementError(w, recordErr)
			return
		}
		writeManagementError(w, err)
		return
	}
	if err := d.appendMCPDebugRecord(r.Context(), p, mcp.DebugRecord{
		ServerID: serverID, Operation: mcp.DebugOperationListTools, SnapshotID: snapshot.ID,
		RequestJSON:  safeDebugJSON(map[string]any{"operation": mcp.DebugOperationListTools}),
		ResponseJSON: safeDebugJSON(snapshot), LatencyMS: time.Since(start).Milliseconds(),
	}); err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (d *Deps) handleDebugMCPCall(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, true)
	if !ok || !requireManagementService(w, d.ManagedMCP) || !requireManagementService(w, d.MCPDebug) {
		return
	}
	var body struct {
		ToolName    string          `json:"tool_name"`
		Arguments   json.RawMessage `json:"arguments"`
		ApprovalRef string          `json:"approval_ref"`
	}
	if err := decodeManagementJSON(r, &body); err != nil || body.ToolName == "" {
		writeError(w, http.StatusBadRequest, "INVALID_DEBUG_REQUEST", "tool_name is required and agent_id is not accepted")
		return
	}
	serverID := r.PathValue("id")
	if _, err := d.ManagedMCP.GetManaged(r.Context(), p.TenantID, serverID); err != nil {
		writeManagementError(w, err)
		return
	}
	start := time.Now()
	principal := mcp.ManagementDebugPrincipal(p.TenantID, p.UserID)
	snapshot, err := d.MCPDebug.ResolveSnapshot(r.Context(), principal, serverID)
	if err != nil {
		if recordErr := d.appendMCPDebugRecord(r.Context(), p, mcp.DebugRecord{
			ServerID: serverID, Operation: mcp.DebugOperationCallTool, ToolName: body.ToolName,
			RequestJSON:  mcpCallDebugRequest(body.ToolName, body.Arguments),
			ResponseJSON: safeDebugJSON(map[string]any{"error": strings.TrimSpace(err.Error())}), ErrorCode: managementErrorCode(err), ErrorMessage: strings.TrimSpace(err.Error()), LatencyMS: time.Since(start).Milliseconds(),
		}); recordErr != nil {
			writeManagementError(w, recordErr)
			return
		}
		writeManagementError(w, err)
		return
	}
	result, err := d.MCPDebug.CallTool(r.Context(), mcp.ToolCallRequest{Principal: principal, ServerID: serverID, SnapshotID: snapshot.ID, ToolName: body.ToolName, ToolCallID: "management-debug", Arguments: body.Arguments, ApprovalRef: body.ApprovalRef})
	if err != nil {
		if recordErr := d.appendMCPDebugRecord(r.Context(), p, mcp.DebugRecord{
			ServerID: serverID, Operation: mcp.DebugOperationCallTool, ToolName: body.ToolName, SnapshotID: snapshot.ID,
			RequestJSON:  mcpCallDebugRequest(body.ToolName, body.Arguments),
			ResponseJSON: safeDebugJSON(map[string]any{"error": strings.TrimSpace(err.Error())}), ErrorCode: managementErrorCode(err), ErrorMessage: strings.TrimSpace(err.Error()), LatencyMS: time.Since(start).Milliseconds(),
		}); recordErr != nil {
			writeManagementError(w, recordErr)
			return
		}
		writeManagementError(w, err)
		return
	}
	if err := d.appendMCPDebugRecord(r.Context(), p, mcp.DebugRecord{
		ServerID: serverID, Operation: mcp.DebugOperationCallTool, ToolName: body.ToolName, SnapshotID: snapshot.ID,
		RequestJSON:  mcpCallDebugRequest(body.ToolName, body.Arguments),
		ResponseJSON: safeDebugJSON(map[string]any{"content": string(result.Content), "artifact_ref": result.ArtifactRef, "is_error": result.IsError, "warnings": result.Warnings}),
		LatencyMS:    time.Since(start).Milliseconds(),
	}); err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (d *Deps) handleListMCPDebugHistory(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, true)
	if !ok || !requireManagementService(w, d.ManagedMCP) {
		return
	}
	serverID := r.PathValue("id")
	if _, err := d.ManagedMCP.GetManaged(r.Context(), p.TenantID, serverID); err != nil {
		writeManagementError(w, err)
		return
	}
	limit := 50
	if value := strings.TrimSpace(r.URL.Query().Get("limit")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 || parsed > 100 {
			writeError(w, http.StatusBadRequest, "INVALID_LIMIT", "limit must be between 1 and 100")
			return
		}
		limit = parsed
	}
	records, err := d.ManagedMCP.ListDebugRecords(r.Context(), p.TenantID, serverID, limit)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": records})
}

func (d *Deps) handleStartMCPOAuth(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, true)
	if !ok || !requireManagementService(w, d.ManagedMCP) || !requireMCPOAuthService(w, d.MCPOAuth) {
		return
	}
	var body struct{}
	if err := decodeManagementJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_OAUTH_REQUEST", err.Error())
		return
	}
	server, err := d.ManagedMCP.GetManaged(r.Context(), p.TenantID, r.PathValue("id"))
	if err != nil {
		writeManagementError(w, err)
		return
	}
	started, err := d.MCPOAuth.Start(r.Context(), mcp.ManagementDebugPrincipal(p.TenantID, p.UserID), server.Definition)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, started)
}

func (d *Deps) handleMCPOAuthStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, false)
	if !ok || !requireManagementService(w, d.ManagedMCP) || !requireMCPOAuthService(w, d.MCPOAuth) {
		return
	}
	server, err := d.ManagedMCP.GetManaged(r.Context(), p.TenantID, r.PathValue("id"))
	if err != nil {
		writeManagementError(w, err)
		return
	}
	status, err := d.MCPOAuth.Status(r.Context(), mcp.ManagementDebugPrincipal(p.TenantID, p.UserID), server.Definition)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (d *Deps) handleRevokeMCPOAuth(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, true)
	if !ok || !requireManagementService(w, d.ManagedMCP) || !requireMCPOAuthService(w, d.MCPOAuth) {
		return
	}
	server, err := d.ManagedMCP.GetManaged(r.Context(), p.TenantID, r.PathValue("id"))
	if err != nil {
		writeManagementError(w, err)
		return
	}
	if err := d.MCPOAuth.Revoke(r.Context(), mcp.ManagementDebugPrincipal(p.TenantID, p.UserID), server.Definition); err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"server_id": server.Definition.ID, "status": "revoked"})
}

func (d *Deps) handleMCPOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if !requireMCPOAuthService(w, d.MCPOAuth) {
		return
	}
	query := r.URL.Query()
	completed, err := d.MCPOAuth.Complete(r.Context(), query.Get("state"), query.Get("code"), query.Get("error"), query.Get("error_description"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "MCP_OAUTH_CALLBACK_FAILED", "MCP OAuth authorization could not be completed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"server_id": completed.ServerID, "status": completed.Status})
}

func (d *Deps) handleListSkills(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, false)
	if !ok || !requireManagementService(w, d.ManagedSkills) {
		return
	}
	items, err := d.ManagedSkills.List(r.Context(), skill.Principal{TenantID: p.TenantID, AgentID: "management"})
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (d *Deps) handlePreviewSkillFile(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, false)
	if !ok || !requireManagementService(w, d.ManagedSkills) {
		return
	}
	preview, err := d.ManagedSkills.PreviewFile(r.Context(), skill.Principal{
		TenantID: p.TenantID,
		AgentID:  "management",
	}, skill.Ref{ID: r.PathValue("id"), Version: r.PathValue("version")}, r.URL.Query().Get("path"))
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

func (d *Deps) handleListSkillFiles(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, false)
	if !ok || !requireManagementService(w, d.ManagedSkills) {
		return
	}
	files, err := d.ManagedSkills.ListFiles(r.Context(), skill.Principal{
		TenantID: p.TenantID,
		AgentID:  "management",
	}, skill.Ref{ID: r.PathValue("id"), Version: r.PathValue("version")})
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, files)
}

// handleListTools lists the globally registered tool definitions so the agent
// config UI can offer a picker instead of a free-text field. The catalog itself
// is global, but each tool is annotated with whether it is configurable (it
// declares a config_schema) and, for the caller's tenant, its current stored
// config and revision. Credentials never appear here — a tool config only ever
// references environment-variable names. Only the read management scope is
// required.
func (d *Deps) handleListTools(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, false)
	if !ok || !requireManagementService(w, d.ToolCatalog) {
		return
	}
	configs := map[string]toolconfig.ManagedToolConfig{}
	if d.ToolConfig != nil {
		list, err := d.ToolConfig.List(r.Context(), p.TenantID)
		if err != nil {
			writeManagementError(w, err)
			return
		}
		for _, managed := range list {
			configs[managed.Definition.ToolName] = managed
		}
	}
	defs := d.ToolCatalog.List(r.Context())
	items := make([]map[string]any, 0, len(defs))
	for _, def := range defs {
		configurable := len(def.ConfigSchema) > 0
		item := map[string]any{
			"definition": map[string]any{
				"name":         def.Name,
				"version":      def.Version,
				"display_name": def.DisplayName,
				"description":  def.Description,
				"risk_level":   string(def.RiskLevel),
			},
			"configurable": configurable,
		}
		if configurable {
			item["config_schema"] = json.RawMessage(def.ConfigSchema)
		}
		if managed, exists := configs[def.Name]; exists {
			item["config"] = json.RawMessage(managed.Definition.Config)
			item["revision"] = managed.Revision
			item["updated_by"] = managed.UpdatedBy
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "tenant_id": p.TenantID})
}

// handleSaveToolConfig persists a tenant's configuration for a configurable
// tool. The tool must exist in the catalog and declare a config_schema; the
// submitted config is validated against that schema before persistence. Writes
// use optimistic concurrency via expected_revision.
func (d *Deps) handleSaveToolConfig(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, true)
	if !ok || !requireManagementService(w, d.ToolConfig) || !requireManagementService(w, d.ToolCatalog) {
		return
	}
	name := r.PathValue("name")
	var body struct {
		Config           json.RawMessage `json:"config"`
		ExpectedRevision int64           `json:"expected_revision"`
	}
	if err := decodeManagementJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	var schema json.RawMessage
	found := false
	for _, def := range d.ToolCatalog.List(r.Context()) {
		if def.Name == name {
			found = true
			schema = def.ConfigSchema
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "TOOL_NOT_FOUND", "unknown tool: "+name)
		return
	}
	if len(schema) == 0 {
		writeError(w, http.StatusBadRequest, "TOOL_NOT_CONFIGURABLE", "tool does not declare a config schema: "+name)
		return
	}
	if len(body.Config) == 0 {
		writeError(w, http.StatusBadRequest, "INVALID_CONFIGURATION", "config is required")
		return
	}
	if err := toolgateway.NewJSONSchemaValidator(16).Validate(r.Context(), schema, body.Config); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_CONFIGURATION", strings.TrimSpace(err.Error()))
		return
	}
	saved, err := d.ToolConfig.Save(r.Context(), p.TenantID, p.UserID, toolconfig.Definition{ToolName: name, Config: body.Config}, body.ExpectedRevision)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// handleDeleteToolConfig removes a tenant's configuration for a tool.
func (d *Deps) handleDeleteToolConfig(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, true)
	if !ok || !requireManagementService(w, d.ToolConfig) {
		return
	}
	name := r.PathValue("name")
	current, err := d.ToolConfig.GetManaged(r.Context(), p.TenantID, name)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	if err := d.ToolConfig.Delete(r.Context(), p.TenantID, name, current.Revision); err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "name": name})
}

// handleListHTTPTools lists the calling tenant's authored HTTP tool definitions.
func (d *Deps) handleListHTTPTools(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, false)
	if !ok || !requireManagementService(w, d.HTTPTools) {
		return
	}
	items, err := d.HTTPTools.List(r.Context(), p.TenantID)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "tenant_id": p.TenantID})
}

// handleSaveHTTPTool creates/updates a tenant HTTP tool definition. The tool's
// input_schema is required and must be a valid JSON Schema object so the model
// can call it; output_schema, when present, must also be valid. Writes use
// optimistic concurrency via expected_revision.
func (d *Deps) handleSaveHTTPTool(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, true)
	if !ok || !requireManagementService(w, d.HTTPTools) {
		return
	}
	var body struct {
		Definition       httptooldef.Definition `json:"definition"`
		ExpectedRevision int64                  `json:"expected_revision"`
	}
	if err := decodeManagementJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	if body.Definition.ToolName != r.PathValue("name") {
		writeError(w, http.StatusBadRequest, "HTTP_TOOL_NAME_MISMATCH", "path and definition tool_name must match")
		return
	}
	if !isJSONObject(body.Definition.InputSchema) {
		writeError(w, http.StatusBadRequest, "INVALID_CONFIGURATION", "input_schema is required and must be a JSON object so the model can call the tool")
		return
	}
	if len(body.Definition.OutputSchema) > 0 && !isJSONObject(body.Definition.OutputSchema) {
		writeError(w, http.StatusBadRequest, "INVALID_CONFIGURATION", "output_schema must be a JSON object")
		return
	}
	saved, err := d.HTTPTools.Save(r.Context(), p.TenantID, p.UserID, body.Definition, body.ExpectedRevision)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// handleDeleteHTTPTool removes a tenant HTTP tool definition.
func (d *Deps) handleDeleteHTTPTool(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, true)
	if !ok || !requireManagementService(w, d.HTTPTools) {
		return
	}
	name := r.PathValue("name")
	current, err := d.HTTPTools.GetManaged(r.Context(), p.TenantID, name)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	if err := d.HTTPTools.Delete(r.Context(), p.TenantID, name, current.Revision); err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "name": name})
}

// isJSONObject reports whether raw is a non-empty JSON object, the shape a tool
// input/output schema must take.
func isJSONObject(raw json.RawMessage) bool {
	if len(raw) == 0 || !json.Valid(raw) {
		return false
	}
	var probe map[string]json.RawMessage
	return json.Unmarshal(raw, &probe) == nil
}

func (d *Deps) handleInspectSkillPackage(w http.ResponseWriter, r *http.Request) {
	_, ok := managementPrincipal(w, r, false)
	if !ok || !requireManagementService(w, d.ManagedSkills) {
		return
	}
	inspection, err := inspectSkillPackageUpload(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_SKILL_PACKAGE", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, inspection)
}

func (d *Deps) handleUploadSkill(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, true)
	if !ok || !requireManagementService(w, d.ManagedSkills) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, skill.DefaultMaxPackageBytes+(1<<20))
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_SKILL_UPLOAD", "multipart Skill ZIP package is required")
		return
	}
	defer r.MultipartForm.RemoveAll()
	if packageFiles := r.MultipartForm.File["package"]; len(packageFiles) > 0 {
		header := packageFiles[0]
		if header.Size > skill.DefaultMaxPackageBytes {
			writeError(w, http.StatusBadRequest, "SKILL_PACKAGE_TOO_LARGE", "Skill ZIP exceeds 50 MiB")
			return
		}
		inspection, err := inspectParsedSkillPackageUpload(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_SKILL_PACKAGE", err.Error())
			return
		}
		definition := inspection.Definition
		definition.TenantID = p.TenantID
		definition.Policy.Scope = skill.ScopeTenant
		definition.Policy.MaxInputTokens = 0
		updateSkillID := strings.TrimSpace(r.FormValue("update_skill_id"))
		baseVersion := strings.TrimSpace(r.FormValue("base_version"))
		var updateRef *skill.Ref
		if updateSkillID != "" || baseVersion != "" {
			if updateSkillID == "" || baseVersion == "" {
				writeError(w, http.StatusBadRequest, "INVALID_SKILL_UPDATE", "update Skill ID and base version are both required")
				return
			}
			base := skill.Ref{ID: updateSkillID, Version: baseVersion}
			if err := skill.ValidateUpdate(base, definition); err != nil {
				writeManagementError(w, err)
				return
			}
			updateRef = &base
		}
		if strings.EqualFold(strings.TrimSpace(r.FormValue("dependencies_configured")), "true") {
			dependencies, err := skillDependenciesFromForm(r)
			if err != nil {
				writeError(w, http.StatusBadRequest, "INVALID_SKILL_DEPENDENCIES", err.Error())
				return
			}
			definition.Dependencies = dependencies
		}
		if err := validateManagedSkillInjection(definition); err != nil {
			writeManagementError(w, err)
			return
		}
		principal := skill.Principal{TenantID: p.TenantID, AgentID: "management"}
		var record skill.Record
		var created bool
		if updateRef != nil {
			record, created, err = d.ManagedSkills.PublishPackageUpdate(r.Context(), principal, *updateRef, definition, inspection.Content, inspection.Files)
		} else {
			record, created, err = d.ManagedSkills.PublishPackage(r.Context(), principal, definition, inspection.Content, inspection.Files)
		}
		if err != nil {
			writeManagementError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"record": record, "created": created, "package": inspection})
		return
	}

	// Keep the original single-file protocol available for existing clients;
	// the management UI only exposes the portable Skill ZIP workflow.
	var definition skill.Definition
	if err := json.Unmarshal([]byte(r.FormValue("manifest")), &definition); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_SKILL_MANIFEST", err.Error())
		return
	}
	definition.TenantID = p.TenantID
	definition.Policy.Scope = skill.ScopeTenant
	if definition.InjectionStrategy == "" {
		definition.InjectionStrategy = skill.InjectSystem
	}
	if err := validateManagedSkillInjection(definition); err != nil {
		writeManagementError(w, err)
		return
	}
	file, _, err := r.FormFile("content")
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_SKILL_UPLOAD", "content file is required")
		return
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, skill.DefaultMaxContentBytes+1))
	if err != nil || int64(len(content)) > skill.DefaultMaxContentBytes {
		writeError(w, http.StatusBadRequest, "SKILL_TOO_LARGE", "skill content exceeds 1 MiB")
		return
	}
	record, created, err := d.ManagedSkills.Publish(r.Context(), skill.Principal{TenantID: p.TenantID, AgentID: "management"}, definition, content)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"record": record, "created": created})
}

func validateManagedSkillInjection(definition skill.Definition) error {
	if definition.InjectionStrategy == skill.InjectSystem {
		return nil
	}
	return fmt.Errorf("%w: managed executable Skill requires a Skill execution binding adapter", skill.ErrInvalidDefinition)
}

func (d *Deps) handleRetireSkill(w http.ResponseWriter, r *http.Request) {
	p, ok := managementPrincipal(w, r, true)
	if !ok || !requireManagementService(w, d.ManagedSkills) {
		return
	}
	retirement, created, err := d.ManagedSkills.Retire(r.Context(), skill.Principal{
		TenantID: p.TenantID,
		AgentID:  "management",
	}, skill.Ref{ID: r.PathValue("id"), Version: r.PathValue("version")})
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"retirement": retirement, "created": created})
}

func inspectSkillPackageUpload(w http.ResponseWriter, r *http.Request) (skill.PackageInspection, error) {
	r.Body = http.MaxBytesReader(w, r.Body, skill.DefaultMaxPackageBytes+(1<<20))
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		return skill.PackageInspection{}, fmt.Errorf("multipart Skill ZIP package is required")
	}
	defer r.MultipartForm.RemoveAll()
	return inspectParsedSkillPackageUpload(r)
}

func inspectParsedSkillPackageUpload(r *http.Request) (skill.PackageInspection, error) {
	if r.MultipartForm == nil || len(r.MultipartForm.File["package"]) != 1 {
		return skill.PackageInspection{}, fmt.Errorf("exactly one Skill ZIP package is required")
	}
	file, header, err := r.FormFile("package")
	if err != nil {
		return skill.PackageInspection{}, fmt.Errorf("Skill ZIP package is required")
	}
	defer file.Close()
	if header.Size <= 0 || header.Size > skill.DefaultMaxPackageBytes {
		return skill.PackageInspection{}, fmt.Errorf("Skill ZIP must not exceed 50 MiB")
	}
	archive, err := io.ReadAll(io.LimitReader(file, skill.DefaultMaxPackageBytes+1))
	if err != nil {
		return skill.PackageInspection{}, fmt.Errorf("read Skill ZIP: %w", err)
	}
	if int64(len(archive)) > skill.DefaultMaxPackageBytes {
		return skill.PackageInspection{}, fmt.Errorf("Skill ZIP must not exceed 50 MiB")
	}
	return skill.InspectPackage(archive)
}

func skillDependenciesFromForm(r *http.Request) (skill.Dependencies, error) {
	values := func(key string) []string {
		if r.MultipartForm == nil {
			return nil
		}
		return uniqueTrimmedStrings(r.MultipartForm.Value[key])
	}
	dependencies := skill.Dependencies{
		Tools:      values("tool_dependency"),
		MCPServers: values("mcp_server_dependency"),
	}
	for _, value := range values("skill_dependency") {
		separator := strings.LastIndex(value, "@")
		if separator <= 0 || separator == len(value)-1 {
			return skill.Dependencies{}, fmt.Errorf("Skill dependency must use id@version: %q", value)
		}
		dependencies.Skills = append(dependencies.Skills, skill.Ref{ID: value[:separator], Version: value[separator+1:]})
	}
	return dependencies, nil
}

func uniqueTrimmedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func decodeManagementJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func (d *Deps) appendMCPDebugRecord(ctx context.Context, principal authcontext.Principal, record mcp.DebugRecord) error {
	record.ID = newMCPDebugRecordID()
	record.TenantID = principal.TenantID
	record.OperatorID = principal.UserID
	_, err := d.ManagedMCP.AppendDebugRecord(ctx, record)
	return err
}

func newMCPDebugRecordID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return "mcpdbg_" + hex.EncodeToString(bytes[:])
	}
	return fmt.Sprintf("mcpdbg_%d", time.Now().UnixNano())
}

func mcpCallDebugRequest(toolName string, arguments json.RawMessage) json.RawMessage {
	return safeDebugJSON(map[string]any{"tool_name": toolName, "arguments": arguments})
}

func safeDebugJSON(value any) json.RawMessage {
	var raw []byte
	switch typed := value.(type) {
	case json.RawMessage:
		raw = append([]byte(nil), typed...)
	default:
		var err error
		raw, err = json.Marshal(value)
		if err != nil {
			return json.RawMessage(`{"unavailable":true}`)
		}
	}
	if len(raw) > 64*1024 {
		return json.RawMessage(fmt.Sprintf(`{"truncated":true,"bytes":%d}`, len(raw)))
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return json.RawMessage(`{"unavailable":true}`)
	}
	encoded, err := json.Marshal(redactDebugValue(decoded))
	if err != nil {
		return json.RawMessage(`{"unavailable":true}`)
	}
	return encoded
}

func redactDebugValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, nested := range typed {
			if isSensitiveDebugKey(key) {
				out[key] = "[REDACTED]"
				continue
			}
			out[key] = redactDebugValue(nested)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, nested := range typed {
			out[i] = redactDebugValue(nested)
		}
		return out
	default:
		return typed
	}
}

func isSensitiveDebugKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, marker := range []string{"authorization", "password", "secret", "token", "api_key", "apikey", "access_key", "credential"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}

func managementErrorCode(err error) string {
	var authorizationRequired *mcp.AuthorizationRequiredError
	switch {
	case errors.As(err, &authorizationRequired):
		return "MCP_AUTHORIZATION_REQUIRED"
	case errors.Is(err, agentregistry.ErrAgentConfigControlNotFound), errors.Is(err, agentregistry.ErrPromptNotFound), errors.Is(err, skill.ErrNotFound), errors.Is(err, mcp.ErrServerNotFound), errors.Is(err, modeladmin.ErrProviderNotFound), errors.Is(err, toolconfig.ErrToolConfigNotFound), errors.Is(err, httptooldef.ErrNotFound):
		return "NOT_FOUND"
	case errors.Is(err, agentregistry.ErrAgentConfigControlConflict), errors.Is(err, agentregistry.ErrAgentConfigVersionImmutable), errors.Is(err, agentregistry.ErrPromptVersionExists), errors.Is(err, mcp.ErrManagedConflict), errors.Is(err, skill.ErrVersionConflict), errors.Is(err, modeladmin.ErrManagedConflict), errors.Is(err, toolconfig.ErrManagedConflict), errors.Is(err, httptooldef.ErrManagedConflict):
		return "REVISION_CONFLICT"
	case errors.Is(err, agentregistry.ErrAgentConfigControlInvalid), errors.Is(err, agentregistry.ErrPromptInvalid), errors.Is(err, skill.ErrInvalidDefinition), errors.Is(err, skill.ErrInvalidPackage), errors.Is(err, modeladmin.ErrInvalidDefinition), errors.Is(err, toolconfig.ErrInvalidDefinition), errors.Is(err, httptooldef.ErrInvalidDefinition):
		return "INVALID_CONFIGURATION"
	case errors.Is(err, mcp.ErrPermissionDenied), errors.Is(err, skill.ErrPermissionDenied):
		return "PERMISSION_DENIED"
	case errors.Is(err, skill.ErrLifecycleUnsupported):
		return "LIFECYCLE_UNSUPPORTED"
	default:
		return "MANAGEMENT_FAILED"
	}
}

func writeManagementError(w http.ResponseWriter, err error) {
	var authorizationRequired *mcp.AuthorizationRequiredError
	if errors.As(err, &authorizationRequired) {
		writeJSON(w, http.StatusPreconditionRequired, map[string]any{
			"error_code": "MCP_AUTHORIZATION_REQUIRED", "message": "MCP OAuth authorization is required",
			"server_id":     authorizationRequired.ServerID,
			"authorization": map[string]any{"type": "oauth2", "provider": authorizationRequired.Provider, "scopes": authorizationRequired.Scopes, "resource": authorizationRequired.Resource, "start_url": "/api/v1/admin/mcp-servers/" + url.PathEscape(authorizationRequired.ServerID) + "/oauth/start"},
		})
		return
	}
	status := http.StatusInternalServerError
	code := managementErrorCode(err)
	switch {
	case errors.Is(err, agentregistry.ErrAgentConfigControlNotFound), errors.Is(err, agentregistry.ErrPromptNotFound), errors.Is(err, skill.ErrNotFound), errors.Is(err, mcp.ErrServerNotFound), errors.Is(err, modeladmin.ErrProviderNotFound), errors.Is(err, toolconfig.ErrToolConfigNotFound), errors.Is(err, httptooldef.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, agentregistry.ErrAgentConfigControlConflict), errors.Is(err, agentregistry.ErrAgentConfigVersionImmutable), errors.Is(err, agentregistry.ErrPromptVersionExists), errors.Is(err, mcp.ErrManagedConflict), errors.Is(err, skill.ErrVersionConflict), errors.Is(err, modeladmin.ErrManagedConflict), errors.Is(err, toolconfig.ErrManagedConflict), errors.Is(err, httptooldef.ErrManagedConflict):
		status = http.StatusConflict
	case errors.Is(err, agentregistry.ErrAgentConfigControlInvalid), errors.Is(err, agentregistry.ErrPromptInvalid), errors.Is(err, skill.ErrInvalidDefinition), errors.Is(err, skill.ErrInvalidPackage), errors.Is(err, modeladmin.ErrInvalidDefinition), errors.Is(err, toolconfig.ErrInvalidDefinition), errors.Is(err, httptooldef.ErrInvalidDefinition):
		status = http.StatusBadRequest
	case errors.Is(err, mcp.ErrPermissionDenied), errors.Is(err, skill.ErrPermissionDenied):
		status = http.StatusForbidden
	case errors.Is(err, skill.ErrLifecycleUnsupported):
		status = http.StatusNotImplemented
	}
	writeError(w, status, code, strings.TrimSpace(err.Error()))
}
