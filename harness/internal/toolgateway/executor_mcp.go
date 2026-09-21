package toolgateway

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type MCPCapabilityService interface {
	ResolveSnapshot(ctx context.Context, req ResolveMCPSnapshotRequest) (*MCPCapabilitySnapshot, error)
	CallTool(ctx context.Context, req MCPToolCallRequest) (*MCPToolResult, error)
}

type ResolveMCPSnapshotRequest struct {
	TenantID  string
	UserID    string
	AgentID   string
	ServerIDs []string
	Trace     observability.TraceContext
}

type MCPCapabilitySnapshot struct {
	SnapshotID     string
	ServerID       string
	ToolsRef       string
	ResourcesRef   string
	PromptsRef     string
	CapabilityHash string
	PolicyHash     string
	CreatedAt      time.Time
}

type MCPToolCallRequest struct {
	TenantID   string
	UserID     string
	SessionID  string
	RunID      string
	StepID     string
	AgentID    string
	ToolCallID string
	ServerID   string
	SnapshotID string
	ToolName   string
	Arguments  json.RawMessage
	Timeout    time.Duration
	Trace      observability.TraceContext
}

type MCPToolResult struct {
	Data         json.RawMessage
	Text         string
	MimeType     string
	ResourceRefs []string
	Debug        json.RawMessage
}

type MCPExecutor struct {
	service MCPCapabilityService
}

func NewMCPExecutor(service MCPCapabilityService) *MCPExecutor {
	return &MCPExecutor{service: service}
}

func (e *MCPExecutor) Type() ToolType {
	return ToolTypeMCP
}

func (e *MCPExecutor) Execute(ctx context.Context, def *ToolDefinition, req ToolCallRequest) (*ToolRawResult, error) {
	if e.service == nil {
		return nil, NewToolError(ErrorTypeInternal, "mcp capability service is required", false, nil)
	}
	if def.MCP == nil || def.MCP.ServerID == "" || def.MCP.MCPToolName == "" {
		return nil, NewToolError(ErrorTypeInternal, "mcp tool spec is required", false, nil)
	}
	timeout := req.Policy.Timeout
	if timeout <= 0 {
		timeout = def.Timeout
	}
	serverID := def.MCP.ServerID
	snapshotID := def.MCP.SnapshotID
	toolName := def.MCP.MCPToolName
	// MCP capability snapshots are frozen per Run. The static registry owns the
	// executable adapter, while the trusted runtime binding owns the exact
	// server/snapshot selected during context assembly.
	if value := req.Metadata[MetadataHarnessSourceRef]; value != "" {
		if value != serverID {
			return nil, NewToolError(ErrorTypePermissionDenied, "mcp server binding mismatch", false, nil)
		}
		serverID = value
	}
	if value := req.Metadata[MetadataHarnessSnapshotRef]; value != "" {
		snapshotID = value
	}
	result, err := e.service.CallTool(ctx, MCPToolCallRequest{
		TenantID:   req.TenantID,
		UserID:     req.UserID,
		SessionID:  req.SessionID,
		RunID:      req.RunID,
		StepID:     req.StepID,
		AgentID:    req.AgentID,
		ToolCallID: req.ToolCallID,
		ServerID:   serverID,
		SnapshotID: snapshotID,
		ToolName:   toolName,
		Arguments:  append(json.RawMessage(nil), req.Arguments...),
		Timeout:    timeout,
		Trace:      observability.MustTraceContext(ctx),
	})
	if err != nil {
		var toolErr *ToolError
		if errors.As(err, &toolErr) {
			return nil, err
		}
		return nil, NewToolError(ErrorTypeUpstreamError, "mcp tool call failed", true, err)
	}
	if result == nil {
		result = &MCPToolResult{}
	}
	return &ToolRawResult{
		MimeType:     result.MimeType,
		Data:         append(json.RawMessage(nil), result.Data...),
		Text:         result.Text,
		ArtifactRefs: append([]string(nil), result.ResourceRefs...),
		Debug:        append(json.RawMessage(nil), result.Debug...),
	}, nil
}
