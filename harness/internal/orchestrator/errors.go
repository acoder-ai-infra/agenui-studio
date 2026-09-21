package orchestrator

import "errors"

var (
	ErrMissingDependency         = errors.New("orchestrator dependency is missing")
	ErrInvalidRequest            = errors.New("invalid orchestrator request")
	ErrBindingPersistenceFailed  = errors.New("agent binding persistence failed")
	ErrBindingFailureWriteFailed = errors.New("agent binding failure persistence failed")
	ErrDispatchFailed            = errors.New("agent dispatch failed")
	ErrPreRuntimeFailureWrite    = errors.New("pre-runtime failure persistence failed")
	ErrResumeFailed              = errors.New("agent resume failed")
	ErrCancelFailed              = errors.New("agent cancellation failed")
)

// safeWrappedError 对外只显示稳定文案，同时保留 errors.Is/As 的内部诊断链。
type safeWrappedError struct {
	kind  error
	cause error
}

func (e *safeWrappedError) Error() string { return e.kind.Error() }
func (e *safeWrappedError) Unwrap() []error {
	return []error{e.kind, e.cause}
}

func safeWrap(kind, cause error) error {
	if cause == nil {
		return nil
	}
	return &safeWrappedError{kind: kind, cause: cause}
}
