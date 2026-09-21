package agentruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestRuntimeEventCommitBarrierBlocksUntilDurableAcknowledge(t *testing.T) {
	barrier := newRuntimeEventCommitBarrier(observability.NewULIDGenerator("test"))
	ctx, cancel := context.WithTimeout(withRuntimeEventCommitBarrier(context.Background(), barrier), time.Second)
	defer cancel()
	sent := make(chan observability.AgentEvent, 1)
	done := make(chan error, 1)
	go func() {
		done <- emitRuntimeEventWithCommitBarrier(ctx, observability.AgentEvent{
			EventType: observability.EventModelCallCompleted,
		}, func(event observability.AgentEvent) error {
			sent <- event
			return nil
		})
	}()
	event := <-sent
	if event.EventID == "" {
		t.Fatal("commit barrier did not assign an event_id")
	}
	select {
	case err := <-done:
		t.Fatalf("emit returned before durable acknowledgement: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	barrier.acknowledge(event.EventID, nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("emit failed after acknowledgement: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("emit did not return after durable acknowledgement")
	}
}

func TestRuntimeEventCommitBarrierDoesNotTrustPositiveAdapterSequence(t *testing.T) {
	barrier := newRuntimeEventCommitBarrier(observability.NewULIDGenerator("test"))
	ctx, cancel := context.WithTimeout(withRuntimeEventCommitBarrier(context.Background(), barrier), time.Second)
	defer cancel()
	sent := make(chan observability.AgentEvent, 1)
	done := make(chan error, 1)
	go func() {
		done <- emitRuntimeEventWithCommitBarrier(ctx, observability.AgentEvent{
			EventType: observability.EventModelCallCompleted,
			Sequence:  41, // upstream/adaptor metadata is not a durable-store ack
		}, func(event observability.AgentEvent) error {
			sent <- event
			return nil
		})
	}()
	event := <-sent
	select {
	case err := <-done:
		t.Fatalf("positive adapter sequence bypassed durable acknowledgement: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	barrier.acknowledge(event.EventID, nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("emit failed after durable acknowledgement: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("emit did not return after durable acknowledgement")
	}
}

func TestRuntimeEventCommitBarrierPropagatesAppendFailureAndCancellation(t *testing.T) {
	t.Run("append failure", func(t *testing.T) {
		barrier := newRuntimeEventCommitBarrier(observability.NewULIDGenerator("test"))
		ctx := withRuntimeEventCommitBarrier(context.Background(), barrier)
		want := errors.New("append failed")
		err := emitRuntimeEventWithCommitBarrier(ctx, observability.AgentEvent{
			EventType: observability.EventModelCallCompleted,
		}, func(event observability.AgentEvent) error {
			barrier.acknowledge(event.EventID, want)
			return nil
		})
		if !errors.Is(err, want) {
			t.Fatalf("error=%v want=%v", err, want)
		}
	})

	t.Run("cancelled waiter", func(t *testing.T) {
		barrier := newRuntimeEventCommitBarrier(observability.NewULIDGenerator("test"))
		ctx, cancel := context.WithCancel(withRuntimeEventCommitBarrier(context.Background(), barrier))
		cancel()
		err := emitRuntimeEventWithCommitBarrier(ctx, observability.AgentEvent{
			EventType: observability.EventModelCallCompleted,
		}, func(observability.AgentEvent) error { return nil })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v want context cancellation", err)
		}
		barrier.failAll(context.Canceled)
	})
}
