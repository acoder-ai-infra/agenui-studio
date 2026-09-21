package agentregistry

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	CodeInvalidConfig       ErrorCode = "invalid_config"
	CodeNotFound            ErrorCode = "not_found"
	CodeDisabled            ErrorCode = "disabled"
	CodeDependencyMissing   ErrorCode = "dependency_missing"
	CodePolicyViolation     ErrorCode = "policy_violation"
	CodeRuntimeDryRunFailed ErrorCode = "runtime_dry_run_failed"
	CodeEvalGateFailed      ErrorCode = "eval_gate_failed"
	CodeConflict            ErrorCode = "conflict"
	CodeConfigDrift         ErrorCode = "config_drift"
	CodeLoadFailed          ErrorCode = "load_failed"
)

type RegistryError struct {
	Code    ErrorCode
	Message string
	Field   string
	Err     error
}

func (e *RegistryError) Error() string {
	if e == nil {
		return ""
	}
	if e.Field != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Field)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *RegistryError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func newError(code ErrorCode, field, msg string) *RegistryError {
	return &RegistryError{Code: code, Field: field, Message: msg}
}

func wrapError(code ErrorCode, field, msg string, err error) *RegistryError {
	return &RegistryError{Code: code, Field: field, Message: msg, Err: err}
}

func errorCode(err error) ErrorCode {
	var regErr *RegistryError
	if errors.As(err, &regErr) {
		return regErr.Code
	}
	return CodeInvalidConfig
}

func issueFromError(err error) ValidationIssue {
	var regErr *RegistryError
	if errors.As(err, &regErr) {
		return ValidationIssue{Code: regErr.Code, Field: regErr.Field, Message: regErr.Message}
	}
	return ValidationIssue{Code: CodeInvalidConfig, Message: err.Error()}
}
