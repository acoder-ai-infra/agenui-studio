package extension

import "context"

// EventObserver 是审计 / 指标类旁路 hook。它在每一条 canonical AgentEvent 被
// 持久化之后收到事件；observer 不能阻塞管道，也不能改写事实链。observer
// 失败会被记录（并按策略告警），但绝不会回滚已完成的 Run。
//
// 常见做法是把事件扇出到内存 ring buffer、指标 registry 或外部审计管道。
// Observer 不得在 Observe 返回后仍持有事件 payload（kernel 可能复用 buffer）。
type EventObserver interface {
	Observe(ctx context.Context, ev ProtocolEvent) error
}
