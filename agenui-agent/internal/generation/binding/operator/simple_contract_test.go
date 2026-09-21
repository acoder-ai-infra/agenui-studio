package operator

import (
	"context"
	"encoding/json"
	"testing"
)

type runtimePortStub struct {
	request OperatorRuntimeRequest
	result  OperatorRuntimeResult
}

func (s *runtimePortStub) RunOperator(_ context.Context, request OperatorRuntimeRequest) (OperatorRuntimeResult, error) {
	s.request = request
	return s.result, nil
}

func TestExecuteOperatorUsesSampleValueAndParams(t *testing.T) {
	port := &runtimePortStub{result: OperatorRuntimeResult{Output: json.RawMessage(`"¥68.00"`)}}
	service, err := NewRuntimePortExecutionService(port)
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := NewBindingExecutionContext(BindingExecutionContextConfig{
		TenantID: "tenant", UserID: "user", SessionID: "session", RunID: "run",
		AgentID: "binder", ToolCallID: "tool", IdempotencyKey: "idem",
	})
	if err != nil {
		t.Fatal(err)
	}
	command := OperatorExecutionCommand{OperatorID: 42, SampleValue: json.RawMessage(`6800`), Params: json.RawMessage(`{"currency":"¥"}`)}
	result, err := service.ExecuteOperator(t.Context(), trusted, command)
	if err != nil {
		t.Fatal(err)
	}
	if string(port.request.SampleValue) != `6800` || string(port.request.Params) != `{"currency":"¥"}` {
		t.Fatalf("runtime request = %#v", port.request)
	}
	if string(result.Output) != `"¥68.00"` || result.InputHash == "" || result.OutputHash == "" {
		t.Fatalf("result = %#v", result)
	}
	if err := ValidateExecutionResult(trusted, command, result); err != nil {
		t.Fatal(err)
	}
}

func TestExecuteOperatorRejectsMissingSampleOrParams(t *testing.T) {
	port := &runtimePortStub{result: OperatorRuntimeResult{Output: json.RawMessage(`1`)}}
	service, _ := NewRuntimePortExecutionService(port)
	trusted, _ := NewBindingExecutionContext(BindingExecutionContextConfig{
		TenantID: "tenant", UserID: "user", SessionID: "session", RunID: "run",
		AgentID: "binder", ToolCallID: "tool", IdempotencyKey: "idem",
	})
	for _, command := range []OperatorExecutionCommand{
		{OperatorID: 1, Params: json.RawMessage(`{}`)},
		{OperatorID: 1, SampleValue: json.RawMessage(`1`)},
	} {
		if _, err := service.ExecuteOperator(t.Context(), trusted, command); err == nil {
			t.Fatal("expected invalid command")
		}
	}
}
