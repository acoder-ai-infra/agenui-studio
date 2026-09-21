package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/adk"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// extension_model_scoped_test.go 覆盖两项契约：
//   - BeforeModelHook 能读到本 Run 冻结的 ScopedData 只读视图（入口层 →
//     dispatcher → runtime → Hook 全链路），且改写入参不影响共享快照；
//   - 工具可见集每轮从 Agent 配置全集重新开始收窄（ADR-006 轮间不单调）。

// scopedReaderHook 记录每轮收到的 ScopedData，并尝试改写入参 map 以验证隔离。
type scopedReaderHook struct {
	id string
	// seen 是每轮入参的 map 引用（本 Hook 自己的私有拷贝）。
	seen []map[string]extension.ScopedDataEntry
	// entrySources 是每轮“改写之前”读到的 device.Source，用于验证上一轮
	// 的改写不会泄漏到下一轮。
	entrySources []string
}

func (h *scopedReaderHook) ID() string { return h.id }

func (h *scopedReaderHook) BeforeModel(_ context.Context, req extension.BeforeModelRequest) (extension.BeforeModelResult, error) {
	h.seen = append(h.seen, req.ScopedData)
	h.entrySources = append(h.entrySources, req.ScopedData["device"].Source)
	// 越权改写：本 map 是深拷贝，改动不得回流到共享快照或后续轮次。
	if req.ScopedData != nil {
		req.ScopedData["injected"] = extension.ScopedDataEntry{Source: "hook"}
		if entry, ok := req.ScopedData["device"]; ok {
			entry.Source = "tampered"
			req.ScopedData["device"] = entry
		}
	}
	return extension.BeforeModelResult{Messages: req.Messages, Tools: req.Tools, Options: req.Options}, nil
}

func scopedMiddleware(hook extension.BeforeModelHook, scoped agentruntime.ScopedData) *extensionBeforeModelMiddleware {
	return &extensionBeforeModelMiddleware{
		hooks:      []beforeModelHookBinding{{id: hook.ID(), impl: hook}},
		scopedData: scopedDataToExtensionEntries(scoped),
	}
}

func runScopedData() agentruntime.ScopedData {
	return agentruntime.ScopedData{Run: map[string]agentruntime.ScopedDataItem{
		"device": {Source: "client", Visibility: "internal", Value: json.RawMessage(`{"tools":["nav"]}`)},
	}}
}

