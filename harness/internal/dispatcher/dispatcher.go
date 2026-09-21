package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/scheduler"
)

var (
	ErrRuntimeServiceMissing = errors.New("runtime service missing")
	ErrSchedulerMissing      = errors.New("scheduler missing")
	ErrReadinessGateMissing  = errors.New("executor readiness gate missing")
	ErrExecutorNotReady      = errors.New("execution mode executor not ready")
	ErrProductionMemoryStore = errors.New("production dispatcher cannot use in-memory run request store")
)

type RunDispatcher struct {
	Runtime   *agentruntime.RuntimeService
	Scheduler scheduler.RunScheduler
	Requests  RunRequestStore
	Readiness ExecutorReadinessGate
	Policy    Policy
	Logger    observability.StructuredLogger
	Tracer    observability.TraceProvider
	// InlineModes is a Composition Root decision. Direct is always eligible;
	// managed runtimes may be enabled explicitly when an inline executor exists.
	InlineModes map[ExecutionMode]bool
}

func NewRunDispatcher(runtime *agentruntime.RuntimeService, runScheduler scheduler.RunScheduler, policy Policy, logger observability.StructuredLogger, tracer observability.TraceProvider, requestStores ...RunRequestStore) *RunDispatcher {
	if logger == nil {
		logger = observability.NoopLogger{}
	}
	if tracer == nil {
		tracer = observability.NewNoopTracer("dispatcher")
	}
	if policy.InlineMaxDuration <= 0 {
		policy.InlineMaxDuration = 2 * time.Second
	}
	requests := RunRequestStore(NewInMemoryRunRequestStore())
	if len(requestStores) > 0 && requestStores[0] != nil {
		requests = requestStores[0]
	}
	return &RunDispatcher{Runtime: runtime, Scheduler: runScheduler, Requests: requests, Readiness: P0ExecutorReadinessGate{}, Policy: policy, Logger: logger, Tracer: tracer, InlineModes: map[ExecutionMode]bool{ExecutionModeDirectAction: true}}
}

func (d *RunDispatcher) Dispatch(ctx context.Context, req DispatchRunRequest) (*Result, error) {
	if err := req.Policy.ExecutionMode.Validate(); err != nil {
		return nil, err
	}
	if d.Readiness != nil {
		if err := d.Readiness.Check(ctx, req); err != nil {
			return nil, err
		}
	}
	if d.shouldInline(req.Policy) {
		if d.Runtime == nil {
			return nil, ErrRuntimeServiceMissing
		}
		events, err := d.Runtime.Run(ctx, req.Run)
		if err != nil {
			return nil, err
		}
		return &Result{Mode: ModeInline, Events: events}, nil
	}
	if d.Scheduler == nil {
		return nil, ErrSchedulerMissing
	}
	if d.Requests == nil {
		return nil, ErrRunRequestStoreMissing
	}
	requestRef, err := d.Requests.Put(ctx, req.Run)
	if err != nil {
		return nil, err
	}
	dispatch, err := d.Scheduler.Enqueue(ctx, scheduler.EnqueueRunRequest{
		RunID:      req.Run.RunID,
		SessionID:  req.Run.SessionID,
		AgentID:    req.Run.Definition.AgentID,
		RequestRef: requestRef,
		Priority:   req.Priority,
		DeadlineAt: deadlineFromRun(req.Run),
		Trace:      stableRunTrace(req.Run),
	})
	if err != nil {
		return nil, err
	}
	return &Result{Mode: ModeScheduled, Dispatch: dispatch}, nil
}

// ValidateProduction rejects the single-process request store. A durable
// adapter must commit the frozen Runtime.Run request before queue visibility.
func (d *RunDispatcher) ValidateProduction() error {
	if d.Runtime == nil {
		return ErrRuntimeServiceMissing
	}
	if d.Scheduler == nil {
		return ErrSchedulerMissing
	}
	if d.Requests == nil {
		return ErrRunRequestStoreMissing
	}
	if d.Readiness == nil {
		return ErrReadinessGateMissing
	}
	if marker, ok := d.Requests.(interface{ InMemory() bool }); ok && marker.InMemory() {
		return ErrProductionMemoryStore
	}
	return nil
}

// P0ExecutorReadinessGate reflects the executors currently delivered by this
// repository. DeepAgent, Workflow and Graph remain valid registry contracts,
// but cannot enter the queue until their executors are installed.
type P0ExecutorReadinessGate struct{}

func (P0ExecutorReadinessGate) Check(_ context.Context, req DispatchRunRequest) error {
	switch req.Policy.ExecutionMode {
	case ExecutionModeDirectAction:
		// NativeDirect 没有 checkpoint/resume；需要副作用治理或 HITL 的请求不能先入队再晚失败。
		if req.Policy.HasSideEffect || req.Policy.ResumeRequired {
			return fmt.Errorf("%w: %s requires checkpoint/resume", ErrExecutorNotReady, req.Policy.ExecutionMode)
		}
		return nil
	case ExecutionModeSingleAgent:
		return nil
	case ExecutionModeDeepAgent, ExecutionModeWorkflow, ExecutionModeGraph:
		return fmt.Errorf("%w: %s", ErrExecutorNotReady, req.Policy.ExecutionMode)
	default:
		return req.Policy.ExecutionMode.Validate()
	}
}

func (d *RunDispatcher) Cancel(ctx context.Context, req scheduler.CancelDispatchRequest) error {
	if d.Scheduler == nil {
		return scheduler.ErrDispatchNotFound
	}
	return d.Scheduler.Cancel(ctx, req)
}

func (d *RunDispatcher) shouldInline(policy DispatchPolicy) bool {
	if !d.InlineModes[policy.ExecutionMode] {
		return false
	}
	if policy.ExecutionMode == ExecutionModeDirectAction && (policy.HasSideEffect || policy.ResumeRequired) {
		return false
	}
	return policy.EstimatedDuration <= d.Policy.InlineMaxDuration
}

func deadlineFromRun(req agentruntime.RunRequest) time.Time {
	if req.Definition.Timeout <= 0 {
		return time.Time{}
	}
	return time.Now().Add(req.Definition.Timeout)
}
