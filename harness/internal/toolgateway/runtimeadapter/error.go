package runtimeadapter

import (
	"context"
	"errors"
	"strings"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

// classifyInvocationError 是 Tool Gateway -> Runtime 的唯一错误翻译边界：
// cause 仅供内部诊断，Native/Eino 和规范事件只接收稳定且脱敏的 RuntimeError。
func classifyInvocationError(ctx context.Context, err error, fallbackCode string) *agentruntime.RuntimeError {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		err = ctx.Err()
	}

	var runtimeErr *agentruntime.RuntimeError
	if errors.As(err, &runtimeErr) {
		return agentruntime.DefaultRuntimeErrorClassifier{}.Classify(err, agentruntime.ErrorStageRuntimeAdapter)
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return agentruntime.NewRuntimeError(agentruntime.ErrorTimeout, "TOOL_TIMEOUT", "tool invocation timed out").
			WithRetryable(true).
			WithDependency("tool_gateway").
			WithCause(err)
	case errors.Is(err, context.Canceled):
		return agentruntime.NewRuntimeError(agentruntime.ErrorCancelled, "TOOL_CANCELLED", "tool invocation cancelled").
			WithDependency("tool_gateway").
			WithCause(err)
	case errors.Is(err, agentruntime.ErrProductionCapabilityUnavailable):
		return agentruntime.NewRuntimeError(agentruntime.ErrorDependencyUnavailable, "TOOL_CAPABILITY_UNAVAILABLE", "tool capability is temporarily unavailable").
			WithRetryable(true).
			WithDependency("capability_registry").
			WithCause(err)
	case errors.Is(err, agentruntime.ErrRuntimeCapabilityDrift),
		errors.Is(err, ErrModelContextSnapshotMissing),
		errors.Is(err, ErrToolSnapshotInvalid):
		return agentruntime.NewRuntimeError(agentruntime.ErrorSchemaValidation, "TOOL_SNAPSHOT_DRIFT", "tool capability snapshot drifted").
			WithCause(err)
	case errors.Is(err, ErrToolRouteUnauthorized):
		return agentruntime.NewRuntimeError(agentruntime.ErrorPermissionDenied, "TOOL_ROUTE_UNAUTHORIZED", "tool invocation is not permitted").
			WithCause(err)
	case errors.Is(err, ErrToolStepIdentityInvalid):
		return agentruntime.NewRuntimeError(agentruntime.ErrorSchemaValidation, "TOOL_STEP_ID_INVALID", "tool step identity is invalid").
			WithCause(err)
	case errors.Is(err, ErrMCPBindingInvalid),
		errors.Is(err, ErrInvocationPolicyInvalid),
		errors.Is(err, ErrInvocationInvalid),
		errors.Is(err, ErrToolReferenceInvalid):
		return agentruntime.NewRuntimeError(agentruntime.ErrorSchemaValidation, "TOOL_INVOCATION_INVALID", "tool invocation contract is invalid").
			WithCause(err)
	}

	var toolErr *toolgateway.ToolError
	if errors.As(err, &toolErr) {
		return runtimeErrorFromToolError(toolErr, err)
	}
	if fallbackCode == "" {
		fallbackCode = "TOOL_INVOKE_FAILED"
	}
	return agentruntime.NewRuntimeError(agentruntime.ErrorTool, fallbackCode, "tool invocation failed").WithCause(err)
}

func runtimeErrorFromToolError(toolErr *toolgateway.ToolError, cause error) *agentruntime.RuntimeError {
	errorType := agentruntime.ErrorTool
	retryable := toolErr.Retryable
	message := "tool invocation failed"
	dependency := ""

	switch toolErr.Type {
	case toolgateway.ErrorTypePermissionDenied, toolgateway.ErrorTypeControlRequired:
		errorType = agentruntime.ErrorPermissionDenied
		retryable = false
		message = "tool invocation is not permitted"
	case toolgateway.ErrorTypeSchemaValidationFailed:
		errorType = agentruntime.ErrorSchemaValidation
		retryable = false
		message = "tool arguments failed schema validation"
	case toolgateway.ErrorTypeInvalidArgument:
		errorType = agentruntime.ErrorSchemaValidation
		retryable = false
		message = "tool arguments failed business validation"
	case toolgateway.ErrorTypeTimeout:
		errorType = agentruntime.ErrorTimeout
		retryable = true
		message = "tool invocation timed out"
		dependency = "tool_gateway"
	case toolgateway.ErrorTypeCancelled:
		errorType = agentruntime.ErrorCancelled
		retryable = false
		message = "tool invocation cancelled"
		dependency = "tool_gateway"
	case toolgateway.ErrorTypeRateLimited:
		errorType = agentruntime.ErrorRateLimited
		retryable = true
		message = "tool invocation rate limited"
		dependency = "tool_gateway"
	case toolgateway.ErrorTypeUpstreamError:
		errorType = agentruntime.ErrorDependencyUnavailable
		message = "tool upstream dependency failed"
		dependency = "tool_gateway"
	case toolgateway.ErrorTypeToolNotFound,
		toolgateway.ErrorTypeToolDisabled,
		toolgateway.ErrorTypeToolVersionNotFound,
		toolgateway.ErrorTypeResultTooLarge,
		toolgateway.ErrorTypeNormalizationFailed,
		toolgateway.ErrorTypeIdempotencyConflict,
		toolgateway.ErrorTypeTraceMissing:
		retryable = false
	}

	runtimeErr := agentruntime.NewRuntimeError(errorType, toolRuntimeCode(toolErr.Type), message).
		WithRetryable(retryable).
		WithCause(cause)
	if dependency != "" {
		runtimeErr.WithDependency(dependency)
	}
	return runtimeErr
}

func toolRuntimeCode(errorType toolgateway.ErrorType) string {
	value := strings.ToUpper(strings.ReplaceAll(string(errorType), "-", "_"))
	if value == "" {
		value = "INTERNAL_ERROR"
	}
	return "TOOL_" + value
}