// eino 路径：Hook 每轮都能读到冻结 ScopedData；改写入参不污染下一轮视图。
func TestBeforeModelHookReadsScopedDataEachRound(t *testing.T) {
	hook := &scopedReaderHook{id: "s"}
	mw := scopedMiddleware(hook, runScopedData())
	for round := 1; round <= 2; round++ {
		if _, _, err := mw.BeforeModelRewriteState(context.Background(), toolState("a"), nil); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	if len(hook.seen) != 2 {
		t.Fatalf("hook must run once per round, got %d", len(hook.seen))
	}
	// 每轮进入时看到的必须是未被上轮污染的冻结快照。
	for round, source := range hook.entrySources {
		if source != "client" {
			t.Fatalf("round %d: hook rewrite must not leak across rounds, source=%q", round+1, source)
		}
	}
	for round, seen := range hook.seen {
		entry, ok := seen["device"]
		if !ok {
			t.Fatalf("round %d: scoped data key device missing", round+1)
		}
		if string(entry.Value) != `{"tools":["nav"]}` {
			t.Fatalf("round %d: scoped value = %s", round+1, entry.Value)
		}
	}
	if _, injected := hook.seen[1]["injected"]; !injected {
		// 第二轮自己改写后应当存在（证明改写确实发生在私有拷贝上）。
		t.Fatal("hook rewrite must apply to its own private copy")
	}
}

// 未配置 ScopedData 时 Hook 收到 nil 视图（零成本直通，不构造空 map）。
func TestBeforeModelHookScopedDataEmptyWhenAbsent(t *testing.T) {
	hook := &scopedReaderHook{id: "s"}
	mw := scopedMiddleware(hook, agentruntime.ScopedData{})
	if _, _, err := mw.BeforeModelRewriteState(context.Background(), toolState("a"), nil); err != nil {
		t.Fatal(err)
	}
	if len(hook.seen) != 1 || hook.seen[0] != nil {
		t.Fatalf("absent scoped data must surface as nil, got %#v", hook.seen)
	}
}

// native 路径：ModelInvokeRequest.ScopedData 投影为 Hook 只读视图。
func TestNativeBeforeModelHookReadsScopedData(t *testing.T) {
	hook := &scopedReaderHook{id: "s"}
	catalog, err := kernel.NewExtensionCatalog([]kernel.ExtensionEntry{
		{Kind: kernel.ExtBeforeModelHook, ID: "s", Implementation: hook},
	})
	if err != nil {
		t.Fatal(err)
	}
	transform := newExtensionNativeModelTransform(catalog, kernel.TurnEnvironment{Environment: "local"})
	def := agentruntime.AgentDefinition{AgentID: "agent_1", BeforeModelHooks: []string{"s"}}
	req := agentruntime.ModelInvokeRequest{
		Round:      1,
		Messages:   []agentruntime.ModelCallMessage{{Role: "user", Content: "hi"}},
		ScopedData: runScopedData(),
	}
	if _, err := transform(context.Background(), def, req); err != nil {
		t.Fatal(err)
	}
	if len(hook.seen) != 1 {
		t.Fatalf("native hook must run once, got %d", len(hook.seen))
	}
	if entry, ok := hook.seen[0]["device"]; !ok || string(entry.Value) != `{"tools":["nav"]}` {
		t.Fatalf("native path must surface scoped data, got %#v", hook.seen[0])
	}
	// 深拷贝隔离：Hook 改写不得回写入参快照。
	if req.ScopedData.Run["device"].Source != "client" {
		t.Fatalf("hook rewrite leaked into request snapshot: %q", req.ScopedData.Run["device"].Source)
	}
}

// ADR-006 轮间不单调：首轮收窄到子集后，第二轮候选集必须回到配置全集。
func TestBeforeModelHookRestoresToolBaselineEachRound(t *testing.T) {
	hook := &roundScopedToolsHook{id: "r"}
	mw := &extensionBeforeModelMiddleware{hooks: []beforeModelHookBinding{{id: "r", impl: hook}}}
	state := toolState("a", "b", "c")
	// 首轮收窄到 {a}。
	if _, _, err := mw.BeforeModelRewriteState(context.Background(), state, nil); err != nil {
		t.Fatal(err)
	}
	if got := toolInfoNames(state.ToolInfos); len(got) != 1 || got[0] != "a" {
		t.Fatalf("round 1 visible = %v, want [a]", got)
	}
	// 第二轮 Hook 不收窄：候选集必须是全集，可见集回到 3 个。
	if _, _, err := mw.BeforeModelRewriteState(context.Background(), state, nil); err != nil {
		t.Fatal(err)
	}
	if got := toolInfoNames(state.ToolInfos); len(got) != 3 {
		t.Fatalf("round 2 must restart from the full tool set, got %v", got)
	}
	if len(hook.candidates) != 2 || len(hook.candidates[1]) != 3 {
		t.Fatalf("round 2 candidate set must be the配置全集, got %v", hook.candidates)
	}
}

// roundScopedToolsHook 首轮收窄，后续轮次原样返回，用于验证基线复位。
type roundScopedToolsHook struct {
	id         string
	round      int
	candidates [][]string
}

func (h *roundScopedToolsHook) ID() string { return h.id }

func (h *roundScopedToolsHook) BeforeModel(_ context.Context, req extension.BeforeModelRequest) (extension.BeforeModelResult, error) {
	h.round++
	names := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		names = append(names, tool.Name)
	}
	h.candidates = append(h.candidates, names)
	res := extension.BeforeModelResult{Messages: req.Messages, Tools: req.Tools, Options: req.Options}
	if h.round == 1 {
		res.Tools = []extension.ToolDefinition{{Name: "a"}}
	}
	return res, nil
}

