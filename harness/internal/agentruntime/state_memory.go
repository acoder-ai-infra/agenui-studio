package agentruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

var (
	ErrRunNotFound            = errors.New("run not found")
	ErrStepNotFound           = errors.New("step not found")
	ErrInvalidRunTransition   = errors.New("invalid run status transition")
	ErrInvalidStepTransition  = errors.New("invalid step status transition")
	ErrRunTerminal            = errors.New("run is terminal")
	ErrStepIdentityMismatch   = errors.New("step identity mismatch")
	ErrResumeBindingMismatch  = errors.New("resume binding mismatch")
	ErrResumeAlreadyClaimed   = errors.New("resume already claimed")
	ErrResumeAttemptIDMissing = errors.New("resume attempt_id required")
	ErrResumeClaimLost        = errors.New("resume claim ownership lost")
	ErrResumeEventInvalid     = errors.New("resume event invalid")
)

type resumeBinding struct {
	checkpointID     string
	controlRequestID string
	tokenHash        [sha256.Size]byte
}

type InMemoryStateManager struct {
	mu        sync.Mutex
	ids       observability.IDGenerator
	runs      map[string]*RunSnapshot
	steps     map[string]*StepSnapshot
	events    map[string][]observability.AgentEvent
	fallbacks []FallbackDecision
	resume    map[string]resumeBinding
	sequence  int64
}

func NewInMemoryStateManager() *InMemoryStateManager {
	return &InMemoryStateManager{
		ids:    observability.NewULIDGenerator(""),
		runs:   make(map[string]*RunSnapshot),
		steps:  make(map[string]*StepSnapshot),
		events: make(map[string][]observability.AgentEvent),
		resume: make(map[string]resumeBinding),
	}
}

func (m *InMemoryStateManager) StartRun(_ context.Context, req RunRequest) (*RunSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.runs[req.RunID]; existing != nil {
		if isTerminalRunStatus(existing.Status) {
			return nil, invalidRunTransition(existing.Status, RunStatusRunning)
		}
		if existing.Status == RunStatusRunning {
			return cloneRun(existing), nil
		}
		if !canTransitionRun(existing.Status, RunStatusRunning) {
			return nil, invalidRunTransition(existing.Status, RunStatusRunning)
		}
		existing.Status = RunStatusRunning
		return cloneRun(existing), nil
	}
	snapshot := &RunSnapshot{
		RunID:     req.RunID,
		SessionID: req.SessionID,
		AgentID:   req.Definition.AgentID,
		Status:    RunStatusRunning,
		StartedAt: time.Now(),
	}
	m.runs[req.RunID] = snapshot
	return cloneRun(snapshot), nil
}

func (m *InMemoryStateManager) BindRuntime(_ context.Context, runID string, binding RuntimeBinding) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[runID]
	if !ok {
		return ErrRunNotFound
	}
	if run.RuntimeBinding.SchemaVersion != "" {
		return assertRuntimeBinding(run.RuntimeBinding, binding)
	}
	if run.Status != RunStatusRunning {
		return invalidRunTransition(run.Status, RunStatusRunning)
	}
	run.RuntimeBinding = cloneRuntimeBinding(binding)
	return nil
}

func (m *InMemoryStateManager) BindRuntimeCapabilities(_ context.Context, runID string, binding RuntimeCapabilityBinding) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[runID]
	if !ok {
		return ErrRunNotFound
	}
	if run.RuntimeBinding.SchemaVersion == "" {
		return ErrRuntimeBindingMissing
	}
	if run.Status != RunStatusRunning {
		return invalidRunTransition(run.Status, RunStatusRunning)
	}
	if run.RuntimeBinding.Capabilities != nil {
		return assertRuntimeCapabilityBinding(*run.RuntimeBinding.Capabilities, binding)
	}
	cloned := binding
	cloned.MCPSnapshots = append([]RuntimeMCPSnapshotBinding(nil), binding.MCPSnapshots...)
	run.RuntimeBinding.Capabilities = &cloned
	return nil
}

