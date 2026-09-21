package app

import (
	"context"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

// toolGatewayEventStore keeps Tool Gateway's pre-side-effect durability rule
// on the canonical EventStore. Runtime later forwards the acknowledged event;
// the canonical event_id makes that second append idempotent.
type toolGatewayEventStore struct{ events storage.EventStore }

func (s toolGatewayEventStore) AppendEvent(ctx context.Context, event observability.AgentEvent) (*toolgateway.EventAppendResult, error) {
	result, err := s.events.Append(ctx, event)
	if err != nil {
		return nil, err
	}
	return &toolgateway.EventAppendResult{Event: result.Event}, nil
}

type toolGatewayStepStore struct{ steps storage.StepStore }

func (s toolGatewayStepStore) StartToolStep(ctx context.Context, req toolgateway.StartToolStepRequest) (*toolgateway.StepSnapshot, error) {
	startedAt := time.Now()
	step := &storage.Step{
		StepID: req.StepID, RunID: req.RunID, ParentStepID: req.ParentStepID,
		StepType: "tool", Name: req.ToolName, Status: "running", StartedAt: startedAt,
	}
	if err := s.steps.Upsert(ctx, step); err != nil {
		return nil, err
	}
	return &toolgateway.StepSnapshot{StepID: req.StepID, RunID: req.RunID, Status: step.Status, StartedAt: startedAt}, nil
}

func (s toolGatewayStepStore) CompleteToolStep(ctx context.Context, req toolgateway.CompleteToolStepRequest) error {
	return s.steps.Upsert(ctx, &storage.Step{
		StepID: req.StepID, RunID: req.RunID, StepType: "tool", Status: "completed", EndedAt: req.CompletedAt,
	})
}

func (s toolGatewayStepStore) FailToolStep(ctx context.Context, req toolgateway.FailToolStepRequest) error {
	return s.steps.Upsert(ctx, &storage.Step{
		StepID: req.StepID, RunID: req.RunID, StepType: "tool", Status: "failed", EndedAt: req.FailedAt,
	})
}

var (
	_ toolgateway.EventStore = toolGatewayEventStore{}
	_ toolgateway.StepStore  = toolGatewayStepStore{}
)