// native 路径候选工具集物化：runModelRound 每轮新建的 ModelInvokeRequest 不
// 携带 Tools（由 governor 稍后物化），Hook 候选集必须从 Package 能力快照取
// 全集，且每轮重新开始（不受上轮收窄影响）。
func TestNativeBeforeModelHookMaterializesToolBaselineEachRound(t *testing.T) {
	hook := &roundScopedToolsHook{id: "r"}
	catalog, err := kernel.NewExtensionCatalog([]kernel.ExtensionEntry{
		{Kind: kernel.ExtBeforeModelHook, ID: "r", Implementation: hook},
	})
	if err != nil {
		t.Fatal(err)
	}
	transform := newExtensionNativeModelTransform(catalog, kernel.TurnEnvironment{Environment: "local"})
	def := agentruntime.AgentDefinition{AgentID: "agent_1", BeforeModelHooks: []string{"r"}}
	pkg := agentruntime.ModelContextPackage{}
	pkg.Capabilities.ToolDefinitions = []agentruntime.ModelToolDefinition{
		{Name: "a", Description: "a desc", Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "b"}, {Name: "c"},
	}
	// 首轮：Hook 收窄到 {a}。
	first, err := transform(context.Background(), def, agentruntime.ModelInvokeRequest{
		Round: 1, Package: pkg,
		Messages: []agentruntime.ModelCallMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Tools) != 1 || first.Tools[0].Name != "a" {
		t.Fatalf("round 1 visible tools = %#v, want [a]", first.Tools)
	}
	if string(first.Tools[0].Schema) != `{"type":"object"}` {
		t.Fatalf("narrowed tool must keep its frozen schema: %s", first.Tools[0].Schema)
	}
	// 第二轮：runModelRound 新建的请求同样不携带 Tools；Hook 不收窄时
	// 可见集必须回到配置全集。
	second, err := transform(context.Background(), def, agentruntime.ModelInvokeRequest{
		Round: 2, Package: pkg,
		Messages: []agentruntime.ModelCallMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Tools) != 3 {
		t.Fatalf("round 2 must restart from the full tool set, got %#v", second.Tools)
	}
	if len(hook.candidates) != 2 || len(hook.candidates[0]) != 3 || len(hook.candidates[1]) != 3 {
		t.Fatalf("every round candidate set must be the configured full set, got %v", hook.candidates)
	}
}

// dispatcher 接线：随 Turn 透传的条目还原为 Runtime 的 Run 级 ScopedData。
func TestScopedDataFromTurn(t *testing.T) {
	got := scopedDataFromTurn([]storage.ScopedDataItem{
		{Key: "device", Source: "client", Visibility: "internal", Value: json.RawMessage(`1`), Ref: "ref", Hash: "h"},
		{Key: "", Source: "dropped"},
	})
	if len(got.Run) != 1 {
		t.Fatalf("empty key must be dropped, got %#v", got.Run)
	}
	item := got.Run["device"]
	if item.Source != "client" || item.Visibility != "internal" || string(item.Value) != "1" || item.Ref != "ref" || item.Hash != "h" {
		t.Fatalf("scoped item not threaded through: %#v", item)
	}
	if empty := scopedDataFromTurn(nil); empty.Run != nil {
		t.Fatal("nil input must produce an empty ScopedData")
	}
}

var (
	_ adk.ChatModelAgentMiddleware = (*extensionBeforeModelMiddleware)(nil)
	_ extension.BeforeModelHook    = (*scopedReaderHook)(nil)
	_ extension.BeforeModelHook    = (*roundScopedToolsHook)(nil)
)
