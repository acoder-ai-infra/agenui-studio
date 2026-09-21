package artifact

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrInvalidArgument  ErrorCode = "invalid_argument"
	ErrNotFound         ErrorCode = "not_found"
	ErrConflict         ErrorCode = "conflict"
	ErrPermissionDenied ErrorCode = "permission_denied"
	ErrDeleted          ErrorCode = "deleted"
	ErrTooLarge         ErrorCode = "too_large"
)

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

func (e *Error) Unwrap() error {
	return e.Err
}

func errorf(code ErrorCode, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func IsErrorCode(err error, code ErrorCode) bool {
	var target *Error
	return errors.As(err, &target) && target.Code == code
}
