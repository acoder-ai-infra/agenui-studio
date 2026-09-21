package storage

import (
	"strings"

	"github.com/AGenUI/agenui-studio/harness/internal/identifiercontract"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// MaxUserIDCharacters 与 MySQL sessions.user_id、
// open_turn_idempotency.user_id 的 VARCHAR(64) 契约一致。
const (
	MaxUserIDCharacters         = identifiercontract.MaxUserIDCharacters
	MaxAgentIDCharacters        = identifiercontract.MaxAgentIDCharacters
	MaxIdempotencyKeyCharacters = identifiercontract.MaxIdempotencyKeyCharacters
)

// ValidateUserID 在任何持久化前校验用户身份。数据库不得承担截断或拒绝
// 超长身份的职责，否则不同 SQL mode 会产生不一致行为。
func ValidateUserID(userID string) error {
	return validateIdentifierFields(identifiercontract.UserID(userID))
}

func ValidateTenantID(tenantID string) error {
	return validateIdentifierFields(identifiercontract.TenantID(tenantID))
}

func validateIdentifierFields(fields ...identifiercontract.Field) error {
	if err := identifiercontract.Validate(fields...); err != nil {
		return NewError(ErrInvalidArgument, err.Error())
	}
	return nil
}

// ValidateOpenTurnRequestIdentifiers rejects caller-controlled identifiers
// before generated IDs or any durable state are created.
func ValidateOpenTurnRequestIdentifiers(req OpenTurnRequest, tenantID, userID string) error {
	return validateIdentifierFields(
		identifiercontract.TenantID(tenantID),
		identifiercontract.UserID(userID),
		identifiercontract.SessionID(req.SessionID),
		identifiercontract.AgentID(req.AgentID),
		identifiercontract.AgentBindingID(req.AgentBindingID),
		identifiercontract.IdempotencyKey(req.IdempotencyKey),
	)
}

func ValidateSessionIdentifiers(session *Session) error {
	if session == nil {
		return NewError(ErrInvalidArgument, "session is required")
	}
	return validateIdentifierFields(
		identifiercontract.SessionID(session.ID),
		identifiercontract.TenantID(session.TenantID),
		identifiercontract.UserID(session.UserID),
		identifiercontract.AgentID(session.AgentID),
	)
}

func ValidateMessageIdentifiers(message *Message) error {
	if message == nil {
		return NewError(ErrInvalidArgument, "message is required")
	}
	return validateIdentifierFields(
		identifiercontract.MessageID(message.ID),
		identifiercontract.SessionID(message.SessionID),
		identifiercontract.TurnID(message.TurnID),
		identifiercontract.RunID(message.RunID),
		identifiercontract.TenantID(message.TenantID),
	)
}

func ValidateUsageIdentifiers(record *ModelUsageRecord) error {
	if record == nil {
		return NewError(ErrInvalidArgument, "model usage record is required")
	}
	return validateIdentifierFields(
		identifiercontract.UsageID(record.ID),
		identifiercontract.RequestID(record.RequestID),
		identifiercontract.TraceID(record.TraceID),
		identifiercontract.TenantID(record.TenantID),
		identifiercontract.SessionID(record.SessionID),
		identifiercontract.RunID(record.RunID),
		identifiercontract.AgentID(record.AgentID),
	)
}

func ValidateRunIdentifiers(run *Run) error {
	if run == nil {
		return NewError(ErrInvalidArgument, "run is required")
	}
	return validateIdentifierFields(
		identifiercontract.RunID(run.RunID),
		identifiercontract.SessionID(run.SessionID),
		identifiercontract.TurnID(run.TurnID),
		identifiercontract.ParentRunID(run.ParentRunID),
		identifiercontract.TenantID(run.TenantID),
		identifiercontract.AgentID(run.AgentID),
		identifiercontract.TraceID(run.TraceID),
		identifiercontract.AgentBindingID(run.AgentBindingID),
		identifiercontract.ResumeAttemptID(run.ResumeAttemptID),
	)
}

func ValidateStepIdentifiers(step *Step) error {
	if step == nil {
		return NewError(ErrInvalidArgument, "step is required")
	}
	return validateIdentifierFields(
		identifiercontract.StepID(step.StepID),
		identifiercontract.RunID(step.RunID),
		identifiercontract.ParentStepID(step.ParentStepID),
	)
}

func ValidateEventIdentifiers(event observability.AgentEvent) error {
	return validateIdentifierFields(
		identifiercontract.EventID(event.EventID),
		identifiercontract.RunID(event.RunID),
		identifiercontract.SessionID(event.SessionID),
		identifiercontract.StepID(event.StepID),
		identifiercontract.AgentID(event.AgentID),
		identifiercontract.AgentType(event.AgentType),
		identifiercontract.TraceID(event.TraceID),
		identifiercontract.SpanID(event.SpanID),
		identifiercontract.ParentSpanID(event.ParentSpanID),
		identifiercontract.IdempotencyKey(event.IdempotencyKey),
	)
}

func ValidateCheckpointIdentifiers(checkpoint *CheckpointMeta) error {
	if checkpoint == nil {
		return NewError(ErrInvalidArgument, "checkpoint is required")
	}
	return validateIdentifierFields(
		identifiercontract.CheckpointID(checkpoint.CheckpointID),
		identifiercontract.RunID(checkpoint.RunID),
		identifiercontract.TenantID(checkpoint.TenantID),
	)
}

func ValidateControlRequestIdentifiers(request *ControlRequest) error {
	if request == nil {
		return NewError(ErrInvalidArgument, "control request is required")
	}
	return validateIdentifierFields(
		identifiercontract.RequestID(request.RequestID),
		identifiercontract.RunID(request.RunID),
		identifiercontract.TenantID(request.TenantID),
		identifiercontract.CheckpointID(request.CheckpointID),
		identifiercontract.ResumeTokenHash(request.ResumeTokenHash),
		identifiercontract.ToolUseID(request.ToolUseID),
	)
}

func ValidateOpenTurnCommandIdentifiers(command OpenTurnCommand) error {
	if err := ValidateSessionIdentifiers(&command.Session); err != nil {
		return err
	}
	if err := ValidateRunIdentifiers(&command.Run); err != nil {
		return err
	}
	if err := ValidateMessageIdentifiers(&command.Message); err != nil {
		return err
	}
	for _, event := range command.Events {
		if err := ValidateEventIdentifiers(event); err != nil {
			return err
		}
	}
	return validateIdentifierFields(
		identifiercontract.IdempotencyKey(command.IdempotencyKey),
		identifiercontract.IdempotencyScope(command.IdempotencyScope),
		identifiercontract.RequestHash(command.RequestHash),
	)
}

func ValidateIdemKeyIdentifiers(key IdemKey) error {
	return validateIdentifierFields(
		identifiercontract.TenantID(key.TenantID),
		identifiercontract.GenericIdempotencyID(strings.Join([]string{key.TenantID, key.Namespace, key.Key}, "|")),
	)
}

func ValidateAgentBindingIdentifiers(runID, bindingID string) error {
	return validateIdentifierFields(
		identifiercontract.RunID(runID),
		identifiercontract.AgentBindingID(bindingID),
	)
}

func ValidateResumeAttemptIdentifiers(runID, attemptID string) error {
	return validateIdentifierFields(
		identifiercontract.RunID(runID),
		identifiercontract.ResumeAttemptID(attemptID),
	)
}

func ValidateResumeClaimIdentifiers(command ResumeClaimCommand) error {
	if err := validateIdentifierFields(
		identifiercontract.RunID(command.RunID),
		identifiercontract.SessionID(command.SessionID),
		identifiercontract.CheckpointID(command.CheckpointID),
		identifiercontract.RequestID(command.ControlRequestID),
		identifiercontract.ResumeTokenHash(command.ResumeTokenHash),
		identifiercontract.ResumeAttemptID(command.AttemptID),
	); err != nil {
		return err
	}
	return ValidateEventIdentifiers(command.Event)
}

func ValidateResumeWaitIdentifiers(command ResumeWaitCommand) error {
	if err := ValidateControlRequestIdentifiers(command.Control); err != nil {
		return err
	}
	return ValidateEventIdentifiers(command.Event)
}

func ValidateResumeFailureIdentifiers(command ResumeFailureCommand) error {
	if err := ValidateResumeAttemptIdentifiers(command.RunID, command.AttemptID); err != nil {
		return err
	}
	return ValidateEventIdentifiers(command.Event)
}
