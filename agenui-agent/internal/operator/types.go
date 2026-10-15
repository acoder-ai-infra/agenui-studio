package operator

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const (
	CodeOperatorIDEmpty         = "operator_id_empty"
	CodeOperatorDetailFetchFail = "operator_detail_fetch_failed"
	CodeOperatorNotFound        = "operator_not_found"
	CodeOperatorNotActive       = "operator_not_active"
	CodeOperatorInputType       = "operator_input_type_mismatch"
	CodeOperatorCodeEmpty       = "operator_code_empty"
	CodeUnsupportedLanguage     = "unsupported_language"
	CodeJSCompileFailed         = "js_compile_failed"
	CodeJSEntryMissing          = "js_entry_missing"
	CodeJSExecuteFailed         = "js_execute_failed"
	CodeJSExecuteTimeout        = "js_execute_timeout"
	CodeJSResultInvalid         = "js_result_invalid"
	CodeBuiltinMissing          = "builtin_missing"
	CodeBuiltinExecuteFailed    = "builtin_execute_failed"
	CodeBuiltinResultInvalid    = "builtin_result_invalid"
	CodePanicRecovered          = "panic_recovered"
)

const (
	defaultJSTimeout      = 100 * time.Millisecond
	defaultMaxInputBytes  = 64 * 1024
	defaultMaxOutputBytes = 64 * 1024
	maxJSTimeout          = 500 * time.Millisecond
)

// ExecuteRequest identifies one operator invocation against one field value.
type ExecuteRequest struct {
	OperatorID uint64
	Value      any
	Context    map[string]any
}

// ExecuteResult is total: every failure returns the original field value with
// a structured error instead of propagating an execution error to the caller.
type ExecuteResult struct {
	OperatorID    uint64
	Language      string
	OriginalValue any
	Value         any
	Applied       bool
	Error         *ErrorDetail
}

type ErrorDetail struct {
	Code    string
	Message string
	Cause   string
}

type OperatorDetail struct {
	ID                string
	OperatorVersionID uint64
	OperatorKey       string
	Version           string
	InputSchema       json.RawMessage
	ParamsSchema      json.RawMessage
	OutputSchema      json.RawMessage
	SourceHash        string
	Language          string
	LanguageVersion   string
	Status            string
	Code              string
	Entry             string
	Config            map[string]any
	Limits            Limits
}

type Limits struct {
	TimeoutMS      int
	MaxInputBytes  int
	MaxOutputBytes int
}

func successResult(
	operatorID uint64,
	language string,
	original any,
	value any,
) ExecuteResult {
	return ExecuteResult{
		OperatorID: operatorID, Language: language, OriginalValue: original,
		Value: value, Applied: true,
	}
}

func fallbackResult(
	operatorID uint64,
	language string,
	original any,
	code string,
	message string,
	cause string,
) ExecuteResult {
	return ExecuteResult{
		OperatorID:    operatorID,
		Language:      language,
		OriginalValue: original,
		Value:         original,
		Applied:       false,
		Error: &ErrorDetail{
			Code: code, Message: message, Cause: cause,
		},
	}
}

func operatorVersionID(detail OperatorDetail) uint64 {
	if detail.OperatorVersionID != 0 {
		return detail.OperatorVersionID
	}
	id, err := strconv.ParseUint(strings.TrimSpace(detail.ID), 10, 64)
	if err != nil {
		return 0
	}
	return id
}
