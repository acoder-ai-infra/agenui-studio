// Package runtimeadapter is the single translation boundary between the
// Runtime ToolInvoker port and Tool Gateway's domain contract.
package runtimeadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

var (
	ErrGatewayMissing       = errors.New("tool gateway missing")
	ErrResolverMissing      = errors.New("tool invocation resolver missing")
	ErrInvocationInvalid    = errors.New("tool invocation invalid")
	ErrGatewayResultMissing = errors.New("tool gateway result missing")
)

// InvocationResolver supplies trusted execution policy that must not be
// inferred from model output. Implementations normally resolve the frozen
// Agent binding, tenant authorization and per-tool limits.
type InvocationResolver interface {
	Resolve(ctx context.Context, req agentruntime.ToolInvocationRequest) (ResolvedInvocation, error)
}

type InvocationResolverFunc func(context.Context, agentruntime.ToolInvocationRequest) (ResolvedInvocation, error)

func (f InvocationResolverFunc) Resolve(ctx context.Context, req agentruntime.ToolInvocationRequest) (ResolvedInvocation, error) {
	return f(ctx, req)
}

type ResolvedInvocation struct {
	StepID       string
	ParentStepID string
	ToolName     string
	ToolVersion  string
	Caller       toolgateway.ToolCaller
	Policy       toolgateway.ToolCallPolicy
	Metadata     map[string]string
}

// Adapter implements agentruntime.ToolInvoker. It forwards the exact events
// acknowledged by Tool Gateway's EventStore; it never rebuilds lifecycle
// events from the final result.
type Adapter struct {
	gateway  toolgateway.StreamingToolGateway
	resolver InvocationResolver
}

func New(gateway toolgateway.StreamingToolGateway, resolver InvocationResolver) (*Adapter, error) {
	if gateway == nil {
		return nil, ErrGatewayMissing
	}
	if resolver == nil {
		return nil, ErrResolverMissing
	}
	return &Adapter{gateway: gateway, resolver: resolver}, nil
}

func (a *Adapter) Invoke(ctx context.Context, req agentruntime.ToolInvocationRequest, sink agentruntime.ToolEventSink) (agentruntime.ToolInvocationResult, error) {
	if sink == nil {
		err := fmt.Errorf("%w: event sink is required", ErrInvocationInvalid)
		return agentruntime.ToolInvocationResult{}, classifyInvocationError(ctx, err, "TOOL_INVOCATION_INVALID")
	}
	trusted, ok := observability.TraceContextFrom(ctx)
	if !ok || trusted.TraceID == "" {
		err := fmt.Errorf("%w: trusted trace context is required", ErrInvocationInvalid)
		return agentruntime.ToolInvocationResult{}, classifyInvocationError(ctx, err, "TOOL_INVOCATION_INVALID")
	}
	if err := validateFrozenRoute(req); err != nil {
		return agentruntime.ToolInvocationResult{}, classifyInvocationError(ctx, err, "TOOL_INVOCATION_INVALID")
	}
	resolved, err := a.resolver.Resolve(ctx, req)
	if err != nil {
		return agentruntime.ToolInvocationResult{}, classifyInvocationError(ctx, fmt.Errorf("resolve tool invocation: %w", err), "TOOL_RESOLVE_FAILED")
	}
	if strings.TrimSpace(resolved.StepID) == "" || strings.TrimSpace(resolved.ParentStepID) == "" {
		err := fmt.Errorf("%w: step id and parent step id are required", ErrInvocationInvalid)
		return agentruntime.ToolInvocationResult{}, classifyInvocationError(ctx, err, "TOOL_INVOCATION_INVALID")
	}
	if err := validateResolvedStepIdentity(resolved.StepID, resolved.ParentStepID); err != nil {
		return agentruntime.ToolInvocationResult{}, classifyInvocationError(ctx, err, "TOOL_STEP_ID_INVALID")
	}
	toolName, toolVersion, err := resolvedGatewayIdentity(req, resolved)
	if err != nil {
		return agentruntime.ToolInvocationResult{}, classifyInvocationError(ctx, err, "TOOL_INVOCATION_INVALID")
	}
	if resolved.Caller.Type == "" {
		resolved.Caller = toolgateway.ToolCaller{Type: "agent", AgentID: req.AgentID}
	}
	metadata := cloneStrings(resolved.Metadata)
	metadata[toolgateway.MetadataHarnessSource] = string(req.Source)
	metadata[toolgateway.MetadataHarnessSourceRef] = req.SourceRef
	metadata[toolgateway.MetadataHarnessSnapshotRef] = req.SnapshotRef

	result, err := a.gateway.InvokeWithEvents(ctx, toolgateway.ToolCallRequest{
		ToolCallID:       req.ToolCallID,
		TraceID:          trusted.TraceID,
		SpanID:           trusted.SpanID,
		TenantID:         trusted.TenantID,
		UserID:           trusted.UserID,
		SessionID:        req.SessionID,
		RunID:            req.RunID,
		StepID:           resolved.StepID,
		ParentStepID:     resolved.ParentStepID,
		AgentID:          req.AgentID,
		ToolName:         toolName,
		ToolVersion:      toolVersion,
		Arguments:        append([]byte(nil), req.Arguments...),
		ArgumentsPreview: toolArgumentsPreview(req.Arguments),
		ProcessStage:     req.ProcessStage,
		Caller:           resolved.Caller,
		Policy:           resolved.Policy,
		Metadata:         metadata,
		Resume:           gatewayResume(req.Resume),
	}, persistedEventSink{sink: sink})
	if err != nil {
		var interrupted *toolgateway.ToolInterruptedError
		if errors.As(err, &interrupted) {
			return agentruntime.ToolInvocationResult{}, &agentruntime.ToolInvocationInterruptedError{
				Info: interrupted.Info, State: append(json.RawMessage(nil), interrupted.State...), Cause: interrupted.Cause,
			}
		}
		return agentruntime.ToolInvocationResult{}, classifyInvocationError(ctx, err, "TOOL_GATEWAY_FAILED")
	}
	if result == nil {
		return agentruntime.ToolInvocationResult{}, agentruntime.NewRuntimeError(
			agentruntime.ErrorProtocol,
			"TOOL_GATEWAY_RESULT_INVALID",
			"tool gateway returned no result",
		).WithCause(ErrGatewayResultMissing)
	}
	return projectResult(result), nil
}

