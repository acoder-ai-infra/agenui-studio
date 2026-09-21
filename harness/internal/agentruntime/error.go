package agentruntime

import (
	"context"
	"errors"

	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

var ErrControlRequestCreatorMissing = errors.New("control request creator missing")

type ErrorType string

const (
	ErrorRuntime               ErrorType = "runtime_error"
	ErrorModel                 ErrorType = "model_error"
	ErrorTool                  ErrorType = "tool_error"
	ErrorProtocol              ErrorType = "protocol_error"
	ErrorGuardrailBlock        ErrorType = "guardrail_block"
	ErrorPermissionDenied      ErrorType = "permission_denied"
	ErrorControlTimeout        ErrorType = "control_timeout"
	ErrorContextOverflow       ErrorType = "context_overflow"
	ErrorSchemaValidation      ErrorType = "schema_validation_failed"
	ErrorCheckpoint            ErrorType = "checkpoint_error"
	ErrorRateLimited           ErrorType = "rate_limited"
	ErrorDependencyUnavailable ErrorType = "dependency_unavailable"
	ErrorTimeout               ErrorType = "timeout"
	ErrorCancelled             ErrorType = "cancelled"
	ErrorUnknown               ErrorType = "unknown"
)

type RuntimeErrorStage string

const (
	ErrorStageRuntimeSelection RuntimeErrorStage = "runtime_selection"
	ErrorStageRuntimeAdapter   RuntimeErrorStage = "runtime_adapter"
	ErrorStageModelContext     RuntimeErrorStage = "model_context"
	ErrorStageResume           RuntimeErrorStage = "resume"
	ErrorStageState            RuntimeErrorStage = "state"
	ErrorStageHook             RuntimeErrorStage = "hook"
	ErrorStageFinalization     RuntimeErrorStage = "finalization"
)

type RuntimeError struct {
	Type        ErrorType `json:"type"`
	Code        string    `json:"code"`
	Message     string    `json:"message"`
	Retryable   bool      `json:"retryable"`
	UserVisible bool      `json:"user_visible"`
	Degraded    bool      `json:"degraded"`
	Dependency  string    `json:"dependency,omitempty"`
	DetailRef   string    `json:"detail_ref,omitempty"`
	// RetryBudget 和 RepairFeedback 仅用于 OutputValidator 请求一次模型
	// 修复时的内部重试协议。它们随 run_failed payload 传给调用方，不写入
	// EventError，避免把扩展的业务反馈混入稳定错误摘要。
	RetryBudget    int    `json:"retry_budget,omitempty"`
	RepairFeedback string `json:"repair_feedback,omitempty"`
	Cause          error  `json:"-"`
}

// OutputRetryError is returned by the output-validation hook when a validator
// asks the Harness to make another model attempt. It carries only bounded,
// validator-authored feedback; it never contains the rejected model output.
type OutputRetryError struct {
	ValidatorID    string
	Reason         string
	RepairFeedback string
	RetryBudget    int
}

func NewOutputRetryError(validatorID, reason, repairFeedback string, retryBudget int) *OutputRetryError {
	return &OutputRetryError{ValidatorID: validatorID, Reason: reason, RepairFeedback: repairFeedback, RetryBudget: retryBudget}
}

func (e *OutputRetryError) Error() string {
	if e == nil {
		return ""
	}
	reason := e.Reason
	if reason == "" {
		reason = "output validation requires repair"
	}
	return "output validator " + e.ValidatorID + " requested retry: " + reason
}

func NewRuntimeError(errorType ErrorType, code, message string) *RuntimeError {
	return &RuntimeError{Type: errorType, Code: code, Message: message}
}

func (e *RuntimeError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

func (e *RuntimeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *RuntimeError) WithCause(cause error) *RuntimeError {
	e.Cause = cause
	return e
}

func (e *RuntimeError) WithRetryable(retryable bool) *RuntimeError {
	e.Retryable = retryable
	return e
}

func (e *RuntimeError) WithDependency(dependency string) *RuntimeError {
	e.Dependency = dependency
	return e
}

func (e *RuntimeError) WithUserVisible(userVisible bool) *RuntimeError {
	e.UserVisible = userVisible
	return e
}

func (e *RuntimeError) WithDegraded(degraded bool) *RuntimeError {
	e.Degraded = degraded
	return e
}

func (e *RuntimeError) WithDetailRef(detailRef string) *RuntimeError {
	e.DetailRef = detailRef
	return e
}

func (e *RuntimeError) EventError() *observability.EventError {
	if e == nil {
		return nil
	}
	return &observability.EventError{Code: e.Code, Type: runtimeToEventErrorType(e.Type), Message: e.Message, Retryable: e.Retryable}
}

func (e *RuntimeError) Payload() map[string]any {
	if e == nil {
		return nil
	}
	return map[string]any{
		"type":            e.Type,
		"code":            e.Code,
		"message":         e.Message,
		"retryable":       e.Retryable,
		"user_visible":    e.UserVisible,
		"degraded":        e.Degraded,
		"dependency":      e.Dependency,
		"detail_ref":      e.DetailRef,
		"retry_budget":    e.RetryBudget,
		"repair_feedback": e.RepairFeedback,
	}
}

type RuntimeErrorClassifier interface {
	Classify(err error, stage RuntimeErrorStage) *RuntimeError
}

type DefaultRuntimeErrorClassifier struct{}

func (DefaultRuntimeErrorClassifier) Classify(err error, stage RuntimeErrorStage) *RuntimeError {
	if err == nil {
		return nil
	}
	var typed *RuntimeError
	if errors.As(err, &typed) {
		if typed == err {
			return typed
		}
		copy := *typed
		copy.Cause = err
		return &copy
	}
	var outputRetry *OutputRetryError
	if errors.As(err, &outputRetry) {
		return NewRuntimeError(ErrorSchemaValidation, "OUTPUT_VALIDATION_RETRY", "output validation requires repair").
			WithRetryable(true).
			WithCause(err).
			withOutputRetry(outputRetry)
	}
	switch {
	case isMCPAuthorizationRequired(err):
		return NewRuntimeError(ErrorDependencyUnavailable, "MCP_AUTHORIZATION_REQUIRED", "MCP OAuth authorization is required").WithRetryable(true).WithUserVisible(true).WithDependency("mcp_oauth").WithCause(err)
	case errors.Is(err, context.DeadlineExceeded):
		return NewRuntimeError(ErrorTimeout, "RUNTIME_TIMEOUT", "runtime execution timed out").WithRetryable(true).WithCause(err)
	case errors.Is(err, context.Canceled):
		return NewRuntimeError(ErrorCancelled, "RUN_CANCELLED", "runtime execution cancelled").WithCause(err)
	case errors.Is(err, ErrRuntimeUnavailable), errors.Is(err, ErrRuntimeMissing):
		return NewRuntimeError(ErrorDependencyUnavailable, "RUNTIME_UNAVAILABLE", "runtime unavailable").WithRetryable(true).WithDependency("runtime").WithCause(err)
	case errors.Is(err, ErrRuntimeBindingMismatch):
		return NewRuntimeError(ErrorCheckpoint, "RUNTIME_BINDING_MISMATCH", "runtime binding drifted").WithCause(err)
	case errors.Is(err, ErrRuntimeBindingInvalid), errors.Is(err, ErrRuntimeBindingMissing):
		return NewRuntimeError(ErrorCheckpoint, "RUNTIME_BINDING_INVALID", "runtime binding is invalid").WithCause(err)
	case errors.Is(err, ErrResumeBindingMismatch):
		return NewRuntimeError(ErrorCheckpoint, "RESUME_BINDING_MISMATCH", "resume validation failed").WithCause(err)
	case errors.Is(err, ErrResumeAlreadyClaimed):
		return NewRuntimeError(ErrorCheckpoint, "RESUME_ALREADY_CLAIMED", "resume request already claimed").WithCause(err)
	case errors.Is(err, ErrResumeClaimLost):
		return NewRuntimeError(ErrorCheckpoint, "RESUME_CLAIM_LOST", "resume claim ownership lost").WithRetryable(true).WithCause(err)
	case errors.Is(err, ErrCheckpointIDMissing), errors.Is(err, ErrControlRequestIDMissing), errors.Is(err, ErrResumeTokenMissing), errors.Is(err, ErrResumeAttemptIDMissing):
		return NewRuntimeError(ErrorCheckpoint, "RESUME_CREDENTIALS_MISSING", "resume credentials missing").WithCause(err)
	case errors.Is(err, ErrHookDataConflict):
		return NewRuntimeError(ErrorSchemaValidation, "RUNTIME_HOOK_DATA_CONFLICT", "runtime hook data conflict").WithCause(err)
	case errors.Is(err, ErrHookDataInvalid):
		return NewRuntimeError(ErrorSchemaValidation, "RUNTIME_HOOK_DATA_INVALID", "runtime hook data invalid").WithCause(err)
	case errors.Is(err, ErrSystemPromptBudgetExceeded):
		return NewRuntimeError(ErrorContextOverflow, "SYSTEM_PROMPT_BUDGET_EXCEEDED", "system prompt exceeds model context budget").WithCause(err)
	case errors.Is(err, ErrProductionContextUnavailable):
		return NewRuntimeError(ErrorDependencyUnavailable, "CONTEXT_SNAPSHOT_UNAVAILABLE", "context snapshot unavailable").WithRetryable(true).WithDependency("context_store").WithCause(err)
	case errors.Is(err, ErrProductionCapabilityUnavailable):
		return NewRuntimeError(ErrorDependencyUnavailable, "CAPABILITY_SNAPSHOT_UNAVAILABLE", "capability snapshot unavailable").WithRetryable(true).WithDependency("capability_registry").WithCause(err)
	case errors.Is(err, ErrRuntimeCapabilityDrift):
		return NewRuntimeError(ErrorSchemaValidation, "CAPABILITY_SNAPSHOT_DRIFT", "runtime capability snapshot drifted").WithCause(err)
	case errors.Is(err, contextpkg.ErrBudgetExceeded):
		return NewRuntimeError(ErrorContextOverflow, "MODEL_CONTEXT_BUDGET_EXCEEDED", "model context exceeds input budget").WithCause(err)
	}
	switch stage {
	case ErrorStageRuntimeSelection:
		return NewRuntimeError(ErrorDependencyUnavailable, "RUNTIME_UNAVAILABLE", "runtime unavailable").WithRetryable(true).WithDependency("runtime").WithCause(err)
	case ErrorStageModelContext:
		return NewRuntimeError(ErrorRuntime, "MODEL_CONTEXT_BUILD_FAILED", "model context build failed").WithCause(err)
	case ErrorStageResume:
		return NewRuntimeError(ErrorCheckpoint, "RESUME_FAILED", "runtime resume failed").WithCause(err)
	case ErrorStageState:
		return NewRuntimeError(ErrorDependencyUnavailable, "RUNTIME_STATE_WRITE_FAILED", "runtime state update failed").WithRetryable(true).WithDependency("state_store").WithCause(err)
	case ErrorStageHook:
		return NewRuntimeError(ErrorRuntime, "RUNTIME_HOOK_FAILED", "runtime hook failed").WithCause(err)
	case ErrorStageFinalization:
		return NewRuntimeError(ErrorDependencyUnavailable, "FINALIZATION_WRITE_FAILED", "final response persistence failed").WithRetryable(true).WithDependency("storage").WithCause(err)
	default:
		return NewRuntimeError(ErrorRuntime, "RUNTIME_ADAPTER_FAILED", "runtime adapter failed").WithCause(err)
	}
}

func (e *RuntimeError) withOutputRetry(retry *OutputRetryError) *RuntimeError {
	if e == nil || retry == nil {
		return e
	}
	e.RetryBudget = retry.RetryBudget
	e.RepairFeedback = retry.RepairFeedback
	return e
}

func isMCPAuthorizationRequired(err error) bool {
	var required *mcp.AuthorizationRequiredError
	return errors.As(err, &required)
}

func runtimeErrorFromEvent(eventError *observability.EventError) *RuntimeError {
	if eventError == nil || eventError.Code == "" {
		return DefaultRuntimeErrorClassifier{}.Classify(errors.New("runtime emitted unclassified failure"), ErrorStageRuntimeAdapter)
	}
	errorType, ok := eventToRuntimeErrorType(eventError.Type)
	if !ok {
		return DefaultRuntimeErrorClassifier{}.Classify(errors.New("runtime emitted unclassified failure"), ErrorStageRuntimeAdapter)
	}
	message := eventError.Message
	if message == "" {
		message = "runtime adapter failed"
	}
	return &RuntimeError{Type: errorType, Code: eventError.Code, Message: message, Retryable: eventError.Retryable}
}

func runtimeToEventErrorType(errorType ErrorType) observability.EventErrorType {
	switch errorType {
	case ErrorTimeout, ErrorControlTimeout:
		return observability.EventErrorTimeout
	case ErrorCancelled:
		return observability.EventErrorCancelled
	case ErrorPermissionDenied:
		return observability.EventErrorPermissionDenied
	case ErrorGuardrailBlock:
		return observability.EventErrorGuardrailBlocked
	case ErrorSchemaValidation:
		return observability.EventErrorSchemaValidation
	case ErrorRateLimited:
		return observability.EventErrorRateLimited
	case ErrorContextOverflow:
		return observability.EventErrorResourceExhausted
	case ErrorModel, ErrorTool, ErrorProtocol, ErrorDependencyUnavailable:
		return observability.EventErrorUpstream
	default:
		return observability.EventErrorInternal
	}
}

func eventToRuntimeErrorType(errorType observability.EventErrorType) (ErrorType, bool) {
	switch errorType {
	case observability.EventErrorTimeout:
		return ErrorTimeout, true
	case observability.EventErrorCancelled:
		return ErrorCancelled, true
	case observability.EventErrorPermissionDenied:
		return ErrorPermissionDenied, true
	case observability.EventErrorGuardrailBlocked:
		return ErrorGuardrailBlock, true
	case observability.EventErrorSchemaValidation:
		return ErrorSchemaValidation, true
	case observability.EventErrorRateLimited:
		return ErrorRateLimited, true
	case observability.EventErrorResourceExhausted:
		return ErrorContextOverflow, true
	case observability.EventErrorUpstream:
		return ErrorDependencyUnavailable, true
	case observability.EventErrorType("provider_4xx"),
		observability.EventErrorType("provider_5xx"),
		observability.EventErrorType("context_length_exceeded"),
		observability.EventErrorType("content_filter"),
		observability.EventErrorType("schema_error"),
		observability.EventErrorType("tool_call_invalid"),
		observability.EventErrorType("quota_exceeded"),
		observability.EventErrorType("budget_exceeded"),
		observability.EventErrorType("network_error"),
		observability.EventErrorType("stream_interrupted"),
		observability.EventErrorType("model_unavailable"):
		return ErrorModel, true
	case observability.EventErrorInternal:
		return ErrorRuntime, true
	default:
		return ErrorUnknown, false
	}
}
