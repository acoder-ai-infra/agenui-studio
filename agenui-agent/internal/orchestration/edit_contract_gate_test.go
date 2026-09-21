package orchestration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

type pendingEditDesigningStub struct{}

func (pendingEditDesigningStub) DesigningSnapshot(string, string, string) (string, string, bool) {
	return `{"schema_version":"content_contract.v1"}`, "", true
}
func (pendingEditDesigningStub) RestoreDesigningSnapshot(context.Context, string, string, string) error {
	return nil
}
func (pendingEditDesigningStub) ResolvedEditSnapshot(string, string, string) string { return "" }
func (pendingEditDesigningStub) EditContractSnapshot(string, string, string) string { return "" }
func (pendingEditDesigningStub) DesignEditPending(string, string, string) bool      { return true }
func (pendingEditDesigningStub) RecordDesignSnapshot(string, string, string, string) error {
	return nil
}
func (pendingEditDesigningStub) CurrentDesignSnapshot(string, string, string) string {
	return `{"messages":[]}`
}
func (pendingEditDesigningStub) TakeBindingEditRequest(string, string, string) (string, string, bool) {
	return "", "", false
}

func TestPendingEditCannotInvokeStyleBeforeContractAcceptance(t *testing.T) {
	interceptor := NewTaskInterceptor()
	interceptor.SetDesigningSnapshotProvider(pendingEditDesigningStub{})
	called := false
	outcome, err := interceptor.Intercept(context.Background(), extension.ToolCallInfo{
		Ctx: extension.Context{
			TenantID: "tenant-1", UserID: "user-1", SessionID: "session-1",
			RunID: "run-edit", RootRunID: "run-edit", AgentID: "agenui_agent",
		},
		Name: "task", Arguments: json.RawMessage(`{"subagent_type":"agenui_style","description":"apply edit"}`),
	}, func(context.Context, json.RawMessage) (extension.ToolCallOutcome, error) {
		called = true
		return extension.ToolCallOutcome{Result: `{"unexpected":true}`}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("Style was invoked without an accepted Edit Contract")
	}
	if !strings.Contains(outcome.Result, `"code":"EDIT_CONTRACT_REQUIRED"`) ||
		!strings.Contains(outcome.Result, `"executed":false`) {
		t.Fatalf("outcome=%s", outcome.Result)
	}
}
