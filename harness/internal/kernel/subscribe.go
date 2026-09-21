package kernel

import (
	"context"
	"errors"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
)

// ErrBrokerUnavailable is returned by Subscribe when the kernel has no event
// broker installed. Fail closed: the SDK must not fabricate hot events.
var ErrBrokerUnavailable = errors.New("kernel: event broker is unavailable")

// Subscription is a per-run event stream owned by the kernel. It fans the
// broker's post-persist events (canonical AgentEvent) to the SDK caller.
//
// Contract (design §6.1):
//   - Subscribe MUST be installed before RunEntry.Dispatch/Resume is invoked so
//     hot deltas cannot be lost between OpenTurn and drain start.
//   - Closing the Subscription only unsubscribes; it does not cancel the Run.
//   - Events are already persisted by the emitter; the SDK layer only fans out.
type Subscription struct {
	events chan observability.AgentEvent
	cancel func()
}

// Events returns the receive-only channel. Callers must drain it or Close.
func (s *Subscription) Events() <-chan observability.AgentEvent {
	if s == nil {
		return nil
	}
	return s.events
}

// Close releases the broker subscription. It is safe to call multiple times.
func (s *Subscription) Close() {
	if s == nil || s.cancel == nil {
		return
	}
	s.cancel()
}

// SubscribeRun installs a broker subscription for runID. Callers MUST invoke it
// before triggering the run so no early event is dropped. The returned
// Subscription owns the underlying channel + cancel func.
func (k *Kernel) SubscribeRun(ctx context.Context, runID string) (*Subscription, error) {
	if k == nil || k.Broker == nil {
		return nil, ErrBrokerUnavailable
	}
	ch, cancel, err := k.Broker.Subscribe(ctx, runID)
	if err != nil {
		return nil, err
	}
	// Copy through a bounded kernel-owned channel so subscription cancellation
	// and backpressure stay under the kernel lifecycle.
	out := make(chan observability.AgentEvent, 256)
	forwardCancel := forwardEvents(ch, out, cancel)
	return &Subscription{events: out, cancel: forwardCancel}, nil
}

// SubscribeSession fans in every event in a session (across all runs). It is
// the run-tree fan-in path used when the SDK needs to observe sub-agent child
// runs alongside the parent run. Returns ErrBrokerUnavailable if the broker
// does not implement protocol.SessionSubscriber.
func (k *Kernel) SubscribeSession(ctx context.Context, sessionID string) (*Subscription, error) {
	if k == nil || k.Broker == nil {
		return nil, ErrBrokerUnavailable
	}
	sub, ok := k.Broker.(protocol.SessionSubscriber)
	if !ok {
		return nil, ErrBrokerUnavailable
	}
	ch, cancel, err := sub.SubscribeSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out := make(chan observability.AgentEvent, 256)
	forwardCancel := forwardEvents(ch, out, cancel)
	return &Subscription{events: out, cancel: forwardCancel}, nil
}

// forwardEvents copies from the broker channel into out and closes out when
// the source ends. The returned func cancels the broker subscription; it is
// idempotent and safe to call multiple times.
func forwardEvents(src <-chan observability.AgentEvent, out chan<- observability.AgentEvent, cancel func()) func() {
	done := make(chan struct{})
	go func() {
		defer close(out)
		for {
			select {
			case ev, ok := <-src:
				if !ok {
					return
				}
				select {
				case out <- ev:
				case <-done:
					return
				}
			case <-done:
				return
			}
		}
	}()
	closed := false
	return func() {
		if closed {
			return
		}
		closed = true
		close(done)
		if cancel != nil {
			cancel()
		}
	}
}
