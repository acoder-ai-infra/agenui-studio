package observability

import (
	"context"
	"testing"
	"time"
)

func ctxWithTrace(traceID, runID, tenantID string) context.Context {
	return WithTraceContext(context.Background(), TraceContext{
		TraceID: traceID, RunID: runID, TenantID: tenantID,
	})
}

func TestRingLogger_CapturesAndFiltersByTrace(t *testing.T) {
	rl := NewRingLogger(NoopLogger{}, 10)
	rl.Info(ctxWithTrace("t1", "r1", "acme"), "hello", String("k", "v"))
	rl.Warn(ctxWithTrace("t2", "r2", "other"), "watch out")
	rl.Info(ctxWithTrace("t1", "r1", "acme"), "again")

	byTrace := rl.QueryLogs(LogQuery{TraceID: "t1"})
	if len(byTrace) != 2 {
		t.Fatalf("trace filter: want 2 got %d", len(byTrace))
	}
	// newest-first
	if byTrace[0].Message != "again" {
		t.Fatalf("want newest-first, got %q first", byTrace[0].Message)
	}
	if byTrace[0].TenantID != "acme" || byTrace[0].RunID != "r1" {
		t.Fatalf("trace fields not lifted: %+v", byTrace[0])
	}

	byRun := rl.QueryLogs(LogQuery{RunID: "r2"})
	if len(byRun) != 1 || byRun[0].Message != "watch out" || byRun[0].Level != "warn" {
		t.Fatalf("run filter: %+v", byRun)
	}

	byLevel := rl.QueryLogs(LogQuery{Level: "WARN"})
	if len(byLevel) != 1 {
		t.Fatalf("level filter case-insensitive: want 1 got %d", len(byLevel))
	}
}

func TestRingLogger_CapturesFieldsAndError(t *testing.T) {
	rl := NewRingLogger(NoopLogger{}, 5)
	rl.Error(ctxWithTrace("t", "", ""), "boom", context.Canceled, Int("code", 7))
	got := rl.QueryLogs(LogQuery{TraceID: "t"})
	if len(got) != 1 {
		t.Fatalf("want 1 got %d", len(got))
	}
	if got[0].Error != context.Canceled.Error() {
		t.Fatalf("error not captured: %q", got[0].Error)
	}
	if v, ok := got[0].Fields["code"]; !ok || v != int64(7) {
		t.Fatalf("field not captured: %+v", got[0].Fields)
	}
}

func TestRingLogger_WithBoundFieldsSharesRing(t *testing.T) {
	rl := NewRingLogger(NoopLogger{}, 5)
	child := rl.With(String("component", "gw"))
	child.Info(ctxWithTrace("t", "", ""), "child line")
	// bound field visible via parent's ring
	got := rl.QueryLogs(LogQuery{TraceID: "t"})
	if len(got) != 1 {
		t.Fatalf("child log should land in shared ring: got %d", len(got))
	}
	if got[0].Fields["component"] != "gw" {
		t.Fatalf("bound field missing: %+v", got[0].Fields)
	}
}

func TestRingLogger_OverwritesOldestWhenFull(t *testing.T) {
	rl := NewRingLogger(NoopLogger{}, 3)
	seq := time.Unix(0, 0)
	rl.now = func() time.Time { seq = seq.Add(time.Second); return seq }
	for i, msg := range []string{"a", "b", "c", "d"} {
		_ = i
		rl.Info(ctxWithTrace("t", "", ""), msg)
	}
	got := rl.QueryLogs(LogQuery{})
	if len(got) != 3 {
		t.Fatalf("capacity 3: got %d", len(got))
	}
	// oldest "a" overwritten; newest-first => d, c, b
	if got[0].Message != "d" || got[2].Message != "b" {
		t.Fatalf("ring order wrong: %v", []string{got[0].Message, got[1].Message, got[2].Message})
	}
}
