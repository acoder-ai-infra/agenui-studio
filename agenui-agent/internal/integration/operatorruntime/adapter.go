package operatorruntime

import (
	"context"
	"encoding/json"
	"strings"

	bindingoperator "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/binding/operator"
	platformoperator "github.com/AGenUI/agenui-studio/agenui-agent/internal/operator"
)

// Executor is the narrow surface implemented by the operator platform service.
// The Binder never receives source code or operator details; it supplies only
// the selected immutable operator ID and a canonical parameter object.
type Executor interface {
	Execute(context.Context, platformoperator.ExecuteRequest) platformoperator.ExecuteResult
}

// Adapter connects the published-operator execution implementation to the
// Binder-owned runtime port. The canonical parameters object is the platform
// ABI's transform(value) argument; no hidden field value, model context, host
// identity, or replay key is added. Operator abstracts therefore have to state
// the complete parameter-object and result-object contract. Identity and replay
// fields stay host-owned and are never exposed to JavaScript operator code.
type Adapter struct {
	executor Executor
	slots    chan struct{}
}

func New(executor Executor) (*Adapter, error) {
	return newWithConcurrency(executor, 4)
}

func newWithConcurrency(executor Executor, maximum int) (*Adapter, error) {
	if executor == nil {
		return nil, bindingoperator.ErrRuntimePortUnavailable
	}
	if maximum < 1 || maximum > 64 {
		return nil, bindingoperator.ErrRuntimePortUnavailable
	}
	return &Adapter{executor: executor, slots: make(chan struct{}, maximum)}, nil
}

func (a *Adapter) RunOperator(
	ctx context.Context,
	request bindingoperator.OperatorRuntimeRequest,
) (bindingoperator.OperatorRuntimeResult, error) {
	if ctx == nil {
		return bindingoperator.OperatorRuntimeResult{}, bindingoperator.ErrExecutionFailed
	}
	if err := ctx.Err(); err != nil {
		return bindingoperator.OperatorRuntimeResult{}, err
	}
	if a == nil || a.executor == nil || a.slots == nil {
		return bindingoperator.OperatorRuntimeResult{}, bindingoperator.ErrRuntimePortUnavailable
	}
	if !validRequest(request) {
		return bindingoperator.OperatorRuntimeResult{}, bindingoperator.ErrExecutionFailed
	}

	var sampleValue any
	var params map[string]any
	if err := json.Unmarshal(request.SampleValue, &sampleValue); err != nil {
		return bindingoperator.OperatorRuntimeResult{}, bindingoperator.ErrExecutionFailed
	}
	if err := json.Unmarshal(request.Params, &params); err != nil || params == nil {
		return bindingoperator.OperatorRuntimeResult{}, bindingoperator.ErrExecutionFailed
	}
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	case <-ctx.Done():
		return bindingoperator.OperatorRuntimeResult{}, ctx.Err()
	}
	result := a.executor.Execute(ctx, platformoperator.ExecuteRequest{
		OperatorID: uint64(request.OperatorID),
		Value:      sampleValue,
		Context:    params,
	})
	if err := ctx.Err(); err != nil {
		return bindingoperator.OperatorRuntimeResult{}, err
	}
	if result.OperatorID != uint64(request.OperatorID) {
		return bindingoperator.OperatorRuntimeResult{}, bindingoperator.ErrExecutionFailed
	}
	if !result.Applied {
		if result.Error != nil && result.Error.Code == platformoperator.CodeOperatorNotFound {
			return bindingoperator.OperatorRuntimeResult{}, bindingoperator.ErrUnknownOperator
		}
		return bindingoperator.OperatorRuntimeResult{}, bindingoperator.ErrExecutionFailed
	}
	output, err := json.Marshal(result.Value)
	if err != nil || len(output) == 0 || len(output) > request.MaxOutputBytes {
		return bindingoperator.OperatorRuntimeResult{}, bindingoperator.ErrExecutionFailed
	}
	return bindingoperator.OperatorRuntimeResult{Output: output}, nil
}

func validRequest(request bindingoperator.OperatorRuntimeRequest) bool {
	if request.OperatorID <= 0 || request.MaxOutputBytes <= 0 ||
		len(request.SampleValue) == 0 || len(request.Params) == 0 ||
		len(request.SampleValue)+len(request.Params) > bindingoperator.MaxOperatorParameterBytes ||
		strings.TrimSpace(request.InvocationID) == "" ||
		strings.TrimSpace(request.IdempotencyKey) == "" ||
		!validHash(request.RequestFingerprint) {
		return false
	}
	scope := request.Scope
	for _, value := range []string{
		scope.TenantID, scope.UserID, scope.SessionID, scope.RunID,
		scope.AgentID, scope.ToolCallID,
	} {
		if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
			return false
		}
	}
	return true
}

func validHash(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, current := range value[len("sha256:"):] {
		if !strings.ContainsRune("0123456789abcdef", current) {
			return false
		}
	}
	return true
}

var _ bindingoperator.OperatorRuntimePort = (*Adapter)(nil)
