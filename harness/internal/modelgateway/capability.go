package modelgateway

import (
	"context"
	"fmt"
)

type CapabilityRegistry interface {
	Validate(ctx context.Context, req ModelRequest, target ModelTarget) error
}

type DefaultCapabilityRegistry struct{}

func (DefaultCapabilityRegistry) Validate(_ context.Context, req ModelRequest, target ModelTarget) error {
	if err := validateModelOptions(req); err != nil {
		return err
	}
	cap := target.Capability
	if cap.Model == "" && cap.Provider == "" {
		return nil
	}
	if req.Streaming && !cap.Streaming {
		return &AdapterError{Message: fmt.Sprintf("model %s does not support streaming", target.Model), Class: ErrorProvider4xx, Retryable: false}
	}
	required := req.Required
	if len(req.ToolsSchema) > 0 {
		required.ToolCalling = true
	}
	if req.ResponseSchemaRef != "" {
		required.StructuredOutput = true
		required.JSONSchema = true
	}
	// G-A：response_format 属结构化输出能力。未在能力矩阵声明
	// structured_output 的模型不得静默收到该参数（fail closed 优于
	// 发出模型不支持的请求后拿到不可解析的输出）。
	if req.Options.ResponseFormat != "" {
		required.StructuredOutput = true
	}
	if required.ToolCalling && !cap.ToolCalling {
		return capabilityError(target, "tool calling")
	}
	if required.StructuredOutput && !cap.StructuredOutput {
		return capabilityError(target, "structured output")
	}
	if required.JSONSchema && !cap.JSONSchema {
		return capabilityError(target, "json schema")
	}
	switch req.Options.ReasoningMode {
	case ReasoningEnabled:
		if !cap.Reasoning {
			return capabilityError(target, "reasoning")
		}
	case ReasoningDisabled:
		if cap.Reasoning && !cap.ReasoningToggle {
			return capabilityError(target, "reasoning toggle")
		}
	case "", ReasoningAuto:
	}
	if required.Vision && !cap.Vision {
		return capabilityError(target, "vision")
	}
	requestedOutput := effectiveMaxOutputTokens(req)
	if requestedOutput > 0 && cap.Limits.MaxOutputTokens > 0 && requestedOutput > cap.Limits.MaxOutputTokens {
		return &AdapterError{Message: fmt.Sprintf("requested output tokens %d exceed model limit %d", requestedOutput, cap.Limits.MaxOutputTokens), Class: ErrorBudgetExceeded, Retryable: true}
	}
	return nil
}

func validateModelOptions(req ModelRequest) error {
	options := req.Options
	if options.Temperature != nil && *options.Temperature < 0 {
		return invalidModelOption("temperature must be non-negative")
	}
	if options.TopP != nil && (*options.TopP < 0 || *options.TopP > 1) {
		return invalidModelOption("top_p must be between 0 and 1")
	}
	if options.MaxTokens != nil {
		if *options.MaxTokens <= 0 {
			return invalidModelOption("max_tokens must be positive")
		}
		if req.MaxOutputTokens > 0 && *options.MaxTokens > req.MaxOutputTokens {
			return &AdapterError{Message: fmt.Sprintf("max_tokens %d exceed reserved output budget %d", *options.MaxTokens, req.MaxOutputTokens), Class: ErrorBudgetExceeded, Retryable: false}
		}
	}
	switch options.ToolChoice {
	case "", "auto", "none", "required":
	default:
		return invalidModelOption("tool_choice must be auto, none or required")
	}
	for _, stop := range options.Stop {
		if stop == "" {
			return invalidModelOption("stop must not contain empty values")
		}
	}
	switch options.ReasoningMode {
	case "", ReasoningAuto, ReasoningEnabled, ReasoningDisabled:
	default:
		return invalidModelOption(fmt.Sprintf("invalid reasoning mode %q", options.ReasoningMode))
	}
	if options.ReasoningBudget < 0 || (options.ReasoningMode == ReasoningDisabled && options.ReasoningBudget > 0) {
		return invalidModelOption("invalid reasoning budget")
	}
	if !validReasoningEffort(options.ReasoningEffort) || (options.ReasoningEffort != "" && options.ReasoningMode != ReasoningEnabled) {
		return invalidModelOption("reasoning_effort must be low, medium, high, xhigh or max and requires reasoning enabled")
	}
	return nil
}

func validReasoningEffort(value string) bool {
	switch value {
	case "", "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}

func invalidModelOption(message string) error {
	return &AdapterError{Message: message, Class: ErrorProvider4xx, Retryable: false}
}

func capabilityError(target ModelTarget, capability string) error {
	return &AdapterError{
		Message:   fmt.Sprintf("model %s/%s does not support required capability: %s", target.Provider, target.Model, capability),
		Class:     ErrorProvider4xx,
		Retryable: false,
	}
}

type TokenBudgetChecker interface {
	Check(ctx context.Context, req ModelRequest, target ModelTarget, diagnostics GatewayDiagnostics) error
}

type DefaultTokenBudgetChecker struct{}

func (DefaultTokenBudgetChecker) Check(_ context.Context, req ModelRequest, target ModelTarget, diagnostics GatewayDiagnostics) error {
	limit := req.MaxPromptTokens
	if target.Capability.Limits.MaxContextTokens > 0 {
		targetLimit := target.Capability.Limits.MaxContextTokens
		if output := effectiveMaxOutputTokens(req); output > 0 {
			targetLimit -= output
		}
		if limit <= 0 || targetLimit < limit {
			limit = targetLimit
		}
	}
	if limit <= 0 {
		return nil
	}
	if diagnostics.EstimatedTokens > limit {
		return &AdapterError{
			Message:   fmt.Sprintf("estimated prompt tokens %d exceed budget %d", diagnostics.EstimatedTokens, limit),
			Class:     ErrorContextExceeded,
			Retryable: true,
		}
	}
	return nil
}

func effectiveMaxOutputTokens(req ModelRequest) int {
	if req.Options.MaxTokens != nil {
		return *req.Options.MaxTokens
	}
	return req.MaxOutputTokens
}
