package agentgateway

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// RemoteInvocationRequest is the protocol-neutral input for a remote call. It
// carries only the invocation id, remote tenant, text input, and safe tracing;
// it never copies the full A2A schema or local session/run identity.
type RemoteInvocationRequest struct {
	InvocationID string
	Description  string
	Trace        observability.TraceContext
}

// RemoteInvocationResult is the protocol-neutral output. ChildRunID is always
// empty for remote calls; TaskID/ContextID/State are present only when the
// remote returned a Task.
type RemoteInvocationResult struct {
	Content   string
	TaskID    string
	ContextID string
	State     string
}

// RemoteTaskUpdateKind enumerates the neutral task update kinds.
type RemoteTaskUpdateKind string

const (
	RemoteTaskCreated  RemoteTaskUpdateKind = "created"
	RemoteTaskProgress RemoteTaskUpdateKind = "progress"
	RemoteTaskTerminal RemoteTaskUpdateKind = "terminal"
)

// RemoteTerminalOutcome is the protocol-neutral classification of a terminal
// remote task, decided by the provider so the Service never inspects A2A SDK
// state values. It is empty for non-terminal updates.
type RemoteTerminalOutcome string

const (
	RemoteTerminalCompleted RemoteTerminalOutcome = "completed"
	RemoteTerminalFailed    RemoteTerminalOutcome = "failed"
	RemoteTerminalCancelled RemoteTerminalOutcome = "cancelled"
)

// RemoteTaskUpdate is the protocol-neutral task lifecycle update.
type RemoteTaskUpdate struct {
	Kind      RemoteTaskUpdateKind
	TaskID    string
	ContextID string
	State     string
	Terminal  RemoteTerminalOutcome
}

// RemoteTaskUpdateSink receives neutral task updates. The Service is the only
// layer that turns updates into a2a_task_* canonical events.
type RemoteTaskUpdateSink interface {
	Emit(ctx context.Context, update RemoteTaskUpdate) error
}

// RemoteA2AProvider is the protocol-neutral remote provider port. A2A SDK types
// stay inside its implementation.
type RemoteA2AProvider interface {
	Invoke(ctx context.Context, target RemoteAgent, req RemoteInvocationRequest, updates RemoteTaskUpdateSink) (RemoteInvocationResult, error)
}

// descriptionToMessage builds a single user text message. It never sets a task
// or context id; the remote creates those.
func descriptionToMessage(description string) *a2a.Message {
	message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(description))
	return message
}

// contentFromParts concatenates accepted Text/Data parts. Raw, URL, or
// non-serializable Data are rejected. The aggregate is bounded by the caller.
func contentFromParts(parts a2a.ContentParts) (string, error) {
	var builder strings.Builder
	for _, part := range parts {
		if part == nil {
			continue
		}
		switch content := part.Content.(type) {
		case a2a.Text:
			builder.WriteString(string(content))
		case a2a.Data:
			encoded, err := json.Marshal(content.Value)
			if err != nil {
				return "", newGatewayError(CodeRemoteResultUnsupported, "remote data part is not serializable", nil)
			}
			builder.Write(encoded)
		default:
			// a2a.Raw, a2a.URL or unknown content is rejected in P0.
			return "", newGatewayError(CodeRemoteResultUnsupported, "remote part type not supported", nil)
		}
	}
	return builder.String(), nil
}

// messageResult converts a direct Message reply. Only an Agent-role message may
// be converted; other roles fail closed.
func messageResult(message *a2a.Message, maxBytes int) (RemoteInvocationResult, error) {
	if message == nil || message.Role != a2a.MessageRoleAgent {
		return RemoteInvocationResult{}, newGatewayError(CodeRemoteResultUnsupported, "direct message must be from the agent role", nil)
	}
	content, err := contentFromParts(message.Parts)
	if err != nil {
		return RemoteInvocationResult{}, err
	}
	if err := checkResultBounds(content, maxBytes); err != nil {
		return RemoteInvocationResult{}, err
	}
	return RemoteInvocationResult{Content: content}, nil
}

// completedTaskResult extracts artifacts (Text/Data) from a completed task, or
// falls back to the last Agent message. Empty results fail closed.
func completedTaskResult(task *a2a.Task, maxBytes int) (RemoteInvocationResult, error) {
	var builder strings.Builder
	for _, artifact := range task.Artifacts {
		if artifact == nil {
			continue
		}
		content, err := contentFromParts(artifact.Parts)
		if err != nil {
			return RemoteInvocationResult{}, err
		}
		builder.WriteString(content)
	}
	content := builder.String()
	if content == "" {
		content = lastAgentMessageContent(task)
	}
	if content == "" {
		return RemoteInvocationResult{}, newGatewayError(CodeRemoteResultEmpty, "completed task has no usable artifact or agent message", nil)
	}
	if err := checkResultBounds(content, maxBytes); err != nil {
		return RemoteInvocationResult{}, err
	}
	return RemoteInvocationResult{
		Content:   content,
		TaskID:    string(task.ID),
		ContextID: task.ContextID,
		State:     string(task.Status.State),
	}, nil
}

func lastAgentMessageContent(task *a2a.Task) string {
	for i := len(task.History) - 1; i >= 0; i-- {
		message := task.History[i]
		if message == nil || message.Role != a2a.MessageRoleAgent {
			continue
		}
		if content, err := contentFromParts(message.Parts); err == nil && content != "" {
			return content
		}
	}
	if task.Status.Message != nil && task.Status.Message.Role == a2a.MessageRoleAgent {
		if content, err := contentFromParts(task.Status.Message.Parts); err == nil {
			return content
		}
	}
	return ""
}

func checkResultBounds(content string, maxBytes int) error {
	if len(content) > maxBytes {
		return newGatewayError(CodeRemoteResultTooLarge, "remote result exceeds the result limit", nil)
	}
	if !utf8.ValidString(content) {
		return newGatewayError(CodeRemoteResultUnsupported, "remote result is not valid utf-8", nil)
	}
	return nil
}