func toolArgumentsPreview(arguments json.RawMessage) map[string]any {
	if len(arguments) == 0 {
		return nil
	}
	var preview map[string]any
	if err := json.Unmarshal(arguments, &preview); err != nil || len(preview) == 0 {
		return nil
	}
	return preview
}

func gatewayResume(resume *agentruntime.ToolInvocationResume) *toolgateway.ToolCallResume {
	if resume == nil {
		return nil
	}
	return &toolgateway.ToolCallResume{
		WasInterrupted: resume.WasInterrupted,
		IsResumeTarget: resume.IsResumeTarget,
		State:          append(json.RawMessage(nil), resume.State...),
		Payload:        append(json.RawMessage(nil), resume.Payload...),
	}
}

type persistedEventSink struct {
	sink agentruntime.ToolEventSink
}

func (s persistedEventSink) Emit(ctx context.Context, event observability.AgentEvent) error {
	return s.sink.Emit(ctx, event)
}

func validateFrozenRoute(req agentruntime.ToolInvocationRequest) error {
	if req.ToolCallID == "" || req.SessionID == "" || req.RunID == "" || req.AgentID == "" || req.ToolName == "" {
		return fmt.Errorf("%w: required runtime identity is missing", ErrInvocationInvalid)
	}
	switch req.Source {
	case agentruntime.ToolSourceRegistry:
		if req.SourceRef == "" || req.SnapshotRef != "" {
			return fmt.Errorf("%w: registry source ref is invalid", ErrInvocationInvalid)
		}
		name, version, _ := strings.Cut(req.SourceRef, "@")
		if name != req.ToolName || (req.ToolVersion != "" && version != req.ToolVersion) {
			return fmt.Errorf("%w: registry source ref does not match requested tool", ErrInvocationInvalid)
		}
	case agentruntime.ToolSourceMCP:
		if req.SourceRef == "" || req.SnapshotRef == "" || req.ToolVersion != "" {
			return fmt.Errorf("%w: mcp server and snapshot refs are required", ErrInvocationInvalid)
		}
	case agentruntime.ToolSourceHTTPTool:
		if req.SourceRef == "" || req.SnapshotRef == "" || req.ToolVersion != "" {
			return fmt.Errorf("%w: http tool name and definition refs are required", ErrInvocationInvalid)
		}
		if req.SourceRef != req.ToolName {
			return fmt.Errorf("%w: http tool source ref does not match requested tool", ErrInvocationInvalid)
		}
	case agentruntime.ToolSourceSkill:
		return fmt.Errorf("%w: skill execution requires a Skill Gateway adapter", ErrInvocationInvalid)
	default:
		return fmt.Errorf("%w: unsupported tool source %q", ErrInvocationInvalid, req.Source)
	}
	return nil
}

func resolvedGatewayIdentity(req agentruntime.ToolInvocationRequest, resolved ResolvedInvocation) (string, string, error) {
	name := req.ToolName
	version := req.ToolVersion
	if resolved.ToolName != "" {
		name = resolved.ToolName
	}
	if resolved.ToolVersion != "" {
		version = resolved.ToolVersion
	}
	if name == "" || name != strings.TrimSpace(name) || strings.ContainsAny(name, "@#") ||
		version == "" || version != strings.TrimSpace(version) || strings.ContainsAny(version, "@#") {
		return "", "", fmt.Errorf("%w: resolved gateway tool identity is invalid", ErrInvocationInvalid)
	}
	if req.Source == agentruntime.ToolSourceRegistry {
		expectedName, expectedVersion, ok := strings.Cut(req.SourceRef, "@")
		if !ok || name != expectedName || version != expectedVersion {
			return "", "", fmt.Errorf("%w: resolved registry identity changed the frozen route", ErrInvocationInvalid)
		}
	}
	return name, version, nil
}

func projectResult(result *toolgateway.ToolCallResult) agentruntime.ToolInvocationResult {
	content := result.ModelContextResult
	if len(content) == 0 {
		content = result.ResultPreview
	}
	return agentruntime.ToolInvocationResult{
		Content:    string(content),
		ContentRef: result.ResultRef,
		IsError:    result.Status != toolgateway.ToolCallSucceeded,
	}
}

func cloneStrings(input map[string]string) map[string]string {
	output := make(map[string]string, len(input)+3)
	for key, value := range input {
		output[key] = value
	}
	return output
}

var _ agentruntime.ToolInvoker = (*Adapter)(nil)
