package scheduler

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type InMemoryScheduler struct {
	mu         sync.Mutex
	cfg        Config
	ids        observability.IDGenerator
	dispatches map[string]*RunDispatch
	byRun      map[string]string
}

func NewInMemoryScheduler(cfg Config) *InMemoryScheduler {
	return &InMemoryScheduler{
		cfg:        defaultConfig(cfg),
		ids:        observability.NewULIDGenerator(""),
		dispatches: make(map[string]*RunDispatch),
		byRun:      make(map[string]string),
	}
}

func (s *InMemoryScheduler) Enqueue(_ context.Context, req EnqueueRunRequest) (*RunDispatch, error) {
	if req.RunID == "" {
		return nil, ErrRunIDRequired
	}
	if req.SessionID == "" {
		return nil, ErrSessionIDRequired
	}
	if req.AgentID == "" {
		return nil, ErrAgentIDRequired
	}
	if req.RequestRef == "" {
		return nil, ErrRequestRefRequired
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existingID := s.byRun[req.RunID]; existingID != "" {
		existing := s.dispatches[existingID]
		if existing == nil || existing.SessionID != req.SessionID || existing.AgentID != req.AgentID ||
			existing.RequestRef != req.RequestRef {
			return nil, ErrDispatchConflict
		}
		return cloneDispatch(existing), nil
	}
	if s.cfg.MaxQueueDepth > 0 && s.queueDepthLocked() >= s.cfg.MaxQueueDepth {
		return nil, ErrQueueFull
	}

	now := time.Now()
	priority := req.Priority
	if priority == "" {
		priority = PriorityNormal
	}
	maxAttempts := req.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = s.cfg.MaxAttempts
	}
	deadline := req.DeadlineAt
	if deadline.IsZero() {
		deadline = now.Add(s.cfg.DefaultRunTimeout)
	}
	dispatch := &RunDispatch{
		DispatchID:  s.ids.NewRunID(),
		RunID:       req.RunID,
		SessionID:   req.SessionID,
		AgentID:     req.AgentID,
		RequestRef:  req.RequestRef,
		Priority:    priority,
		Status:      DispatchQueued,
		Attempt:     0,
		MaxAttempts: maxAttempts,
		DeadlineAt:  deadline,
		Trace:       cloneTraceContext(req.Trace),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	s.dispatches[dispatch.DispatchID] = dispatch
	s.byRun[dispatch.RunID] = dispatch.DispatchID
	return cloneDispatch(dispatch), nil
}

func (s *InMemoryScheduler) Acquire(_ context.Context, req AcquireRequest) (*RunDispatch, error) {
	if req.WorkerID == "" {
		return nil, ErrWorkerIDRequired
	}
	now := nowOr(req.Now)
	s.mu.Lock()
	defer s.mu.Unlock()

	candidates := make([]*RunDispatch, 0, len(s.dispatches))
	for _, dispatch := range s.dispatches {
		if dispatch.Status != DispatchQueued {
			continue
		}
		if expireAtDeadline(dispatch, now) {
			continue
		}
		candidates = append(candidates, dispatch)
	}
	if len(candidates) == 0 {
		return nil, ErrNoDispatchAvailable
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if priorityRank(left.Priority) != priorityRank(right.Priority) {
			return priorityRank(left.Priority) < priorityRank(right.Priority)
		}
		return left.CreatedAt.Before(right.CreatedAt)
	})
	dispatch := candidates[0]
	dispatch.Status = DispatchLeased
	dispatch.Attempt++
	dispatch.LeaseOwner = req.WorkerID
	dispatch.LeaseUntil = now.Add(s.cfg.LeaseTTL)
	dispatch.UpdatedAt = now
	return cloneDispatch(dispatch), nil
}

