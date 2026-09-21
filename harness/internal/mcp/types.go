package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrPrincipalRequired = errors.New("mcp principal required")
	ErrPermissionDenied  = errors.New("mcp permission denied")
	ErrServerNotFound    = errors.New("mcp server not found")
	ErrToolNotAllowed    = errors.New("mcp tool not allowed")
	ErrResultTooLarge    = errors.New("mcp result too large")
	ErrSnapshotRequired  = errors.New("mcp capability snapshot required")
	ErrSnapshotNotFound  = errors.New("mcp capability snapshot not found")
	ErrSnapshotStale     = errors.New("mcp capability snapshot stale or invalid")
	ErrOAuthGrantMissing = errors.New("mcp oauth grant missing")
)

type Principal struct {
	TenantID string `json:"tenant_id,omitempty"`
	UserID   string `json:"user_id,omitempty"`
	AgentID  string `json:"agent_id,omitempty"`
	Purpose  string `json:"purpose,omitempty"`
	System   bool   `json:"system,omitempty"`
}

const PrincipalPurposeManagementDebug = "management-debug"

func ManagementDebugPrincipal(tenantID, userID string) Principal {
	return Principal{TenantID: tenantID, UserID: userID, Purpose: PrincipalPurposeManagementDebug}
}

func (p Principal) Validate() error {
	if p.System {
		if p.Purpose != "" {
			return ErrPrincipalRequired
		}
		return nil
	}
	if p.Purpose == PrincipalPurposeManagementDebug {
		if p.TenantID == "" || p.UserID == "" || p.AgentID != "" {
			return ErrPrincipalRequired
		}
		return nil
	}
	if p.Purpose != "" {
		return ErrPrincipalRequired
	}
	if p.TenantID == "" || p.AgentID == "" {
		return ErrPrincipalRequired
	}
	return nil
}

type Scope string

const (
	ScopeSystem Scope = "system"
	ScopeTenant Scope = "tenant"
	ScopeUser   Scope = "user"
)

type AuthConfig struct {
	Type      string   `json:"type,omitempty"`
	Provider  string   `json:"provider,omitempty"`
	Scopes    []string `json:"scopes,omitempty"`
	Resource  string   `json:"resource,omitempty"`
	GrantMode string   `json:"grant_mode,omitempty"`
}

const (
	AuthTypeNone             = ""
	AuthTypeOAuth2           = "oauth2"
	OAuthGrantModeUser       = "user"
	OAuthGrantModeMaintainer = "maintainer"
)

type ServerDefinition struct {
	ID                 string            `json:"id"`
	DisplayName        string            `json:"display_name,omitempty"`
	Version            string            `json:"version"`
	Scope              Scope             `json:"scope"`
	TenantID           string            `json:"tenant_id,omitempty"`
	UserID             string            `json:"user_id,omitempty"`
	AllowedAgents      []string          `json:"allowed_agents,omitempty"`
	BlockedAgents      []string          `json:"blocked_agents,omitempty"`
	HITLTools          []string          `json:"hitl_tools,omitempty"`
	MaxConcurrentCalls int               `json:"max_concurrent_calls,omitempty"`
	MaxResultBytes     int64             `json:"max_result_bytes,omitempty"`
	Transport          string            `json:"transport,omitempty"`
	Endpoint           string            `json:"endpoint,omitempty"`
	ProtocolVersion    string            `json:"protocol_version,omitempty"`
	HeaderEnv          map[string]string `json:"header_env,omitempty"`
	Auth               AuthConfig        `json:"auth,omitempty"`
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type CapabilitySnapshot struct {
	ID             string    `json:"id"`
	ServerID       string    `json:"server_id"`
	ServerVersion  string    `json:"server_version"`
	PrincipalHash  string    `json:"principal_hash"`
	PolicyHash     string    `json:"policy_hash"`
	CapabilityHash string    `json:"capability_hash"`
	Tools          []Tool    `json:"tools,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type CallOptions struct {
	MaxResultBytes int64
}

type ToolResult struct {
	Content     []byte   `json:"content,omitempty"`
	ArtifactRef string   `json:"artifact_ref,omitempty"`
	IsError     bool     `json:"is_error,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
}

type Client interface {
	ListTools(ctx context.Context) ([]Tool, error)
	CallTool(ctx context.Context, name string, arguments json.RawMessage, options CallOptions) (ToolResult, error)
}

type ClientProvider interface {
	Client(ctx context.Context, definition ServerDefinition, principal Principal) (Client, error)
}

type Registry interface {
	Get(ctx context.Context, serverID string) (ServerDefinition, error)
}

type PrincipalRegistry interface {
	GetForPrincipal(ctx context.Context, principal Principal, serverID string) (ServerDefinition, error)
}

type SnapshotStore interface {
	Save(ctx context.Context, snapshot CapabilitySnapshot) error
	Load(ctx context.Context, snapshotID string) (CapabilitySnapshot, error)
}

type ApprovalVerifier interface {
	Verify(ctx context.Context, principal Principal, request ControlRequest, approvalRef string) error
}

type ControlRequest struct {
	Type       string          `json:"type"`
	ServerID   string          `json:"server_id"`
	ToolName   string          `json:"tool_name"`
	ToolCallID string          `json:"tool_call_id"`
	SnapshotID string          `json:"snapshot_id"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

type ApprovalRequiredError struct {
	Request ControlRequest
}

func (e *ApprovalRequiredError) Error() string {
	return "mcp tool requires approval: " + e.Request.ToolName
}

type ToolCallRequest struct {
	Principal   Principal
	ServerID    string
	SnapshotID  string
	ToolName    string
	ToolCallID  string
	Arguments   json.RawMessage
	ApprovalRef string
}
