package operator

import (
	"context"
	"encoding/json"
	"errors"
)

const (
	OperatorExecutionResultSchemaV1 = "agenui.operator-execution-result/v1"

	MaxOperatorParameterBytes = 256 << 10
	MaxOperatorOutputBytes    = 1 << 20
	MaxComputedSnapshotBytes  = 4 << 20
)

var (
	ErrInvalidExecutionContext = errors.New("operator: invalid execution context")
	ErrInvalidCommand          = errors.New("operator: invalid execution command")
	ErrInvalidRuntimeResult    = errors.New("operator: invalid runtime result")
	ErrUnknownOperator         = errors.New("operator: unknown operator_id")
	ErrInputTypeMismatch       = errors.New("operator: input type mismatch")
	ErrIdempotencyConflict     = errors.New("operator: idempotency key reused with a different request")
	ErrRuntimePortUnavailable  = errors.New("operator: runtime port is unavailable")
	ErrExecutionFailed         = errors.New("operator: execution failed")
)

// OperatorExecutionCommand is the complete model-controlled execution input.
// Every identity, idempotency value, byte budget and receipt field is supplied
// by the host through BindingExecutionContext instead.
type OperatorExecutionCommand struct {
	OperatorID  int64           `json:"operator_id"`
	SampleValue json.RawMessage `json:"sample_value"`
	Params      json.RawMessage `json:"params"`
}

// OperatorExecutionResult is a host-validated execution receipt. Output is
// canonical JSON and SourceRoot is derived by the host, never by the model or
// operator platform.
type OperatorExecutionResult struct {
	SchemaVersion        string          `json:"schema_version"`
	ExecutionID          string          `json:"execution_id"`
	RequestFingerprint   string          `json:"request_fingerprint"`
	IdempotencyScopeHash string          `json:"idempotency_scope_hash"`
	OperatorID           int64           `json:"operator_id"`
	InputHash            string          `json:"input_hash"`
	Output               json.RawMessage `json:"output"`
	OutputHash           string          `json:"output_hash"`
	Replayed             bool            `json:"replayed"`
}

type OperatorExecutionService interface {
	ExecuteOperator(context.Context, BindingExecutionContext, OperatorExecutionCommand) (OperatorExecutionResult, error)
}

// OperatorRuntimePort is the production execution boundary awaiting the
// operator platform protocol. Each call accepts exactly one selected operator
// ID and canonical parameters; Binder may make a host-bounded 0..N such calls
// per parent Run. Knowledge details, code and executable refs never cross this
// boundary. Operator discovery and parent-Run resolution remain separate host
// activation dependencies.
type OperatorRuntimePort interface {
	RunOperator(context.Context, OperatorRuntimeRequest) (OperatorRuntimeResult, error)
}

type OperatorRuntimeScope struct {
	TenantID   string `json:"tenant_id"`
	UserID     string `json:"user_id"`
	SessionID  string `json:"session_id"`
	RunID      string `json:"run_id"`
	AgentID    string `json:"agent_id"`
	ToolCallID string `json:"tool_call_id"`
}

type OperatorRuntimeRequest struct {
	Scope              OperatorRuntimeScope `json:"scope"`
	InvocationID       string               `json:"invocation_id"`
	IdempotencyKey     string               `json:"idempotency_key"`
	RequestFingerprint string               `json:"request_fingerprint"`
	OperatorID         int64                `json:"operator_id"`
	SampleValue        json.RawMessage      `json:"sample_value"`
	Params             json.RawMessage      `json:"params"`
	MaxOutputBytes     int                  `json:"max_output_bytes"`
}

// OperatorRuntimeResult is entirely untrusted until the execution service
// canonicalizes and bounds Output. Adapter error strings are never surfaced.
type OperatorRuntimeResult struct {
	Output   json.RawMessage `json:"output"`
	Replayed bool            `json:"replayed"`
}
