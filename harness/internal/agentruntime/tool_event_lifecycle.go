package agentruntime

import (
	"encoding/json"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// isPreExecutionToolFailure recognizes the only canonical tool terminal that
// may exist without tool_call_started: a failure explicitly stating that the
// executor was never entered.
func isPreExecutionToolFailure(event observability.AgentEvent) bool {
	if event.EventType != observability.EventToolCallFailed {
		return false
	}
	type failureEnvelope struct {
		ToolFailure struct {
			Executed *bool `json:"executed"`
		} `json:"tool_failure"`
	}
	for _, payload := range []json.RawMessage{event.PayloadPreview, event.Payload} {
		if len(payload) == 0 {
			continue
		}
		var envelope failureEnvelope
		if json.Unmarshal(payload, &envelope) == nil && envelope.ToolFailure.Executed != nil {
			return !*envelope.ToolFailure.Executed
		}
	}
	return false
}
