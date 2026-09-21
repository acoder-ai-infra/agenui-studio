// Package runtimestore bridges the runtime execution layer to the durable
// storage ledger. It implements agentruntime.RuntimeStateManager backed by
// storage.Stores (D1-A), and provides the config-selectable state manager (D4).
//
// It is the third package that breaks the potential import cycle: it imports
// both agentruntime and storage, while neither imports it.
package runtimestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// Bridge implements agentruntime.RuntimeStateManager on top of storage.Stores.
// Sequence allocation and idempotency live in storage.EventStore, so the runtime
// no longer owns them.
type Bridge struct{ stores storage.Stores }

// NewBridge builds a storage-backed RuntimeStateManager.
func NewBridge(stores storage.Stores) *Bridge {
	return &Bridge{stores: stores}
}

var _ agentruntime.RuntimeStateManager = (*Bridge)(nil)

// StartRun ensures the Run record exists and transitions it created->running.
// It does NOT emit run_started; RuntimeService.pipeEvents emits that via
// AppendEvent so the event flows through the EventStore like any other.
func (b *Bridge) StartRun(ctx context.Context, req agentruntime.RunRequest) (*agentruntime.RunSnapshot, error) {
	run, err := b.stores.Runs.Get(ctx, req.RunID)
	if storage.IsErrorCode(err, storage.ErrNotFound) {
		run = &storage.Run{
			RunID: req.RunID, SessionID: req.SessionID, TenantID: req.TenantID,
			AgentID: req.Definition.AgentID, Runtime: string(req.Definition.Runtime.Type),
			Status: storage.RunStatusCreated, TraceID: req.Trace.TraceID,
		}
		if err := b.stores.Runs.Create(ctx, run); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	if run.Status == storage.RunStatusRunning {
		return snapshotOf(run), nil // idempotent
	}
	if run.Status.IsTerminal() {
		return nil, agentruntime.ErrRunTerminal
	}
	updated, err := b.stores.Runs.CompareAndSetStatus(ctx, req.RunID, run.Status, storage.RunStatusRunning, storage.RunMutation{})
	if err != nil {
		return nil, err
	}
	return snapshotOf(updated), nil
}

func (b *Bridge) CompleteRun(ctx context.Context, runID string) error {
	return b.terminal(ctx, runID, storage.RunStatusCompleted, storage.RunMutation{})
}

func (b *Bridge) FailRun(ctx context.Context, runID string, cause error) error {
	mut := storage.RunMutation{}
	if cause != nil {
		mut.ErrorMessage = cause.Error()
		var runtimeErr *agentruntime.RuntimeError
		if errors.As(cause, &runtimeErr) {
			mut.ErrorCode = runtimeErr.Code
		}
	}
	return b.terminal(ctx, runID, storage.RunStatusFailed, mut)
}

func (b *Bridge) CancelRun(ctx context.Context, req agentruntime.CancelRequest) error {
	return b.terminal(ctx, req.RunID, storage.RunStatusCancelled, storage.RunMutation{ErrorMessage: req.Reason})
}

func (b *Bridge) ExpireRun(ctx context.Context, runID string, cause error) error {
	mut := storage.RunMutation{}
	if cause != nil {
		mut.ErrorMessage = cause.Error()
	}
	return b.terminal(ctx, runID, storage.RunStatusExpired, mut)
}

func (b *Bridge) BindRuntime(ctx context.Context, runID string, binding agentruntime.RuntimeBinding) error {
	raw, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	return b.stores.Runs.BindRuntimeBinding(ctx, runID, raw)
}

func (b *Bridge) BindRuntimeCapabilities(ctx context.Context, runID string, capabilities agentruntime.RuntimeCapabilityBinding) error {
	if err := capabilities.Validate(); err != nil {
		return err
	}
	run, err := b.stores.Runs.Get(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status != storage.RunStatusRunning {
		return storage.NewError(storage.ErrIllegalTransition, "runtime capabilities can only be bound while running")
	}
	var binding agentruntime.RuntimeBinding
	if len(run.RuntimeBinding) == 0 {
		return agentruntime.ErrRuntimeBindingMissing
	}
	if err := json.Unmarshal(run.RuntimeBinding, &binding); err != nil {
		return err
	}
	if err := binding.Validate(); err != nil {
		return err
	}
	if binding.Capabilities != nil {
		if sameRuntimeCapabilities(*binding.Capabilities, capabilities) {
			return nil
		}
		return agentruntime.ErrRuntimeCapabilityDrift
	}
	cloned := capabilities
	cloned.MCPSnapshots = append([]agentruntime.RuntimeMCPSnapshotBinding(nil), capabilities.MCPSnapshots...)
	binding.Capabilities = &cloned
	raw, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	swapped, err := b.stores.Runs.CompareAndSetRuntimeBinding(ctx, runID, run.RuntimeBinding, raw)
	if err != nil {
		return err
	}
	if swapped {
		return nil
	}
	latest, err := b.RuntimeBinding(ctx, runID)
	if err != nil {
		return err
	}
	if latest.Capabilities != nil && sameRuntimeCapabilities(*latest.Capabilities, capabilities) {
		return nil
	}
	return agentruntime.ErrRuntimeCapabilityDrift
}

func sameRuntimeCapabilities(left, right agentruntime.RuntimeCapabilityBinding) bool {
	if left.Hash != right.Hash || len(left.MCPSnapshots) != len(right.MCPSnapshots) {
		return false
	}
	for index := range left.MCPSnapshots {
		if left.MCPSnapshots[index] != right.MCPSnapshots[index] {
			return false
		}
	}
	return true
}

func (b *Bridge) RuntimeBinding(ctx context.Context, runID string) (agentruntime.RuntimeBinding, error) {
	run, err := b.stores.Runs.Get(ctx, runID)
	if err != nil {
		return agentruntime.RuntimeBinding{}, err
	}
	var binding agentruntime.RuntimeBinding
	if len(run.RuntimeBinding) == 0 {
		return binding, agentruntime.ErrRuntimeBindingMissing
	}
	if err := json.Unmarshal(run.RuntimeBinding, &binding); err != nil {
		return binding, err
	}
	return binding, binding.Validate()
}

func (b *Bridge) EnterWaitingControl(ctx context.Context, req agentruntime.WaitingControlRequest) error {
	if b.stores.Resumes == nil {
		return storage.NewError(storage.ErrUnsupportedCapability, "ResumeStore is required")
	}
	sum := sha256.Sum256([]byte(req.ResumeToken))
	return b.stores.Resumes.Wait(ctx, storage.ResumeWaitCommand{
		Control: &storage.ControlRequest{
			RequestID: req.ControlRequestID, RunID: req.RunID, TenantID: req.TenantID,
			CheckpointID: req.CheckpointID, Type: req.Type, Status: "pending",
			ResumeTokenHash: "sha256:" + hex.EncodeToString(sum[:]), PromptPreview: req.PromptPreview,
			SchemaVersion: storage.ControlRequestSchemaVersion,
		},
		Event: req.Event,
	})
}

func (b *Bridge) BeginResume(ctx context.Context, req agentruntime.ResumeRequest, attemptID string, event observability.AgentEvent) error {
	if b.stores.Resumes == nil {
		return storage.NewError(storage.ErrUnsupportedCapability, "ResumeStore is required")
	}
	sum := sha256.Sum256([]byte(req.ResumeToken))
	err := b.stores.Resumes.Claim(ctx, storage.ResumeClaimCommand{
		RunID: req.RunID, SessionID: req.SessionID, CheckpointID: req.CheckpointID,
		ControlRequestID: req.ControlRequestID,
		ResumeTokenHash:  "sha256:" + hex.EncodeToString(sum[:]),
		AttemptID:        attemptID,
		Event:            event,
	})
	return translateResumeError(err)
}

func (b *Bridge) FailResumeAttempt(ctx context.Context, runID, attemptID string, event observability.AgentEvent, cause error, retryable bool) error {
	if b.stores.Resumes == nil {
		return storage.NewError(storage.ErrUnsupportedCapability, "ResumeStore is required")
	}
	errorCode := ""
	var runtimeErr *agentruntime.RuntimeError
	if errors.As(cause, &runtimeErr) {
		errorCode = runtimeErr.Code
	} else if event.Error != nil {
		errorCode = event.Error.Code
	}
	errorMessage := ""
	if cause != nil {
		errorMessage = cause.Error()
	}
	err := b.stores.Resumes.Fail(ctx, storage.ResumeFailureCommand{
		RunID: runID, AttemptID: attemptID, Event: event,
		ErrorCode: errorCode, ErrorMessage: errorMessage, Retryable: retryable,
	})
	return translateResumeError(err)
}

func (b *Bridge) ActivateResume(ctx context.Context, runID, attemptID string) error {
	if b.stores.Resumes == nil {
		return storage.NewError(storage.ErrUnsupportedCapability, "ResumeStore is required")
	}
	return translateResumeError(b.stores.Resumes.Activate(ctx, storage.ResumeActivationCommand{RunID: runID, AttemptID: attemptID}))
}

func translateResumeError(err error) error {
	switch {
	case storage.IsErrorCode(err, storage.ErrResumeBindingMismatch):
		return agentruntime.ErrResumeBindingMismatch
	case storage.IsErrorCode(err, storage.ErrResumeClaimHeld):
		return agentruntime.ErrResumeAlreadyClaimed
	case storage.IsErrorCode(err, storage.ErrResumeClaimLost):
		return agentruntime.ErrResumeClaimLost
	default:
		return err
	}
}

// terminal reads the current status and CAS-transitions to a terminal state.
// If the run is already terminal, it is a no-op (idempotent).
func (b *Bridge) terminal(ctx context.Context, runID string, to storage.RunStatus, mut storage.RunMutation) error {
	run, err := b.stores.Runs.Get(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status.IsTerminal() {
		return nil
	}
	_, err = b.stores.Runs.CompareAndSetStatus(ctx, runID, run.Status, to, mut)
	return err
}

func (b *Bridge) StartStep(ctx context.Context, runID string, start agentruntime.StepStart) (*agentruntime.StepSnapshot, error) {
	step := &storage.Step{StepID: start.StepID, RunID: runID, ParentStepID: start.ParentStepID, StepType: string(start.Kind), Name: start.Name, Status: string(agentruntime.StepStatusRunning), StartedAt: time.Now()}
	if err := b.stores.Steps.Upsert(ctx, step); err != nil {
		return nil, err
	}
	return &agentruntime.StepSnapshot{StepID: start.StepID, RunID: runID, Kind: start.Kind, Name: start.Name, ParentStepID: start.ParentStepID, Status: agentruntime.StepStatusRunning, StartedAt: step.StartedAt}, nil
}

func (b *Bridge) EnterStepWaitingControl(ctx context.Context, runID, stepID string) error {
	return b.stores.Steps.Upsert(ctx, &storage.Step{StepID: stepID, RunID: runID, Status: string(agentruntime.StepStatusWaitingControl)})
}

func (b *Bridge) CancelStep(ctx context.Context, runID, stepID string) error {
	return b.stores.Steps.Upsert(ctx, &storage.Step{StepID: stepID, RunID: runID, Status: string(agentruntime.StepStatusCancelled), EndedAt: time.Now()})
}

func (b *Bridge) CompleteStep(ctx context.Context, runID, stepID string) error {
	return b.stores.Steps.Upsert(ctx, &storage.Step{StepID: stepID, RunID: runID, Status: string(agentruntime.StepStatusCompleted), EndedAt: time.Now()})
}

func (b *Bridge) FailStep(ctx context.Context, runID, stepID string, _ error) error {
	return b.stores.Steps.Upsert(ctx, &storage.Step{StepID: stepID, RunID: runID, Status: string(agentruntime.StepStatusFailed), EndedAt: time.Now()})
}

// AppendEvent delegates to the EventStore, the sole owner of sequence + idempotency.
func (b *Bridge) AppendEvent(ctx context.Context, event observability.AgentEvent) (*agentruntime.EventAppendResult, error) {
	res, err := b.stores.Events.Append(ctx, event)
	if err != nil {
		return nil, err
	}
	return &agentruntime.EventAppendResult{Sequence: res.Event.Sequence}, nil
}

// MarkFallback records a fallback decision as a fallback_applied event.
func (b *Bridge) MarkFallback(ctx context.Context, decision agentruntime.FallbackDecision) error {
	_, err := b.stores.Events.Append(ctx, observability.AgentEvent{
		RunID:      decision.RunID,
		EventType:  observability.EventFallbackApplied,
		Visibility: observability.VisibilityDebug,
		Payload:    agentruntime.JSONPayload(decision),
	})
	return err
}

func snapshotOf(run *storage.Run) *agentruntime.RunSnapshot {
	return &agentruntime.RunSnapshot{
		RunID:           run.RunID,
		SessionID:       run.SessionID,
		AgentID:         run.AgentID,
		Status:          agentruntime.RunStatus(run.Status),
		StartedAt:       run.StartedAt,
		CompletedAt:     run.EndedAt,
		ErrorCode:       run.ErrorCode,
		ErrorMessage:    run.ErrorMessage,
		ResumeAttemptID: run.ResumeAttemptID,
	}
}
