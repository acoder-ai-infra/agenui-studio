package toolgateway

import (
	"encoding/json"
	"errors"
)

// ToolInterruptedError suspends a ToolCall without completing or failing it.
// Info is safe user-facing control data; State is opaque tool-owned resume
// state. Runtime adapters translate this signal to their native interrupt.
type ToolInterruptedError struct {
	Info  any
	State json.RawMessage
	Cause error
}

func (e *ToolInterruptedError) Error() string {
	return "tool call suspended for control request"
}

func (e *ToolInterruptedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func IsToolInterrupted(err error) bool {
	var interrupted *ToolInterruptedError
	return errors.As(err, &interrupted)
}

func validateToolCallResume(resume *ToolCallResume) error {
	if resume == nil {
		return nil
	}
	if !resume.WasInterrupted || !resume.IsResumeTarget {
		return NewToolError(ErrorTypeSchemaValidationFailed, "tool resume target is invalid", false, nil)
	}
	if len(resume.State) > 0 && !json.Valid(resume.State) {
		return NewToolError(ErrorTypeSchemaValidationFailed, "tool resume state is invalid", false, nil)
	}
	if len(resume.Payload) > 0 && !json.Valid(resume.Payload) {
		return NewToolError(ErrorTypeSchemaValidationFailed, "tool resume payload is invalid", false, nil)
	}
	return nil
}
