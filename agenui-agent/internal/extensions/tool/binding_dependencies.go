package tool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/binding/operator"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

const (
	BindingDependenciesProviderID  = "agenui.tools.binding_dependencies.v1"
	BindingDependenciesAgentID     = "agenui_binder"
	BindingDependenciesToolVersion = "1.1.0"

	ExecuteOperatorName = "agenui_execute_operator"

	BindingDependencyToolSchemaV1 = "agenui.binding-dependency-tool/v1"

	BindingToolErrorInvalidRequest      = "INVALID_REQUEST"
	BindingToolErrorForbidden           = "FORBIDDEN"
	BindingToolErrorNotFound            = "NOT_FOUND"
	BindingToolErrorUnavailable         = "DEPENDENCY_UNAVAILABLE"
	BindingToolErrorIdempotencyConflict = "IDEMPOTENCY_CONFLICT"
	BindingToolErrorExecutionFailed     = "EXECUTION_FAILED"

	maxBindingIdentityBytes   = 256
	maxExecuteArgumentsBytes  = operator.MaxOperatorParameterBytes + 4096
	maxBindingToolOutputBytes = 4 << 20
)

type BindingExecutionMaterial struct {
	// ArtifactRunID is the trusted Root Run that owns the AGenUI generation
	// artifacts. A Binder FunctionTool executes inside a child Run, so the
	// child call.Ctx.RunID must never be used as the persistence identity.
	ArtifactRunID string
}

type BindingContextResolver interface {
	// ResolveExecutionMaterial must use an authoritative durable child-to-parent
	// relationship. Selecting the latest Run in the same Session is not a
	// production-safe implementation when Runs overlap or history is replayed.
	ResolveExecutionMaterial(context.Context, extension.Context) (BindingExecutionMaterial, error)
}

type BindingDependenciesProvider struct {
	tools []extension.FunctionTool
}

func NewBindingDependenciesProvider(
	resolver BindingContextResolver,
	execution operator.OperatorExecutionService,
) (*BindingDependenciesProvider, error) {
	if resolver == nil {
		return nil, errors.New("binding dependency tools: context resolver is required")
	}
	if execution == nil {
		return nil, errors.New("binding dependency tools: operator execution is required")
	}
	provider := &BindingDependenciesProvider{}
	provider.tools = []extension.FunctionTool{&bindingDependencyFunction{
		name: ExecuteOperatorName,
		invoke: func(ctx context.Context, call extension.FunctionCall) (*extension.FunctionResult, error) {
			return provider.invokeExecuteOperator(ctx, call, resolver, execution)
		},
	}}
	return provider, nil
}

func (*BindingDependenciesProvider) ID() string { return BindingDependenciesProviderID }

func (p *BindingDependenciesProvider) FunctionTools() []extension.FunctionTool {
	if p == nil {
		return nil
	}
	return append([]extension.FunctionTool(nil), p.tools...)
}

type bindingDependencyFunction struct {
	name   string
	invoke func(context.Context, extension.FunctionCall) (*extension.FunctionResult, error)
}

func (f *bindingDependencyFunction) Name() string { return f.name }

