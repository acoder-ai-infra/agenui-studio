package agentgateway

import (
	"context"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// LocalRunExecutor is the Composition Root boundary that creates the durable
// child Run, builds its own ContextSnapshot and invokes Orchestrator/Runtime.
// Agent Gateway does not duplicate Session, Run, Scheduler or storage logic.
type LocalRunExecutor interface {
	ExecuteChild(ctx context.Context, req LocalRunExecutionRequest) (agentruntime.SubAgentInvocationResult, error)
}

// LocalChildResumer 是可选的子 Run 恢复边界：凭 child control 绑定走
// canonical Resume 正门恢复处于 waiting_control 的子 Run，并等待其终态。
type LocalChildResumer interface {
	ResumeChild(ctx context.Context, req LocalRunResumeRequest) (agentruntime.SubAgentInvocationResult, error)
}

type LocalRunResumeRequest struct {
	TaskID          string
	TenantID        string
	SessionID       string
	ParentRunID     string
	Control         agentruntime.SubAgentChildControl
	ResponsePayload []byte
	Trace           observability.TraceContext
}

type LocalRunExecutionRequest struct {
	TaskID      string
	AttemptID   string
	TenantID    string
	SessionID   string
	ParentRunID string
	Target      ResolvedTarget
	Description string
	InputParts  []contextpkg.ContentPart
	ScopedData  agentruntime.ScopedData
	Depth       int
	Trace       observability.TraceContext
}

// LocalChildResumerProvider 是 LocalProvider 的可选恢复扩展。
type LocalChildResumerProvider interface {
	Resume(ctx context.Context, req LocalRunResumeRequest) (agentruntime.SubAgentInvocationResult, error)
}

type RuntimeLocalProvider struct{ Executor LocalRunExecutor }

func (p RuntimeLocalProvider) Invoke(ctx context.Context, req LocalInvocationRequest) (agentruntime.SubAgentInvocationResult, error) {
	if p.Executor == nil {
		return agentruntime.SubAgentInvocationResult{}, ErrProviderUnavailable
	}
	return p.Executor.ExecuteChild(ctx, LocalRunExecutionRequest{
		TaskID: req.Invocation.InvocationID, AttemptID: req.Invocation.AttemptID,
		TenantID: req.Invocation.TenantID, SessionID: req.Invocation.SessionID,
		ParentRunID: req.Invocation.ParentRunID, Target: req.Target,
		Description: req.Invocation.Description,
		InputParts:  append([]contextpkg.ContentPart(nil), req.Invocation.InputParts...),
		ScopedData:  req.ScopedData, Depth: req.Depth,
		Trace: req.Invocation.Trace,
	})
}

// Resume 透传给 Composition Root 的 LocalChildResumer；未接线时 fail-closed。
func (p RuntimeLocalProvider) Resume(ctx context.Context, req LocalRunResumeRequest) (agentruntime.SubAgentInvocationResult, error) {
	resumer, ok := p.Executor.(LocalChildResumer)
	if !ok || p.Executor == nil {
		return agentruntime.SubAgentInvocationResult{}, ErrProviderUnavailable
	}
	return resumer.ResumeChild(ctx, req)
}

var _ LocalProvider = RuntimeLocalProvider{}
var _ LocalChildResumerProvider = RuntimeLocalProvider{}
