package ruleworker

import (
	"context"
	"testing"

	harness "github.com/AGenUI/agenui-studio/harness/sdk"
	"github.com/AGenUI/agenui-studio/harness/sdk/testkit"
)

func TestHarnessCompleterReturnsParserRunResult(t *testing.T) {
	engine := testkit.NewMockEngine(testkit.Scenario{
		Events: []harness.Event{{EventType: harness.EventRunCompleted}},
		Result: harness.ResultView{Content: "```json\n{\"revisionDelta\":{\"newDocuments\":[]}}\n```"},
	})
	completer, err := NewHarnessCompleter(engine, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := completer.Complete(context.Background(), "system", "rules")
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"revisionDelta":{"newDocuments":[]}}` {
		t.Fatalf("result = %q", got)
	}
}

func TestHarnessCompleterReturnsRunFailure(t *testing.T) {
	engine := testkit.NewMockEngine(testkit.Scenario{Events: []harness.Event{{
		EventType: harness.EventRunFailed,
		Error:     &harness.EventError{Message: "invalid output"},
	}}})
	completer, err := NewHarnessCompleter(engine, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := completer.Complete(context.Background(), "system", "rules"); err == nil {
		t.Fatal("expected parser failure")
	}
}
