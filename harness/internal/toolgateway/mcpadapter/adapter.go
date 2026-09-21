package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

var _ toolgateway.MCPCapabilityService = (*Adapter)(nil)

// Adapter exposes the governed MCP capability service through Tool Gateway's
// executor-facing port. It deliberately has no approval input: approvals must
// come from the canonical ControlRequest/Resume flow, never tool metadata.
type Adapter struct {
	service *mcp.Service
}

func New(service *mcp.Service) *Adapter {
	return &Adapter{service: service}
}

func (a *Adapter) ResolveSnapshot(
	ctx context.Context,
	req toolgateway.ResolveMCPSnapshotRequest,
) (*toolgateway.MCPCapabilitySnapshot, error) {
	if a == nil || a.service == nil {
		return nil, internalError("mcp capability service is required", nil)
	}
	if len(req.ServerIDs) != 1 || req.ServerIDs[0] == "" {
		return nil, internalError("exactly one mcp server id is required", nil)
	}
	if err := validateTraceIdentity(req.Trace, req.TenantID, req.UserID, req.AgentID); err != nil {
		return nil, err
	}

	ctx = withTrace(ctx, req.Trace)
	snapshot, err := a.service.ResolveSnapshot(ctx, mcp.Principal{
		TenantID: req.TenantID,
		UserID:   req.UserID,
		AgentID:  req.AgentID,
	}, req.ServerIDs[0])
	if err != nil {
		return nil, mapError(err)
	}
	if snapshot.ID == "" || snapshot.ServerID != req.ServerIDs[0] {
		return nil, toolgateway.NewToolError(
			toolgateway.ErrorTypeToolVersionNotFound,
			"mcp capability snapshot identity mismatch",
			false,
			nil,
		)
	}
	return &toolgateway.MCPCapabilitySnapshot{
		SnapshotID:     snapshot.ID,
		ServerID:       snapshot.ServerID,
		CapabilityHash: snapshot.CapabilityHash,
		PolicyHash:     snapshot.PolicyHash,
		CreatedAt:      snapshot.CreatedAt,
	}, nil
}

func (a *Adapter) CallTool(
	ctx context.Context,
	req toolgateway.MCPToolCallRequest,
) (*toolgateway.MCPToolResult, error) {
	if a == nil || a.service == nil {
		return nil, internalError("mcp capability service is required", nil)
	}
	if err := validateToolCallIdentity(req); err != nil {
		return nil, err
	}
	if req.Timeout < 0 {
		return nil, internalError("mcp tool timeout must not be negative", nil)
	}
	if err := validateTraceIdentity(req.Trace, req.TenantID, req.UserID, req.AgentID); err != nil {
		return nil, err
	}

	ctx = withTrace(ctx, req.Trace)
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	result, err := a.service.CallTool(ctx, mcp.ToolCallRequest{
		Principal: mcp.Principal{
			TenantID: req.TenantID,
			UserID:   req.UserID,
			AgentID:  req.AgentID,
		},
		ServerID:   req.ServerID,
		SnapshotID: req.SnapshotID,
		ToolName:   req.ToolName,
		ToolCallID: req.ToolCallID,
		Arguments:  append(json.RawMessage(nil), req.Arguments...),
		// ApprovalRef intentionally remains empty. Tool metadata is untrusted;
		// an approval-required response must become a canonical control request.
	})
	if err != nil {
		return nil, mapError(err)
	}
	if result.IsError {
		return nil, toolgateway.NewToolError(
			toolgateway.ErrorTypeUpstreamError,
			"mcp tool returned an error result",
			false,
			nil,
		)
	}
	return mapResult(result)
}

func validateToolCallIdentity(req toolgateway.MCPToolCallRequest) error {
	required := []struct {
		name  string
		value string
	}{
		{name: "server id", value: req.ServerID},
		{name: "snapshot id", value: req.SnapshotID},
		{name: "tool name", value: req.ToolName},
		{name: "tool call id", value: req.ToolCallID},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return internalError("mcp "+field.name+" is required", nil)
		}
	}
	return nil
}

