package agentgateway

import (
	"errors"
)

// GatewayErrorCode is a safe, stable error identifier surfaced to the caller as
// an event error code. All plugin errors are Retryable=false in P0.
type GatewayErrorCode string

const (
	CodeProviderUnavailable  GatewayErrorCode = "sub_agent_provider_unavailable"
	CodeProviderUnsupported  GatewayErrorCode = "sub_agent_provider_unsupported"
	CodeTargetUnauthorized   GatewayErrorCode = "sub_agent_target_unauthorized"
	CodeTargetResolveFailed  GatewayErrorCode = "sub_agent_target_resolve_failed"
	CodeTaskInputInvalid     GatewayErrorCode = "task_input_invalid"
	CodeTaskOutputInvalid    GatewayErrorCode = "task_output_invalid"
	CodeNestingLimit         GatewayErrorCode = "sub_agent_nesting_limit"
	CodePluginUnavailable    GatewayErrorCode = "gateway_plugin_unavailable"
	CodePluginConfigInvalid  GatewayErrorCode = "gateway_plugin_config_invalid"
	CodePluginFailed         GatewayErrorCode = "gateway_plugin_failed"
	CodePluginPostCallFailed GatewayErrorCode = "gateway_plugin_post_call_failed"

	// Remote A2A provider error codes.
	CodeRemoteTargetNotFound     GatewayErrorCode = "remote_target_not_found"
	CodeRemoteTargetInvalid      GatewayErrorCode = "remote_target_invalid"
	CodeRemoteCredentials        GatewayErrorCode = "remote_credentials_unavailable"
	CodeAgentCardDiscoveryFailed GatewayErrorCode = "agent_card_discovery_failed"
	CodeA2AProtocolIncompatible  GatewayErrorCode = "a2a_protocol_incompatible"
	CodeRemoteTransportFailed    GatewayErrorCode = "remote_transport_failed"
	CodeRemoteOutcomeUnknown     GatewayErrorCode = "remote_outcome_unknown"
	CodeRemoteTaskInvalid        GatewayErrorCode = "remote_task_invalid"
	CodeRemoteTaskFailed         GatewayErrorCode = "remote_task_failed"
	CodeRemoteTaskRejected       GatewayErrorCode = "remote_task_rejected"
	CodeRemoteTaskCancelled      GatewayErrorCode = "remote_task_cancelled"
	CodeRemoteInputRequired      GatewayErrorCode = "remote_input_required"
	CodeRemoteAuthRequired       GatewayErrorCode = "remote_auth_required"
	CodeRemoteStateUnsupported   GatewayErrorCode = "remote_state_unsupported"
	CodeRemoteResultUnsupported  GatewayErrorCode = "remote_result_unsupported"
	CodeRemoteResultTooLarge     GatewayErrorCode = "remote_result_too_large"
	CodeRemoteResultEmpty        GatewayErrorCode = "remote_result_empty"
	CodeRemoteTimeout            GatewayErrorCode = "remote_timeout"
)

// GatewayError is a typed, safe error carrying a stable code and an optional
// internal cause. The cause never leaks credentials, URLs, or user payloads.
type GatewayError struct {
	Code    GatewayErrorCode
	Message string
	cause   error
}

func (e *GatewayError) Error() string {
	if e == nil {
		return ""
	}
	base := string(e.Code)
	if e.Message != "" {
		base += ": " + e.Message
	}
	if e.cause != nil {
		base += ": " + e.cause.Error()
	}
	return base
}

// Unwrap exposes the internal cause so callers can errors.Is/As it.
func (e *GatewayError) Unwrap() error { return e.cause }

// Is matches by code so errors.Is against the sentinels below works.
func (e *GatewayError) Is(target error) bool {
	var other *GatewayError
	if !errors.As(target, &other) {
		return false
	}
	return e.Code == other.Code
}

func newGatewayError(code GatewayErrorCode, message string, cause error) *GatewayError {
	return &GatewayError{Code: code, Message: message, cause: cause}
}

// Sentinel errors for errors.Is matching.
var (
	ErrProviderUnavailable  = &GatewayError{Code: CodeProviderUnavailable}
	ErrProviderUnsupported  = &GatewayError{Code: CodeProviderUnsupported}
	ErrTargetUnauthorized   = &GatewayError{Code: CodeTargetUnauthorized}
	ErrTargetResolveFailed  = &GatewayError{Code: CodeTargetResolveFailed}
	ErrTaskInputInvalid     = &GatewayError{Code: CodeTaskInputInvalid}
	ErrTaskOutputInvalid    = &GatewayError{Code: CodeTaskOutputInvalid}
	ErrNestingLimit         = &GatewayError{Code: CodeNestingLimit}
	ErrPluginUnavailable    = &GatewayError{Code: CodePluginUnavailable}
	ErrPluginConfigInvalid  = &GatewayError{Code: CodePluginConfigInvalid}
	ErrPluginFailed         = &GatewayError{Code: CodePluginFailed}
	ErrPluginPostCallFailed = &GatewayError{Code: CodePluginPostCallFailed}
)

// SafeErrorCode extracts a safe, stable error code from a gateway error or
// falls back to a generic internal code.
func SafeErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var gwErr *GatewayError
	if errors.As(err, &gwErr) {
		return string(gwErr.Code)
	}
	return "gateway_internal_error"
}