func (m *InMemoryStateManager) RuntimeBinding(_ context.Context, runID string) (RuntimeBinding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[runID]
	if !ok {
		return RuntimeBinding{}, ErrRunNotFound
	}
	if run.RuntimeBinding.SchemaVersion == "" {
		return RuntimeBinding{}, ErrRuntimeBindingMissing
	}
	if err := run.RuntimeBinding.Validate(); err != nil {
		return RuntimeBinding{}, err
	}
	return cloneRuntimeBinding(run.RuntimeBinding), nil
}

func (m *InMemoryStateManager) CompleteRun(_ context.Context, runID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[runID]
	if !ok {
		return ErrRunNotFound
	}
	if !canTransitionRun(run.Status, RunStatusCompleted) {
		return invalidRunTransition(run.Status, RunStatusCompleted)
	}
	run.Status = RunStatusCompleted
	run.CompletedAt = time.Now()
	delete(m.resume, runID)
	clearResumeClaim(run)
	return nil
}

func (m *InMemoryStateManager) FailRun(_ context.Context, runID string, err error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[runID]
	if !ok {
		return ErrRunNotFound
	}
	if !canTransitionRun(run.Status, RunStatusFailed) {
		return invalidRunTransition(run.Status, RunStatusFailed)
	}
	run.Status = RunStatusFailed
	run.FailedAt = time.Now()
	delete(m.resume, runID)
	clearResumeClaim(run)
	if err != nil {
		run.ErrorMessage = err.Error()
		var runtimeErr *RuntimeError
		if errors.As(err, &runtimeErr) {
			run.ErrorCode = runtimeErr.Code
		}
	}
	return nil
}

func (m *InMemoryStateManager) CancelRun(_ context.Context, req CancelRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[req.RunID]
	if !ok {
		return ErrRunNotFound
	}
	if !canTransitionRun(run.Status, RunStatusCancelled) {
		return invalidRunTransition(run.Status, RunStatusCancelled)
	}
	run.Status = RunStatusCancelled
	run.CompletedAt = time.Now()
	run.ErrorCode = "RUN_CANCELLED"
	run.ErrorMessage = req.Reason
	delete(m.resume, req.RunID)
	clearResumeClaim(run)
	return nil
}

func (m *InMemoryStateManager) ExpireRun(_ context.Context, runID string, err error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[runID]
	if !ok {
		return ErrRunNotFound
	}
	if !canTransitionRun(run.Status, RunStatusExpired) {
		return invalidRunTransition(run.Status, RunStatusExpired)
	}
	run.Status = RunStatusExpired
	run.FailedAt = time.Now()
	delete(m.resume, runID)
	clearResumeClaim(run)
	if err != nil {
		run.ErrorMessage = err.Error()
		var runtimeErr *RuntimeError
		if errors.As(err, &runtimeErr) {
			run.ErrorCode = runtimeErr.Code
		}
	}
	return nil
}

