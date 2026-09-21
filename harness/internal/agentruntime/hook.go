package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var (
	ErrHookMissing      = errors.New("runtime hook missing")
	ErrHookIDMissing    = errors.New("runtime hook id required")
	ErrHookPointMissing = errors.New("runtime hook point required")
	ErrHookDuplicate    = errors.New("runtime hook duplicate")
	ErrHookDataConflict = errors.New("runtime hook scoped data conflict")
	ErrHookDataInvalid  = errors.New("runtime hook scoped data invalid")
	ErrHookPanic        = errors.New("runtime hook panic")
)

const DefaultRuntimeHookTimeout = 3 * time.Second

type RuntimeHookPoint string

const (
	HookBeforeBuild    RuntimeHookPoint = "runtime.before_build"
	HookAfterBuild     RuntimeHookPoint = "runtime.after_build"
	HookBeforeRun      RuntimeHookPoint = "runtime.before_run"
	HookAfterRun       RuntimeHookPoint = "runtime.after_run"
	HookBeforeStep     RuntimeHookPoint = "runtime.before_step"
	HookAfterStep      RuntimeHookPoint = "runtime.after_step"
	HookBeforeResponse RuntimeHookPoint = "runtime.before_response"
	HookOnError        RuntimeHookPoint = "runtime.on_error"
	HookOnInterrupt    RuntimeHookPoint = "runtime.on_interrupt"
	HookOnFallback     RuntimeHookPoint = "runtime.on_fallback"
)

type RuntimeHookFailurePolicy string

const (
	HookFailClosed RuntimeHookFailurePolicy = "fail_closed"
	HookFailOpen   RuntimeHookFailurePolicy = "fail_open"
)

type RuntimeHookInput struct {
	Point     RuntimeHookPoint
	Run       RunRequest
	Handle    *AgentHandle
	Fallback  *FallbackDecision
	Error     *RuntimeError
	Step      *RuntimeHookStep
	Interrupt *RuntimeHookInterrupt
	// Response 仅在 HookBeforeResponse 切点填充：携带累积的最终响应文本，
	// 供输出校验类 hook（如 OutputValidator 桥接器）在 final_response 落盘
	// 前实施 fail-closed 拦截。
	Response *RuntimeHookResponse
}

// RuntimeHookResponse 是 before_response 切点的只读响应视图。
type RuntimeHookResponse struct {
	// Text 是本次 Run 累积的最终响应文本。
	Text string
}

type RuntimeHookStep struct {
	StepID       string
	Kind         StepKind
	Name         string
	ParentStepID string
	Status       StepStatus
	Error        *RuntimeError
}

type RuntimeHookInterrupt struct {
	CheckpointID     string
	ControlRequestID string
	ControlType      string
}

type RuntimeHookOutput struct {
	ScopedData ScopedData
}

type RuntimeHook interface {
	Execute(ctx context.Context, input RuntimeHookInput) (RuntimeHookOutput, error)
}

type RuntimeHookFunc func(context.Context, RuntimeHookInput) (RuntimeHookOutput, error)

func (f RuntimeHookFunc) Execute(ctx context.Context, input RuntimeHookInput) (RuntimeHookOutput, error) {
	return f(ctx, input)
}

type RuntimeHookRegistration struct {
	ID             string
	Point          RuntimeHookPoint
	Priority       int
	FailurePolicy  RuntimeHookFailurePolicy
	AllowOverwrite bool
	Timeout        time.Duration
	Hook           RuntimeHook
}

type RuntimeHookPipeline struct {
	mu            sync.RWMutex
	registrations map[RuntimeHookPoint][]RuntimeHookRegistration
	ids           map[string]struct{}
}

type RuntimeHookSnapshot struct {
	registrations map[RuntimeHookPoint][]RuntimeHookRegistration
}

type runtimeHookSnapshotContextKey struct{}

func NewRuntimeHookPipeline(registrations ...RuntimeHookRegistration) (*RuntimeHookPipeline, error) {
	pipeline := &RuntimeHookPipeline{
		registrations: make(map[RuntimeHookPoint][]RuntimeHookRegistration),
		ids:           make(map[string]struct{}),
	}
	for _, registration := range registrations {
		if err := pipeline.Register(registration); err != nil {
			return nil, err
		}
	}
	return pipeline, nil
}

func (p *RuntimeHookPipeline) Register(registration RuntimeHookRegistration) error {
	if registration.Hook == nil {
		return ErrHookMissing
	}
	if registration.ID == "" {
		return ErrHookIDMissing
	}
	if registration.Point == "" {
		return ErrHookPointMissing
	}
	if !isRuntimeHookPoint(registration.Point) {
		return fmt.Errorf("%w: %s", ErrHookPointMissing, registration.Point)
	}
	if registration.FailurePolicy == "" {
		registration.FailurePolicy = HookFailClosed
	}
	if registration.FailurePolicy != HookFailClosed && registration.FailurePolicy != HookFailOpen {
		return fmt.Errorf("%w: failure_policy=%s", ErrHookDataInvalid, registration.FailurePolicy)
	}
	if registration.Timeout <= 0 {
		registration.Timeout = DefaultRuntimeHookTimeout
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.ids[registration.ID]; ok {
		return fmt.Errorf("%w: %s", ErrHookDuplicate, registration.ID)
	}
	p.ids[registration.ID] = struct{}{}
	p.registrations[registration.Point] = append(p.registrations[registration.Point], registration)
	sort.SliceStable(p.registrations[registration.Point], func(i, j int) bool {
		left := p.registrations[registration.Point][i]
		right := p.registrations[registration.Point][j]
		if left.Priority == right.Priority {
			return left.ID < right.ID
		}
		return left.Priority < right.Priority
	})
	return nil
}

func isRuntimeHookPoint(point RuntimeHookPoint) bool {
	switch point {
	case HookBeforeBuild, HookAfterBuild, HookBeforeRun, HookAfterRun, HookBeforeStep, HookAfterStep,
		HookBeforeResponse, HookOnError, HookOnInterrupt, HookOnFallback:
		return true
	default:
		return false
	}
}

func (p *RuntimeHookPipeline) Registrations(point RuntimeHookPoint) []RuntimeHookRegistration {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	registrations := p.registrations[point]
	out := make([]RuntimeHookRegistration, len(registrations))
	copy(out, registrations)
	return out
}

func (p *RuntimeHookPipeline) Snapshot() RuntimeHookSnapshot {
	snapshot := RuntimeHookSnapshot{registrations: make(map[RuntimeHookPoint][]RuntimeHookRegistration)}
	if p == nil {
		return snapshot
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for point, registrations := range p.registrations {
		copied := make([]RuntimeHookRegistration, len(registrations))
		copy(copied, registrations)
		snapshot.registrations[point] = copied
	}
	return snapshot
}

func (s RuntimeHookSnapshot) Registrations(point RuntimeHookPoint) []RuntimeHookRegistration {
	registrations := s.registrations[point]
	out := make([]RuntimeHookRegistration, len(registrations))
	copy(out, registrations)
	return out
}

func WithRuntimeHookSnapshot(ctx context.Context, snapshot RuntimeHookSnapshot) context.Context {
	return context.WithValue(ctx, runtimeHookSnapshotContextKey{}, snapshot)
}

func RuntimeHookSnapshotFrom(ctx context.Context) (RuntimeHookSnapshot, bool) {
	snapshot, ok := ctx.Value(runtimeHookSnapshotContextKey{}).(RuntimeHookSnapshot)
	return snapshot, ok
}
