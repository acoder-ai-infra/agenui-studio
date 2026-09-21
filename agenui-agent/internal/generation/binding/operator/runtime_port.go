package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
)

type RuntimePortExecutionService struct {
	runtime OperatorRuntimePort
}

func NewRuntimePortExecutionService(runtime OperatorRuntimePort) (*RuntimePortExecutionService, error) {
	if runtime == nil {
		return nil, ErrRuntimePortUnavailable
	}
	return &RuntimePortExecutionService{runtime: runtime}, nil
}

func (s *RuntimePortExecutionService) ExecuteOperator(
	ctx context.Context,
	trusted BindingExecutionContext,
	command OperatorExecutionCommand,
) (OperatorExecutionResult, error) {
	if ctx == nil {
		return OperatorExecutionResult{}, ErrInvalidExecutionContext
	}
	if err := ctx.Err(); err != nil {
		return OperatorExecutionResult{}, err
	}
	if s == nil || s.runtime == nil {
		return OperatorExecutionResult{}, ErrRuntimePortUnavailable
	}
	if !trusted.valid() {
		return OperatorExecutionResult{}, ErrInvalidExecutionContext
	}
	canonicalSample, canonicalParams, err := validateCommand(command)
	if err != nil {
		return OperatorExecutionResult{}, err
	}
	inputHash := hashBytes(append(append([]byte{}, canonicalSample...), canonicalParams...))
	idempotencyScopeHash := trusted.idempotencyScopeHash()
	fingerprint, err := requestFingerprint(trusted.runtimeScope(), command.OperatorID, inputHash)
	if err != nil {
		return OperatorExecutionResult{}, ErrInvalidCommand
	}
	executionID := executionID(idempotencyScopeHash, fingerprint)
	request := OperatorRuntimeRequest{
		Scope: trusted.runtimeScope(), InvocationID: executionID,
		IdempotencyKey: trusted.idempotencyKey, RequestFingerprint: fingerprint,
		OperatorID:     command.OperatorID,
		SampleValue:    append(json.RawMessage(nil), canonicalSample...),
		Params:         append(json.RawMessage(nil), canonicalParams...),
		MaxOutputBytes: MaxOperatorOutputBytes,
	}
	runtimeResult, err := s.runtime.RunOperator(ctx, cloneRuntimeRequest(request))
	if contextErr := authoritativeContextError(ctx, err); contextErr != nil {
		return OperatorExecutionResult{}, contextErr
	}
	if err != nil {
		if errors.Is(err, ErrIdempotencyConflict) {
			return OperatorExecutionResult{}, ErrIdempotencyConflict
		}
		if errors.Is(err, ErrUnknownOperator) {
			return OperatorExecutionResult{}, ErrUnknownOperator
		}
		return OperatorExecutionResult{}, ErrExecutionFailed
	}
	if len(runtimeResult.Output) == 0 || len(runtimeResult.Output) > MaxOperatorOutputBytes {
		return OperatorExecutionResult{}, ErrInvalidRuntimeResult
	}
	canonicalOutput, value, err := canonicalizeJSON(runtimeResult.Output, false)
	if err != nil || len(canonicalOutput) == 0 || len(canonicalOutput) > MaxOperatorOutputBytes ||
		validateBindableValue(value) != nil {
		return OperatorExecutionResult{}, ErrInvalidRuntimeResult
	}
	outputHash := hashBytes(canonicalOutput)
	return OperatorExecutionResult{
		SchemaVersion: OperatorExecutionResultSchemaV1, ExecutionID: executionID,
		RequestFingerprint: fingerprint, IdempotencyScopeHash: idempotencyScopeHash,
		OperatorID: command.OperatorID, InputHash: inputHash,
		Output: append(json.RawMessage(nil), canonicalOutput...), OutputHash: outputHash,
		Replayed: runtimeResult.Replayed,
	}, nil
}

