package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestInMemorySchedulerEnqueueAcquireComplete(t *testing.T) {
	s := NewInMemoryScheduler(Config{LeaseTTL: time.Minute})
	dispatch, err := s.Enqueue(context.Background(), testEnqueue("run_1", PriorityNormal))
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}
	if dispatch.Status != DispatchQueued {
		t.Fatalf("unexpected status: %s", dispatch.Status)
	}

	leased, err := s.Acquire(context.Background(), AcquireRequest{WorkerID: "worker_1"})
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	if leased.RunID != "run_1" || leased.Status != DispatchLeased || leased.LeaseOwner != "worker_1" {
		t.Fatalf("unexpected leased dispatch: %#v", leased)
	}
	if err := s.Heartbeat(context.Background(), HeartbeatRequest{DispatchID: leased.DispatchID, WorkerID: "worker_1"}); err != nil {
		t.Fatalf("heartbeat failed: %v", err)
	}
	if err := s.Complete(context.Background(), CompleteDispatchRequest{DispatchID: leased.DispatchID, WorkerID: "worker_1"}); err != nil {
		t.Fatalf("complete failed: %v", err)
	}
	got, ok := s.Dispatch(leased.DispatchID)
	if !ok {
		t.Fatal("dispatch missing")
	}
	if got.Status != DispatchCompleted {
		t.Fatalf("expected completed: %#v", got)
	}
}

func TestInMemorySchedulerClonesTraceBaggage(t *testing.T) {
	s := NewInMemoryScheduler(Config{})
	req := testEnqueue("run_trace_clone", PriorityNormal)
	req.Trace.Baggage = map[string]string{"attempt": "original"}
	dispatch, err := s.Enqueue(context.Background(), req)
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	req.Trace.Baggage["attempt"] = "request mutation"
	dispatch.Trace.Baggage["attempt"] = "result mutation"
	stored, ok := s.Dispatch(dispatch.DispatchID)
	if !ok || stored.Trace.Baggage["attempt"] != "original" {
		t.Fatalf("scheduler retained caller-owned trace baggage: %#v", stored.Trace.Baggage)
	}
	stored.Trace.Baggage["attempt"] = "read mutation"
	again, _ := s.Dispatch(dispatch.DispatchID)
	if again.Trace.Baggage["attempt"] != "original" {
		t.Fatalf("scheduler exposed stored trace baggage: %#v", again.Trace.Baggage)
	}
}

func TestInMemorySchedulerPriorityAcquire(t *testing.T) {
	s := NewInMemoryScheduler(Config{})
	_, _ = s.Enqueue(context.Background(), testEnqueue("run_low", PriorityLow))
	_, _ = s.Enqueue(context.Background(), testEnqueue("run_high", PriorityHigh))
	_, _ = s.Enqueue(context.Background(), testEnqueue("run_normal", PriorityNormal))

	leased, err := s.Acquire(context.Background(), AcquireRequest{WorkerID: "worker_1"})
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	if leased.RunID != "run_high" {
		t.Fatalf("expected high priority first, got %s", leased.RunID)
	}
}

