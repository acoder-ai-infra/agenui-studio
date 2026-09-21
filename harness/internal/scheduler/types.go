package scheduler

import (
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type Priority string

const (
	PriorityHigh   Priority = "high"
	PriorityNormal Priority = "normal"
	PriorityLow    Priority = "low"
)

type DispatchStatus string

const (
	DispatchQueued     DispatchStatus = "queued"
	DispatchLeased     DispatchStatus = "leased"
	DispatchRunning    DispatchStatus = "running"
	DispatchCompleted  DispatchStatus = "completed"
	DispatchFailed     DispatchStatus = "failed"
	DispatchCancelled  DispatchStatus = "cancelled"
	DispatchExpired    DispatchStatus = "expired"
	DispatchDeadLetter DispatchStatus = "dead_letter"
)

type ErrorType string

const (
	ErrorTypeUnknown          ErrorType = "unknown"
	ErrorTypeTimeout          ErrorType = "timeout"
	ErrorTypeLeaseExpired     ErrorType = "lease_expired"
	ErrorTypeWorkerCrash      ErrorType = "worker_crash"
	ErrorTypeTransient        ErrorType = "transient"
	ErrorTypePermissionDenied ErrorType = "permission_denied"
	ErrorTypeGuardrailBlocked ErrorType = "guardrail_blocked"
	ErrorTypeCancelled        ErrorType = "cancelled"
)

type EnqueueRunRequest struct {
	RunID       string                     `json:"run_id"`
	SessionID   string                     `json:"session_id"`
	AgentID     string                     `json:"agent_id"`
	RequestRef  string                     `json:"request_ref"`
	Priority    Priority                   `json:"priority,omitempty"`
	DeadlineAt  time.Time                  `json:"deadline_at,omitempty"`
	MaxAttempts int                        `json:"max_attempts,omitempty"`
	Trace       observability.TraceContext `json:"trace"`
}

type AcquireRequest struct {
	WorkerID string    `json:"worker_id"`
	Now      time.Time `json:"now,omitempty"`
}

type HeartbeatRequest struct {
	DispatchID string    `json:"dispatch_id"`
	WorkerID   string    `json:"worker_id"`
	Now        time.Time `json:"now,omitempty"`
}

type CompleteDispatchRequest struct {
	DispatchID string    `json:"dispatch_id"`
	WorkerID   string    `json:"worker_id"`
	Now        time.Time `json:"now,omitempty"`
}

type FailDispatchRequest struct {
	DispatchID string    `json:"dispatch_id"`
	WorkerID   string    `json:"worker_id"`
	ErrorType  ErrorType `json:"error_type,omitempty"`
	Message    string    `json:"message,omitempty"`
	Retryable  bool      `json:"retryable"`
	Now        time.Time `json:"now,omitempty"`
}

type CancelDispatchRequest struct {
	RunID      string    `json:"run_id,omitempty"`
	DispatchID string    `json:"dispatch_id,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Now        time.Time `json:"now,omitempty"`
}

type RunDispatch struct {
	DispatchID  string                     `json:"dispatch_id"`
	RunID       string                     `json:"run_id"`
	SessionID   string                     `json:"session_id"`
	AgentID     string                     `json:"agent_id"`
	RequestRef  string                     `json:"request_ref"`
	Priority    Priority                   `json:"priority"`
	Status      DispatchStatus             `json:"status"`
	Attempt     int                        `json:"attempt"`
	MaxAttempts int                        `json:"max_attempts"`
	LeaseOwner  string                     `json:"lease_owner,omitempty"`
	LeaseUntil  time.Time                  `json:"lease_until,omitempty"`
	DeadlineAt  time.Time                  `json:"deadline_at,omitempty"`
	Trace       observability.TraceContext `json:"trace"`
	ErrorType   ErrorType                  `json:"error_type,omitempty"`
	Message     string                     `json:"message,omitempty"`
	CreatedAt   time.Time                  `json:"created_at"`
	UpdatedAt   time.Time                  `json:"updated_at"`
}

type Config struct {
	LeaseTTL          time.Duration
	DefaultRunTimeout time.Duration
	MaxAttempts       int
	MaxQueueDepth     int
}

type HandlerError struct {
	Type      ErrorType
	Message   string
	Retryable bool
	Err       error
}

func (e HandlerError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return string(e.Type)
}

func (e HandlerError) Unwrap() error {
	return e.Err
}
