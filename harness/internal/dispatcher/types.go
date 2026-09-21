package dispatcher

import (
	"context"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/executionmode"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/scheduler"
)

// ExecutionMode 保留为兼容别名；执行范式由 executionmode 包统一 owning。
type ExecutionMode = executionmode.Mode

const (
	ExecutionModeDirectAction = executionmode.DirectAction
	ExecutionModeSingleAgent  = executionmode.SingleAgent
	ExecutionModeDeepAgent    = executionmode.DeepAgent
	ExecutionModeWorkflow     = executionmode.Workflow
	ExecutionModeGraph        = executionmode.Graph
)

type DispatchPolicy struct {
	PolicyID          string        `json:"policy_id"`
	ExecutionMode     ExecutionMode `json:"execution_mode"`
	EstimatedDuration time.Duration `json:"estimated_duration,omitempty"`
	HasSideEffect     bool          `json:"has_side_effect"`
	ResumeRequired    bool          `json:"resume_required"`
	ReasonCodes       []string      `json:"reason_codes,omitempty"`
}

type DispatchRunRequest struct {
	Run      agentruntime.RunRequest `json:"run"`
	Policy   DispatchPolicy          `json:"policy"`
	Priority scheduler.Priority      `json:"priority,omitempty"`
}

type Mode string

const (
	ModeInline    Mode = "inline"
	ModeScheduled Mode = "scheduled"
)

type Result struct {
	Mode     Mode
	Events   <-chan observability.AgentEvent
	Dispatch *scheduler.RunDispatch
}

type Policy struct {
	InlineMaxDuration time.Duration
}

// ExecutorReadinessGate prevents a registered execution mode from being
// dispatched before a compatible executor has been installed.
type ExecutorReadinessGate interface {
	Check(ctx context.Context, req DispatchRunRequest) error
}