func TestInMemorySchedulerAcquireClassifiesExpiredDeadline(t *testing.T) {
	now := time.Now()
	s := NewInMemoryScheduler(Config{})
	req := testEnqueue("run_expired_before_acquire", PriorityNormal)
	req.DeadlineAt = now.Add(time.Second)
	dispatch, err := s.Enqueue(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(context.Background(), AcquireRequest{WorkerID: "worker_1", Now: now.Add(2 * time.Second)}); !errors.Is(err, ErrNoDispatchAvailable) {
		t.Fatalf("Acquire() error = %v", err)
	}
	got, _ := s.Dispatch(dispatch.DispatchID)
	if got.Status != DispatchExpired || got.ErrorType != ErrorTypeTimeout || got.Message != ErrDispatchDeadline.Error() {
		t.Fatalf("expired queued dispatch lost timeout classification: %#v", got)
	}
}

func TestInMemorySchedulerConcurrentAcquireSingleWinner(t *testing.T) {
	s := NewInMemoryScheduler(Config{LeaseTTL: time.Minute})
	_, err := s.Enqueue(context.Background(), testEnqueue("run_1", PriorityNormal))
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	const workers = 32
	var wg sync.WaitGroup
	successCh := make(chan *RunDispatch, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dispatch, err := s.Acquire(context.Background(), AcquireRequest{WorkerID: fmt.Sprintf("worker_%d", i)})
			if err == nil {
				successCh <- dispatch
			} else if !errors.Is(err, ErrNoDispatchAvailable) {
				t.Errorf("unexpected acquire error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	close(successCh)

	var successes []*RunDispatch
	for dispatch := range successCh {
		successes = append(successes, dispatch)
	}
	if len(successes) != 1 {
		t.Fatalf("expected one acquire winner, got %d", len(successes))
	}
}

func TestInMemorySchedulerHeartbeatRequiresLeaseOwner(t *testing.T) {
	s := NewInMemoryScheduler(Config{})
	_, _ = s.Enqueue(context.Background(), testEnqueue("run_1", PriorityNormal))
	leased, err := s.Acquire(context.Background(), AcquireRequest{WorkerID: "worker_1"})
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	if err := s.Heartbeat(context.Background(), HeartbeatRequest{DispatchID: leased.DispatchID, WorkerID: "worker_2"}); !errors.Is(err, ErrLeaseOwnerMismatch) {
		t.Fatalf("expected lease owner mismatch, got %v", err)
	}
}

func TestInMemorySchedulerRequeueExpiredLease(t *testing.T) {
	now := time.Now()
	s := NewInMemoryScheduler(Config{LeaseTTL: time.Second, MaxAttempts: 2})
	_, _ = s.Enqueue(context.Background(), testEnqueue("run_1", PriorityNormal))
	leased, err := s.Acquire(context.Background(), AcquireRequest{WorkerID: "worker_1", Now: now})
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}

	count, err := s.RequeueExpired(context.Background(), now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("requeue expired failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected one expired dispatch, got %d", count)
	}
	got, ok := s.Dispatch(leased.DispatchID)
	if !ok {
		t.Fatal("dispatch missing")
	}
	if got.Status != DispatchQueued {
		t.Fatalf("expected requeued, got %#v", got)
	}
}

func TestInMemorySchedulerDeadlineExpiresAtFinalAttempt(t *testing.T) {
	now := time.Now()
	s := NewInMemoryScheduler(Config{LeaseTTL: time.Minute, MaxAttempts: 1})
	req := testEnqueue("run_deadline_final_attempt", PriorityNormal)
	req.DeadlineAt = now.Add(time.Second)
	if _, err := s.Enqueue(context.Background(), req); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}
	leased, err := s.Acquire(context.Background(), AcquireRequest{WorkerID: "worker_1", Now: now})
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	if leased.Attempt != leased.MaxAttempts {
		t.Fatalf("test did not reach final attempt: %#v", leased)
	}

	count, err := s.RequeueExpired(context.Background(), now.Add(2*time.Second))
	if err != nil || count != 1 {
		t.Fatalf("RequeueExpired() = %d, %v", count, err)
	}
	got, ok := s.Dispatch(leased.DispatchID)
	if !ok || got.Status != DispatchExpired || got.ErrorType != ErrorTypeTimeout || got.Message != ErrDispatchDeadline.Error() {
		t.Fatalf("deadline dispatch must expire instead of dead-lettering: %#v", got)
	}
}

func TestInMemorySchedulerExpiredLeaseCannotComplete(t *testing.T) {
	now := time.Now()
	s := NewInMemoryScheduler(Config{LeaseTTL: time.Second})
	_, _ = s.Enqueue(context.Background(), testEnqueue("run_1", PriorityNormal))
	leased, err := s.Acquire(context.Background(), AcquireRequest{WorkerID: "worker_1", Now: now})
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}

	err = s.Complete(context.Background(), CompleteDispatchRequest{
		DispatchID: leased.DispatchID,
		WorkerID:   "worker_1",
		Now:        now.Add(2 * time.Second),
	})
	if !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("expected lease expired, got %v", err)
	}
}