func (m *InMemoryStateManager) EnterWaitingControl(_ context.Context, req WaitingControlRequest) error {
	if req.RunID == "" {
		return ErrRunIDMissing
	}
	if req.SessionID == "" {
		return ErrSessionMissing
	}
	if req.CheckpointID == "" {
		return ErrCheckpointIDMissing
	}
	if req.ControlRequestID == "" {
		return ErrControlRequestIDMissing
	}
	if req.ResumeToken == "" {
		return ErrResumeTokenMissing
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[req.RunID]
	if !ok {
		return ErrRunNotFound
	}
	if run.Status != RunStatusRunning {
		return invalidRunTransition(run.Status, RunStatusWaitingControl)
	}
	run.Status = RunStatusWaitingControl
	run.CheckpointID = req.CheckpointID
	run.PendingControlRequestID = req.ControlRequestID
	clearResumeClaim(run)
	m.resume[req.RunID] = resumeBinding{
		checkpointID:     req.CheckpointID,
		controlRequestID: req.ControlRequestID,
		tokenHash:        sha256.Sum256([]byte(req.ResumeToken)),
	}
	return nil
}

func (m *InMemoryStateManager) BeginResume(_ context.Context, req ResumeRequest, attemptID string, event observability.AgentEvent) error {
	if attemptID == "" {
		return ErrResumeAttemptIDMissing
	}
	if event.EventType != EventResumeAccepted || event.RunID != req.RunID || event.EventID == "" {
		return ErrResumeEventInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[req.RunID]
	if !ok {
		return ErrRunNotFound
	}
	if run.Status == RunStatusRunning {
		return ErrResumeAlreadyClaimed
	}
	if run.Status != RunStatusWaitingControl && run.Status != RunStatusResuming {
		return invalidRunTransition(run.Status, RunStatusResuming)
	}
	binding, ok := m.resume[req.RunID]
	tokenHash := sha256.Sum256([]byte(req.ResumeToken))
	if !ok || run.SessionID != req.SessionID || binding.checkpointID != req.CheckpointID || binding.controlRequestID != req.ControlRequestID || subtle.ConstantTimeCompare(binding.tokenHash[:], tokenHash[:]) != 1 {
		return ErrResumeBindingMismatch
	}
	if run.Status == RunStatusResuming {
		if run.ResumeAttemptID == attemptID {
			for _, persisted := range m.events[req.RunID] {
				if resumeEventIdentityMatches(persisted, event) && resumeEventIntentEqual(persisted, event) {
					return nil
				}
			}
		}
		return ErrResumeAlreadyClaimed
	}
	// attemptID 是唯一 owner。异常中断不在本进程自动接管，避免引入续租和
	// 常驻扫描；由外部触发的一次性修复或人工确认后再重试。
	run.Status = RunStatusResuming
	run.ResumeAttemptID = attemptID
	m.appendEventLocked(event)
	return nil
}

func resumeEventIdentityMatches(left, right observability.AgentEvent) bool {
	return left.EventID == right.EventID ||
		(right.IdempotencyKey != "" && left.IdempotencyKey == right.IdempotencyKey)
}

// resumeEventIntentEqual ignores fields assigned by the in-memory EventStore
// (sequence/timestamp/default schema), but rejects reusing an event identity
// for different resume facts.
func resumeEventIntentEqual(left, right observability.AgentEvent) bool {
	if left.RunID != right.RunID || left.SessionID != right.SessionID || left.TraceID != right.TraceID ||
		left.EventType != right.EventType || left.StepID != right.StepID || left.AgentID != right.AgentID ||
		left.Runtime != right.Runtime || left.PayloadRef != right.PayloadRef || left.DebugRef != right.DebugRef ||
		!bytes.Equal(left.Payload, right.Payload) || !bytes.Equal(left.PayloadPreview, right.PayloadPreview) ||
		!bytes.Equal(left.Usage, right.Usage) {
		return false
	}
	if left.Error == nil || right.Error == nil {
		return left.Error == nil && right.Error == nil
	}
	return *left.Error == *right.Error
}

func (m *InMemoryStateManager) FailResumeAttempt(_ context.Context, runID, attemptID string, event observability.AgentEvent, resumeErr error, retryable bool) error {
	if attemptID == "" {
		return ErrResumeAttemptIDMissing
	}
	if event.EventType != EventResumeFailed || event.RunID != runID || event.EventID == "" {
		return ErrResumeEventInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[runID]
	if !ok {
		return ErrRunNotFound
	}
	if run.Status != RunStatusResuming {
		return invalidRunTransition(run.Status, RunStatusWaitingControl)
	}
	if run.ResumeAttemptID != attemptID {
		return ErrResumeClaimLost
	}
	m.appendEventLocked(event)
	if retryable {
		run.Status = RunStatusWaitingControl
		clearResumeClaim(run)
		return nil
	}
	run.Status = RunStatusFailed
	run.FailedAt = time.Now()
	delete(m.resume, runID)
	clearResumeClaim(run)
	if resumeErr != nil {
		run.ErrorMessage = resumeErr.Error()
		var runtimeErr *RuntimeError
		if errors.As(resumeErr, &runtimeErr) {
			run.ErrorCode = runtimeErr.Code
		}
	}
	return nil
}

func (m *InMemoryStateManager) ActivateResume(_ context.Context, runID, attemptID string) error {
	if attemptID == "" {
		return ErrResumeAttemptIDMissing
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[runID]
	if !ok {
		return ErrRunNotFound
	}
	if run.Status != RunStatusResuming {
		return invalidRunTransition(run.Status, RunStatusRunning)
	}
	if run.ResumeAttemptID != attemptID {
		return ErrResumeClaimLost
	}
	run.Status = RunStatusRunning
	run.PendingControlRequestID = ""
	delete(m.resume, runID)
	clearResumeClaim(run)
	return nil
}

func (m *InMemoryStateManager) StartStep(_ context.Context, runID string, start StepStart) (*StepSnapshot, error) {
	if start.StepID == "" || start.Kind == "" {
		return nil, ErrInvalidStorePayload
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.steps[stepKey(runID, start.StepID)]; existing != nil {
		if existing.Kind != start.Kind || existing.Name != start.Name || existing.ParentStepID != start.ParentStepID {
			return nil, fmt.Errorf("%w: step_id=%s", ErrStepIdentityMismatch, start.StepID)
		}
		if existing.Status == StepStatusRunning {
			return cloneStep(existing), nil
		}
		return nil, invalidStepTransition(existing.Status, StepStatusRunning)
	}
	step := &StepSnapshot{
		RunID:        runID,
		StepID:       start.StepID,
		Kind:         start.Kind,
		Name:         start.Name,
		ParentStepID: start.ParentStepID,
		Metadata:     cloneStringMap(start.Metadata),
		Status:       StepStatusRunning,
		StartedAt:    time.Now(),
	}
	m.steps[stepKey(runID, start.StepID)] = step
	return cloneStep(step), nil
}

func (m *InMemoryStateManager) EnterStepWaitingControl(_ context.Context, runID, stepID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	step := m.steps[stepKey(runID, stepID)]
	if step == nil {
		return ErrStepNotFound
	}
	if step.Status == StepStatusWaitingControl {
		return nil
	}
	if !canTransitionStep(step.Status, StepStatusWaitingControl) {
		return invalidStepTransition(step.Status, StepStatusWaitingControl)
	}
	step.Status = StepStatusWaitingControl
	return nil
}

func (m *InMemoryStateManager) CompleteStep(_ context.Context, runID, stepID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	step := m.steps[stepKey(runID, stepID)]
	if step == nil {
		return ErrStepNotFound
	}
	if step.Status == StepStatusCompleted {
		return nil
	}
	if !canTransitionStep(step.Status, StepStatusCompleted) {
		return invalidStepTransition(step.Status, StepStatusCompleted)
	}
	step.Status = StepStatusCompleted
	step.EndedAt = time.Now()
	return nil
}

func (m *InMemoryStateManager) FailStep(_ context.Context, runID, stepID string, _ error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	step := m.steps[stepKey(runID, stepID)]
	if step == nil {
		return ErrStepNotFound
	}
	if step.Status == StepStatusFailed {
		return nil
	}
	if !canTransitionStep(step.Status, StepStatusFailed) {
		return invalidStepTransition(step.Status, StepStatusFailed)
	}
	step.Status = StepStatusFailed
	step.EndedAt = time.Now()
	return nil
}

func (m *InMemoryStateManager) CancelStep(_ context.Context, runID, stepID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	step := m.steps[stepKey(runID, stepID)]
	if step == nil {
		return ErrStepNotFound
	}
	if step.Status == StepStatusCancelled {
		return nil
	}
	if !canTransitionStep(step.Status, StepStatusCancelled) {
		return invalidStepTransition(step.Status, StepStatusCancelled)
	}
	step.Status = StepStatusCancelled
	step.EndedAt = time.Now()
	return nil
}

func (m *InMemoryStateManager) AppendEvent(_ context.Context, event observability.AgentEvent) (*EventAppendResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if event.RunID != "" {
		if run := m.runs[event.RunID]; run != nil && isTerminalRunStatus(run.Status) {
			return nil, fmt.Errorf("%w: run_id=%s status=%s event_type=%s", ErrRunTerminal, event.RunID, run.Status, event.EventType)
		}
	}
	return m.appendEventLocked(event), nil
}

func (m *InMemoryStateManager) appendEventLocked(event observability.AgentEvent) *EventAppendResult {
	if event.EventID == "" {
		event.EventID = m.ids.NewEventID()
	}
	if event.SchemaVersion == "" {
		event.SchemaVersion = observability.AgentEventSchemaVersion
	}
	if event.Visibility == "" {
		event.Visibility = observability.VisibilityDebug
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	m.sequence++
	event.Sequence = m.sequence
	m.events[event.RunID] = append(m.events[event.RunID], event)
	return &EventAppendResult{Sequence: m.sequence}
}

func (m *InMemoryStateManager) MarkFallback(_ context.Context, decision FallbackDecision) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if decision.CreatedAt.IsZero() {
		decision.CreatedAt = time.Now()
	}
	m.fallbacks = append(m.fallbacks, decision)
	return nil
}

func (m *InMemoryStateManager) Fallbacks() []FallbackDecision {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]FallbackDecision, len(m.fallbacks))
	copy(out, m.fallbacks)
	return out
}

func (m *InMemoryStateManager) Run(runID string) (RunSnapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[runID]
	if !ok {
		return RunSnapshot{}, false
	}
	return *cloneRun(run), true
}

func (m *InMemoryStateManager) Events(runID string) []observability.AgentEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	events := m.events[runID]
	out := make([]observability.AgentEvent, len(events))
	copy(out, events)
	return out
}

func (m *InMemoryStateManager) Step(runID, stepID string) (StepSnapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	step := m.steps[stepKey(runID, stepID)]
	if step == nil {
		return StepSnapshot{}, false
	}
	return *cloneStep(step), true
}

func stepKey(runID, stepID string) string {
	return runID + ":" + stepID
}

func canTransitionRun(from, to RunStatus) bool {
	if from == to {
		return true
	}
	if isTerminalRunStatus(from) {
		return false
	}
	switch from {
	case RunStatusCreated:
		return to == RunStatusRunning || to == RunStatusFailed || to == RunStatusCancelled || to == RunStatusExpired
	case RunStatusRunning, RunStatusResuming:
		return to == RunStatusWaitingControl || to == RunStatusCompleted || to == RunStatusFailed || to == RunStatusCancelled || to == RunStatusExpired
	case RunStatusWaitingControl:
		return to == RunStatusResuming || to == RunStatusFailed || to == RunStatusCancelled || to == RunStatusExpired
	default:
		return false
	}
}

func canTransitionStep(from, to StepStatus) bool {
	if from == to {
		return true
	}
	if isTerminalStepStatus(from) {
		return false
	}
	switch from {
	case StepStatusCreated:
		return to == StepStatusRunning || to == StepStatusSkipped || to == StepStatusFailed || to == StepStatusCancelled
	case StepStatusRunning:
		return to == StepStatusWaitingControl || to == StepStatusCompleted || to == StepStatusFailed || to == StepStatusCancelled
	case StepStatusWaitingControl:
		return to == StepStatusRunning || to == StepStatusFailed || to == StepStatusCancelled
	default:
		return false
	}
}

func isTerminalRunStatus(status RunStatus) bool {
	return status == RunStatusCompleted || status == RunStatusFailed || status == RunStatusCancelled || status == RunStatusExpired
}

func isTerminalStepStatus(status StepStatus) bool {
	return status == StepStatusCompleted || status == StepStatusFailed || status == StepStatusCancelled || status == StepStatusSkipped
}

func invalidRunTransition(from, to RunStatus) error {
	return fmt.Errorf("%w: %s -> %s", ErrInvalidRunTransition, from, to)
}

func invalidStepTransition(from, to StepStatus) error {
	return fmt.Errorf("%w: %s -> %s", ErrInvalidStepTransition, from, to)
}

func clearResumeClaim(run *RunSnapshot) {
	run.ResumeAttemptID = ""
}

func cloneRun(run *RunSnapshot) *RunSnapshot {
	if run == nil {
		return nil
	}
	out := *run
	out.RuntimeBinding = cloneRuntimeBinding(run.RuntimeBinding)
	return &out
}

func cloneStep(step *StepSnapshot) *StepSnapshot {
	if step == nil {
		return nil
	}
	out := *step
	out.Metadata = cloneStringMap(step.Metadata)
	return &out
}

func cloneStringMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}
