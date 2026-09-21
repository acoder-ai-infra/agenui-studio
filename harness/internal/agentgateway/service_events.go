package agentgateway

import (
	"context"
	"encoding/json"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/identifiercontract"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type gatewayEventPayload struct {
	SchemaVersion string          `json:"schema_version"`
	TaskID        string          `json:"task_id"`
	AttemptID     string          `json:"attempt_id"`
	TargetAgentID string          `json:"target_agent_id"`
	Binding       ProviderBinding `json:"provider_binding"`
	Details       map[string]any  `json:"details,omitempty"`
}

func (s *Service) emit(ctx context.Context, sink agentruntime.SubAgentEventSink, req AuthorizedInvocation, eventType observability.EventType, details map[string]any, eventErr *observability.EventError) error {
	if sink == nil {
		return newGatewayError(CodeProviderUnavailable, "sub-agent event sink unavailable", nil)
	}
	payload, err := json.Marshal(gatewayEventPayload{
		SchemaVersion: GatewayTaskSchemaVersion, TaskID: req.InvocationID, AttemptID: req.AttemptID,
		TargetAgentID: req.SubAgentRef, Binding: req.Binding, Details: details,
	})
	if err != nil {
		return err
	}
	return sink.Emit(ctx, observability.AgentEvent{
		EventID: s.ids.NewEventID(), SchemaVersion: observability.AgentEventSchemaVersion,
		IdempotencyKey: identifiercontract.ComposeIdempotencyKey(req.ParentRunID, req.InvocationID, req.AttemptID, string(eventType)),
		TraceID:        req.Trace.TraceID, SpanID: req.Trace.SpanID, ParentSpanID: req.Trace.ParentSpanID,
		SessionID: req.SessionID, RunID: req.ParentRunID, AgentID: req.ParentAgentID,
		EventType: eventType, Visibility: observability.VisibilityDebug, Payload: payload,
		Error: eventErr, CreatedAt: time.Now().UTC(),
	})
}

func resultPayload(result agentruntime.SubAgentInvocationResult, facts *invocationFacts) map[string]any {
	details := map[string]any{"child_run_id": result.ChildRunID, "content_ref": result.ContentRef}
	if facts != nil {
		details["provider_outcome"] = facts.ProviderOutcome()
		details["audit_fields"] = facts.snapshotFields()
	}
	return details
}

func failurePayload(cause error, facts *invocationFacts) map[string]any {
	details := map[string]any{"error_code": SafeErrorCode(cause)}
	if facts != nil {
		details["provider_outcome"] = facts.ProviderOutcome()
		details["audit_fields"] = facts.snapshotFields()
	}
	return details
}

func a2aEventType(update RemoteTaskUpdate) observability.EventType {
	switch update.Kind {
	case RemoteTaskCreated:
		return observability.EventA2ATaskCreated
	case RemoteTaskProgress:
		return observability.EventA2ATaskProgress
	case RemoteTaskTerminal:
		switch update.Terminal {
		case RemoteTerminalCompleted:
			return observability.EventA2ATaskCompleted
		case RemoteTerminalCancelled:
			return observability.EventA2ATaskCancelled
		default:
			return observability.EventA2ATaskFailed
		}
	}
	return ""
}
