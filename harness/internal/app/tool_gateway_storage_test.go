package app

import (
	"context"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage/memory"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

func TestToolGatewayStorageAdaptersUseCanonicalStores(t *testing.T) {
	stores := memory.New().Stores()
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "tenant"})
	events := toolGatewayEventStore{events: stores.Events}
	event := observability.AgentEvent{
		EventID: "event-tool-start", IdempotencyKey: "tool-start", RunID: "run-1",
		EventType: observability.EventToolCallStarted, Visibility: observability.VisibilityDebug,
	}
	first, err := events.AppendEvent(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	second, err := events.AppendEvent(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	if first.Event.Sequence != 1 || second.Event.Sequence != 1 || first.Event.EventID != second.Event.EventID {
		t.Fatalf("canonical event append is not idempotent: first=%#v second=%#v", first, second)
	}

	steps := toolGatewayStepStore{steps: stores.Steps}
	started, err := steps.StartToolStep(ctx, toolgateway.StartToolStepRequest{
		RunID: "run-1", StepID: "opaque-tool-step", ParentStepID: "runtime-step", ToolName: "harness.echo",
	})
	if err != nil || started.Status != "running" {
		t.Fatalf("start tool step: snapshot=%#v err=%v", started, err)
	}
	completedAt := time.Now()
	if err := steps.CompleteToolStep(ctx, toolgateway.CompleteToolStepRequest{
		RunID: "run-1", StepID: "opaque-tool-step", CompletedAt: completedAt,
	}); err != nil {
		t.Fatal(err)
	}
	persisted, err := stores.Steps.ListByRun(ctx, "run-1")
	if err != nil || len(persisted) != 1 {
		t.Fatalf("list tool step: steps=%#v err=%v", persisted, err)
	}
	step := persisted[0]
	if step.Status != "completed" || step.StepType != "tool" || step.Name != "harness.echo" || step.ParentStepID != "runtime-step" || step.StartedAt.IsZero() || step.EndedAt.IsZero() {
		t.Fatalf("canonical tool step is incomplete: %#v", step)
	}
}
