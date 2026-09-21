package app

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
)

// extension_model_tools_test.go 覆盖 A1 BeforeModelHook 的工具可见集收窄与
// ModelCallOptions.ToolChoice 护栏（ADR-006/007）。

// visibleToolsHook 是一个只改工具可见集 / ToolChoice 的测试 Hook。
type visibleToolsHook struct {
	id      string
	visible []string // nil=不收窄；非 nil=按名选择子集
	choice  string
}

func (h visibleToolsHook) ID() string { return h.id }
func (h visibleToolsHook) BeforeModel(_ context.Context, req extension.BeforeModelRequest) (extension.BeforeModelResult, error) {
	res := extension.BeforeModelResult{Messages: req.Messages, Tools: req.Tools, Options: req.Options}
	if h.visible != nil {
		selected := make([]extension.ToolDefinition, 0, len(h.visible))
		for _, name := range h.visible {
			selected = append(selected, extension.ToolDefinition{Name: name})
		}
		res.Tools = selected
	}
	if h.choice != "" {
		res.Options.ToolChoice = h.choice
	}
	return res, nil
}

func toolState(names ...string) *adk.ChatModelAgentState {
	infos := make([]*schema.ToolInfo, 0, len(names))
	for _, name := range names {
		infos = append(infos, &schema.ToolInfo{Name: name, Desc: name + " desc"})
	}
	return &adk.ChatModelAgentState{
		Messages:  []*schema.Message{{Role: schema.User, Content: "hi"}},
		ToolInfos: infos,
	}
}

func toolInfoNames(infos []*schema.ToolInfo) []string {
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		names = append(names, info.Name)
	}
	return names
}

// 可见集收窄：Hook 选择子集时 state.ToolInfos 按选择收窄；完整 schema/desc
// 从原 ToolInfo 保留。
func TestBeforeModelHookNarrowsVisibleTools(t *testing.T) {
	mw := newBeforeModelMiddleware("v", visibleToolsHook{id: "v", visible: []string{"b"}})
	state := toolState("a", "b", "c")
	_, state, err := mw.BeforeModelRewriteState(context.Background(), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := toolInfoNames(state.ToolInfos); len(got) != 1 || got[0] != "b" {
		t.Fatalf("visible tools = %v, want [b]", got)
	}
	if state.ToolInfos[0].Desc != "b desc" {
		t.Fatalf("narrowed tool must keep original schema/desc: %q", state.ToolInfos[0].Desc)
	}
}

// nil 选择=不收窄：全集原样保留。
func TestBeforeModelHookNilSelectionKeepsAllTools(t *testing.T) {
	mw := newBeforeModelMiddleware("v", visibleToolsHook{id: "v", visible: nil})
	state := toolState("a", "b", "c")
	_, state, err := mw.BeforeModelRewriteState(context.Background(), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := toolInfoNames(state.ToolInfos); len(got) != 3 {
		t.Fatalf("nil selection must keep all tools, got %v", got)
	}
}

// 空集：Hook 返回空（非 nil）工具集时本轮模型不可见任何工具。
func TestBeforeModelHookEmptySelectionHidesAllTools(t *testing.T) {
	mw := newBeforeModelMiddleware("v", visibleToolsHook{id: "v", visible: []string{}})
	state := toolState("a", "b")
	_, state, err := mw.BeforeModelRewriteState(context.Background(), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.ToolInfos) != 0 {
		t.Fatalf("empty selection must hide all tools, got %v", toolInfoNames(state.ToolInfos))
	}
}

// 越权新增被忽略：Hook 返回不在候选集合内的工具名不会放大可见集。
func TestBeforeModelHookIgnoresUnknownToolNames(t *testing.T) {
	mw := newBeforeModelMiddleware("v", visibleToolsHook{id: "v", visible: []string{"a", "ghost"}})
	state := toolState("a", "b")
	_, state, err := mw.BeforeModelRewriteState(context.Background(), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := toolInfoNames(state.ToolInfos); len(got) != 1 || got[0] != "a" {
		t.Fatalf("unknown tool must be dropped, got %v", got)
	}
}

// ToolChoice 四取值：auto/required/none 与本轮可见集内的具体工具名放行；
// 命中可见集时 ToolChoice 经 ctx 下发。
func TestBeforeModelHookToolChoiceAcceptsReservedAndVisibleNames(t *testing.T) {
	for _, choice := range []string{"auto", "required", "none", "a"} {
		mw := newBeforeModelMiddleware("c", visibleToolsHook{id: "c", visible: []string{"a"}, choice: choice})
		ctx, _, err := mw.BeforeModelRewriteState(context.Background(), toolState("a", "b"), nil)
		if err != nil {
			t.Fatalf("choice %q must be accepted: %v", choice, err)
		}
		if got, ok := agentruntime.ModelCallToolChoiceFrom(ctx); !ok || got != choice {
			t.Fatalf("choice %q must be pushed via ctx, got %q ok=%v", choice, got, ok)
		}
	}
}

// ToolChoice 指定不在可见集内的具体工具名 → fail closed（ADR-007 护栏）。
func TestBeforeModelHookToolChoiceUnknownNameFailsClosed(t *testing.T) {
	// 可见集为 {a}，但强制选择 b（不可见）→ 拒绝。
	mw := newBeforeModelMiddleware("c", visibleToolsHook{id: "c", visible: []string{"a"}, choice: "b"})
	if _, _, err := mw.BeforeModelRewriteState(context.Background(), toolState("a", "b"), nil); err == nil {
		t.Fatal("tool_choice pointing outside visible set must fail closed")
	}
}
