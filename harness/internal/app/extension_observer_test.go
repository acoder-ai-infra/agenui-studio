package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
)

// countingObserver 记录 Observe 次数，并可按需返回错误 / panic / 阻塞。
type countingObserver struct {
	calls   atomic.Int64
	lastEvt atomic.Value
	fail    error
	panicOn bool
	block   time.Duration
}

func (o *countingObserver) Observe(ctx context.Context, ev extension.ProtocolEvent) error {
	o.calls.Add(1)
	o.lastEvt.Store(ev)
	if o.panicOn {
		panic("observer boom")
	}
	if o.block > 0 {
		select {
		case <-time.After(o.block):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return o.fail
}

func mustObserverCatalog(t *testing.T, entries ...kernel.ExtensionEntry) *kernel.ExtensionCatalog {
	t.Helper()
	catalog, err := kernel.NewExtensionCatalog(entries)
	if err != nil {
		t.Fatalf("build extension catalog: %v", err)
	}
	return catalog
}

func testAgentEvent() observability.AgentEvent {
	return observability.AgentEvent{
		EventID:   "evt_1",
		Sequence:  7,
		RunID:     "run_1",
		SessionID: "sess_1",
		EventType: observability.EventRunStarted,
		Error: &observability.EventError{
			Code: "E1", Type: observability.EventErrorInternal, Message: "m", Retryable: true,
		},
		CreatedAt: time.Now(),
	}
}

// TestObservedBrokerFansOutOnPublish 验证 Publish 触发 observer 并透传事件字段。
func TestObservedBrokerFansOutOnPublish(t *testing.T) {
	obs := &countingObserver{}
	catalog := mustObserverCatalog(t, kernel.ExtensionEntry{
		ID: "audit", Kind: kernel.ExtEventObserver, Implementation: obs,
	})
	broker := newObservedBroker(protocol.NewMemoryBroker(), catalog, observability.NoopLogger{})

	if err := broker.Publish(context.Background(), testAgentEvent()); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := obs.calls.Load(); got != 1 {
		t.Fatalf("observer calls = %d; want 1", got)
	}
	ev, _ := obs.lastEvt.Load().(extension.ProtocolEvent)
	if ev.EventID != "evt_1" || ev.RunID != "run_1" || ev.EventType != string(observability.EventRunStarted) {
		t.Fatalf("observer saw unexpected event: %+v", ev)
	}
	if ev.Error == nil || ev.Error.Code != "E1" || !ev.Error.Retryable {
		t.Fatalf("observer error projection missing: %+v", ev.Error)
	}
}

// TestObservedBrokerFailOpen 验证 observer 报错 / panic 均不影响事件发布。
func TestObservedBrokerFailOpen(t *testing.T) {
	failing := &countingObserver{fail: errors.New("audit backend down")}
	panicking := &countingObserver{panicOn: true}
	trailing := &countingObserver{}
	catalog := mustObserverCatalog(t,
		kernel.ExtensionEntry{ID: "a_failing", Kind: kernel.ExtEventObserver, Order: 1, Implementation: failing},
		kernel.ExtensionEntry{ID: "b_panicking", Kind: kernel.ExtEventObserver, Order: 2, Implementation: panicking},
		kernel.ExtensionEntry{ID: "c_trailing", Kind: kernel.ExtEventObserver, Order: 3, Implementation: trailing},
	)
	inner := protocol.NewMemoryBroker()
	broker := newObservedBroker(inner, catalog, observability.NoopLogger{})

	ctx := context.Background()
	sub, cancel, err := inner.Subscribe(ctx, "run_1")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	if err := broker.Publish(ctx, testAgentEvent()); err != nil {
		t.Fatalf("publish must stay fail-open, got: %v", err)
	}
	// 前序 observer 失败 / panic 后，后续 observer 仍执行。
	if trailing.calls.Load() != 1 {
		t.Fatalf("trailing observer calls = %d; want 1", trailing.calls.Load())
	}
	// 事件仍到达底层 broker 的订阅者。
	select {
	case ev := <-sub:
		if ev.EventID != "evt_1" {
			t.Fatalf("subscriber saw %q; want evt_1", ev.EventID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not receive the published event")
	}
}

// TestObservedBrokerTimeoutBounded 验证慢 observer 被 per-entry 超时约束，
// 不会拖住 Publish。
func TestObservedBrokerTimeoutBounded(t *testing.T) {
	slow := &countingObserver{block: 5 * time.Second}
	catalog := mustObserverCatalog(t, kernel.ExtensionEntry{
		ID: "slow", Kind: kernel.ExtEventObserver, Timeout: 50 * time.Millisecond, Implementation: slow,
	})
	broker := newObservedBroker(protocol.NewMemoryBroker(), catalog, observability.NoopLogger{})

	start := time.Now()
	if err := broker.Publish(context.Background(), testAgentEvent()); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("publish blocked %v by slow observer; timeout not applied", elapsed)
	}
}

// TestObservedBrokerPreservesSessionSubscriber 验证包装后保留底层 broker 的
// SessionSubscriber 可选能力（run-tree 订阅依赖类型断言探测）。
func TestObservedBrokerPreservesSessionSubscriber(t *testing.T) {
	obs := &countingObserver{}
	catalog := mustObserverCatalog(t, kernel.ExtensionEntry{
		ID: "audit", Kind: kernel.ExtEventObserver, Implementation: obs,
	})
	broker := newObservedBroker(protocol.NewMemoryBroker(), catalog, observability.NoopLogger{})
	if _, ok := broker.(protocol.SessionSubscriber); !ok {
		t.Fatal("observedBroker lost SessionSubscriber capability of MemoryBroker")
	}
}

// TestObservedBrokerNoObserversZeroCost 验证目录为空时原样返回底层 broker。
func TestObservedBrokerNoObserversZeroCost(t *testing.T) {
	inner := protocol.NewMemoryBroker()
	if got := newObservedBroker(inner, nil, observability.NoopLogger{}); got != protocol.EventBroker(inner) {
		t.Fatal("nil catalog must return the underlying broker unchanged")
	}
	empty := mustObserverCatalog(t)
	if got := newObservedBroker(inner, empty, observability.NoopLogger{}); got != protocol.EventBroker(inner) {
		t.Fatal("empty catalog must return the underlying broker unchanged")
	}
}
