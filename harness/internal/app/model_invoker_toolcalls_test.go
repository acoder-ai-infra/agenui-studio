package app

import (
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

// model_invoker_toolcalls_test.go 覆盖 A4：一次模型响应中的 0/1/N 个工具调用
// 都被完整归一（不再只取 [0]），并在字段缺失时用同 index 的流式 delta 回填。

func aggFrom(id, name, args string) *toolCallDeltaAgg {
	a := &toolCallDeltaAgg{id: id, name: name}
	a.args.WriteString(args)
	return a
}

func TestNormalizeResponseToolCallsZero(t *testing.T) {
	if got := normalizeResponseToolCalls(&modelgateway.ModelResponse{}, nil, nil); len(got) != 0 {
		t.Fatalf("no tool calls must yield empty, got %d", len(got))
	}
	if got := normalizeResponseToolCalls(nil, nil, nil); len(got) != 0 {
		t.Fatalf("nil response must yield empty, got %d", len(got))
	}
}

func TestNormalizeResponseToolCallsMultiplePreservesOrderAndFields(t *testing.T) {
	resp := &modelgateway.ModelResponse{ToolCalls: []modelgateway.ModelToolCall{
		{ToolCallID: "tc1", Name: "weather", Arguments: `{"city":"沪"}`},
		{ToolCallID: "tc2", Name: "route", Arguments: `{"to":"京"}`},
		{ToolCallID: "tc3", Name: "clock", Arguments: `{}`},
	}}
	got := normalizeResponseToolCalls(resp, nil, nil)
	if len(got) != 3 {
		t.Fatalf("all N tool calls must be preserved, got %d", len(got))
	}
	if got[0].ToolCallID != "tc1" || got[1].Name != "route" || string(got[2].Arguments) != `{}` {
		t.Fatalf("order/fields must survive: %#v", got)
	}
}

// response 条目字段缺失时用同 index 的流式 delta 聚合回填。
func TestNormalizeResponseToolCallsBackfillFromDelta(t *testing.T) {
	resp := &modelgateway.ModelResponse{ToolCalls: []modelgateway.ModelToolCall{
		{Name: "weather"}, // 缺 id 与 arguments
	}}
	deltas := map[int]*toolCallDeltaAgg{0: aggFrom("tc-stream", "weather", `{"city":"沪"}`)}
	got := normalizeResponseToolCalls(resp, deltas, []int{0})
	if len(got) != 1 || got[0].ToolCallID != "tc-stream" || string(got[0].Arguments) != `{"city":"沪"}` {
		t.Fatalf("missing fields must be backfilled from delta: %#v", got)
	}
}

// response.ToolCalls 为空但有流式 delta：按 index 升序回退，且丢弃无 name 的碎片。
func TestNormalizeResponseToolCallsDeltaOnlyFallbackSorted(t *testing.T) {
	deltas := map[int]*toolCallDeltaAgg{
		2: aggFrom("tc2", "route", `{"to":"京"}`),
		0: aggFrom("tc0", "weather", `{"city":"沪"}`),
		1: aggFrom("", "", ""), // 无 name 碎片：忽略
	}
	got := normalizeResponseToolCalls(&modelgateway.ModelResponse{}, deltas, []int{2, 0, 1})
	if len(got) != 2 {
		t.Fatalf("delta-only fallback must drop nameless fragments, got %d: %#v", len(got), got)
	}
	if got[0].Name != "weather" || got[1].Name != "route" {
		t.Fatalf("delta-only fallback must sort by index: %#v", got)
	}
}
