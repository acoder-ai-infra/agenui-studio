package agentbinding

import (
	"errors"
	"sort"
)

type ErrorCode string

const (
	CodeInvalidRequest           ErrorCode = "binding_invalid"
	CodeSourceInvalid            ErrorCode = "binding_source_invalid"
	CodeControlRuleInvalid       ErrorCode = "control_rule_invalid"
	CodeControlDenied            ErrorCode = "control_denied"
	CodeAskUserRequired          ErrorCode = "ask_user_required"
	CodeAgentNotFound            ErrorCode = "agent_not_found"
	CodeAgentDisabled            ErrorCode = "agent_disabled"
	CodeAgentVersionUnavailable  ErrorCode = "agent_version_unavailable"
	CodeAgentUnavailable         ErrorCode = "agent_unavailable"
	CodeConfigResolveFailed      ErrorCode = "config_resolve_failed"
	CodeConfigInvalid            ErrorCode = "config_invalid"
	CodeCapabilitySnapshotFailed ErrorCode = "capability_snapshot_failed"
	CodeExecutionModeUnsupported ErrorCode = "execution_mode_unsupported"
	CodeExecutionModeNotAllowed  ErrorCode = "execution_mode_not_allowed"
	CodeExecutionModeMismatch    ErrorCode = "execution_mode_mismatch"
	CodeTargetInvalid            ErrorCode = "target_invalid"
	CodeDefinitionInvalid        ErrorCode = "agent_definition_invalid"
	CodeConfigSnapshotMissing    ErrorCode = "config_snapshot_missing"
	CodeConfigHashMissing        ErrorCode = "config_hash_missing"
	CodeBindingHashMismatch      ErrorCode = "binding_hash_mismatch"
)

type FailureStage string

const (
	StageSourceSelection FailureStage = "source_selection"
	StageConfigResolve   FailureStage = "config_resolve"
	StageFinalize        FailureStage = "binding_finalize"
	StageStaticValidate  FailureStage = "static_validation"
)

type codedError interface {
	error
	BindingErrorCode() ErrorCode
	BindingSafeMessage() string
	BindingRetryable() bool
	BindingFailureStage() FailureStage
}

// Error 对外只暴露稳定安全文案；原始 cause 仅供 errors.Is/As 和内部观测使用。
type Error struct {
	code      ErrorCode
	stage     FailureStage
	retryable bool
	cause     error
}

func NewError(stage FailureStage, code ErrorCode, retryable bool, cause error) *Error {
	return &Error{code: code, stage: stage, retryable: retryable, cause: cause}
}

func (e *Error) Error() string                     { return SafeMessage(e.code) }
func (e *Error) Unwrap() error                     { return e.cause }
func (e *Error) BindingErrorCode() ErrorCode       { return e.code }
func (e *Error) BindingSafeMessage() string        { return SafeMessage(e.code) }
func (e *Error) BindingRetryable() bool            { return e.retryable }
func (e *Error) BindingFailureStage() FailureStage { return e.stage }

type ClarificationType string

const ClarificationAskUser ClarificationType = "ask_user"

// BindingClarification is a draft describing missing binding facts. It is not
// a durable ControlRequest and carries no checkpoint or resume authority.
type BindingClarification struct {
	Type          ClarificationType `json:"type"`
	Code          ErrorCode         `json:"code"`
	MissingFields []string          `json:"missing_fields"`
	SafeMessage   string            `json:"safe_message"`
}

var ErrAskUserRequired = errors.New("ask user required")

type AskUserError struct {
	Clarification BindingClarification
	stage         FailureStage
}

func NewAskUserError(stage FailureStage, missingFields ...string) *AskUserError {
	fields := append([]string(nil), missingFields...)
	sort.Strings(fields)
	return &AskUserError{
		stage: stage,
		Clarification: BindingClarification{
			Type:          ClarificationAskUser,
			Code:          CodeAskUserRequired,
			MissingFields: fields,
			SafeMessage:   SafeMessage(CodeAskUserRequired),
		},
	}
}

func (e *AskUserError) Error() string                     { return e.Clarification.SafeMessage }
func (e *AskUserError) Is(target error) bool              { return target == ErrAskUserRequired }
func (e *AskUserError) BindingErrorCode() ErrorCode       { return CodeAskUserRequired }
func (e *AskUserError) BindingSafeMessage() string        { return e.Clarification.SafeMessage }
func (e *AskUserError) BindingRetryable() bool            { return false }
func (e *AskUserError) BindingFailureStage() FailureStage { return e.stage }

func SafeMessage(code ErrorCode) string {
	switch code {
	case CodeInvalidRequest:
		return "agent binding request is invalid"
	case CodeSourceInvalid:
		return "agent binding source is invalid"
	case CodeControlRuleInvalid:
		return "control rule is invalid"
	case CodeControlDenied:
		return "agent binding is denied by control policy"
	case CodeAskUserRequired:
		return "more information is required to bind an agent"
	case CodeAgentNotFound:
		return "requested agent was not found"
	case CodeAgentDisabled:
		return "requested agent is disabled"
	case CodeAgentVersionUnavailable:
		return "requested agent version is unavailable"
	case CodeAgentUnavailable:
		return "requested agent is unavailable"
	case CodeConfigResolveFailed:
		return "agent configuration could not be resolved"
	case CodeConfigInvalid:
		return "resolved agent configuration is invalid"
	case CodeCapabilitySnapshotFailed:
		return "agent capability snapshot could not be resolved"
	case CodeExecutionModeUnsupported:
		return "execution mode is unsupported"
	case CodeExecutionModeNotAllowed:
		return "execution mode is not allowed for this agent"
	case CodeExecutionModeMismatch:
		return "execution mode does not match the resolved configuration"
	case CodeTargetInvalid:
		return "execution target is invalid"
	case CodeDefinitionInvalid:
		return "agent definition is invalid"
	case CodeConfigSnapshotMissing:
		return "configuration snapshot is missing"
	case CodeConfigHashMissing:
		return "configuration hash is missing"
	case CodeBindingHashMismatch:
		return "agent binding hash does not match its canonical content"
	default:
		return "agent binding failed"
	}
}

func CodeOf(err error) ErrorCode {
	if err == nil {
		return ""
	}
	var coded codedError
	if errors.As(err, &coded) {
		return coded.BindingErrorCode()
	}
	return CodeConfigResolveFailed
}

func StageOf(err error) FailureStage {
	if err == nil {
		return ""
	}
	var coded codedError
	if errors.As(err, &coded) {
		return coded.BindingFailureStage()
	}
	return StageConfigResolve
}

func SafeMessageOf(err error) string {
	if err == nil {
		return ""
	}
	var coded codedError
	if errors.As(err, &coded) {
		return coded.BindingSafeMessage()
	}
	return SafeMessage(CodeConfigResolveFailed)
}

func RetryableOf(err error) bool {
	var coded codedError
	return errors.As(err, &coded) && coded.BindingRetryable()
}

func IsAgentUnavailable(err error) bool {
	switch CodeOf(err) {
	case CodeAgentNotFound, CodeAgentDisabled, CodeAgentVersionUnavailable, CodeAgentUnavailable:
		return true
	default:
		return false
	}
}
