package harness

import (
	"errors"
	"io"
)

// SDK 边界暴露的哨兵错误。消费方通过 errors.Is 分类故障，不再基于字符串匹配。
var (
	// ErrClosed 在 Engine 已经 Close 后再调用 Start / Resume / Cancel /
	// Subscribe 时返回。EventStream.Next 在底层订阅被 Engine.Close 关闭时
	// 也会返回它。
	ErrClosed = errors.New("harness: engine closed")

	// ErrNotReady 表示 kernel 仍处于异步 readiness 阶段（例如 schema
	// migration 尚未完成）。
	ErrNotReady = errors.New("harness: engine not ready")

	// ErrConflict 表示 StartRequest 复用了已存在的
	// (Identity, IdempotencyKey) 但 Message / ScopedData 主体不同。调用方
	// 必须以相同 body 重发，或换一个新的 idempotency key。
	ErrConflict = errors.New("harness: idempotency conflict")

	// ErrIdempotencyMismatch 是 ErrConflict 的一个特化：在复用同一
	// IdempotencyKey 时 identity 或 session 发生了漂移。
	ErrIdempotencyMismatch = errors.New("harness: idempotency identity mismatch")

	// ErrChildControlUnsupported 表示某个 platform_child_run 尝试创建自己
	// 的 ControlRequest。SDK 不会暴露子 Run 的 Control 状态；HITL 归父 Run。
	ErrChildControlUnsupported = errors.New("harness: child control request unsupported")

	// ErrCapabilityDrift 在 Resume 时发现持久化的 AgentBinding / 配置 /
	// capability 快照与当前 kernel 不再匹配（例如一次滚动发布替换了底层
	// 实现）。fail closed：调用方必须回滚到上一个 release 或开新 Run。
	ErrCapabilityDrift = errors.New("harness: capability drift detected")

	// ErrInvalidRequest 表示调用方传入的 DTO 未通过结构校验。使用 %w
	// 包裹以保留原始细节。
	ErrInvalidRequest = errors.New("harness: invalid request")

	// ErrUnsupportedCapability 表示当前 kernel 无法提供请求的能力
	//（例如在没有 SessionSubscriber-capable broker 的情况下要求 session fan-in）。
	ErrUnsupportedCapability = errors.New("harness: unsupported capability")

	// ErrNotFound 表示查询目标（Session / Message / Run 等持久化记录）不存在。
	// 只读查询 API 用它区分“记录缺失”与“越权不可见”。
	ErrNotFound = errors.New("harness: not found")

	// ErrPermissionDenied 表示宿主查询目标存在但不属于调用方 Identity 的
	// tenant / user 作用域。fail closed：不会通过错误内容泄露记录是否存在
	// 以外的任何信息。
	ErrPermissionDenied = errors.New("harness: permission denied")

	// EOF 从 io 包再导出，方便调用方 range over EventStream.Next 时无需自己
	// import io。它是 Run 已经产出终态事件的规范信号。
	EOF = io.EOF
)

// IsTerminalStreamError 判断 err 是否是终态型 EventStream 错误：io.EOF、
// ErrClosed 或它们的包裹变体。调用方用它跳出 Next 循环，而无需 switch 多个
// 哨兵。
func IsTerminalStreamError(err error) bool {
	return err != nil && (errors.Is(err, io.EOF) || errors.Is(err, ErrClosed))
}
