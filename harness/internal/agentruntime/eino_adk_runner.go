package agentruntime

import (
	"context"
	"errors"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

var ErrEinoAgentMissing = errors.New("eino agent missing")

type ADKEinoExecutionFactory struct{}

func (ADKEinoExecutionFactory) New(ctx context.Context, agent adk.ResumableAgent, checkpoints adk.CheckPointStore) (EinoExecution, error) {
	if agent == nil {
		return nil, ErrEinoAgentMissing
	}
	return &adkEinoExecution{runner: adk.NewRunner(ctx, adk.RunnerConfig{
		Agent:           agent,
		EnableStreaming: true,
		CheckPointStore: checkpoints,
	})}, nil
}

type adkEinoExecution struct {
	runner *adk.Runner
}

func (e *adkEinoExecution) Run(ctx context.Context, messages []*schema.Message, checkpointID string) (EinoEventIterator, adk.AgentCancelFunc, error) {
	if e == nil || e.runner == nil {
		return nil, nil, ErrEinoAgentMissing
	}
	cancelOption, cancel := adk.WithCancel()
	options := []adk.AgentRunOption{cancelOption}
	if checkpointID != "" {
		options = append(options, adk.WithCheckPointID(checkpointID))
	}
	return e.runner.Run(ctx, messages, options...), cancel, nil
}

func (e *adkEinoExecution) Resume(ctx context.Context, checkpointID string, params *adk.ResumeParams) (EinoEventIterator, adk.AgentCancelFunc, error) {
	if e == nil || e.runner == nil {
		return nil, nil, ErrEinoAgentMissing
	}
	cancelOption, cancel := adk.WithCancel()
	if params == nil {
		iterator, err := e.runner.Resume(ctx, checkpointID, cancelOption)
		return iterator, cancel, err
	}
	iterator, err := e.runner.ResumeWithParams(ctx, checkpointID, params, cancelOption)
	return iterator, cancel, err
}
