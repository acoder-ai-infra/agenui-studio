package scheduler

import (
	"context"
	"errors"
	"time"
)

var (
	ErrRunIDRequired       = errors.New("run_id required")
	ErrSessionIDRequired   = errors.New("session_id required")
	ErrAgentIDRequired     = errors.New("agent_id required")
	ErrRequestRefRequired  = errors.New("request_ref required")
	ErrWorkerIDRequired    = errors.New("worker_id required")
	ErrDispatchIDRequired  = errors.New("dispatch_id required")
	ErrDispatchNotFound    = errors.New("dispatch not found")
	ErrQueueFull           = errors.New("run queue full")
	ErrNoDispatchAvailable = errors.New("no dispatch available")
	ErrLeaseOwnerMismatch  = errors.New("lease owner mismatch")
	ErrLeaseExpired        = errors.New("lease expired")
	ErrDispatchDeadline    = errors.New("dispatch deadline exceeded")
	ErrTerminalDispatch    = errors.New("dispatch is terminal")
	ErrDispatchConflict    = errors.New("dispatch identity conflicts with existing run")
)

func expireAtDeadline(dispatch *RunDispatch, now time.Time) bool {
	if dispatch == nil || dispatch.DeadlineAt.IsZero() || now.Before(dispatch.DeadlineAt) {
		return false
	}
	dispatch.Status = DispatchExpired
	dispatch.ErrorType = ErrorTypeTimeout
	dispatch.Message = ErrDispatchDeadline.Error()
	dispatch.LeaseOwner = ""
	dispatch.LeaseUntil = time.Time{}
	dispatch.UpdatedAt = now
	return true
}

type RunScheduler interface {
	Enqueue(ctx context.Context, req EnqueueRunRequest) (*RunDispatch, error)
	Acquire(ctx context.Context, req AcquireRequest) (*RunDispatch, error)
	Heartbeat(ctx context.Context, req HeartbeatRequest) error
	Complete(ctx context.Context, req CompleteDispatchRequest) error
	Fail(ctx context.Context, req FailDispatchRequest) error
	Cancel(ctx context.Context, req CancelDispatchRequest) error
	RequeueExpired(ctx context.Context, now time.Time) (int, error)
}

func defaultConfig(cfg Config) Config {
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 30 * time.Second
	}
	if cfg.DefaultRunTimeout <= 0 {
		cfg.DefaultRunTimeout = 5 * time.Minute
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 2
	}
	return cfg
}

func nowOr(reqNow time.Time) time.Time {
	if reqNow.IsZero() {
		return time.Now()
	}
	return reqNow
}

func isTerminal(status DispatchStatus) bool {
	switch status {
	case DispatchCompleted, DispatchFailed, DispatchCancelled, DispatchExpired, DispatchDeadLetter:
		return true
	default:
		return false
	}
}
