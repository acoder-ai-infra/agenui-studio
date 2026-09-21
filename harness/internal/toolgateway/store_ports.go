package toolgateway

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type EventStore interface {
	AppendEvent(ctx context.Context, event observability.AgentEvent) (*EventAppendResult, error)
}

type EventAppendResult struct {
	Event observability.AgentEvent
}

type ToolTerminalBinding struct {
	Keys           []string `json:"keys"`
	ToolCallID     string   `json:"tool_call_id"`
	ToolName       string   `json:"tool_name"`
	ToolVersion    string   `json:"tool_version,omitempty"`
	ArgumentsHash  string   `json:"arguments_hash"`
	DefinitionHash string   `json:"definition_hash"`
	LogicalKeyHash string   `json:"logical_key_hash"`
}

// ToolTerminalCommitter persists a terminal event and all idempotency terminal
// records in one local database transaction. Production implementations are
// expected to use the same SQL database; cross-store transactions are forbidden.
type ToolTerminalCommitter interface {
	CommitTerminal(ctx context.Context, event observability.AgentEvent, binding ToolTerminalBinding) (*EventAppendResult, error)
}

// ProductionDependency lets startup validation distinguish durable adapters
// from development implementations without depending on concrete packages.
type ProductionDependency interface {
	ProductionReady() bool
}

type MemoryEventStore struct {
	mu            sync.Mutex
	events        []observability.AgentEvent
	nextSequence  map[string]int64
	byEventID     map[string]observability.AgentEvent
	byIdempotency map[string]observability.AgentEvent
	ids           observability.IDGenerator
}

type BestEffortMemoryToolTerminalCommitter struct {
	mu     sync.Mutex
	events EventStore
}

func NewBestEffortMemoryToolTerminalCommitter(events EventStore) *BestEffortMemoryToolTerminalCommitter {
	return &BestEffortMemoryToolTerminalCommitter{events: events}
}

func (*BestEffortMemoryToolTerminalCommitter) ProductionReady() bool { return false }

func (c *BestEffortMemoryToolTerminalCommitter) CommitTerminal(ctx context.Context, event observability.AgentEvent, binding ToolTerminalBinding) (*EventAppendResult, error) {
	if c == nil || c.events == nil {
		return nil, NewToolError(ErrorTypeInternal, "terminal committer event store is required", false, nil)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	result, err := c.events.AppendEvent(ctx, event)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, NewToolError(ErrorTypeInternal, "terminal committer received nil event acknowledgement", false, nil)
	}
	return &EventAppendResult{Event: cloneAgentEvent(result.Event)}, nil
}

func NewMemoryEventStore() *MemoryEventStore {
	return &MemoryEventStore{
		nextSequence:  make(map[string]int64),
		byEventID:     make(map[string]observability.AgentEvent),
		byIdempotency: make(map[string]observability.AgentEvent),
		ids:           observability.NewULIDGenerator(""),
	}
}

func (*MemoryEventStore) ProductionReady() bool { return false }

func (s *MemoryEventStore) AppendEvent(_ context.Context, event observability.AgentEvent) (*EventAppendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nextSequence == nil {
		s.nextSequence = make(map[string]int64)
	}
	if s.byEventID == nil {
		s.byEventID = make(map[string]observability.AgentEvent)
	}
	if s.byIdempotency == nil {
		s.byIdempotency = make(map[string]observability.AgentEvent)
	}
	if s.ids == nil {
		s.ids = observability.NewULIDGenerator("")
	}

	var err error
	event, err = normalizeCanonicalEvent(s.ids, event)
	if err != nil {
		return nil, err
	}

	if existing, ok := s.byEventID[event.EventID]; ok {
		return &EventAppendResult{Event: cloneAgentEvent(existing)}, nil
	}
	if event.IdempotencyKey != "" {
		if existing, ok := s.byIdempotency[event.IdempotencyKey]; ok {
			return &EventAppendResult{Event: cloneAgentEvent(existing)}, nil
		}
	}

	s.nextSequence[event.RunID]++
	event.Sequence = s.nextSequence[event.RunID]
	persisted := cloneAgentEvent(event)
	s.events = append(s.events, persisted)
	s.byEventID[persisted.EventID] = persisted
	if persisted.IdempotencyKey != "" {
		s.byIdempotency[persisted.IdempotencyKey] = persisted
	}
	return &EventAppendResult{Event: cloneAgentEvent(persisted)}, nil
}

func (s *MemoryEventStore) Events() []observability.AgentEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	events := make([]observability.AgentEvent, len(s.events))
	for i := range s.events {
		events[i] = cloneAgentEvent(s.events[i])
	}
	return events
}

