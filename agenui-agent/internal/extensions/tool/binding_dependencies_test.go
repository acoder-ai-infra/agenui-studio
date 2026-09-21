package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/binding/operator"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

type bindingResolverStub struct{ runID string }

type operatorRuntimeStub struct {
	handler func(operator.OperatorRuntimeRequest) (json.RawMessage, error)
}

func (s operatorRuntimeStub) RunOperator(
	_ context.Context,
	request operator.OperatorRuntimeRequest,
) (operator.OperatorRuntimeResult, error) {
	output, err := s.handler(request)
	return operator.OperatorRuntimeResult{Output: output}, err
}

func (s bindingResolverStub) ResolveExecutionMaterial(context.Context, extension.Context) (BindingExecutionMaterial, error) {
	return BindingExecutionMaterial{ArtifactRunID: s.runID}, nil
}

func TestBindingDependenciesProviderDryRunsSampleValueAndParams(t *testing.T) {
	runtime := operatorRuntimeStub{handler: func(call operator.OperatorRuntimeRequest) (json.RawMessage, error) {
		if call.OperatorID != 42 || string(call.SampleValue) != `6800` || string(call.Params) != `{"currency":"¥"}` {
			t.Fatalf("invocation=%#v", call)
		}
		return json.RawMessage(`"¥68.00"`), nil
	}}
	mock, err := operator.NewRuntimePortExecutionService(runtime)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewBindingDependenciesProvider(bindingResolverStub{runID: "root-run"}, mock)
	if err != nil {
		t.Fatal(err)
	}
	tools := provider.FunctionTools()
	if len(tools) != 1 || tools[0].Name() != ExecuteOperatorName {
		t.Fatalf("tools=%#v", tools)
	}
	result, err := tools[0].Invoke(t.Context(), extension.FunctionCall{
		Ctx:  extension.Context{TenantID: "tenant", UserID: "user", SessionID: "session", RunID: "child", AgentID: BindingDependenciesAgentID},
		Name: ExecuteOperatorName, Version: BindingDependenciesToolVersion,
		Arguments: json.RawMessage(`{"operator_id":42,"sample_value":6800,"params":{"currency":"¥"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Data), `"output":"¥68.00"`) {
		t.Fatalf("result=%s", result.Data)
	}
}

func TestBindingDependenciesRejectsOldParametersOnlyShape(t *testing.T) {
	runtime := operatorRuntimeStub{handler: func(operator.OperatorRuntimeRequest) (json.RawMessage, error) {
		return json.RawMessage(`1`), nil
	}}
	mock, _ := operator.NewRuntimePortExecutionService(runtime)
	provider, _ := NewBindingDependenciesProvider(bindingResolverStub{runID: "root-run"}, mock)
	result, err := provider.FunctionTools()[0].Invoke(t.Context(), extension.FunctionCall{
		Ctx:  extension.Context{TenantID: "tenant", UserID: "user", SessionID: "session", RunID: "child", AgentID: BindingDependenciesAgentID},
		Name: ExecuteOperatorName, Version: BindingDependenciesToolVersion,
		Arguments: json.RawMessage(`{"operator_id":42,"parameters":{"value":6800}}`),
	})
	if err == nil || result != nil {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}
