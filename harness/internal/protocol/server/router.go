// Package server wires the protocol layer HTTP/SSE handlers onto a net/http mux.
// It is the outermost edge of the Harness: it depends downward on the protocol
// projection layer and the Phase 1 services (storage, control, artifact,
// observability); nothing imports it.
package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/AGenUI/agenui-studio/harness/internal/agentregistry"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/control"
	"github.com/AGenUI/agenui-studio/harness/internal/control/controlticket"
	"github.com/AGenUI/agenui-studio/harness/internal/httptooldef"
	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/modeladmin"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
	"github.com/AGenUI/agenui-studio/harness/internal/skill"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	"github.com/AGenUI/agenui-studio/harness/internal/tenantadmin"
	"github.com/AGenUI/agenui-studio/harness/internal/toolconfig"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

// RunDispatcher is the injected port that hands a freshly-opened run to
// execution (inline / scheduled). It is intentionally a narrow port so the
// protocol layer does not import dispatcher/agentruntime directly; the concrete
// dispatcher is wired at composition root. May be nil in P0 (OpenTurn still
// persists the fact-first ledger; dispatch is a TODO).
type RunDispatcher interface {
	Dispatch(ctx context.Context, turn *storage.OpenTurnResult) error
}

// RunCanceller is separate from RunDispatcher so protocol tests and adapters
// that only create runs do not gain a fake cancellation implementation.
type RunCanceller interface {
	Cancel(ctx context.Context, sessionID, runID, reason string) error
}

type MCPOAuthAuthorizer interface {
	Start(context.Context, mcp.Principal, mcp.ServerDefinition) (mcp.OAuthAuthorizationStart, error)
	Complete(context.Context, string, string, string, string) (mcp.OAuthAuthorizationCompletion, error)
	Status(context.Context, mcp.Principal, mcp.ServerDefinition) (mcp.OAuthAuthorizationStatus, error)
	Revoke(context.Context, mcp.Principal, mcp.ServerDefinition) error
}

// Deps are the collaborators the protocol server needs. All Phase 1 services are
// injected so the server can be tested against the memory backend.
type Deps struct {
	Stores     storage.Stores
	RunService *storage.RunService
	// DefaultAgentID 由 Composition Root 从已校验配置注入，协议层不读取配置文件。
	DefaultAgentID string
	// TurnPreparer 在 OpenTurn 之前执行入口层扩展预回合管线（IdentityResolver /
	// RunInitializer / ContextContributor / InputNormalizer），与 SDK 入口共享
	// 同一治理链。由 Composition Root 注入 kernel.TurnPipeline；nil 或无阶段
	// 时零成本跳过（hosted 默认无扩展）。
	TurnPreparer   *kernel.TurnPipeline
	Control        *control.Service
	ControlTickets *controlticket.Codec
	Artifacts      *artifact.Store
	Broker         protocol.EventBroker
	HotBuffer      protocol.HotStreamBuffer
	SSE            *protocol.SSEAdapter
	Registry       protocol.Registry
	Filter         protocol.VisibilityFilter
	Dispatcher     RunDispatcher // may be nil
	Canceller      RunCanceller
	Logger         observability.StructuredLogger
	// ContextSnapshots resolves frozen context snapshots by artifact ref.
	// Nil when not wired (endpoint returns 501).
	ContextSnapshots ContextSnapshotResolver
	// System is a pre-marshaled, redacted config snapshot served by
	// GET /api/v1/debug/info. Nil when introspection is not wired.
	System json.RawMessage
	// LogQuery backs GET /api/v1/debug/logs. Nil unless the logger is a
	// RingLogger (endpoint returns 501 when nil).
	LogQuery observability.LogQuerier
	// AgentCatalog is the pre-marshaled agent summary list served by
	// GET /api/v1/debug/agents. Nil when no agent registry is composed.
	AgentCatalog json.RawMessage
	// AgentConfigs maps agent_id -> pre-marshaled effective-config JSON, served
	// by GET /api/v1/debug/agents/{id}. Nil/empty when no agents are registered.
	AgentConfigs       map[string]json.RawMessage
	AgentConfigSource  *agentregistry.Service
	AgentConfigControl *agentregistry.AgentConfigControlService
	Prompts            agentregistry.PromptStore
	ManagedMCP         *mcp.SQLManagedRegistry
	MCPDebug           *mcp.Service
	MCPOAuth           MCPOAuthAuthorizer
	ManagedSkills      *skill.Service
	// ToolCatalog lists the globally registered tool definitions for the agent
	// config UI. Nil when no tool registry is composed (endpoint returns 501).
	ToolCatalog toolgateway.ToolLister
	// ToolConfig stores tenant-scoped configuration for configurable tools
	// (git_clone, HTTP tools). Nil when no managed SQL store is composed.
	ToolConfig *toolconfig.SQLManagedRegistry
	// HTTPTools stores tenant-authored HTTP tool definitions. Nil when no managed
	// SQL store is composed.
	HTTPTools *httptooldef.SQLManagedRegistry
	// ManagedModelProviders stores tenant model gateway provider configs.
	ManagedModelProviders *modeladmin.SQLManagedRegistry
	// ValidateModelProvider dry-runs a provider definition (build + secret
	// resolution) before persisting; nil skips validation. Set by app composition
	// to avoid an import cycle (server must not import internal/app).
	ValidateModelProvider func(context.Context, modeladmin.ProviderDefinition) error
	ConfigEnvironment     agentregistry.ConfigEnvironment
	// Tenants is the platform-global tenant directory backing the Owner console
	// CRUD and the secret-key login gate. Nil when no managed SQL store is
	// composed (endpoints return 501).
	Tenants *tenantadmin.SQLManagedRegistry
	// LocalTenantSwitch enables the console tenant selector when the configured
	// authenticator trusts request identity headers. JWT deployments leave it false.
	LocalTenantSwitch bool
}

