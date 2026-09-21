package extension

import "fmt"

// FunctionErrorType is the public, runtime-neutral failure taxonomy available
// to host function tools. The composition root translates it to Tool Gateway's
// internal error contract without exposing internal packages to SDK consumers.
type FunctionErrorType string

const (
	FunctionErrorInvalidArgument  FunctionErrorType = "invalid_argument"
	FunctionErrorPermissionDenied FunctionErrorType = "permission_denied"
	FunctionErrorUnavailable      FunctionErrorType = "unavailable"
	FunctionErrorInternal         FunctionErrorType = "internal"
)

type FunctionError struct {
	Type      FunctionErrorType
	Message   string
	Retryable bool
	Cause     error
}

func NewFunctionError(errorType FunctionErrorType, message string, retryable bool, cause error) *FunctionError {
	return &FunctionError{Type: errorType, Message: message, Retryable: retryable, Cause: cause}
}

func (e *FunctionError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return string(e.Type)
	}
	return fmt.Sprintf("%s: %s", e.Type, e.Message)
}

func (e *FunctionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
