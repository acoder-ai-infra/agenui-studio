package agentruntime

import (
	"context"
	"errors"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storagewrite"
)

var (
	ErrStorageWriterMissing = errors.New("storage writer missing")
	ErrInvalidStorePayload  = errors.New("invalid store payload")
)

type RunStoreAction string

const (
	RunStoreActionStart            RunStoreAction = "start"
	RunStoreActionBindRuntime      RunStoreAction = "bind_runtime"
	RunStoreActionBindCapabilities RunStoreAction = "bind_capabilities"
	RunStoreActionComplete         RunStoreAction = "complete"
	RunStoreActionFail             RunStoreAction = "fail"
	RunStoreActionCancel           RunStoreAction = "cancel"
	RunStoreActionExpire           RunStoreAction = "expire"
	RunStoreActionWaitControl      RunStoreAction = "wait_control"
	RunStoreActionBeginResume      RunStoreAction = "begin_resume"
	RunStoreActionFailResume       RunStoreAction = "fail_resume"
	RunStoreActionActivateResume   RunStoreAction = "activate_resume"
)

type RunStoreWrite struct {
	Action          RunStoreAction
	Run             RunRequest
	Cancel          CancelRequest
	Resume          ResumeRequest
	Waiting         WaitingControlRequest
	Binding         RuntimeBinding
	Capabilities    RuntimeCapabilityBinding
	ResumeAttemptID string
	Event           observability.AgentEvent
	RuntimeError    *RuntimeError
	Error           string
}

type StepStoreAction string

const (
	StepStoreActionStart    StepStoreAction = "start"
	StepStoreActionWait     StepStoreAction = "wait_control"
	StepStoreActionComplete StepStoreAction = "complete"
	StepStoreActionFail     StepStoreAction = "fail"
	StepStoreActionCancel   StepStoreAction = "cancel"
)

type StepStoreWrite struct {
	Action StepStoreAction
	StepID string
	Step   StepStart
	Error  string
}

type FallbackStoreWrite struct {
	Decision FallbackDecision
}

func NewRuntimeStateStorePorts(state RuntimeStateManager) map[storagewrite.StoreName]storagewrite.StorePort {
	return map[storagewrite.StoreName]storagewrite.StorePort{
		storagewrite.StoreRun:      runtimeRunStorePort{state: state},
		storagewrite.StoreStep:     runtimeStepStorePort{state: state},
		storagewrite.StoreFallback: runtimeFallbackStorePort{state: state},
		storagewrite.StoreEvent:    runtimeEventStorePort{state: state},
	}
}

type runtimeRunStorePort struct {
	state RuntimeStateManager
}

func (p runtimeRunStorePort) Write(ctx context.Context, write storagewrite.Write) (storagewrite.WriteReceipt, error) {
	payload, ok := write.Payload.(RunStoreWrite)
	if !ok {
		return storagewrite.WriteReceipt{}, ErrInvalidStorePayload
	}
	switch payload.Action {
	case RunStoreActionStart:
		_, err := p.state.StartRun(ctx, payload.Run)
		return storagewrite.WriteReceipt{Store: write.Store, Ref: write.Ref}, err
	case RunStoreActionBindRuntime:
		return storagewrite.WriteReceipt{Store: write.Store, Ref: write.Ref}, p.state.BindRuntime(ctx, write.RunID, payload.Binding)
	case RunStoreActionBindCapabilities:
		return storagewrite.WriteReceipt{Store: write.Store, Ref: write.Ref}, p.state.BindRuntimeCapabilities(ctx, write.RunID, payload.Capabilities)
	case RunStoreActionComplete:
		return storagewrite.WriteReceipt{Store: write.Store, Ref: write.Ref}, p.state.CompleteRun(ctx, write.RunID)
	case RunStoreActionFail:
		var runErr error
		if payload.RuntimeError != nil {
			runErr = payload.RuntimeError
		} else {
			runErr = errors.New(payload.Error)
		}
		return storagewrite.WriteReceipt{Store: write.Store, Ref: write.Ref}, p.state.FailRun(ctx, write.RunID, runErr)
	case RunStoreActionCancel:
		cancel := payload.Cancel
		if cancel.RunID == "" {
			cancel.RunID = write.RunID
		}
		if cancel.SessionID == "" {
			cancel.SessionID = write.SessionID
		}
		return storagewrite.WriteReceipt{Store: write.Store, Ref: write.Ref}, p.state.CancelRun(ctx, cancel)
	case RunStoreActionExpire:
		var runErr error
		if payload.RuntimeError != nil {
			runErr = payload.RuntimeError
		} else {
			runErr = errors.New(payload.Error)
		}
		return storagewrite.WriteReceipt{Store: write.Store, Ref: write.Ref}, p.state.ExpireRun(ctx, write.RunID, runErr)
	case RunStoreActionWaitControl:
		return storagewrite.WriteReceipt{Store: write.Store, Ref: write.Ref}, p.state.EnterWaitingControl(ctx, payload.Waiting)
	case RunStoreActionBeginResume:
		return storagewrite.WriteReceipt{Store: write.Store, Ref: write.Ref}, p.state.BeginResume(ctx, payload.Resume, payload.ResumeAttemptID, payload.Event)
	case RunStoreActionFailResume:
		var resumeErr error
		if payload.RuntimeError != nil {
			resumeErr = payload.RuntimeError
		} else {
			resumeErr = errors.New(payload.Error)
		}
		return storagewrite.WriteReceipt{Store: write.Store, Ref: write.Ref}, p.state.FailResumeAttempt(ctx, write.RunID, payload.ResumeAttemptID, payload.Event, resumeErr, payload.RuntimeError != nil && payload.RuntimeError.Retryable)
	case RunStoreActionActivateResume:
		return storagewrite.WriteReceipt{Store: write.Store, Ref: write.Ref}, p.state.ActivateResume(ctx, write.RunID, payload.ResumeAttemptID)
	default:
		return storagewrite.WriteReceipt{}, ErrInvalidStorePayload
	}
}

