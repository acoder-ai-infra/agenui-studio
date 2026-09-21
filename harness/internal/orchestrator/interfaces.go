package orchestrator

import (
	"context"

	"github.com/AGenUI/agenui-studio/harness/internal/agentbinding"
	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/dispatcher"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/scheduler"
	"github.com/AGenUI/agenui-studio/harness/internal/storagewrite"
)

// BindingResolver 是 Orchestrator 对 Agent Binding 的唯一依赖。
// Registry 、会话默认值和控制面规则的解析都封装在该窄口之后。
type BindingResolver interface {
	Resolve(ctx context.Context, req agentbinding.BindingRequest) (agentbinding.Result, error)
}

type Orchestrator interface {
	Run(ctx context.Context, req RunRequest) (*dispatcher.Result, error)
	Resume(ctx context.Context, req agentruntime.ResumeRequest) (<-chan observability.AgentEvent, error)
	Cancel(ctx context.Context, req agentruntime.CancelRequest) error
}

type Dispatcher interface {
	Dispatch(ctx context.Context, req dispatcher.DispatchRunRequest) (*dispatcher.Result, error)
	Cancel(ctx context.Context, req scheduler.CancelDispatchRequest) error
}

// RuntimeController 只收窄 foundation RuntimeService 的恢复和取消能力。
// Run 必须经过 Dispatcher，避免 Orchestrator 重做 inline / scheduled 决策。
type RuntimeController interface {
	Resume(ctx context.Context, req agentruntime.ResumeRequest) (<-chan observability.AgentEvent, error)
	Cancel(ctx context.Context, req agentruntime.CancelRequest) error
}

type StorageWriteExecutor interface {
	Execute(ctx context.Context, plan storagewrite.Plan) (storagewrite.Result, error)
}
