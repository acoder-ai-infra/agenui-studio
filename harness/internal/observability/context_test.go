package observability

import (
	"context"
	"testing"
)

func TestTraceContextHelpers(t *testing.T) {
	ctx := WithTraceContext(context.Background(), TraceContext{
		TraceID:        "trace_1",
		SpanID:         "span_1",
		ConversationID: "csid_1",
	})

	ctx = WithRun(ctx, "session_1", "run_1")
	ctx = WithAgent(ctx, "agent_1", "planner", "v1")

	tc, ok := TraceContextFrom(ctx)
	if !ok {
		t.Fatal("trace context missing")
	}
	if tc.TraceID != "trace_1" || tc.SessionID != "session_1" || tc.RunID != "run_1" {
		t.Fatalf("unexpected trace context: %#v", tc)
	}
	if tc.AgentID != "agent_1" || tc.AgentType != "planner" || tc.AgentVersion != "v1" {
		t.Fatalf("unexpected agent fields: %#v", tc)
	}
}
