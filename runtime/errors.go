package runtime

import "fmt"

const (
	CodeBindingSourcePathInvalid       = "BINDING_SOURCE_PATH_INVALID"
	CodeBindingTargetPathInvalid       = "BINDING_TARGET_PATH_INVALID"
	CodeBindingWildcardMismatch        = "BINDING_WILDCARD_MISMATCH"
	CodeBindingRequiredValueMissing    = "BINDING_REQUIRED_VALUE_MISSING"
	CodeBindingTargetAssignFailed      = "BINDING_TARGET_ASSIGN_FAILED"
	CodeSourceJoinNotProven            = "SOURCE_JOIN_NOT_PROVEN"
	CodeOperatorNotPublished           = "OPERATOR_NOT_PUBLISHED"
	CodeOperatorSourceHashMismatch     = "OPERATOR_SOURCE_HASH_MISMATCH"
	CodeOperatorInputValidationFailed  = "OPERATOR_INPUT_VALIDATION_FAILED"
	CodeOperatorParamsValidationFailed = "OPERATOR_PARAMS_VALIDATION_FAILED"
	CodeOperatorExecutionFailed        = "OPERATOR_EXECUTION_FAILED"
	CodeOperatorOutputValidationFailed = "OPERATOR_OUTPUT_VALIDATION_FAILED"
	CodeOperatorTimeout                = "OPERATOR_TIMEOUT"
	CodeOperatorOutputTooLarge         = "OPERATOR_OUTPUT_TOO_LARGE"
	CodeFixedIndexOutOfRange           = "FIXED_INDEX_OUT_OF_RANGE"
	CodeMapKeyNotFound                 = "MAP_KEY_NOT_FOUND"
	CodePickNotFound                   = "PICK_NOT_FOUND"
	CodePickNotUnique                  = "PICK_NOT_UNIQUE"
)

// ExecutionError is the stable Runtime error envelope. Local Binding and
// Operator failures remain distinguishable from upstream data-source errors.
type ExecutionError struct {
	Code              string `json:"code"`
	Message           string `json:"message,omitempty"`
	BindingID         string `json:"bindingId,omitempty"`
	OperatorVersionID uint64 `json:"operatorVersionId,omitempty"`
	Retryable         bool   `json:"retryable"`
	Executed          bool   `json:"executed"`
	Cause             string `json:"cause,omitempty"`
}

func (e *ExecutionError) Error() string {
	if e == nil {
		return "runtime: unknown execution error"
	}
	message := e.Message
	if message == "" {
		message = e.Code
	}
	if e.Cause != "" {
		message += ": " + e.Cause
	}
	return fmt.Sprintf("runtime: %s: %s", e.Code, message)
}

func executionError(code, message, bindingID string, operatorID uint64, executed bool, cause error) *ExecutionError {
	err := &ExecutionError{
		Code: code, Message: message, BindingID: bindingID,
		OperatorVersionID: operatorID, Retryable: false, Executed: executed,
	}
	if cause != nil {
		err.Cause = cause.Error()
	}
	return err
}

type operatorFailure struct {
	code string
	err  error
}

func (e *operatorFailure) Error() string {
	if e == nil || e.err == nil {
		return "operator execution failed"
	}
	return e.err.Error()
}

func (e *operatorFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func failOperator(code string, err error) error {
	return &operatorFailure{code: code, err: err}
}