func (f *bindingDependencyFunction) Invoke(
	ctx context.Context,
	call extension.FunctionCall,
) (*extension.FunctionResult, error) {
	if ctx == nil {
		return nil, errors.New("binding dependency tools: context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f == nil || f.invoke == nil || call.Ctx.AgentID != BindingDependenciesAgentID ||
		call.Name != f.name || (call.Version != "" && call.Version != BindingDependenciesToolVersion) ||
		!validBindingCallContext(call.Ctx) {
		return bindingToolFailure(BindingToolErrorForbidden)
	}
	return f.invoke(ctx, call)
}

type executeOperatorInput struct {
	OperatorID  int64           `json:"operator_id"`
	SampleValue json.RawMessage `json:"sample_value"`
	Params      json.RawMessage `json:"params"`
}

type executeOperatorReceipt struct {
	ExecutionID string          `json:"execution_id"`
	OperatorID  int64           `json:"operator_id"`
	Output      json.RawMessage `json:"output"`
	OutputHash  string          `json:"output_hash"`
	Replayed    bool            `json:"replayed"`
}

type executeOperatorOutput struct {
	SchemaVersion string                  `json:"schema_version"`
	OK            bool                    `json:"ok"`
	Execution     *executeOperatorReceipt `json:"execution,omitempty"`
}

func (p *BindingDependenciesProvider) invokeExecuteOperator(
	ctx context.Context,
	call extension.FunctionCall,
	resolver BindingContextResolver,
	execution operator.OperatorExecutionService,
) (*extension.FunctionResult, error) {
	var input executeOperatorInput
	if err := decodeBindingToolArguments(call.Arguments, maxExecuteArgumentsBytes, &input); err != nil ||
		input.OperatorID <= 0 || len(input.SampleValue) == 0 || !json.Valid(input.SampleValue) ||
		!validParametersObject(input.Params) {
		return bindingToolFailure(BindingToolErrorInvalidRequest)
	}
	canonicalSample, err := canonicalBindingJSON(input.SampleValue)
	if err != nil {
		return bindingToolFailure(BindingToolErrorInvalidRequest)
	}
	canonicalParams, err := canonicalBindingJSON(input.Params)
	if err != nil || len(canonicalSample)+len(canonicalParams) > operator.MaxOperatorParameterBytes {
		return bindingToolFailure(BindingToolErrorInvalidRequest)
	}
	material, err := resolver.ResolveExecutionMaterial(ctx, call.Ctx)
	if err != nil {
		return bindingDependencyFailure(ctx, err)
	}
	if !validBindingIdentity(material.ArtifactRunID) {
		return bindingToolFailure(BindingToolErrorUnavailable)
	}
	command := operator.OperatorExecutionCommand{
		OperatorID:  input.OperatorID,
		SampleValue: append(json.RawMessage(nil), canonicalSample...),
		Params:      append(json.RawMessage(nil), canonicalParams...),
	}
	toolCallID, idempotencyKey := bindingOperatorExecutionIdentity(
		call.Ctx,
		material.ArtifactRunID,
		command,
	)
	trusted, err := operator.NewBindingExecutionContext(operator.BindingExecutionContextConfig{
		TenantID: call.Ctx.TenantID, UserID: call.Ctx.UserID, SessionID: call.Ctx.SessionID,
		RunID: material.ArtifactRunID, AgentID: call.Ctx.AgentID,
		ToolCallID: toolCallID, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return bindingToolFailure(BindingToolErrorUnavailable)
	}
	result, err := execution.ExecuteOperator(ctx, trusted, command)
	if contextErr := bindingContextError(ctx, err); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		return bindingToolFailure(bindingOperatorErrorCode(err))
	}
	if err := operator.ValidateExecutionResult(trusted, command, result); err != nil {
		return bindingToolFailure(BindingToolErrorUnavailable)
	}
	return marshalBindingToolResult(executeOperatorOutput{
		SchemaVersion: BindingDependencyToolSchemaV1, OK: true,
		Execution: &executeOperatorReceipt{
			ExecutionID: result.ExecutionID, OperatorID: result.OperatorID,
			Output:     append(json.RawMessage(nil), result.Output...),
			OutputHash: result.OutputHash, Replayed: result.Replayed,
		},
	})
}

func bindingOperatorExecutionIdentity(
	call extension.Context,
	artifactRunID string,
	command operator.OperatorExecutionCommand,
) (toolCallID string, idempotencyKey string) {
	payload, _ := json.Marshal(struct {
		SchemaVersion string          `json:"schema_version"`
		TenantID      string          `json:"tenant_id"`
		UserID        string          `json:"user_id"`
		SessionID     string          `json:"session_id"`
		RunID         string          `json:"run_id"`
		AgentID       string          `json:"agent_id"`
		OperatorID    int64           `json:"operator_id"`
		SampleValue   json.RawMessage `json:"sample_value"`
		Params        json.RawMessage `json:"params"`
	}{
		SchemaVersion: "agenui.binding-operator-invocation/v1",
		TenantID:      call.TenantID, UserID: call.UserID, SessionID: call.SessionID,
		RunID: artifactRunID, AgentID: call.AgentID,
		OperatorID: command.OperatorID, SampleValue: command.SampleValue, Params: command.Params,
	})
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	return "binding-operator-call-" + digest, "binding-operator-idem-" + digest
}

type bindingToolInvocationError struct {
	code string
}

func (e *bindingToolInvocationError) Error() string {
	if e == nil {
		return "binding operator tool failed"
	}
	return "binding operator tool failed: " + e.code
}

func decodeBindingToolArguments(raw json.RawMessage, maxBytes int, target any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || len(raw) > maxBytes || bytes.Equal(trimmed, []byte("null")) {
		return errors.New("invalid arguments")
	}
	if err := validateUniqueBoundedBindingJSON(trimmed); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}

func canonicalBindingJSON(raw json.RawMessage) (json.RawMessage, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(value)
	return canonical, err
}

func validParametersObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' ||
		len(raw) > operator.MaxOperatorParameterBytes {
		return false
	}
	var value map[string]json.RawMessage
	return decodeBindingToolArguments(raw, operator.MaxOperatorParameterBytes, &value) == nil && value != nil
}

const (
	maxBindingJSONDepth = 32
	maxBindingJSONNodes = 32_768
)

type bindingJSONBudget struct{ nodes int }

func validateUniqueBoundedBindingJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	budget := bindingJSONBudget{nodes: maxBindingJSONNodes}
	if err := scanUniqueBindingJSON(decoder, &budget, 1); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func scanUniqueBindingJSON(decoder *json.Decoder, budget *bindingJSONBudget, depth int) error {
	if budget == nil || budget.nodes <= 0 || depth > maxBindingJSONDepth {
		return errors.New("JSON structure exceeds safe bounds")
	}
	budget.nodes--
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			if keyErr != nil {
				return keyErr
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("duplicate object key")
			}
			seen[key] = struct{}{}
			if err := scanUniqueBindingJSON(decoder, budget, depth+1); err != nil {
				return err
			}
		}
		end, endErr := decoder.Token()
		if endErr != nil || end != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
	case '[':
		for decoder.More() {
			if err := scanUniqueBindingJSON(decoder, budget, depth+1); err != nil {
				return err
			}
		}
		end, endErr := decoder.Token()
		if endErr != nil || end != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func validBindingCallContext(call extension.Context) bool {
	return validBindingIdentity(call.TenantID) && validBindingIdentity(call.UserID) &&
		validBindingIdentity(call.SessionID) && validBindingIdentity(call.RunID) &&
		validBindingIdentity(call.AgentID)
}

func validBindingIdentity(value string) bool {
	return value != "" && len(value) <= maxBindingIdentityBytes && utf8.ValidString(value) &&
		strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

func bindingDependencyFailure(ctx context.Context, err error) (*extension.FunctionResult, error) {
	if contextErr := bindingContextError(ctx, err); contextErr != nil {
		return nil, contextErr
	}
	return bindingToolFailure(BindingToolErrorUnavailable)
}

func bindingOperatorErrorCode(err error) string {
	switch {
	case errors.Is(err, operator.ErrInvalidCommand):
		return BindingToolErrorInvalidRequest
	case errors.Is(err, operator.ErrIdempotencyConflict):
		return BindingToolErrorIdempotencyConflict
	case errors.Is(err, operator.ErrUnknownOperator):
		return BindingToolErrorNotFound
	case errors.Is(err, operator.ErrInvalidExecutionContext),
		errors.Is(err, operator.ErrInvalidRuntimeResult),
		errors.Is(err, operator.ErrRuntimePortUnavailable):
		return BindingToolErrorUnavailable
	default:
		return BindingToolErrorExecutionFailed
	}
}

func bindingContextError(ctx context.Context, err error) error {
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

func bindingToolFailure(code string) (*extension.FunctionResult, error) {
	return nil, &bindingToolInvocationError{code: code}
}

func marshalBindingToolResult(value any) (*extension.FunctionResult, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("binding dependency tools: result is unavailable")
	}
	if len(raw) > maxBindingToolOutputBytes {
		return bindingToolFailure(BindingToolErrorUnavailable)
	}
	return &extension.FunctionResult{Data: raw, MimeType: "application/json"}, nil
}

var _ extension.ToolProvider = (*BindingDependenciesProvider)(nil)
var _ extension.FunctionTool = (*bindingDependencyFunction)(nil)
