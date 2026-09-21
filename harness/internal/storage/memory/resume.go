package memory

import (
	"context"
	"crypto/subtle"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

const controlStatusAnswered = "answered"

type resumeStore struct {
	runs        *runStore
	checkpoints *checkpointStore
	controls    *controlStore
	events      *eventStore
}

func newResumeStore(runs *runStore, checkpoints *checkpointStore, controls *controlStore, events *eventStore) *resumeStore {
	return &resumeStore{runs: runs, checkpoints: checkpoints, controls: controls, events: events}
}

func (s *resumeStore) Wait(ctx context.Context, cmd storage.ResumeWaitCommand) error {
	if err := validateResumeWait(cmd); err != nil {
		return err
	}
	s.runs.mu.Lock()
	s.checkpoints.mu.Lock()
	s.controls.mu.Lock()
	s.events.mu.Lock()
	defer s.runs.mu.Unlock()
	defer s.checkpoints.mu.Unlock()
	defer s.controls.mu.Unlock()
	defer s.events.mu.Unlock()
	run, ok := s.runs.byID[cmd.Control.RunID]
	if !ok {
		return storage.NewError(storage.ErrNotFound, "run not found: "+cmd.Control.RunID)
	}
	checkpoint, ok := s.checkpoints.byID[cmd.Control.CheckpointID]
	if !ok {
		return storage.NewError(storage.ErrNotFound, "checkpoint not found: "+cmd.Control.CheckpointID)
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(run.TenantID); err != nil {
		return err
	}
	if checkpoint.RunID != run.RunID || checkpoint.TenantID != run.TenantID || cmd.Control.TenantID != run.TenantID {
		return storage.NewError(storage.ErrResumeBindingMismatch, "control checkpoint binding mismatch")
	}
	if existing, exists := s.controls.byID[cmd.Control.RequestID]; exists {
		if existing.RunID == cmd.Control.RunID && existing.CheckpointID == cmd.Control.CheckpointID && existing.ResumeTokenHash == cmd.Control.ResumeTokenHash {
			return nil
		}
		return storage.NewError(storage.ErrConflict, "control request already exists")
	}
	if run.Status != storage.RunStatusRunning {
		return storage.NewError(storage.ErrIllegalTransition, "run is not running")
	}
	control := *cmd.Control
	if control.CreatedAt.IsZero() {
		control.CreatedAt = time.Now()
	}
	control.Version = 1
	s.controls.byID[control.RequestID] = &control
	before := *run
	run.Status = storage.RunStatusWaitingControl
	run.Version++
	if _, err := s.events.appendLockedForTenant(cmd.Event, run.TenantID); err != nil {
		delete(s.controls.byID, control.RequestID)
		*run = before
		return err
	}
	return nil
}

func (s *resumeStore) Claim(ctx context.Context, cmd storage.ResumeClaimCommand) error {
	if err := validateResumeClaim(cmd); err != nil {
		return err
	}
	// 固定加锁顺序，确保凭证校验、claim 与事件写入是一个原子临界区。
	s.runs.mu.Lock()
	s.controls.mu.Lock()
	s.events.mu.Lock()
	defer s.runs.mu.Unlock()
	defer s.controls.mu.Unlock()
	defer s.events.mu.Unlock()

	run, ok := s.runs.byID[cmd.RunID]
	if !ok {
		return storage.NewError(storage.ErrNotFound, "run not found: "+cmd.RunID)
	}
	control, ok := s.controls.byID[cmd.ControlRequestID]
	if !ok {
		return storage.NewError(storage.ErrNotFound, "control request not found: "+cmd.ControlRequestID)
	}
	if err := enforceResumeScope(ctx, run, control); err != nil {
		return err
	}
	if !resumeBindingMatches(run, control, cmd) {
		return storage.NewError(storage.ErrResumeBindingMismatch, "resume credentials do not match persisted control request")
	}

	if run.Status == storage.RunStatusResuming {
		if run.ResumeAttemptID == cmd.AttemptID {
			if existing, ok := findResumeEventLocked(s.events, run.TenantID, cmd.Event); ok {
				if storage.SameResumeEvent(existing, cmd.Event, cmd.AttemptID) {
					return nil
				}
				return storage.NewError(storage.ErrConflict, "resume event identity is already bound to another action")
			}
		}
		return storage.NewError(storage.ErrResumeClaimHeld, "resume claim is held by another attempt")
	}
	if run.Status != storage.RunStatusWaitingControl {
		return storage.NewError(storage.ErrConflict, "run is not resumable: "+string(run.Status))
	}

	before := *run
	run.Status = storage.RunStatusResuming
	run.ResumeAttemptID = cmd.AttemptID
	run.Version++
	result, err := s.events.appendLockedForTenant(cmd.Event, run.TenantID)
	if err != nil {
		*run = before
		return err
	}
	if result.Idempotent && !storage.SameResumeEvent(result.Event, cmd.Event, cmd.AttemptID) {
		*run = before
		return storage.NewError(storage.ErrConflict, "resume event identity is already bound to another action")
	}
	return nil
}

func (s *resumeStore) Fail(ctx context.Context, cmd storage.ResumeFailureCommand) error {
	if err := validateResumeFailure(cmd); err != nil {
		return err
	}
	s.runs.mu.Lock()
	s.events.mu.Lock()
	defer s.runs.mu.Unlock()
	defer s.events.mu.Unlock()

	run, ok := s.runs.byID[cmd.RunID]
	if !ok {
		return storage.NewError(storage.ErrNotFound, "run not found: "+cmd.RunID)
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(run.TenantID); err != nil {
		return err
	}
	if existing, ok := findResumeEventLocked(s.events, run.TenantID, cmd.Event); ok {
		if storage.SameResumeEvent(existing, cmd.Event, cmd.AttemptID) {
			return nil
		}
		return storage.NewError(storage.ErrConflict, "resume event identity is already bound to another action")
	}
	if !resumeOwnerMatches(run, cmd.AttemptID) {
		return storage.NewError(storage.ErrResumeClaimLost, "resume claim ownership lost")
	}

	before := *run
	if cmd.Retryable {
		run.Status = storage.RunStatusWaitingControl
		// 可重试失败不是终态，必须清掉历史终态信息，避免后续执行读到陈旧错误。
		run.EndedAt = time.Time{}
		run.ErrorCode = ""
		run.ErrorMessage = ""
	} else {
		run.Status = storage.RunStatusFailed
		run.EndedAt = time.Now()
		run.ErrorCode = cmd.ErrorCode
		run.ErrorMessage = cmd.ErrorMessage
	}
	clearResumeClaim(run)
	run.Version++
	result, err := s.events.appendLockedForTenant(cmd.Event, run.TenantID)
	if err != nil {
		*run = before
		return err
	}
	if result.Idempotent && !storage.SameResumeEvent(result.Event, cmd.Event, cmd.AttemptID) {
		*run = before
		return storage.NewError(storage.ErrConflict, "resume event identity is already bound to another action")
	}
	return nil
}

func (s *resumeStore) Activate(ctx context.Context, cmd storage.ResumeActivationCommand) error {
	if cmd.RunID == "" || cmd.AttemptID == "" {
		return storage.NewError(storage.ErrInvalidArgument, "run_id and attempt_id required")
	}
	if err := storage.ValidateResumeAttemptIdentifiers(cmd.RunID, cmd.AttemptID); err != nil {
		return err
	}
	s.runs.mu.Lock()
	defer s.runs.mu.Unlock()
	run, ok := s.runs.byID[cmd.RunID]
	if !ok {
		return storage.NewError(storage.ErrNotFound, "run not found: "+cmd.RunID)
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(run.TenantID); err != nil {
		return err
	}
	if !resumeOwnerMatches(run, cmd.AttemptID) {
		return storage.NewError(storage.ErrResumeClaimLost, "resume claim ownership lost")
	}
	run.Status = storage.RunStatusRunning
	clearResumeClaim(run)
	run.Version++
	return nil
}

func validateResumeClaim(cmd storage.ResumeClaimCommand) error {
	if cmd.RunID == "" || cmd.SessionID == "" || cmd.CheckpointID == "" || cmd.ControlRequestID == "" ||
		cmd.ResumeTokenHash == "" || cmd.AttemptID == "" || cmd.Event.EventID == "" {
		return storage.NewError(storage.ErrInvalidArgument, "incomplete resume claim")
	}
	if err := storage.ValidateResumeClaimIdentifiers(cmd); err != nil {
		return err
	}
	if err := storage.ValidateResumeEvent(cmd.Event, cmd.RunID, cmd.AttemptID, observability.EventResumeAccepted); err != nil {
		return err
	}
	return validateEvent(cmd.Event)
}

func validateResumeWait(cmd storage.ResumeWaitCommand) error {
	if cmd.Control == nil || cmd.Control.RequestID == "" || cmd.Control.RunID == "" || cmd.Control.TenantID == "" || cmd.Control.CheckpointID == "" || cmd.Control.Type == "" || cmd.Control.ResumeTokenHash == "" || cmd.Event.EventID == "" {
		return storage.NewError(storage.ErrInvalidArgument, "incomplete resume wait")
	}
	if err := storage.ValidateResumeWaitIdentifiers(cmd); err != nil {
		return err
	}
	if cmd.Control.Status != "pending" || cmd.Control.SchemaVersion != storage.ControlRequestSchemaVersion || cmd.Event.RunID != cmd.Control.RunID || cmd.Event.EventType != observability.EventControlRequestCreated {
		return storage.NewError(storage.ErrInvalidArgument, "invalid resume wait")
	}
	return validateEvent(cmd.Event)
}

func validateResumeFailure(cmd storage.ResumeFailureCommand) error {
	if cmd.RunID == "" || cmd.AttemptID == "" || cmd.Event.EventID == "" {
		return storage.NewError(storage.ErrInvalidArgument, "incomplete resume failure")
	}
	if err := storage.ValidateResumeFailureIdentifiers(cmd); err != nil {
		return err
	}
	if err := storage.ValidateResumeEvent(cmd.Event, cmd.RunID, cmd.AttemptID, observability.EventResumeFailed); err != nil {
		return err
	}
	return validateEvent(cmd.Event)
}

func enforceResumeScope(ctx context.Context, run *storage.Run, control *storage.ControlRequest) error {
	scope := storage.ScopeFromLenient(ctx)
	if err := scope.EnforceTenant(run.TenantID); err != nil {
		return err
	}
	return scope.EnforceTenant(control.TenantID)
}

func resumeBindingMatches(run *storage.Run, control *storage.ControlRequest, cmd storage.ResumeClaimCommand) bool {
	return run.SessionID == cmd.SessionID && run.TenantID == control.TenantID && control.RunID == cmd.RunID &&
		control.CheckpointID == cmd.CheckpointID && control.Status == controlStatusAnswered &&
		constantTimeEqual(control.ResumeTokenHash, cmd.ResumeTokenHash)
}

func constantTimeEqual(left, right string) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func resumeOwnerMatches(run *storage.Run, attemptID string) bool {
	return run.Status == storage.RunStatusResuming && run.ResumeAttemptID == attemptID
}

func clearResumeClaim(run *storage.Run) {
	run.ResumeAttemptID = ""
}

func findResumeEventLocked(events *eventStore, tenantID string, desired observability.AgentEvent) (observability.AgentEvent, bool) {
	eventID := ""
	if runID, ok := events.byEvent[desired.EventID]; ok {
		if runID != desired.RunID {
			return observability.AgentEvent{EventID: desired.EventID, RunID: runID}, true
		}
		eventID = desired.EventID
	} else if desired.IdempotencyKey != "" {
		key := tenantID + "|" + desired.IdempotencyKey
		eventID = events.byIdem[key]
	}
	if eventID == "" {
		return observability.AgentEvent{}, false
	}
	for _, event := range events.byRun[events.byEvent[eventID]] {
		if event.EventID == eventID {
			return event, true
		}
	}
	return observability.AgentEvent{EventID: eventID}, true
}

var _ storage.ResumeStore = (*resumeStore)(nil)