// ValidateExecutionResult re-authenticates an execution service result at the
// tool boundary. This prevents a custom/production service implementation
// from forging the selected ID, source root, receipt identity or output hash.
func ValidateExecutionResult(
	trusted BindingExecutionContext,
	command OperatorExecutionCommand,
	result OperatorExecutionResult,
) error {
	if !trusted.valid() || result.SchemaVersion != OperatorExecutionResultSchemaV1 || result.OperatorID != command.OperatorID {
		return ErrInvalidRuntimeResult
	}
	sample, params, err := validateCommand(command)
	if err != nil {
		return err
	}
	inputHash := hashBytes(append(append([]byte{}, sample...), params...))
	wantFingerprint, err := requestFingerprint(trusted.runtimeScope(), command.OperatorID, inputHash)
	if err != nil {
		return ErrInvalidRuntimeResult
	}
	if len(result.Output) == 0 || len(result.Output) > MaxOperatorOutputBytes {
		return ErrInvalidRuntimeResult
	}
	canonicalOutput, value, err := canonicalizeJSON(result.Output, false)
	if err != nil || len(canonicalOutput) > MaxOperatorOutputBytes || validateBindableValue(value) != nil {
		return ErrInvalidRuntimeResult
	}
	outputHash := hashBytes(canonicalOutput)
	idempotencyScopeHash := trusted.idempotencyScopeHash()
	if result.InputHash != inputHash || result.RequestFingerprint != wantFingerprint ||
		result.IdempotencyScopeHash != idempotencyScopeHash ||
		result.ExecutionID != executionID(idempotencyScopeHash, wantFingerprint) ||
		result.OutputHash != outputHash ||
		!bytes.Equal(canonicalOutput, result.Output) ||
		!json.Valid(result.Output) {
		return ErrInvalidRuntimeResult
	}
	return nil
}

func validateCommand(command OperatorExecutionCommand) (json.RawMessage, json.RawMessage, error) {
	if command.OperatorID <= 0 || len(command.SampleValue) == 0 || len(command.Params) == 0 ||
		len(command.SampleValue)+len(command.Params) > MaxOperatorParameterBytes {
		return nil, nil, ErrInvalidCommand
	}
	canonicalSample, sample, err := canonicalizeJSON(command.SampleValue, false)
	if err != nil || validateBindableValue(sample) != nil {
		return nil, nil, ErrInvalidCommand
	}
	canonicalParams, value, err := canonicalizeJSON(command.Params, true)
	parameters, ok := value.(map[string]any)
	if err != nil || !ok || validateBindableValue(parameters) != nil {
		return nil, nil, ErrInvalidCommand
	}
	return append(json.RawMessage(nil), canonicalSample...), append(json.RawMessage(nil), canonicalParams...), nil
}

func requestFingerprint(scope OperatorRuntimeScope, operatorID int64, inputHash string) (string, error) {
	if !validRuntimeScope(scope) || operatorID <= 0 || !validSHA256Hash(inputHash) {
		return "", ErrInvalidCommand
	}
	payload := struct {
		SchemaVersion  string               `json:"schema_version"`
		Scope          OperatorRuntimeScope `json:"scope"`
		OperatorID     int64                `json:"operator_id"`
		InputHash      string               `json:"input_hash"`
		MaxOutputBytes int                  `json:"max_output_bytes"`
	}{
		SchemaVersion: "agenui.operator-runtime-request/v1", Scope: scope,
		OperatorID: operatorID, InputHash: inputHash, MaxOutputBytes: MaxOperatorOutputBytes,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return hashBytes(raw), nil
}

func executionID(idempotencyScopeHash, fingerprint string) string {
	digest := hashBytes([]byte(idempotencyScopeHash + "\x00" + fingerprint))
	return "operator_exec_" + hashPrefix(digest, 24)
}

func validRuntimeScope(scope OperatorRuntimeScope) bool {
	return validIdentity(scope.TenantID) && validIdentity(scope.UserID) &&
		validIdentity(scope.SessionID) && validIdentity(scope.RunID) &&
		validIdentity(scope.AgentID) && validIdentity(scope.ToolCallID)
}

func cloneRuntimeRequest(request OperatorRuntimeRequest) OperatorRuntimeRequest {
	request.SampleValue = append(json.RawMessage(nil), request.SampleValue...)
	request.Params = append(json.RawMessage(nil), request.Params...)
	return request
}

func authoritativeContextError(ctx context.Context, err error) error {
	if ctx != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

var _ OperatorExecutionService = (*RuntimePortExecutionService)(nil)