func TestInMemorySchedulerRetryExhaustionDeadLetter(t *testing.T) {
	s := NewInMemoryScheduler(Config{MaxAttempts: 2})
	_, _ = s.Enqueue(context.Background(), testEnqueue("run_1", PriorityNormal))
	leased, _ := s.Acquire(context.Background(), AcquireRequest{WorkerID: "worker_1"})
	if err := s.Fail(context.Background(), FailDispatchRequest{DispatchID: leased.DispatchID, WorkerID: "worker_1", Retryable: true, ErrorType: ErrorTypeTransient}); err != nil {
		t.Fatalf("fail failed: %v", err)
	}
	leased, _ = s.Acquire(context.Background(), AcquireRequest{WorkerID: "worker_2"})
	if err := s.Fail(context.Background(), FailDispatchRequest{DispatchID: leased.DispatchID, WorkerID: "worker_2", Retryable: true, ErrorType: ErrorTypeTransient}); err != nil {
		t.Fatalf("fail failed: %v", err)
	}
	got, _ := s.Dispatch(leased.DispatchID)
	if got.Status != DispatchDeadLetter {
		t.Fatalf("expected dead letter, got %#v", got)
	}
}

func TestInMemorySchedulerCancelBlocksComplete(t *testing.T) {
	s := NewInMemoryScheduler(Config{})
	_, _ = s.Enqueue(context.Background(), testEnqueue("run_1", PriorityNormal))
	leased, _ := s.Acquire(context.Background(), AcquireRequest{WorkerID: "worker_1"})
	if err := s.Cancel(context.Background(), CancelDispatchRequest{DispatchID: leased.DispatchID, Reason: "user_cancelled"}); err != nil {
		t.Fatalf("cancel failed: %v", err)
	}
	if err := s.Complete(context.Background(), CompleteDispatchRequest{DispatchID: leased.DispatchID, WorkerID: "worker_1"}); !errors.Is(err, ErrTerminalDispatch) {
		t.Fatalf("expected terminal dispatch error, got %v", err)
	}
	got, _ := s.Dispatch(leased.DispatchID)
	if got.Status != DispatchCancelled {
		t.Fatalf("cancelled status should not be overwritten: %#v", got)
	}
	if got.LeaseOwner != "" || !got.LeaseUntil.IsZero() {
		t.Fatalf("cancelled dispatch retained lease: %#v", got)
	}
	if err := s.Cancel(context.Background(), CancelDispatchRequest{DispatchID: leased.DispatchID, Reason: "duplicate"}); err != nil {
		t.Fatalf("duplicate cancel must be idempotent: %v", err)
	}
	again, _ := s.Dispatch(leased.DispatchID)
	if again.Message != "user_cancelled" {
		t.Fatalf("duplicate cancel changed the original fact: %#v", again)
	}
}

func TestInMemorySchedulerCancelDoesNotOverwriteTerminalStatus(t *testing.T) {
	for _, status := range []DispatchStatus{DispatchCompleted, DispatchFailed, DispatchExpired, DispatchDeadLetter} {
		t.Run(string(status), func(t *testing.T) {
			s := NewInMemoryScheduler(Config{})
			dispatch, err := s.Enqueue(context.Background(), testEnqueue("run_terminal_"+string(status), PriorityNormal))
			if err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			s.dispatches[dispatch.DispatchID].Status = status
			s.mu.Unlock()
			if err := s.Cancel(context.Background(), CancelDispatchRequest{DispatchID: dispatch.DispatchID}); !errors.Is(err, ErrTerminalDispatch) {
				t.Fatalf("Cancel() error = %v", err)
			}
			got, _ := s.Dispatch(dispatch.DispatchID)
			if got.Status != status {
				t.Fatalf("terminal status overwritten: got=%s want=%s", got.Status, status)
			}
		})
	}
}