func (s *InMemoryScheduler) Heartbeat(_ context.Context, req HeartbeatRequest) error {
	if req.DispatchID == "" {
		return ErrDispatchIDRequired
	}
	if req.WorkerID == "" {
		return ErrWorkerIDRequired
	}
	now := nowOr(req.Now)
	s.mu.Lock()
	defer s.mu.Unlock()
	dispatch, err := s.dispatchLocked(req.DispatchID)
	if err != nil {
		return err
	}
	if isTerminal(dispatch.Status) {
		return ErrTerminalDispatch
	}
	if expireAtDeadline(dispatch, now) {
		return ErrDispatchDeadline
	}
	if dispatch.LeaseOwner != req.WorkerID {
		return ErrLeaseOwnerMismatch
	}
	if leaseExpired(dispatch, now) {
		return ErrLeaseExpired
	}
	dispatch.Status = DispatchRunning
	dispatch.LeaseUntil = now.Add(s.cfg.LeaseTTL)
	dispatch.UpdatedAt = now
	return nil
}

func (s *InMemoryScheduler) Complete(_ context.Context, req CompleteDispatchRequest) error {
	if req.DispatchID == "" {
		return ErrDispatchIDRequired
	}
	if req.WorkerID == "" {
		return ErrWorkerIDRequired
	}
	now := nowOr(req.Now)
	s.mu.Lock()
	defer s.mu.Unlock()
	dispatch, err := s.dispatchLocked(req.DispatchID)
	if err != nil {
		return err
	}
	if isTerminal(dispatch.Status) {
		return ErrTerminalDispatch
	}
	if expireAtDeadline(dispatch, now) {
		return ErrDispatchDeadline
	}
	if dispatch.LeaseOwner != req.WorkerID {
		return ErrLeaseOwnerMismatch
	}
	if leaseExpired(dispatch, now) {
		return ErrLeaseExpired
	}
	dispatch.Status = DispatchCompleted
	dispatch.LeaseOwner = ""
	dispatch.LeaseUntil = time.Time{}
	dispatch.UpdatedAt = now
	return nil
}

func (s *InMemoryScheduler) Fail(_ context.Context, req FailDispatchRequest) error {
	if req.DispatchID == "" {
		return ErrDispatchIDRequired
	}
	if req.WorkerID == "" {
		return ErrWorkerIDRequired
	}
	now := nowOr(req.Now)
	s.mu.Lock()
	defer s.mu.Unlock()
	dispatch, err := s.dispatchLocked(req.DispatchID)
	if err != nil {
		return err
	}
	if isTerminal(dispatch.Status) {
		return ErrTerminalDispatch
	}
	if expireAtDeadline(dispatch, now) {
		return nil
	}
	if dispatch.LeaseOwner != req.WorkerID {
		return ErrLeaseOwnerMismatch
	}
	if leaseExpired(dispatch, now) {
		return ErrLeaseExpired
	}
	dispatch.ErrorType = req.ErrorType
	if dispatch.ErrorType == "" {
		dispatch.ErrorType = ErrorTypeUnknown
	}
	dispatch.Message = req.Message
	dispatch.LeaseOwner = ""
	dispatch.LeaseUntil = time.Time{}
	dispatch.UpdatedAt = now
	if req.Retryable && dispatch.Attempt < dispatch.MaxAttempts {
		dispatch.Status = DispatchQueued
		return nil
	}
	if req.Retryable {
		dispatch.Status = DispatchDeadLetter
		return nil
	}
	dispatch.Status = DispatchFailed
	return nil
}

func (s *InMemoryScheduler) Cancel(_ context.Context, req CancelDispatchRequest) error {
	now := nowOr(req.Now)
	s.mu.Lock()
	defer s.mu.Unlock()
	dispatch, err := s.resolveDispatchLocked(req.DispatchID, req.RunID)
	if err != nil {
		return err
	}
	if dispatch.Status == DispatchCancelled {
		return nil
	}
	if isTerminal(dispatch.Status) {
		return ErrTerminalDispatch
	}
	if expireAtDeadline(dispatch, now) {
		return ErrDispatchDeadline
	}
	dispatch.Status = DispatchCancelled
	dispatch.ErrorType = ErrorTypeCancelled
	dispatch.Message = req.Reason
	dispatch.LeaseOwner = ""
	dispatch.LeaseUntil = time.Time{}
	dispatch.UpdatedAt = now
	return nil
}