func normalizeCanonicalEvent(ids observability.IDGenerator, event observability.AgentEvent) (observability.AgentEvent, error) {
	if event.SchemaVersion == "" {
		event.SchemaVersion = observability.AgentEventSchemaVersion
	}
	if event.SchemaVersion != observability.AgentEventSchemaVersion {
		return observability.AgentEvent{}, fmt.Errorf("unsupported agent event schema version %q", event.SchemaVersion)
	}
	if !isCanonicalEventType(event.EventType) {
		return observability.AgentEvent{}, fmt.Errorf("unknown canonical event type %q", event.EventType)
	}
	if event.Visibility == "" {
		event.Visibility = observability.VisibilityDebug
	}
	if !isCanonicalEventVisibility(event.Visibility) {
		return observability.AgentEvent{}, fmt.Errorf("unknown canonical event visibility %q", event.Visibility)
	}
	if event.EventID == "" && ids != nil {
		event.EventID = ids.NewEventID()
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	if isRunScopedToolEvent(event.EventType) {
		switch {
		case event.TraceID == "":
			return observability.AgentEvent{}, fmt.Errorf("trace id is required for %s", event.EventType)
		case event.RunID == "":
			return observability.AgentEvent{}, fmt.Errorf("run id is required for %s", event.EventType)
		case event.SessionID == "":
			return observability.AgentEvent{}, fmt.Errorf("session id is required for %s", event.EventType)
		case event.EventID == "":
			return observability.AgentEvent{}, fmt.Errorf("event id is required for %s", event.EventType)
		case event.CreatedAt.IsZero():
			return observability.AgentEvent{}, fmt.Errorf("created time is required for %s", event.EventType)
		}
	}
	return event, nil
}

func isCanonicalEventType(eventType observability.EventType) bool {
	switch eventType {
	case observability.EventSessionCreated,
		observability.EventUserMessageReceived,
		observability.EventRunCreated,
		observability.EventRunStarted,
		observability.EventRunCompleted,
		observability.EventRunFailed,
		observability.EventRunCancelled,
		observability.EventRunExpired,
		observability.EventResumeAccepted,
		observability.EventResumeFailed,
		observability.EventAgentBinding,
		observability.EventAgentBindingFailed,
		observability.EventContextBuildStarted,
		observability.EventContextSnapshotCreated,
		observability.EventContextDeltaCreated,
		observability.EventModelContextBuilt,
		observability.EventModelContextBuildFailed,
		observability.EventContextBuildFailed,
		observability.EventAgentStarted,
		observability.EventAgentTextDelta,
		observability.EventReasoningSummary,
		observability.EventAgentCompleted,
		observability.EventAgentFailed,
		observability.EventModelCallStarted,
		observability.EventModelTokenDelta,
		observability.EventModelThoughtDelta,
		observability.EventModelToolCallDelta,
		observability.EventModelUsageDelta,
		observability.EventModelCallCompleted,
		observability.EventModelCallFailed,
		observability.EventModelFallbackApplied,
		observability.EventToolCallStarted,
		observability.EventToolCallProgress,
		observability.EventToolCallCompleted,
		observability.EventToolCallFailed,
		observability.EventToolCallCancelled,
		observability.EventToolArtifactCreated,
		observability.EventSubAgentStarted,
		observability.EventSubAgentProgress,
		observability.EventSubAgentCompleted,
		observability.EventSubAgentFailed,
		observability.EventA2ATaskCreated,
		observability.EventA2ATaskProgress,
		observability.EventA2ATaskCompleted,
		observability.EventA2ATaskFailed,
		observability.EventA2ATaskCancelled,
		observability.EventWorkflowStepStarted,
		observability.EventWorkflowStepCompleted,
		observability.EventWorkflowStepFailed,
		observability.EventSkillStarted,
		observability.EventSkillCompleted,
		observability.EventSkillFailed,
		observability.EventControlRequestCreated,
		observability.EventControlResponseReceived,
		observability.EventControlRequestExpired,
		observability.EventGuardrailTriggered,
		observability.EventGuardrailBlocked,
		observability.EventCheckpointCreated,
		observability.EventArtifactCreated,
		observability.EventFallbackApplied,
		observability.EventFeedbackReceived,
		observability.EventFinalResponse:
		return true
	default:
		return false
	}
}

func isCanonicalEventVisibility(visibility observability.EventVisibility) bool {
	switch visibility {
	case observability.VisibilityUserVisible,
		observability.VisibilityDebug,
		observability.VisibilityInternal,
		observability.VisibilityRestricted:
		return true
	default:
		return false
	}
}

func isRunScopedToolEvent(eventType observability.EventType) bool {
	switch eventType {
	case observability.EventToolCallStarted,
		observability.EventToolCallProgress,
		observability.EventToolCallCompleted,
		observability.EventToolCallFailed,
		observability.EventToolCallCancelled,
		observability.EventToolArtifactCreated:
		return true
	default:
		return false
	}
}

func cloneAgentEvent(event observability.AgentEvent) observability.AgentEvent {
	event.Payload = append([]byte(nil), event.Payload...)
	event.PayloadPreview = append([]byte(nil), event.PayloadPreview...)
	event.Usage = append([]byte(nil), event.Usage...)
	if event.Error != nil {
		eventError := *event.Error
		event.Error = &eventError
	}
	return event
}

type StepStore interface {
	StartToolStep(ctx context.Context, req StartToolStepRequest) (*StepSnapshot, error)
	CompleteToolStep(ctx context.Context, req CompleteToolStepRequest) error
	FailToolStep(ctx context.Context, req FailToolStepRequest) error
}

type StartToolStepRequest struct {
	TenantID     string
	SessionID    string
	RunID        string
	StepID       string
	ParentStepID string
	AgentID      string
	ToolCallID   string
	ToolName     string
	Trace        observability.TraceContext
}

type CompleteToolStepRequest struct {
	RunID       string
	StepID      string
	ToolCallID  string
	ResultRef   string
	CompletedAt time.Time
}

type FailToolStepRequest struct {
	RunID      string
	StepID     string
	ToolCallID string
	ErrorType  string
	FailedAt   time.Time
}

type StepSnapshot struct {
	StepID    string
	RunID     string
	Status    string
	StartedAt time.Time
}

type IdempotencyStore interface {
	Get(ctx context.Context, key string) (*IdempotencyRecord, bool, error)
	Put(ctx context.Context, rec IdempotencyRecord) error
}

type IdempotencyClaimer interface {
	Claim(ctx context.Context, rec IdempotencyRecord) (*IdempotencyRecord, bool, error)
}

type IdempotencyReleaser interface {
	Release(ctx context.Context, key string, toolCallID string) error
}

// IdempotencyResumer atomically transitions every key bound to one ToolCall.
// Resume returns true only for the caller that changed the complete key set
// from suspended to running; production adapters must use one local transaction.
type IdempotencyResumer interface {
	Suspend(ctx context.Context, keys []string, toolCallID string) error
	Resume(ctx context.Context, keys []string, toolCallID string) (bool, error)
}

type IdempotencyRecord struct {
	Key            string
	ToolCallID     string
	ToolName       string
	ToolVersion    string
	ArgumentsHash  string
	DefinitionHash string
	LogicalKeyHash string
	Status         ToolCallStatus
	Result         *ToolCallResult
	Failure        *ToolFailure
	CreatedAt      time.Time
}

type MemoryIdempotencyStore struct {
	mu      sync.Mutex
	records map[string]IdempotencyRecord
}

func NewMemoryIdempotencyStore() *MemoryIdempotencyStore {
	return &MemoryIdempotencyStore{records: make(map[string]IdempotencyRecord)}
}

func (*MemoryIdempotencyStore) ProductionReady() bool { return false }

func (s *MemoryIdempotencyStore) Get(_ context.Context, key string) (*IdempotencyRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[key]
	if !ok {
		return nil, false, nil
	}
	cloned := cloneIdempotencyRecord(rec)
	return &cloned, true, nil
}

func (s *MemoryIdempotencyStore) Claim(_ context.Context, rec IdempotencyRecord) (*IdempotencyRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.records[rec.Key]; ok {
		cloned := cloneIdempotencyRecord(existing)
		return &cloned, false, nil
	}
	s.records[rec.Key] = cloneIdempotencyRecord(rec)
	cloned := cloneIdempotencyRecord(rec)
	return &cloned, true, nil
}

func (s *MemoryIdempotencyStore) Put(_ context.Context, rec IdempotencyRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, found := s.records[rec.Key]
	if !found {
		s.records[rec.Key] = cloneIdempotencyRecord(rec)
		return nil
	}
	if !sameIdempotencyBinding(existing, rec) {
		return NewToolError(ErrorTypeIdempotencyConflict, "idempotency record binding cannot change", false, nil)
	}
	if isTerminalIdempotencyStatus(existing.Status) {
		if existing.Status == rec.Status {
			return nil
		}
		return NewToolError(ErrorTypeIdempotencyConflict, "idempotency terminal is immutable", false, nil)
	}
	if existing.Status == ToolCallSuspended && rec.Status == ToolCallSuspended {
		return nil
	}
	if existing.Status != ToolCallRunning {
		return NewToolError(ErrorTypeIdempotencyConflict, "idempotency record has an invalid state", false, nil)
	}
	if rec.Status == ToolCallRunning {
		return nil
	}
	if rec.Status == ToolCallSuspended {
		s.records[rec.Key] = cloneIdempotencyRecord(rec)
		return nil
	}
	if !isTerminalIdempotencyStatus(rec.Status) {
		return NewToolError(ErrorTypeIdempotencyConflict, "idempotency transition must end in a terminal state", false, nil)
	}
	s.records[rec.Key] = cloneIdempotencyRecord(rec)
	return nil
}

func (s *MemoryIdempotencyStore) Release(_ context.Context, key string, toolCallID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[key]
	if ok && rec.Status == ToolCallRunning && rec.ToolCallID == toolCallID {
		delete(s.records, key)
	}
	return nil
}

func (s *MemoryIdempotencyStore) Suspend(_ context.Context, keys []string, toolCallID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys = nonEmptyIdempotencyKeys(keys)
	if len(keys) == 0 {
		return NewToolError(ErrorTypeIdempotencyConflict, "idempotency suspension keys are missing", false, nil)
	}
	status, err := s.boundKeySetStatus(keys, toolCallID)
	if err != nil {
		return err
	}
	if status == ToolCallSuspended {
		return nil
	}
	if status != ToolCallRunning {
		return NewToolError(ErrorTypeIdempotencyConflict, "only a running tool call can be suspended", false, nil)
	}
	for _, key := range keys {
		rec := s.records[key]
		rec.Status = ToolCallSuspended
		s.records[key] = cloneIdempotencyRecord(rec)
	}
	return nil
}

func (s *MemoryIdempotencyStore) Resume(_ context.Context, keys []string, toolCallID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys = nonEmptyIdempotencyKeys(keys)
	if len(keys) == 0 {
		return false, NewToolError(ErrorTypeIdempotencyConflict, "idempotency resume keys are missing", false, nil)
	}
	status, err := s.boundKeySetStatus(keys, toolCallID)
	if err != nil {
		return false, err
	}
	if status == ToolCallRunning {
		return false, nil
	}
	if status != ToolCallSuspended {
		return false, NewToolError(ErrorTypeIdempotencyConflict, "only a suspended tool call can be resumed", false, nil)
	}
	for _, key := range keys {
		rec := s.records[key]
		rec.Status = ToolCallRunning
		s.records[key] = cloneIdempotencyRecord(rec)
	}
	return true, nil
}

func (s *MemoryIdempotencyStore) boundKeySetStatus(keys []string, toolCallID string) (ToolCallStatus, error) {
	var status ToolCallStatus
	for _, key := range keys {
		rec, ok := s.records[key]
		if !ok || rec.ToolCallID != toolCallID {
			return "", NewToolError(ErrorTypeIdempotencyConflict, "idempotency resume binding is missing", false, nil)
		}
		if status == "" {
			status = rec.Status
			continue
		}
		if rec.Status != status {
			return "", NewToolError(ErrorTypeIdempotencyConflict, "idempotency key set has inconsistent states", false, nil)
		}
	}
	return status, nil
}

func nonEmptyIdempotencyKeys(keys []string) []string {
	result := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, key)
	}
	return result
}

func cloneIdempotencyRecord(rec IdempotencyRecord) IdempotencyRecord {
	if rec.Result != nil {
		rec.Result = cloneToolCallResult(rec.Result)
	}
	if rec.Failure != nil {
		failure := *rec.Failure
		failure.ModelGuidance.AllowedNextActions = append([]string(nil), failure.ModelGuidance.AllowedNextActions...)
		failure.ModelGuidance.ForbiddenClaims = append([]string(nil), failure.ModelGuidance.ForbiddenClaims...)
		rec.Failure = &failure
	}
	return rec
}

func sameIdempotencyBinding(left, right IdempotencyRecord) bool {
	return left.ToolCallID == right.ToolCallID &&
		left.ToolName == right.ToolName &&
		left.ToolVersion == right.ToolVersion &&
		left.ArgumentsHash == right.ArgumentsHash &&
		left.DefinitionHash == right.DefinitionHash &&
		left.LogicalKeyHash == right.LogicalKeyHash
}

func isTerminalIdempotencyStatus(status ToolCallStatus) bool {
	switch status {
	case ToolCallSucceeded, ToolCallFailed, ToolCallCancelled:
		return true
	default:
		return false
	}
}
