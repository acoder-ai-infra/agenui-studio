package anthropic

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

type wireError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type errorEnvelope struct {
	Type  string     `json:"type"`
	Error *wireError `json:"error"`
}

func classifyHTTPError(status int, raw []byte) *mg.AdapterError {
	var envelope errorEnvelope
	_ = json.Unmarshal(raw, &envelope)
	err := classifyWireError(envelope.Error, status)
	if err.Message == "anthropic provider error" && len(raw) > 0 {
		err.Message = fmt.Sprintf("anthropic provider HTTP %d", status)
	}
	return err
}

func classifyWireError(wire *wireError, status int) *mg.AdapterError {
	message := "anthropic provider error"
	errorType := ""
	if wire != nil {
		message = wire.Message
		errorType = wire.Type
		if message == "" {
			message = errorType
		}
	}
	lower := strings.ToLower(errorType + " " + message)
	result := &mg.AdapterError{Message: message, HTTPStatus: status, Class: mg.ErrorProvider4xx, Retryable: false}
	switch {
	case status == http.StatusTooManyRequests || strings.Contains(lower, "rate_limit"):
		result.Class, result.Retryable = mg.ErrorRateLimited, true
	case status >= 500 || status == 529 || strings.Contains(lower, "overloaded") || strings.Contains(lower, "api_error"):
		result.Class, result.Retryable = mg.ErrorProvider5xx, true
	case strings.Contains(lower, "context") && (strings.Contains(lower, "length") || strings.Contains(lower, "token")):
		result.Class, result.Retryable = mg.ErrorContextExceeded, true
	case status == 0:
		result.Class = mg.ErrorUnknown
	}
	return result
}
