package modelgateway

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type ModelErrorClass string

const (
	ErrorProvider4xx       ModelErrorClass = "provider_4xx"
	ErrorProvider5xx       ModelErrorClass = "provider_5xx"
	ErrorRateLimited       ModelErrorClass = "rate_limited"
	ErrorTimeout           ModelErrorClass = "timeout"
	ErrorContextExceeded   ModelErrorClass = "context_length_exceeded"
	ErrorContentFilter     ModelErrorClass = "content_filter"
	ErrorSchema            ModelErrorClass = "schema_error"
	ErrorToolCallInvalid   ModelErrorClass = "tool_call_invalid"
	ErrorQuotaExceeded     ModelErrorClass = "quota_exceeded"
	ErrorBudgetExceeded    ModelErrorClass = "budget_exceeded"
	ErrorModelUnavailable  ModelErrorClass = "model_unavailable"
	ErrorNetwork           ModelErrorClass = "network_error"
	ErrorStreamInterrupted ModelErrorClass = "stream_interrupted"
	ErrorUnknown           ModelErrorClass = "unknown"
)

type ErrorClassifier interface {
	Classify(ctx context.Context, err error) ClassifiedError
}

type ClassifiedError struct {
	Class      ModelErrorClass
	Code       string
	Message    string
	Retryable  bool
	HTTPStatus int
}

func (e ClassifiedError) EventError() *observability.EventError {
	return &observability.EventError{Code: e.Code, Type: observability.EventErrorType(e.Class), Message: e.Message, Retryable: e.Retryable}
}

type DefaultErrorClassifier struct{}

func (DefaultErrorClassifier) Classify(_ context.Context, err error) ClassifiedError {
	if err == nil {
		return ClassifiedError{Class: ErrorUnknown, Code: "MODEL_UNKNOWN", Message: "unknown model error"}
	}
	var ae *AdapterError
	if errors.As(err, &ae) {
		return classifyAdapterError(ae)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return classified(ErrorTimeout, "MODEL_TIMEOUT", err.Error(), true)
	}
	if errors.Is(err, context.Canceled) {
		return classified(ErrorStreamInterrupted, "MODEL_STREAM_INTERRUPTED", err.Error(), true)
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return classified(ErrorNetwork, "MODEL_NETWORK_ERROR", err.Error(), true)
	}
	return classifyMessage(err.Error(), true)
}

func classifyAdapterError(err *AdapterError) ClassifiedError {
	if err.Class != "" {
		code := codeForClass(err.Class)
		return ClassifiedError{Class: err.Class, Code: code, Message: err.Message, Retryable: err.Retryable, HTTPStatus: err.HTTPStatus}
	}
	if err.HTTPStatus == 429 {
		return ClassifiedError{Class: ErrorRateLimited, Code: codeForClass(ErrorRateLimited), Message: err.Message, Retryable: true, HTTPStatus: err.HTTPStatus}
	}
	if err.HTTPStatus >= 500 {
		return ClassifiedError{Class: ErrorProvider5xx, Code: codeForClass(ErrorProvider5xx), Message: err.Message, Retryable: true, HTTPStatus: err.HTTPStatus}
	}
	if err.HTTPStatus >= 400 {
		return ClassifiedError{Class: ErrorProvider4xx, Code: codeForClass(ErrorProvider4xx), Message: err.Message, Retryable: false, HTTPStatus: err.HTTPStatus}
	}
	return classifyMessage(err.Message, err.Retryable)
}

func classifyMessage(message string, retryable bool) ClassifiedError {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "throttl"), strings.Contains(lower, "rate limit"), strings.Contains(lower, "ratelimit"), strings.Contains(lower, "429"), strings.Contains(lower, "tpm"):
		return classified(ErrorRateLimited, codeForClass(ErrorRateLimited), message, true)
	case strings.Contains(lower, "context length"), strings.Contains(lower, "context_length"), strings.Contains(lower, "maximum context"), strings.Contains(lower, "token overflow"):
		return classified(ErrorContextExceeded, codeForClass(ErrorContextExceeded), message, true)
	case strings.Contains(lower, "content filter"), strings.Contains(lower, "safety"), strings.Contains(lower, "guardrail"):
		return classified(ErrorContentFilter, codeForClass(ErrorContentFilter), message, false)
	case strings.Contains(lower, "schema"), strings.Contains(lower, "json"):
		return classified(ErrorSchema, codeForClass(ErrorSchema), message, retryable)
	case strings.Contains(lower, "quota"):
		return classified(ErrorQuotaExceeded, codeForClass(ErrorQuotaExceeded), message, false)
	case strings.Contains(lower, "budget"):
		return classified(ErrorBudgetExceeded, codeForClass(ErrorBudgetExceeded), message, false)
	case strings.Contains(lower, "timeout"), strings.Contains(lower, "deadline"):
		return classified(ErrorTimeout, codeForClass(ErrorTimeout), message, true)
	default:
		return classified(ErrorUnknown, codeForClass(ErrorUnknown), message, retryable)
	}
}

func classified(class ModelErrorClass, code, message string, retryable bool) ClassifiedError {
	return ClassifiedError{Class: class, Code: code, Message: message, Retryable: retryable}
}

func codeForClass(class ModelErrorClass) string {
	switch class {
	case ErrorProvider4xx:
		return "MODEL_PROVIDER_4XX"
	case ErrorProvider5xx:
		return "MODEL_PROVIDER_5XX"
	case ErrorRateLimited:
		return "MODEL_RATE_LIMITED"
	case ErrorTimeout:
		return "MODEL_TIMEOUT"
	case ErrorContextExceeded:
		return "MODEL_CONTEXT_LENGTH_EXCEEDED"
	case ErrorContentFilter:
		return "MODEL_CONTENT_FILTER"
	case ErrorSchema:
		return "MODEL_SCHEMA_ERROR"
	case ErrorToolCallInvalid:
		return "MODEL_TOOL_CALL_INVALID"
	case ErrorQuotaExceeded:
		return "MODEL_QUOTA_EXCEEDED"
	case ErrorBudgetExceeded:
		return "MODEL_BUDGET_EXCEEDED"
	case ErrorModelUnavailable:
		return "MODEL_UNAVAILABLE"
	case ErrorNetwork:
		return "MODEL_NETWORK_ERROR"
	case ErrorStreamInterrupted:
		return "MODEL_STREAM_INTERRUPTED"
	default:
		return "MODEL_UNKNOWN_ERROR"
	}
}