type runtimeStepStorePort struct {
	state RuntimeStateManager
}

func (p runtimeStepStorePort) Write(ctx context.Context, write storagewrite.Write) (storagewrite.WriteReceipt, error) {
	payload, ok := write.Payload.(StepStoreWrite)
	if !ok || payload.StepID == "" {
		return storagewrite.WriteReceipt{}, ErrInvalidStorePayload
	}
	switch payload.Action {
	case StepStoreActionStart:
		step := payload.Step
		if step.StepID == "" {
			step.StepID = payload.StepID
		}
		_, err := p.state.StartStep(ctx, write.RunID, step)
		return storagewrite.WriteReceipt{Store: write.Store, Ref: write.Ref}, err
	case StepStoreActionWait:
		return storagewrite.WriteReceipt{Store: write.Store, Ref: write.Ref}, p.state.EnterStepWaitingControl(ctx, write.RunID, payload.StepID)
	case StepStoreActionComplete:
		return storagewrite.WriteReceipt{Store: write.Store, Ref: write.Ref}, p.state.CompleteStep(ctx, write.RunID, payload.StepID)
	case StepStoreActionFail:
		return storagewrite.WriteReceipt{Store: write.Store, Ref: write.Ref}, p.state.FailStep(ctx, write.RunID, payload.StepID, errors.New(payload.Error))
	case StepStoreActionCancel:
		return storagewrite.WriteReceipt{Store: write.Store, Ref: write.Ref}, p.state.CancelStep(ctx, write.RunID, payload.StepID)
	default:
		return storagewrite.WriteReceipt{}, ErrInvalidStorePayload
	}
}

type runtimeFallbackStorePort struct {
	state RuntimeStateManager
}

func (p runtimeFallbackStorePort) Write(ctx context.Context, write storagewrite.Write) (storagewrite.WriteReceipt, error) {
	payload, ok := write.Payload.(FallbackStoreWrite)
	if !ok {
		return storagewrite.WriteReceipt{}, ErrInvalidStorePayload
	}
	return storagewrite.WriteReceipt{Store: write.Store, Ref: write.Ref}, p.state.MarkFallback(ctx, payload.Decision)
}

type runtimeEventStorePort struct {
	state RuntimeStateManager
}

func (p runtimeEventStorePort) Write(ctx context.Context, write storagewrite.Write) (storagewrite.WriteReceipt, error) {
	event, ok := write.Payload.(observability.AgentEvent)
	if !ok {
		return storagewrite.WriteReceipt{}, ErrInvalidStorePayload
	}
	res, err := p.state.AppendEvent(ctx, event)
	receipt := storagewrite.WriteReceipt{Store: write.Store, Ref: write.Ref}
	if res != nil {
		receipt.Sequence = res.Sequence // carry the allocated sequence back (P1-8)
	}
	return receipt, err
}
