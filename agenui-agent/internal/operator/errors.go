package operator

import "errors"

var (
	ErrOperatorNotFound = errors.New("operator not found")
	errJSTimeout        = errors.New("js operator execution timeout")
	errContextCanceled  = errors.New("js operator execution context canceled")
)
