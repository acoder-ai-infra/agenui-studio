// Package identifiercontract owns the character limits shared by SDK entry
// points, durable ledger stores, registry validation, and schema readiness.
package identifiercontract

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	MaxTenantIDCharacters             = 64
	MaxUserIDCharacters               = 64
	MaxSessionIDCharacters            = 64
	MaxRunIDCharacters                = 64
	MaxTurnIDCharacters               = 64
	MaxMessageIDCharacters            = 64
	MaxEventIDCharacters              = 64
	MaxStepIDCharacters               = 64
	MaxCheckpointIDCharacters         = 64
	MaxRequestIDCharacters            = 64
	MaxTraceIDCharacters              = 64
	MaxSpanIDCharacters               = 64
	MaxToolUseIDCharacters            = 64
	MaxUsageIDCharacters              = 64
	MaxAgentTypeCharacters            = 64
	MaxAgentIDCharacters              = 128
	MaxAgentBindingIDCharacters       = 128
	MaxResumeAttemptIDCharacters      = 128
	MaxIdempotencyKeyCharacters       = 128
	MaxIdempotencyScopeCharacters     = 128
	MaxResumeTokenHashCharacters      = 128
	MaxAgentVersionCharacters         = 255
	MaxGenericIdempotencyIDCharacters = 320
)

// Field is a named identifier and its durable character limit. Constructors
// below keep callers from duplicating column-width knowledge.
type Field struct {
	name          string
	value         string
	maxCharacters int
}

// ValidationError deliberately omits the identifier value so errors and logs
// cannot leak tenant or user identities.
type ValidationError struct {
	Field         string
	MaxCharacters int
	InvalidUTF8   bool
}

func (e *ValidationError) Error() string {
	if e.InvalidUTF8 {
		return fmt.Sprintf("%s must be valid UTF-8", e.Field)
	}
	return fmt.Sprintf("%s must not exceed %d characters", e.Field, e.MaxCharacters)
}

// Validate accepts empty optional identifiers; requiredness remains owned by
// the request or record validator that understands the operation semantics.
func Validate(fields ...Field) error {
	for _, field := range fields {
		if field.value == "" {
			continue
		}
		if !utf8.ValidString(field.value) {
			return &ValidationError{Field: field.name, MaxCharacters: field.maxCharacters, InvalidUTF8: true}
		}
		if utf8.RuneCountInString(field.value) > field.maxCharacters {
			return &ValidationError{Field: field.name, MaxCharacters: field.maxCharacters}
		}
	}
	return nil
}

// ComposeIdempotencyKey preserves existing short keys and replaces only keys
// that cannot fit the durable ledger contract with a stable, opaque digest.
func ComposeIdempotencyKey(parts ...string) string {
	value := strings.Join(parts, ":")
	if utf8.ValidString(value) && utf8.RuneCountInString(value) <= MaxIdempotencyKeyCharacters {
		return value
	}

	hash := sha256.New()
	var length [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(part))
	}
	return "idempotency:sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func bounded(name, value string, maxCharacters int) Field {
	return Field{name: name, value: value, maxCharacters: maxCharacters}
}

func TenantID(value string) Field    { return bounded("tenant_id", value, MaxTenantIDCharacters) }
func UserID(value string) Field      { return bounded("user_id", value, MaxUserIDCharacters) }
func SessionID(value string) Field   { return bounded("session_id", value, MaxSessionIDCharacters) }
func RunID(value string) Field       { return bounded("run_id", value, MaxRunIDCharacters) }
func ParentRunID(value string) Field { return bounded("parent_run_id", value, MaxRunIDCharacters) }
func TurnID(value string) Field      { return bounded("turn_id", value, MaxTurnIDCharacters) }
func MessageID(value string) Field   { return bounded("message_id", value, MaxMessageIDCharacters) }
func EventID(value string) Field     { return bounded("event_id", value, MaxEventIDCharacters) }
func ClientEventID(value string) Field {
	return bounded("client_event_id", value, MaxEventIDCharacters)
}
func StepID(value string) Field       { return bounded("step_id", value, MaxStepIDCharacters) }
func ParentStepID(value string) Field { return bounded("parent_step_id", value, MaxStepIDCharacters) }
func CheckpointID(value string) Field {
	return bounded("checkpoint_id", value, MaxCheckpointIDCharacters)
}
func RequestID(value string) Field    { return bounded("request_id", value, MaxRequestIDCharacters) }
func RequestHash(value string) Field  { return bounded("request_hash", value, MaxRequestIDCharacters) }
func TraceID(value string) Field      { return bounded("trace_id", value, MaxTraceIDCharacters) }
func SpanID(value string) Field       { return bounded("span_id", value, MaxSpanIDCharacters) }
func ParentSpanID(value string) Field { return bounded("parent_span_id", value, MaxSpanIDCharacters) }
func ToolUseID(value string) Field    { return bounded("tool_use_id", value, MaxToolUseIDCharacters) }
func UsageID(value string) Field      { return bounded("usage_id", value, MaxUsageIDCharacters) }
func AgentType(value string) Field    { return bounded("agent_type", value, MaxAgentTypeCharacters) }
func AgentID(value string) Field      { return bounded("agent_id", value, MaxAgentIDCharacters) }
func AgentBindingID(value string) Field {
	return bounded("agent_binding_id", value, MaxAgentBindingIDCharacters)
}
func ResumeAttemptID(value string) Field {
	return bounded("resume_attempt_id", value, MaxResumeAttemptIDCharacters)
}
func IdempotencyKey(value string) Field {
	return bounded("idempotency_key", value, MaxIdempotencyKeyCharacters)
}
func IdempotencyScope(value string) Field {
	return bounded("idempotency_scope", value, MaxIdempotencyScopeCharacters)
}
func ResumeTokenHash(value string) Field {
	return bounded("resume_token_hash", value, MaxResumeTokenHashCharacters)
}
func AgentVersion(value string) Field {
	return bounded("agent_version", value, MaxAgentVersionCharacters)
}
func GenericIdempotencyID(value string) Field {
	return bounded("idempotency_id", value, MaxGenericIdempotencyIDCharacters)
}