func mapResult(result mcp.ToolResult) (*toolgateway.MCPToolResult, error) {
	mapped := &toolgateway.MCPToolResult{}
	if len(result.Content) > 0 {
		content := append([]byte(nil), result.Content...)
		if json.Valid(content) {
			mapped.Data = json.RawMessage(content)
			mapped.MimeType = "application/json"
		} else {
			if !utf8.Valid(content) {
				return nil, toolgateway.NewToolError(
					toolgateway.ErrorTypeNormalizationFailed,
					"mcp tool returned unsupported binary content",
					false,
					nil,
				)
			}
			mapped.Text = string(content)
			mapped.MimeType = "text/plain; charset=utf-8"
		}
	}
	if result.ArtifactRef != "" {
		mapped.ResourceRefs = []string{result.ArtifactRef}
	}
	if len(result.Warnings) > 0 {
		debug, err := json.Marshal(struct {
			Warnings []string `json:"warnings"`
		}{Warnings: append([]string(nil), result.Warnings...)})
		if err != nil {
			return nil, internalError("encode mcp result warnings", err)
		}
		mapped.Debug = debug
	}
	return mapped, nil
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return toolgateway.NewToolError(toolgateway.ErrorTypeTimeout, "mcp tool call timed out", true, err)
	}
	if errors.Is(err, context.Canceled) {
		return toolgateway.NewToolError(toolgateway.ErrorTypeCancelled, "mcp tool call cancelled", false, err)
	}
	var approval *mcp.ApprovalRequiredError
	if errors.As(err, &approval) {
		return toolgateway.NewToolError(
			toolgateway.ErrorTypeControlRequired,
			"mcp tool requires canonical approval",
			false,
			err,
		)
	}
	if errors.Is(err, mcp.ErrPrincipalRequired) || errors.Is(err, mcp.ErrPermissionDenied) || errors.Is(err, mcp.ErrToolNotAllowed) {
		return toolgateway.NewToolError(toolgateway.ErrorTypePermissionDenied, "mcp tool permission denied", false, err)
	}
	if errors.Is(err, mcp.ErrServerNotFound) {
		return toolgateway.NewToolError(toolgateway.ErrorTypeToolNotFound, "mcp server not found", false, err)
	}
	if errors.Is(err, mcp.ErrSnapshotRequired) || errors.Is(err, mcp.ErrSnapshotNotFound) || errors.Is(err, mcp.ErrSnapshotStale) {
		return toolgateway.NewToolError(
			toolgateway.ErrorTypeToolVersionNotFound,
			"mcp capability snapshot is unavailable",
			false,
			err,
		)
	}
	if errors.Is(err, mcp.ErrResultTooLarge) {
		return toolgateway.NewToolError(toolgateway.ErrorTypeResultTooLarge, "mcp tool result is too large", false, err)
	}
	return toolgateway.NewToolError(toolgateway.ErrorTypeUpstreamError, "mcp capability service failed", true, err)
}

func validateTraceIdentity(trace observability.TraceContext, tenantID, userID, agentID string) error {
	if (trace.TenantID != "" && trace.TenantID != tenantID) ||
		(trace.UserID != "" && trace.UserID != userID) ||
		(trace.AgentID != "" && trace.AgentID != agentID) {
		return toolgateway.NewToolError(
			toolgateway.ErrorTypePermissionDenied,
			"mcp request identity does not match trace context",
			false,
			nil,
		)
	}
	return nil
}

func withTrace(ctx context.Context, trace observability.TraceContext) context.Context {
	if trace.TraceID == "" {
		return ctx
	}
	return observability.WithTraceContext(ctx, trace)
}

func internalError(message string, cause error) error {
	return toolgateway.NewToolError(toolgateway.ErrorTypeInternal, message, false, cause)
}