func (d *Deps) filter() protocol.VisibilityFilter {
	if d.Filter != nil {
		return d.Filter
	}
	return protocol.DefaultVisibilityFilter{}
}

func (d *Deps) sse() *protocol.SSEAdapter {
	if d.SSE != nil {
		return d.SSE
	}
	return protocol.NewSSEAdapter()
}

// NewRouter builds the protocol mux with all P0 routes registered (P3-D4 route
// shapes). The caller wraps it with observability.HTTPMiddleware.
func NewRouter(deps Deps) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/sessions/{sid}/runs", deps.handleCreateRun)
	mux.HandleFunc("POST /api/v1/sessions/{sid}/runs/{rid}/cancel", deps.handleCancelRun)
	mux.HandleFunc("GET /api/v1/sessions/{sid}/runs/{rid}/events", deps.handleEvents)
	mux.HandleFunc("GET /api/v1/sessions/{sid}/runs/{rid}/stream-buffer", deps.handleStreamBuffer)
	mux.HandleFunc("POST /api/v1/control-requests/{rid}/responses", deps.handleControlResponse)
	mux.HandleFunc("POST /api/v1/artifacts/download-url", deps.handleCreateDownloadURL)
	mux.HandleFunc("GET /api/v1/artifacts/download/{token}", deps.handleArtifactDownload)
	mux.HandleFunc("GET /api/v1/artifacts/{ref...}", deps.handleArtifact)
	mux.HandleFunc("GET /api/v1/sessions", deps.handleListSessions)
	mux.HandleFunc("GET /api/v1/sessions/{sid}/messages", deps.handleListMessages)
	mux.HandleFunc("GET /api/v1/sessions/{sid}/runs", deps.handleListRuns)
	mux.HandleFunc("GET /api/v1/runs/{rid}/result", deps.handleRunResult)
	mux.HandleFunc("GET /api/v1/runs/{rid}/context-snapshot", deps.handleContextSnapshot)
	// Vercel AI SDK v5 UI message stream endpoint.
	mux.HandleFunc("POST /api/v1/ai/chat", deps.handleAIChat)
	// Agent-addressed chat is additive and keeps the legacy AI endpoint stable.
	mux.HandleFunc("POST /api/v1/agents/{agentId}/chat", deps.handleAgentChat)
	mux.HandleFunc("GET /api/v1/agents/{agentId}/chat-profile", deps.handleAgentChatProfile)
	mux.HandleFunc("GET /api/v1/sessions/{sid}/chat-transcript", deps.handleAgentChatTranscript)
	// Re-attach a live AI-SDK stream to an existing run (e.g. after a control was
	// answered) instead of polling the transcript.
	mux.HandleFunc("GET /api/v1/sessions/{sid}/runs/{rid}/chat-stream", deps.handleAgentChatResumeStream)
	// Debug console introspection (all gated by tc.DebugEnabled).
	mux.HandleFunc("GET /api/v1/debug/info", deps.handleDebugInfo)
	mux.HandleFunc("GET /api/v1/debug/events", deps.handleDebugEvents)
	mux.HandleFunc("GET /api/v1/debug/steps", deps.handleDebugSteps)
	mux.HandleFunc("GET /api/v1/debug/control-requests", deps.handleDebugControlRequests)
	mux.HandleFunc("GET /api/v1/debug/checkpoints", deps.handleDebugCheckpoints)
	mux.HandleFunc("GET /api/v1/debug/logs", deps.handleDebugLogs)
	mux.HandleFunc("GET /api/v1/debug/agents", deps.handleDebugAgents)
	mux.HandleFunc("GET /api/v1/debug/agents/{id}", deps.handleDebugAgent)
	mux.HandleFunc("GET /api/v1/usage", deps.handleUsage)
	mux.HandleFunc("GET /api/v1/admin/agent-configs", deps.handleListAgentConfigs)
	mux.HandleFunc("GET /api/v1/admin/agent-configs/{id}/draft", deps.handleGetAgentConfigDraft)
	mux.HandleFunc("PUT /api/v1/admin/agent-configs/{id}/draft", deps.handleSaveAgentConfigDraft)
	mux.HandleFunc("GET /api/v1/admin/agent-configs/{id}/versions", deps.handleListAgentConfigVersions)
	mux.HandleFunc("GET /api/v1/admin/agent-configs/{id}/release", deps.handleGetAgentConfigRelease)
	mux.HandleFunc("POST /api/v1/admin/agent-configs/{id}/publish", deps.handlePublishAgentConfig)
	mux.HandleFunc("POST /api/v1/admin/agent-configs/{id}/promote", deps.handlePromoteAgentConfig)
	mux.HandleFunc("GET /api/v1/admin/agent-configs/{id}/prompt-versions", deps.handleListPromptVersions)
	mux.HandleFunc("POST /api/v1/admin/agent-configs/{id}/prompt-versions", deps.handleCreatePromptVersion)
	mux.HandleFunc("GET /api/v1/admin/agent-relations", deps.handleListAgentRelations)
	mux.HandleFunc("GET /api/v1/admin/tools", deps.handleListTools)
	mux.HandleFunc("PUT /api/v1/admin/tools/{name}/config", deps.handleSaveToolConfig)
	mux.HandleFunc("DELETE /api/v1/admin/tools/{name}/config", deps.handleDeleteToolConfig)
	mux.HandleFunc("GET /api/v1/admin/http-tools", deps.handleListHTTPTools)
	mux.HandleFunc("PUT /api/v1/admin/http-tools/{name}", deps.handleSaveHTTPTool)
	mux.HandleFunc("DELETE /api/v1/admin/http-tools/{name}", deps.handleDeleteHTTPTool)
	mux.HandleFunc("GET /api/v1/admin/mcp-servers", deps.handleListMCPServers)
	mux.HandleFunc("POST /api/v1/admin/mcp-servers", deps.handleCreateMCPServer)
	mux.HandleFunc("PUT /api/v1/admin/mcp-servers/{id}", deps.handleSaveMCPServer)
	mux.HandleFunc("POST /api/v1/admin/mcp-servers/{id}/debug/tools", deps.handleDebugMCPTools)
	mux.HandleFunc("POST /api/v1/admin/mcp-servers/{id}/debug/call", deps.handleDebugMCPCall)
	mux.HandleFunc("GET /api/v1/admin/mcp-servers/{id}/debug/history", deps.handleListMCPDebugHistory)
	mux.HandleFunc("POST /api/v1/admin/mcp-servers/{id}/oauth/start", deps.handleStartMCPOAuth)
	mux.HandleFunc("GET /api/v1/admin/mcp-servers/{id}/oauth/status", deps.handleMCPOAuthStatus)
	mux.HandleFunc("DELETE /api/v1/admin/mcp-servers/{id}/oauth", deps.handleRevokeMCPOAuth)
	mux.HandleFunc("GET /api/v1/mcp/oauth/callback", deps.handleMCPOAuthCallback)
	mux.HandleFunc("GET /api/v1/admin/model-providers", deps.handleListModelProviders)
	mux.HandleFunc("PUT /api/v1/admin/model-providers/{id}", deps.handleSaveModelProvider)
	mux.HandleFunc("DELETE /api/v1/admin/model-providers/{id}", deps.handleDeleteModelProvider)
	mux.HandleFunc("GET /api/v1/admin/skills", deps.handleListSkills)
	mux.HandleFunc("POST /api/v1/admin/skills/inspect", deps.handleInspectSkillPackage)
	mux.HandleFunc("POST /api/v1/admin/skills/upload", deps.handleUploadSkill)
	mux.HandleFunc("GET /api/v1/admin/skills/{id}/versions/{version}/files", deps.handleListSkillFiles)
	mux.HandleFunc("GET /api/v1/admin/skills/{id}/versions/{version}/file-preview", deps.handlePreviewSkillFile)
	mux.HandleFunc("DELETE /api/v1/admin/skills/{id}/versions/{version}", deps.handleRetireSkill)
	// Platform-global tenant directory (Owner console CRUD).
	mux.HandleFunc("GET /api/v1/owner/tenants", deps.handleListTenants)
	mux.HandleFunc("POST /api/v1/owner/tenants", deps.handleCreateTenant)
	mux.HandleFunc("PUT /api/v1/owner/tenants/{id}", deps.handleUpdateTenant)
	mux.HandleFunc("DELETE /api/v1/owner/tenants/{id}", deps.handleDeleteTenant)
	mux.HandleFunc("POST /api/v1/owner/tenants/{id}/regenerate-key", deps.handleRegenerateTenantKey)
	// Tenant login gate (secret-key → HttpOnly cookie session). Reachable
	// without an existing tenant/principal; the handler validates the key.
	mux.HandleFunc("/api/v1/tenant-session", deps.handleTenantSession)
	// Non-secret tenant directory for the login picker (id/name/status only).
	mux.HandleFunc("GET /api/v1/tenant-options", deps.handleListTenantOptions)
	return mux
}

