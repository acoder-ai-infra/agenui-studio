package app

import (
	"context"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
)

// extension_observer.go 实现 EventObserver 扩展的服务端 fan-out（R3）。
//
// EventObserver 此前挂在 SDK 客户端 EventStream.Next 的拉取路径上，存在三个
// 缺陷：Resume/Subscribe 流不触发、调用方不 drain 就没有审计、HTTP 入口完全
// 不经过。本文件把 fan-out 下沉到事件发布点：composer 用 observedBroker 装饰
// protocol.EventBroker，所有 Publish 方（formalRunDispatcher.drain、
// agent_gateway_local、embedded_engine 等）只持有 broker 接口，包装一次即
// 全链路覆盖，且能观察到 session_created / run_created 等不经过 runtime 的
// 事件。
//
// 契约（canonical §7.1）：EventObserver 是只读旁路审计通道，恒 fail-open——
// observer 超时、报错、panic 都不得阻塞或失败事件发布。

// eventObserverBinding 把一个已注册的 EventObserver 与其 ExtensionEntry 元数据
// （派生 ID、超时）配对，供发布点逐事件执行。
type eventObserverBinding struct {
	// id 是派生 ID，格式 extension:event_observer:<entryID>（命名对齐约定），
	// 用于日志反查注册的扩展。
	id       string
	timeout  time.Duration
	observer extension.EventObserver
}

// observedBroker 装饰 protocol.EventBroker：Publish 先同步执行 observer
// fan-out（fail-open），再委托底层 broker。Subscribe 原样透传。
type observedBroker struct {
	next      protocol.EventBroker
	observers []eventObserverBinding
	logger    observability.StructuredLogger
}

// observedSessionBroker 在 observedBroker 之上补齐 protocol.SessionSubscriber
// 可选能力：底层 broker 支持 session 级订阅时，包装后不得丢失该能力
// （kernel.SubscribeRunTree 与 agent_chat_handler 依赖类型断言探测）。
type observedSessionBroker struct {
	observedBroker
	session protocol.SessionSubscriber
}

func (b *observedSessionBroker) SubscribeSession(ctx context.Context, sessionID string) (<-chan observability.AgentEvent, func(), error) {
	return b.session.SubscribeSession(ctx, sessionID)
}

// newObservedBroker 用 catalog 中的 event_observer 条目装饰 next。没有任何
// observer 时原样返回 next（零开销）。返回值保留底层 SessionSubscriber 能力。
func newObservedBroker(next protocol.EventBroker, catalog *kernel.ExtensionCatalog, logger observability.StructuredLogger) protocol.EventBroker {
	if next == nil || catalog == nil {
		return next
	}
	entries := catalog.ByKind(kernel.ExtEventObserver)
	if len(entries) == 0 {
		return next
	}
	if logger == nil {
		logger = observability.NoopLogger{}
	}
	bindings := make([]eventObserverBinding, 0, len(entries))
	for _, entry := range entries {
		obs, ok := entry.Implementation.(extension.EventObserver)
		if !ok {
			// 类型不匹配的注册在 Build 阶段已被 fail-closed 校验；此处防御性
			// 跳过，避免运行期 panic。
			continue
		}
		bindings = append(bindings, eventObserverBinding{
			id:       "extension:" + string(kernel.ExtEventObserver) + ":" + entry.ID,
			timeout:  entry.Timeout,
			observer: obs,
		})
	}
	if len(bindings) == 0 {
		return next
	}
	base := observedBroker{next: next, observers: bindings, logger: logger}
	if session, ok := next.(protocol.SessionSubscriber); ok {
		return &observedSessionBroker{observedBroker: base, session: session}
	}
	return &base
}

func (b *observedBroker) Publish(ctx context.Context, ev observability.AgentEvent) error {
	b.fanOut(ctx, ev)
	return b.next.Publish(ctx, ev)
}

func (b *observedBroker) Subscribe(ctx context.Context, runID string) (<-chan observability.AgentEvent, func(), error) {
	return b.next.Subscribe(ctx, runID)
}

// fanOut 把事件逐个转发给 observer。任何 observer 的错误 / 超时 / panic 都只
// 记日志，绝不影响事件发布（fail-open 旁路契约）。
func (b *observedBroker) fanOut(ctx context.Context, ev observability.AgentEvent) {
	if len(b.observers) == 0 {
		return
	}
	protoEvent := agentEventToProtocolEvent(ev)
	for _, binding := range b.observers {
		b.observeOne(ctx, binding, protoEvent)
	}
}

// observeOne 执行单个 observer，隔离其超时与 panic。
func (b *observedBroker) observeOne(ctx context.Context, binding eventObserverBinding, ev extension.ProtocolEvent) {
	obCtx := ctx
	if binding.timeout > 0 {
		var cancel context.CancelFunc
		obCtx, cancel = context.WithTimeout(ctx, binding.timeout)
		defer cancel()
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			b.logger.Error(ctx, "event observer panicked", nil,
				observability.String("observer_id", binding.id),
				observability.String("event_id", ev.EventID),
			)
		}
	}()
	if err := binding.observer.Observe(obCtx, ev); err != nil {
		b.logger.Error(ctx, "event observer failed", err,
			observability.String("observer_id", binding.id),
			observability.String("event_id", ev.EventID),
		)
	}
}

// agentEventToProtocolEvent 把 canonical AgentEvent 投影为 observer 可见的
// extension.ProtocolEvent（迁移自 harness 包旧 fanEventToObservers 的转换）。
// 大 payload 仍以 PayloadRef 传递；observer 永远拿不到完整 payload 内联字节。
func agentEventToProtocolEvent(ev observability.AgentEvent) extension.ProtocolEvent {
	out := extension.ProtocolEvent{
		EventID:    ev.EventID,
		Sequence:   ev.Sequence,
		SessionID:  ev.SessionID,
		RunID:      ev.RunID,
		StepID:     ev.StepID,
		AgentID:    ev.AgentID,
		EventType:  string(ev.EventType),
		Visibility: string(ev.Visibility),
		Payload:    ev.PayloadPreview,
		PayloadRef: ev.PayloadRef,
		Usage:      ev.Usage,
		CreatedAt:  ev.CreatedAt,
	}
	if ev.Error != nil {
		out.Error = &extension.ProtocolEventError{
			Code:      ev.Error.Code,
			Type:      string(ev.Error.Type),
			Message:   ev.Error.Message,
			Retryable: ev.Error.Retryable,
		}
	}
	return out
}
