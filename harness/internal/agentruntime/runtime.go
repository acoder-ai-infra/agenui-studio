package agentruntime

import (
	"context"
	"errors"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storagewrite"
)

var (
	ErrProductionMemoryBackend       = errors.New("in-memory runtime backend forbidden in production")
	ErrProductionWriterUnverified    = errors.New("production storage writer must support startup validation")
	ErrProductionAssemblerUnverified = errors.New("production context assembler must support startup validation")
)

type AgentRuntime interface {
	Name() string
	Descriptor(ctx context.Context) RuntimeDescriptor
	Capabilities(ctx context.Context) RuntimeCapabilities
	ValidateConfig(ctx context.Context, def AgentDefinition) error
	Build(ctx context.Context, def AgentDefinition) (AgentHandle, error)
	Run(ctx context.Context, req RunRequest) (<-chan observability.AgentEvent, error)
	Resume(ctx context.Context, req ResumeRequest) (<-chan observability.AgentEvent, error)
	Cancel(ctx context.Context, req CancelRequest) error
	Health(ctx context.Context) RuntimeHealth
	Shutdown(ctx context.Context) error
}

type RuntimeStateManager interface {
	StartRun(ctx context.Context, req RunRequest) (*RunSnapshot, error)
	BindRuntime(ctx context.Context, runID string, binding RuntimeBinding) error
	BindRuntimeCapabilities(ctx context.Context, runID string, binding RuntimeCapabilityBinding) error
	RuntimeBinding(ctx context.Context, runID string) (RuntimeBinding, error)
	CompleteRun(ctx context.Context, runID string) error
	FailRun(ctx context.Context, runID string, err error) error
	CancelRun(ctx context.Context, req CancelRequest) error
	ExpireRun(ctx context.Context, runID string, err error) error
	EnterWaitingControl(ctx context.Context, req WaitingControlRequest) error
	// Resume claim 必须由生产 State Store 以同库本地事务/CAS 原子实现：
	// 校验凭证并写入 attemptID（fencing owner）。Fail/Activate 只能更新同一
	// attemptID；异常卡住时由外部触发修复或人工处理，不在进程内启动续租/扫描。
	BeginResume(ctx context.Context, req ResumeRequest, attemptID string, event observability.AgentEvent) error
	FailResumeAttempt(ctx context.Context, runID, attemptID string, event observability.AgentEvent, err error, retryable bool) error
	ActivateResume(ctx context.Context, runID, attemptID string) error
	StartStep(ctx context.Context, runID string, step StepStart) (*StepSnapshot, error)
	EnterStepWaitingControl(ctx context.Context, runID, stepID string) error
	CompleteStep(ctx context.Context, runID, stepID string) error
	FailStep(ctx context.Context, runID, stepID string, err error) error
	CancelStep(ctx context.Context, runID, stepID string) error
	AppendEvent(ctx context.Context, event observability.AgentEvent) (*EventAppendResult, error)
	MarkFallback(ctx context.Context, decision FallbackDecision) error
}

type RuntimeService struct {
	Runtime         AgentRuntime
	Runtimes        map[RuntimeType]AgentRuntime
	State           RuntimeStateManager
	Writer          StorageWriteExecutor
	ControlRequests ControlRequestCreator
	Assembler       RuntimeContextAssembler
	Logger          observability.StructuredLogger
	Tracer          observability.TraceProvider
	IDs             observability.IDGenerator
	Errors          RuntimeErrorClassifier
	Hooks           *RuntimeHookPipeline
	Finalization    FinalizationPolicy
	active          *activeRunRegistry
}

type StorageWriteExecutor interface {
	Execute(ctx context.Context, plan storagewrite.Plan) (storagewrite.Result, error)
}

type ControlRequestCreator interface {
	// The returned event is the exact user-visible event Runtime must publish.
	// Harness composition may add an opaque control ticket while keeping the
	// Runtime's plaintext resume token out of AgentEvent.
	CreateControlRequest(ctx context.Context, req ControlRequestCreateRequest) (observability.AgentEvent, error)
}