func TestInMemorySchedulerCancelAfterDeadlineExpires(t *testing.T) {
	now := time.Now()
	s := NewInMemoryScheduler(Config{})
	req := testEnqueue("run_cancel_after_deadline", PriorityNormal)
	req.DeadlineAt = now.Add(time.Second)
	dispatch, err := s.Enqueue(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(context.Background(), CancelDispatchRequest{DispatchID: dispatch.DispatchID, Now: now.Add(2 * time.Second)}); !errors.Is(err, ErrDispatchDeadline) {
		t.Fatalf("Cancel() error = %v", err)
	}
	got, _ := s.Dispatch(dispatch.DispatchID)
	if got.Status != DispatchExpired || got.ErrorType != ErrorTypeTimeout {
		t.Fatalf("deadline was overwritten by cancel: %#v", got)
	}
}

func TestWorkerExecuteOnceCompletesDispatch(t *testing.T) {
	s := NewInMemoryScheduler(Config{})
	_, _ = s.Enqueue(context.Background(), testEnqueue("run_1", PriorityNormal))
	worker := NewWorker(s, "worker_1", observability.NoopLogger{}, observability.NewNoopTracer("test"))

	if err := worker.ExecuteOnce(context.Background(), func(ctx context.Context, dispatch RunDispatch) error {
		if dispatch.RunID != "run_1" {
			t.Fatalf("unexpected run id: %s", dispatch.RunID)
		}
		return nil
	}); err != nil {
		t.Fatalf("execute once failed: %v", err)
	}
	got, _ := s.DispatchByRun("run_1")
	if got.Status != DispatchCompleted || got.LeaseOwner != "" || !got.LeaseUntil.IsZero() {
		t.Fatalf("expected completed, got %#v", got)
	}
}

func TestWorkerExecuteOnceRetryableFailureRequeues(t *testing.T) {
	s := NewInMemoryScheduler(Config{MaxAttempts: 2})
	_, _ = s.Enqueue(context.Background(), testEnqueue("run_1", PriorityNormal))
	worker := NewWorker(s, "worker_1", observability.NoopLogger{}, observability.NewNoopTracer("test"))

	err := worker.ExecuteOnce(context.Background(), func(ctx context.Context, dispatch RunDispatch) error {
		return HandlerError{Type: ErrorTypeTransient, Message: "temporary failure", Retryable: true}
	})
	if err != nil {
		t.Fatalf("worker should convert handler error to fail state, got %v", err)
	}
	got, _ := s.DispatchByRun("run_1")
	if got.Status != DispatchQueued {
		t.Fatalf("expected requeued, got %#v", got)
	}
}

func TestWorkerPreservesTypedHandlerErrorWithDeadlineCause(t *testing.T) {
	s := NewInMemoryScheduler(Config{MaxAttempts: 2})
	_, _ = s.Enqueue(context.Background(), testEnqueue("run_typed_deadline_cause", PriorityNormal))
	worker := NewWorker(s, "worker_1", nil, nil)

	err := worker.ExecuteOnce(context.Background(), func(context.Context, RunDispatch) error {
		return HandlerError{
			Type:      ErrorTypeTransient,
			Message:   "upstream deadline",
			Retryable: true,
			Err:       context.DeadlineExceeded,
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.DispatchByRun("run_typed_deadline_cause")
	if got.Status != DispatchQueued || got.ErrorType != ErrorTypeTransient {
		t.Fatalf("typed handler error was overwritten: %#v", got)
	}
}

func TestWorkerRenewsShortLeaseWithDefaultHeartbeat(t *testing.T) {
	s := NewInMemoryScheduler(Config{LeaseTTL: 20 * time.Millisecond})
	_, _ = s.Enqueue(context.Background(), testEnqueue("run_heartbeat", PriorityNormal))
	worker := NewWorker(s, "worker_1", nil, nil)

	if err := worker.ExecuteOnce(context.Background(), func(ctx context.Context, _ RunDispatch) error {
		select {
		case <-time.After(60 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}); err != nil {
		t.Fatalf("long handler lost lease: %v", err)
	}
	got, _ := s.DispatchByRun("run_heartbeat")
	if got.Status != DispatchCompleted {
		t.Fatalf("dispatch = %#v", got)
	}
}

func TestInMemorySchedulerRejectsConflictingImmutableDispatch(t *testing.T) {
	s := NewInMemoryScheduler(Config{})
	base := testEnqueue("run_conflict", PriorityNormal)
	if _, err := s.Enqueue(context.Background(), base); err != nil {
		t.Fatalf("first Enqueue() error = %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*EnqueueRunRequest)
	}{
		{name: "session", mutate: func(req *EnqueueRunRequest) { req.SessionID = "other" }},
		{name: "agent", mutate: func(req *EnqueueRunRequest) { req.AgentID = "other" }},
		{name: "ref", mutate: func(req *EnqueueRunRequest) { req.RequestRef = "other" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := base
			test.mutate(&req)
			if _, err := s.Enqueue(context.Background(), req); !errors.Is(err, ErrDispatchConflict) {
				t.Fatalf("Enqueue() error = %v, want conflict", err)
			}
		})
	}
	if got, err := s.Enqueue(context.Background(), base); err != nil || got.RunID != base.RunID {
		t.Fatalf("identical retry = %#v, %v", got, err)
	}
}

func TestWorkerMapsRuntimeCancellationToCancelledDispatch(t *testing.T) {
	s := NewInMemoryScheduler(Config{})
	_, _ = s.Enqueue(context.Background(), testEnqueue("run_cancelled_by_runtime", PriorityNormal))
	worker := NewWorker(s, "worker_1", nil, nil)
	if err := worker.ExecuteOnce(context.Background(), func(context.Context, RunDispatch) error {
		return HandlerError{Type: ErrorTypeCancelled, Message: "runtime cancelled"}
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.DispatchByRun("run_cancelled_by_runtime")
	if got.Status != DispatchCancelled || got.ErrorType != ErrorTypeCancelled {
		t.Fatalf("dispatch = %#v", got)
	}
}

func TestWorkerEnforcesDispatchDeadline(t *testing.T) {
	s := NewInMemoryScheduler(Config{LeaseTTL: time.Second})
	req := testEnqueue("run_deadline", PriorityNormal)
	req.DeadlineAt = time.Now().Add(30 * time.Millisecond)
	_, _ = s.Enqueue(context.Background(), req)
	worker := NewWorker(s, "worker_1", observability.NoopLogger{}, observability.NewNoopTracer("test"))

	if err := worker.ExecuteOnce(context.Background(), func(ctx context.Context, _ RunDispatch) error {
		<-ctx.Done()
		return ctx.Err()
	}); err != nil {
		t.Fatalf("ExecuteOnce() error = %v", err)
	}
	got, _ := s.DispatchByRun("run_deadline")
	if got.Status != DispatchExpired || got.ErrorType != ErrorTypeTimeout {
		t.Fatalf("deadline dispatch = %#v", got)
	}
}

func testEnqueue(runID string, priority Priority) EnqueueRunRequest {
	return EnqueueRunRequest{
		RunID:      runID,
		SessionID:  "session_1",
		AgentID:    "agent_1",
		RequestRef: "run-request://" + runID,
		Priority:   priority,
		Trace:      observability.TraceContext{TraceID: "trace_1"},
	}
}