// --- shared helpers -----------------------------------------------------------

// targetFor derives the client target from the request. P0: ordinary client
// (user_visible only). A trusted debug console is authorized via the x-debug
// header set by the auth layer (never trusted from an unauthenticated edge).
func targetFor(r *http.Request) protocol.ClientTarget {
	tc := observability.MustTraceContext(r.Context())
	t := protocol.ClientTarget{Platform: tc.Channel}
	if tc.DebugEnabled {
		t.AllowedVisibilities = []observability.EventVisibility{
			observability.VisibilityDebug,
			observability.VisibilityInternal,
			observability.VisibilityRestricted,
		}
	}
	return t
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError emits a stable machine-readable error envelope.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error_code": code, "message": message})
}

// statusForStorageErr maps storage error codes to HTTP status.
func statusForStorageErr(err error) int {
	switch {
	case storage.IsErrorCode(err, storage.ErrNotFound):
		return http.StatusNotFound
	case storage.IsErrorCode(err, storage.ErrPermissionDenied), storage.IsErrorCode(err, storage.ErrTenantMismatch):
		return http.StatusForbidden
	case storage.IsErrorCode(err, storage.ErrInvalidArgument):
		return http.StatusBadRequest
	case storage.IsErrorCode(err, storage.ErrConflict), storage.IsErrorCode(err, storage.ErrCASMismatch):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
