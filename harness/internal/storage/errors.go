package storage

import (
	"errors"
	"fmt"
)

// ErrorCode is a stable machine-readable classification for storage errors.
type ErrorCode string

const (
	ErrInvalidArgument       ErrorCode = "invalid_argument"
	ErrNotFound              ErrorCode = "not_found"
	ErrConflict              ErrorCode = "conflict"
	ErrCASMismatch           ErrorCode = "cas_mismatch"
	ErrIdempotentReplay      ErrorCode = "idempotent_replay"
	ErrPermissionDenied      ErrorCode = "permission_denied"
	ErrTenantMismatch        ErrorCode = "tenant_mismatch"
	ErrIllegalTransition     ErrorCode = "illegal_transition"
	ErrUnsupportedCapability ErrorCode = "unsupported_capability"
	ErrResumeBindingMismatch ErrorCode = "resume_binding_mismatch"
	ErrResumeClaimHeld       ErrorCode = "resume_claim_held"
	ErrResumeClaimLost       ErrorCode = "resume_claim_lost"
)

// Error is the canonical storage error carrying an ErrorCode.
type Error struct {
	Code    ErrorCode
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Code, e.Err)
	}
	return string(e.Code)
}

func (e *Error) Unwrap() error { return e.Err }

func errorf(code ErrorCode, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// NewError builds a storage Error with the given code and message. Backends and
// sibling packages (memory, mysql, control) use this to return canonical errors.
func NewError(code ErrorCode, message string) error {
	return &Error{Code: code, Message: message}
}

// IsErrorCode reports whether err is a storage Error with the given code.
func IsErrorCode(err error, code ErrorCode) bool {
	if err == nil {
		return false
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code == code
	}
	return false
}