func (s *InMemoryScheduler) RequeueExpired(_ context.Context, now time.Time) (int, error) {
	now = nowOr(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, dispatch := range s.dispatches {
		if isTerminal(dispatch.Status) {
			continue
		}
		expiredByLease := (dispatch.Status == DispatchLeased || dispatch.Status == DispatchRunning) && !dispatch.LeaseUntil.IsZero() && !now.Before(dispatch.LeaseUntil)
		expiredByDeadline := !dispatch.DeadlineAt.IsZero() && !now.Before(dispatch.DeadlineAt)
		if !expiredByLease && !expiredByDeadline {
			continue
		}
		if expiredByDeadline {
			// deadline 是本次 Dispatch 的绝对执行边界，与 attempt 是否耗尽无关。
			expireAtDeadline(dispatch, now)
			count++
			continue
		}
		dispatch.ErrorType = ErrorTypeLeaseExpired
		dispatch.LeaseOwner = ""
		dispatch.LeaseUntil = time.Time{}
		dispatch.UpdatedAt = now
		if dispatch.Attempt < dispatch.MaxAttempts {
			dispatch.Status = DispatchQueued
		} else {
			dispatch.Status = DispatchDeadLetter
		}
		count++
	}
	return count, nil
}

func (s *InMemoryScheduler) Dispatch(dispatchID string) (RunDispatch, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dispatch := s.dispatches[dispatchID]
	if dispatch == nil {
		return RunDispatch{}, false
	}
	return *cloneDispatch(dispatch), true
}

func (s *InMemoryScheduler) DispatchByRun(runID string) (RunDispatch, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dispatchID := s.byRun[runID]
	if dispatchID == "" {
		return RunDispatch{}, false
	}
	dispatch := s.dispatches[dispatchID]
	if dispatch == nil {
		return RunDispatch{}, false
	}
	return *cloneDispatch(dispatch), true
}

func (s *InMemoryScheduler) queueDepthLocked() int {
	count := 0
	for _, dispatch := range s.dispatches {
		if dispatch.Status == DispatchQueued {
			count++
		}
	}
	return count
}

func (s *InMemoryScheduler) dispatchLocked(dispatchID string) (*RunDispatch, error) {
	dispatch := s.dispatches[dispatchID]
	if dispatch == nil {
		return nil, ErrDispatchNotFound
	}
	return dispatch, nil
}

func (s *InMemoryScheduler) resolveDispatchLocked(dispatchID, runID string) (*RunDispatch, error) {
	if dispatchID != "" {
		return s.dispatchLocked(dispatchID)
	}
	if runID != "" {
		dispatchID = s.byRun[runID]
	}
	if dispatchID == "" {
		return nil, ErrDispatchIDRequired
	}
	return s.dispatchLocked(dispatchID)
}

func priorityRank(priority Priority) int {
	switch priority {
	case PriorityHigh:
		return 0
	case PriorityNormal, "":
		return 1
	case PriorityLow:
		return 2
	default:
		return 1
	}
}

func leaseExpired(dispatch *RunDispatch, now time.Time) bool {
	return !dispatch.LeaseUntil.IsZero() && !now.Before(dispatch.LeaseUntil)
}

func cloneDispatch(dispatch *RunDispatch) *RunDispatch {
	if dispatch == nil {
		return nil
	}
	out := *dispatch
	out.Trace = cloneTraceContext(dispatch.Trace)
	return &out
}

func cloneTraceContext(trace observability.TraceContext) observability.TraceContext {
	if trace.Baggage == nil {
		return trace
	}
	source := trace.Baggage
	trace.Baggage = make(map[string]string, len(source))
	for key, value := range source {
		trace.Baggage[key] = value
	}
	return trace
}
