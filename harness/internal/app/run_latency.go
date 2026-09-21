package app

import (
	"context"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// run_latency.go 实现 A6 的两项框架级延迟打点（ADR-015）：
//
//   - run_first_delta_latency：Run 开始（run_started 落账）到首个模型
//     delta（model_token_delta）发布的框架侧耗时；
//   - run_cancel_latency：收到 Cancel 到框架完成取消派发（run_cancelled
//     调度）的耗时。
//
// 无独立 metrics 子系统，按 Run 维度输出结构化日志（含 run_id/agent_id/
// latency_ms），灰度期以监控发现回归，不设 CI 阈值门禁。

// firstDeltaLatencyTracker 在单条事件流内测量首个模型 delta 相对 run_started
// 的延迟。零值可用；非并发安全（每条流一个实例、单 goroutine 消费）。
type firstDeltaLatencyTracker struct {
	startedAt time.Time
	logged    bool
}

// observe 处理一条即将转发的 canonical 事件：记录 run_started 时间戳，并在
// 首个 model_token_delta 上输出 run_first_delta_latency 结构化日志。
func (t *firstDeltaLatencyTracker) observe(ctx context.Context, event observability.AgentEvent) {
	switch event.EventType {
	case observability.EventRunStarted:
		if t.startedAt.IsZero() && !event.CreatedAt.IsZero() {
			t.startedAt = event.CreatedAt
		}
	case observability.EventModelTokenDelta:
		if t.logged || t.startedAt.IsZero() || event.CreatedAt.IsZero() {
			return
		}
		t.logged = true
		latency := event.CreatedAt.Sub(t.startedAt)
		if latency < 0 {
			latency = 0
		}
		observability.LoggerFrom(ctx, observability.NoopLogger{}).Info(ctx, "run_first_delta_latency",
			observability.String("run_id", event.RunID),
			observability.String("agent_id", event.AgentID),
			observability.Int64("latency_ms", latency.Milliseconds()),
		)
	}
}

// logRunCancelLatency 输出 run_cancel_latency 结构化日志：从收到 Cancel 到
// 框架完成取消派发的耗时。
func logRunCancelLatency(ctx context.Context, runID, agentID string, since time.Time) {
	latency := time.Since(since)
	if latency < 0 {
		latency = 0
	}
	observability.LoggerFrom(ctx, observability.NoopLogger{}).Info(ctx, "run_cancel_latency",
		observability.String("run_id", runID),
		observability.String("agent_id", agentID),
		observability.Int64("latency_ms", latency.Milliseconds()),
	)
}
