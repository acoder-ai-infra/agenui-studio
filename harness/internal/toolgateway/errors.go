package toolgateway

import (
	"errors"
	"fmt"
)

type ErrorType string

const (
	ErrorTypeToolNotFound           ErrorType = "tool_not_found"
	ErrorTypeToolDisabled           ErrorType = "tool_disabled"
	ErrorTypeToolVersionNotFound    ErrorType = "tool_version_not_found"
	ErrorTypeSchemaValidationFailed ErrorType = "schema_validation_failed"
	ErrorTypeInvalidArgument        ErrorType = "invalid_argument"
	ErrorTypePermissionDenied       ErrorType = "permission_denied"
	ErrorTypeControlRequired        ErrorType = "control_required"
	ErrorTypeTimeout                ErrorType = "timeout"
	ErrorTypeCancelled              ErrorType = "cancelled"
	ErrorTypeRateLimited            ErrorType = "rate_limited"
	ErrorTypeUpstreamError          ErrorType = "upstream_error"
	ErrorTypeResultTooLarge         ErrorType = "result_too_large"
	ErrorTypeArtifactError          ErrorType = "artifact_error"
	ErrorTypeNormalizationFailed    ErrorType = "normalization_failed"
	ErrorTypeIdempotencyConflict    ErrorType = "idempotency_conflict"
	ErrorTypeDuplicateInflight      ErrorType = "duplicate_inflight"
	ErrorTypeTraceMissing           ErrorType = "trace_missing"
	ErrorTypeInternal               ErrorType = "internal_error"
)

type ToolError struct {
	Type      ErrorType
	Message   string
	Retryable bool
	Cause     error
}

func NewToolError(errorType ErrorType, message string, retryable bool, cause error) *ToolError {
	return &ToolError{
		Type:      errorType,
		Message:   message,
		Retryable: retryable,
		Cause:     cause,
	}
}

func (e *ToolError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return fmt.Sprintf("%s: %s", e.Type, e.Message)
	}
	return string(e.Type)
}

func (e *ToolError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func IsErrorType(err error, errorType ErrorType) bool {
	var toolErr *ToolError
	if !errors.As(err, &toolErr) {
		return false
	}
	return toolErr.Type == errorType
}

func AsToolError(err error, target **ToolError) bool {
	return errors.As(err, target)
}
