package scheduler

import (
	"context"
	"errors"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type DispatchHandler func(ctx context.Context, dispatch RunDispatch) error

type Worker struct {
	Scheduler         RunScheduler
	WorkerID          string
	HeartbeatInterval time.Duration
	Logger            observability.StructuredLogger
	Tracer            observability.TraceProvider
}

func NewWorker(s RunScheduler, workerID string, logger observability.StructuredLogger, tracer observability.TraceProvider) *Worker {
	if logger == nil {
		logger = observability.NoopLogger{}
	}
	if tracer == nil {
		tracer = observability.NewNoopTracer("scheduler")
	}
	return &Worker{Scheduler: s, WorkerID: workerID, Logger: logger, Tracer: tracer, HeartbeatInterval: 10 * time.Second}
}

func (w *Worker) ExecuteOnce(ctx context.Context, handler DispatchHandler) error {
	if w.Scheduler == nil {
		return ErrDispatchNotFound
	}
	if w.WorkerID == "" {
		return ErrWorkerIDRequired
	}
	dispatch, err := w.Scheduler.Acquire(ctx, AcquireRequest{WorkerID: w.WorkerID})
	if err != nil {
		return err
	}

	ctx = observability.WithTraceContext(ctx, dispatch.Trace)
	ctx = observability.WithLogger(ctx, w.Logger)
	ctx, span := w.Tracer.Start(ctx, "scheduler.dispatch",
		observability.String("dispatch_id", dispatch.DispatchID),
		observability.String("run_id", dispatch.RunID),
		observability.String("worker_id", w.WorkerID),
	)
	defer span.End()

	if err := w.Scheduler.Heartbeat(ctx, HeartbeatRequest{DispatchID: dispatch.DispatchID, WorkerID: w.WorkerID}); err != nil {
		span.RecordError(err)
		return err
	}
	if handler == nil {
		err := errors.New("dispatch handler missing")
		span.RecordError(err)
		return w.Scheduler.Fail(ctx, FailDispatchRequest{
			DispatchID: dispatch.DispatchID,
			WorkerID:   w.WorkerID,
			ErrorType:  ErrorTypeUnknown,
			Message:    err.Error(),
			Retryable:  false,
		})
	}
	handlerCtx := ctx
	cancelHandler := func() {}
	if !dispatch.DeadlineAt.IsZero() {
		handlerCtx, cancelHandler = context.WithDeadline(ctx, dispatch.DeadlineAt)
	}
	handlerErr, heartbeatErr := w.executeWithHeartbeat(handlerCtx, *dispatch, handler)
	handlerContextErr := handlerCtx.Err()
	cancelHandler()
	if heartbeatErr != nil {
		span.RecordError(heartbeatErr)
		// A lost lease is no longer ours to mutate; RequeueExpired or the
		// durable Scheduler backend decides its next state.
		return heartbeatErr
	}
	if handlerErr == nil {
		handlerErr = handlerContextErr
	}
	if handlerErr != nil {
		err := handlerErr
		span.RecordError(err)
		var classified HandlerError
		if errors.As(err, &classified) {
			// Handler 已给出领域分类时优先保留，避免其底层 cause 覆盖 retry 策略。
		} else if errors.Is(handlerContextErr, context.DeadlineExceeded) {
			classified = HandlerError{Type: ErrorTypeTimeout, Message: ErrDispatchDeadline.Error(), Retryable: false, Err: err}
		} else if errors.Is(handlerContextErr, context.Canceled) {
			classified = HandlerError{Type: ErrorTypeCancelled, Message: "dispatch context cancelled", Retryable: false, Err: err}
		} else {
			classified = HandlerError{Type: ErrorTypeUnknown, Message: err.Error(), Retryable: false, Err: err}
		}
		if classified.Type == "" {
			classified.Type = ErrorTypeUnknown
		}
		if classified.Type == ErrorTypeCancelled {
			return w.Scheduler.Cancel(ctx, CancelDispatchRequest{
				DispatchID: dispatch.DispatchID,
				Reason:     classified.Error(),
			})
		}
		return w.Scheduler.Fail(ctx, FailDispatchRequest{
			DispatchID: dispatch.DispatchID,
			WorkerID:   w.WorkerID,
			ErrorType:  classified.Type,
			Message:    classified.Error(),
			Retryable:  classified.Retryable,
		})
	}
	return w.Scheduler.Complete(ctx, CompleteDispatchRequest{DispatchID: dispatch.DispatchID, WorkerID: w.WorkerID})
}

func (w *Worker) executeWithHeartbeat(ctx context.Context, dispatch RunDispatch, handler DispatchHandler) (error, error) {
	interval := w.HeartbeatInterval
	if !dispatch.LeaseUntil.IsZero() {
		leaseInterval := time.Until(dispatch.LeaseUntil) / 3
		if leaseInterval > 0 && (interval <= 0 || leaseInterval < interval) {
			interval = leaseInterval
		}
	}
	if interval <= 0 {
		return handler(ctx, dispatch), nil
	}

	handlerCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	done := make(chan struct{})
	heartbeatDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				heartbeatDone <- nil
				return
			case <-handlerCtx.Done():
				heartbeatDone <- nil
				return
			case <-ticker.C:
				err := w.Scheduler.Heartbeat(handlerCtx, HeartbeatRequest{DispatchID: dispatch.DispatchID, WorkerID: w.WorkerID})
				if err != nil {
					cancel(err)
					heartbeatDone <- err
					return
				}
			}
		}
	}()
	err := handler(handlerCtx, dispatch)
	close(done)
	return err, <-heartbeatDone
}
