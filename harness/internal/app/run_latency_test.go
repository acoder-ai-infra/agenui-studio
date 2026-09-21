package app

import (
	"context"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// run_latency_test.go 覆盖 A6：首个模型 delta 与取消延迟的结构化日志打点。

type capturedLog struct {
	msg    string
	fields map[string]any
}

type capturingLogger struct{ entries *[]capturedLog }

func (c capturingLogger) record(msg string, fields []observability.Field) {
	m := make(map[string]any, len(fields))
	for _, f := range fields {
		switch {
		case f.String != "":
			m[f.Key] = f.String
		default:
			m[f.Key] = f.Integer
		}
	}
	*c.entries = append(*c.entries, capturedLog{msg: msg, fields: m})
}

func (c capturingLogger) Debug(_ context.Context, msg string, fields ...observability.Field) {
	c.record(msg, fields)
}
func (c capturingLogger) Info(_ context.Context, msg string, fields ...observability.Field) {
	c.record(msg, fields)
}
func (c capturingLogger) Warn(_ context.Context, msg string, fields ...observability.Field) {
	c.record(msg, fields)
}
func (c capturingLogger) Error(_ context.Context, msg string, _ error, fields ...observability.Field) {
	c.record(msg, fields)
}
func (c capturingLogger) With(...observability.Field) observability.StructuredLogger { return c }
func (c capturingLogger) Sync()                                                      {}

func find(entries []capturedLog, msg string) (capturedLog, bool) {
	for _, e := range entries {
		if e.msg == msg {
			return e, true
		}
	}
	return capturedLog{}, false
}

func TestFirstDeltaLatencyLoggedOnce(t *testing.T) {
	var entries []capturedLog
	ctx := observability.WithLogger(context.Background(), capturingLogger{entries: &entries})
	base := time.Now()

	var tr firstDeltaLatencyTracker
	tr.observe(ctx, observability.AgentEvent{EventType: observability.EventRunStarted, RunID: "r1", AgentID: "a1", CreatedAt: base})
	// 首个 delta：应记录一次，latency=150ms。
	tr.observe(ctx, observability.AgentEvent{EventType: observability.EventModelTokenDelta, RunID: "r1", AgentID: "a1", CreatedAt: base.Add(150 * time.Millisecond)})
	// 第二个 delta：不应重复记录。
	tr.observe(ctx, observability.AgentEvent{EventType: observability.EventModelTokenDelta, RunID: "r1", AgentID: "a1", CreatedAt: base.Add(300 * time.Millisecond)})

	count := 0
	for _, e := range entries {
		if e.msg == "run_first_delta_latency" {
			count++
			if e.fields["run_id"] != "r1" || e.fields["agent_id"] != "a1" {
				t.Fatalf("latency log must carry run/agent dims: %#v", e.fields)
			}
			if e.fields["latency_ms"] != int64(150) {
				t.Fatalf("latency_ms = %v, want 150", e.fields["latency_ms"])
			}
		}
	}
	if count != 1 {
		t.Fatalf("run_first_delta_latency must log exactly once, got %d", count)
	}
}

func TestFirstDeltaLatencyNotLoggedWithoutRunStarted(t *testing.T) {
	var entries []capturedLog
	ctx := observability.WithLogger(context.Background(), capturingLogger{entries: &entries})
	var tr firstDeltaLatencyTracker
	// 没有 run_started 基线时不打点（避免虚假延迟）。
	tr.observe(ctx, observability.AgentEvent{EventType: observability.EventModelTokenDelta, RunID: "r1", CreatedAt: time.Now()})
	if _, ok := find(entries, "run_first_delta_latency"); ok {
		t.Fatal("must not log first-delta latency without a run_started baseline")
	}
}

func TestCancelLatencyLogged(t *testing.T) {
	var entries []capturedLog
	ctx := observability.WithLogger(context.Background(), capturingLogger{entries: &entries})
	logRunCancelLatency(ctx, "r9", "a9", time.Now().Add(-40*time.Millisecond))
	entry, ok := find(entries, "run_cancel_latency")
	if !ok {
		t.Fatal("run_cancel_latency must be logged")
	}
	if entry.fields["run_id"] != "r9" || entry.fields["agent_id"] != "a9" {
		t.Fatalf("cancel latency log must carry run/agent dims: %#v", entry.fields)
	}
	if lat, _ := entry.fields["latency_ms"].(int64); lat < 0 {
		t.Fatalf("cancel latency must be non-negative, got %v", entry.fields["latency_ms"])
	}
}