type ControlRequestCreateRequest struct {
	TenantID      string
	UserID        string
	SessionID     string
	RunID         string
	Type          string
	CheckpointID  string
	RequestID     string
	ResumeToken   string
	PromptPreview string
	Event         observability.AgentEvent
}

type productionValidator interface {
	ValidateProduction() error
}

// RuntimeInterruptBindingProvider keeps resume credentials out of AgentEvent.
// RuntimeService consumes the one-time binding when a canonical
// control_request_created event arrives.
type RuntimeInterruptBindingProvider interface {
	TakeInterruptBinding(ctx context.Context, event observability.AgentEvent) (WaitingControlRequest, error)
}

func NewRuntimeService(runtime AgentRuntime, state RuntimeStateManager, logger observability.StructuredLogger, tracer observability.TraceProvider) *RuntimeService {
	if logger == nil {
		logger = observability.NoopLogger{}
	}
	if tracer == nil {
		tracer = observability.NewNoopTracer("agentruntime")
	}
	hooks, _ := NewRuntimeHookPipeline()
	service := &RuntimeService{
		Runtime:      runtime,
		Runtimes:     make(map[RuntimeType]AgentRuntime),
		State:        state,
		Assembler:    NewDefaultRuntimeContextAssembler(nil),
		Logger:       logger,
		Tracer:       tracer,
		IDs:          observability.NewULIDGenerator(""),
		Errors:       DefaultRuntimeErrorClassifier{},
		Hooks:        hooks,
		Finalization: DefaultFinalizationPolicy(),
		active:       newActiveRunRegistry(),
	}
	if runtime != nil {
		service.RegisterRuntime(inferRuntimeType(runtime), runtime)
	}
	if state != nil {
		ports := NewRuntimeStateStorePorts(state)
		ports[storagewrite.StoreMessage] = storagewrite.NewMemoryPort(storagewrite.StoreMessage)
		ports[storagewrite.StoreArtifact] = storagewrite.NewMemoryPort(storagewrite.StoreArtifact)
		service.Writer = storagewrite.NewExecutor(ports)
	}
	return service
}

// NewProductionRuntimeService is the only supported production composition
// entrypoint. NewRuntimeService intentionally remains development/test friendly.
func NewProductionRuntimeService(
	runtime AgentRuntime,
	state RuntimeStateManager,
	writer StorageWriteExecutor,
	assembler RuntimeContextAssembler,
	logger observability.StructuredLogger,
	tracer observability.TraceProvider,
) (*RuntimeService, error) {
	service := NewRuntimeService(runtime, state, logger, tracer)
	service.Writer = writer
	service.Assembler = assembler
	if err := service.ValidateProduction(); err != nil {
		return nil, err
	}
	return service, nil
}

func (s *RuntimeService) ValidateProduction() error {
	if s == nil || !s.hasRuntime() {
		return ErrRuntimeMissing
	}
	if s.State == nil {
		return ErrStateMissing
	}
	if _, ok := s.State.(*InMemoryStateManager); ok {
		return ErrProductionMemoryBackend
	}
	writer, ok := s.Writer.(productionValidator)
	if !ok {
		return ErrProductionWriterUnverified
	}
	if err := writer.ValidateProduction(); err != nil {
		return err
	}
	assembler, ok := s.Assembler.(productionValidator)
	if !ok {
		return ErrProductionAssemblerUnverified
	}
	return assembler.ValidateProduction()
}

func (s *RuntimeService) RegisterRuntime(runtimeType RuntimeType, runtime AgentRuntime) {
	if runtime == nil {
		return
	}
	if s.Runtimes == nil {
		s.Runtimes = make(map[RuntimeType]AgentRuntime)
	}
	if runtimeType == "" || runtimeType == RuntimeTypeAuto {
		runtimeType = inferRuntimeType(runtime)
	}
	s.Runtimes[runtimeType] = runtime
	if s.Runtime == nil {
		s.Runtime = runtime
	}
}
