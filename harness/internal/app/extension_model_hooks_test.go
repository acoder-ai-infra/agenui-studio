package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type recordingEmitter struct{ events []observability.AgentEvent }

func (e *recordingEmitter) Emit(_ context.Context, event observability.AgentEvent) error {
	e.events = append(e.events, event)
	return nil
}

type stubHook struct {
	id     string
	fn     func(req extension.BeforeModelRequest) (extension.BeforeModelResult, error)
	rounds []int
}

func (s *stubHook) ID() string { return s.id }
func (s *stubHook) BeforeModel(_ context.Context, req extension.BeforeModelRequest) (extension.BeforeModelResult, error) {
	s.rounds = append(s.rounds, req.Round)
	return s.fn(req)
}

// newBeforeModelMiddleware 组装单 Hook 的 middleware（测试辅助）。
func newBeforeModelMiddleware(id string, impl extension.BeforeModelHook) *extensionBeforeModelMiddleware {
	return &extensionBeforeModelMiddleware{hooks: []beforeModelHookBinding{{id: id, impl: impl}}}
}

type stubInterceptor struct {
	id string
	fn func(ctx context.Context, call extension.ToolCallInfo, next extension.ToolCallNext) (extension.ToolCallOutcome, error)
}

func (s *stubInterceptor) ID() string { return s.id }
func (s *stubInterceptor) Intercept(ctx context.Context, call extension.ToolCallInfo, next extension.ToolCallNext) (extension.ToolCallOutcome, error) {
	return s.fn(ctx, call, next)
}

// 验证逐轮改写：每轮模型调用前生效、round 递增、改写留 debug 事件、
// 工具协议配对字段透传。
func TestExtensionBeforeModelMiddlewareRewritesEveryRound(t *testing.T) {
	hook := &stubHook{id: "rw", fn: func(req extension.BeforeModelRequest) (extension.BeforeModelResult, error) {
		messages := append([]extension.BeforeModelMessage(nil), req.Messages...)
		messages = append(messages, extension.BeforeModelMessage{Role: "user", Content: "轮次追加"})
		return extension.BeforeModelResult{Messages: messages}, nil
	}}
	middleware := newBeforeModelMiddleware("rw", hook)
	emitter := &recordingEmitter{}
	ctx := agentruntime.WithRuntimeEventEmitter(context.Background(), emitter)

	state := &adk.ChatModelAgentState{Messages: []*schema.Message{
		{Role: schema.User, Content: "你好"},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "tc1", Type: "function", Function: schema.FunctionCall{Name: "task", Arguments: `{"q":1}`}}}},
		{Role: schema.Tool, ToolCallID: "tc1", Content: "tool result"},
	}}
	for round := 1; round <= 3; round++ {
		var err error
		_, state, err = middleware.BeforeModelRewriteState(ctx, state, nil)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	if len(hook.rounds) != 3 || hook.rounds[2] != 3 {
		t.Fatalf("round counter = %v", hook.rounds)
	}
	// 每轮追加一条：3 原始 + 3 轮追加。
	if len(state.Messages) != 6 {
		t.Fatalf("messages = %d", len(state.Messages))
	}
	if state.Messages[1].ToolCalls[0].ID != "tc1" || state.Messages[2].ToolCallID != "tc1" {
		t.Fatalf("tool pairing must survive rewrite: %#v", state.Messages[1])
	}
	// 每轮改写留一对 started/completed step 事件：3 轮 → 6 条。
	if len(emitter.events) != 6 || emitter.events[0].EventType != observability.EventRuntimeStepStarted || emitter.events[1].EventType != observability.EventRuntimeStepCompleted || emitter.events[0].Visibility != observability.VisibilityDebug {
		t.Fatalf("rewrite must leave paired debug trail: %#v", emitter.events)
	}
	if emitter.events[0].StepID == "" || emitter.events[0].StepID != emitter.events[1].StepID {
		t.Fatalf("trail pair must share a step id: %#v", emitter.events[:2])
	}
	if !strings.Contains(string(emitter.events[1].Payload), string(agentruntime.StepKindProcessorHook)) {
		t.Fatalf("trail payload must carry processor_hook step kind: %s", emitter.events[1].Payload)
	}
}

// 空消息序列与非法 role 均 fail closed。
func TestExtensionBeforeModelMiddlewareFailsClosed(t *testing.T) {
	empty := newBeforeModelMiddleware("e", &stubHook{id: "e", fn: func(extension.BeforeModelRequest) (extension.BeforeModelResult, error) {
		return extension.BeforeModelResult{}, nil
	}})
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{{Role: schema.User, Content: "hi"}}}
	if _, _, err := empty.BeforeModelRewriteState(context.Background(), state, nil); err == nil {
		t.Fatal("empty result must fail closed")
	}
	badRole := newBeforeModelMiddleware("b", &stubHook{id: "b", fn: func(extension.BeforeModelRequest) (extension.BeforeModelResult, error) {
		return extension.BeforeModelResult{Messages: []extension.BeforeModelMessage{{Role: "robot", Content: "x"}}}, nil
	}})
	if _, _, err := badRole.BeforeModelRewriteState(context.Background(), state, nil); err == nil {
		t.Fatal("invalid role must fail closed")
	}
	boom := newBeforeModelMiddleware("x", &stubHook{id: "x", fn: func(extension.BeforeModelRequest) (extension.BeforeModelResult, error) {
		return extension.BeforeModelResult{}, errors.New("boom")
	}})
	if _, _, err := boom.BeforeModelRewriteState(context.Background(), state, nil); err == nil {
		t.Fatal("hook error must fail closed")
	}
}

// 验证工具环绕：入参改写传递给真实执行、模型可见结果替换为摘要、留痕含
// 原始/改写指纹（复刻 agenui task_store 语义）。
func TestExtensionInterceptorMiddlewareRewritesArgumentsAndResult(t *testing.T) {
	var sideStore string
	interceptor := &stubInterceptor{id: "task_store", fn: func(ctx context.Context, call extension.ToolCallInfo, next extension.ToolCallNext) (extension.ToolCallOutcome, error) {
		if call.Name != "task" {
			return next(ctx, call.Arguments)
		}
		outcome, err := next(ctx, json.RawMessage(`{"q":"rewritten"}`))
		if err != nil {
			return extension.ToolCallOutcome{}, err
		}
		sideStore = outcome.Result // 全量结果写宿主自有存储
		return extension.ToolCallOutcome{Result: "summary: stored"}, nil
	}}
	middleware := &extensionInterceptorMiddleware{extensionID: "task_store", impl: interceptor}
	emitter := &recordingEmitter{}
	ctx := agentruntime.WithRuntimeEventEmitter(context.Background(), emitter)

	var executedArgs string
	endpoint := adk.InvokableToolCallEndpoint(func(_ context.Context, argumentsInJSON string, _ ...einotool.Option) (string, error) {
		executedArgs = argumentsInJSON
		return "full sub-agent result", nil
	})
	wrapped, err := middleware.WrapInvokableToolCall(ctx, endpoint, &adk.ToolContext{Name: "task", CallID: "tc9"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := wrapped(ctx, `{"q":"original"}`)
	if err != nil {
		t.Fatal(err)
	}
	if executedArgs != `{"q":"rewritten"}` {
		t.Fatalf("argument rewrite must reach real execution: %s", executedArgs)
	}
	if result != "summary: stored" || sideStore != "full sub-agent result" {
		t.Fatalf("result=%q sideStore=%q", result, sideStore)
	}
	if len(emitter.events) != 2 || emitter.events[1].Visibility != observability.VisibilityDebug {
		t.Fatalf("rewrite trail missing: %#v", emitter.events)
	}
	payload := string(emitter.events[1].Payload)
	if !strings.Contains(payload, "original_hash") || !strings.Contains(payload, "rewritten_hash") {
		t.Fatalf("trail must carry hashes: %s", payload)
	}

	// 非目标工具直通：结果不变、不留痕。
	passthrough, err := middleware.WrapInvokableToolCall(ctx, endpoint, &adk.ToolContext{Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	result, err = passthrough(ctx, `{}`)
	if err != nil || result != "full sub-agent result" {
		t.Fatalf("passthrough result=%q err=%v", result, err)
	}
	if len(emitter.events) != 2 {
		t.Fatalf("passthrough must not leave trail: %#v", emitter.events)
	}
}

func TestExtensionInterceptorMiddlewareReceivesFullConfiguredLargeResult(t *testing.T) {
	fullResult := `{"items":"` + strings.Repeat("x", 8<<10) + `"}`
	var captured string
	interceptor := &stubInterceptor{id: "api_search_store", fn: func(ctx context.Context, call extension.ToolCallInfo, next extension.ToolCallNext) (extension.ToolCallOutcome, error) {
		outcome, err := next(ctx, call.Arguments)
		if err != nil {
			return extension.ToolCallOutcome{}, err
		}
		captured = outcome.Result
		return extension.ToolCallOutcome{Result: "summary: stored"}, nil
	}}
	middleware := &extensionInterceptorMiddleware{extensionID: "api_search_store", impl: interceptor}
	endpoint := adk.InvokableToolCallEndpoint(func(context.Context, string, ...einotool.Option) (string, error) {
		return fullResult, nil
	})
	wrapped, err := middleware.WrapInvokableToolCall(context.Background(), endpoint, &adk.ToolContext{
		Name:   "search_developer_apis",
		CallID: "tc-large",
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := wrapped(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if captured != fullResult {
		t.Fatalf("interceptor captured bytes = %d, want %d", len(captured), len(fullResult))
	}
	if result != "summary: stored" {
		t.Fatalf("model-visible result = %q", result)
	}
}

// 短路（不调用 next）与拦截器错误的语义。
func TestExtensionInterceptorMiddlewareShortCircuitAndError(t *testing.T) {
	shortCircuit := &extensionInterceptorMiddleware{extensionID: "sc", impl: &stubInterceptor{id: "sc", fn: func(context.Context, extension.ToolCallInfo, extension.ToolCallNext) (extension.ToolCallOutcome, error) {
		return extension.ToolCallOutcome{Result: "synthetic"}, nil
	}}}
	executed := false
	endpoint := adk.InvokableToolCallEndpoint(func(context.Context, string, ...einotool.Option) (string, error) {
		executed = true
		return "real", nil
	})
	wrapped, _ := shortCircuit.WrapInvokableToolCall(context.Background(), endpoint, &adk.ToolContext{Name: "t"})
	result, err := wrapped(context.Background(), `{}`)
	if err != nil || result != "synthetic" || executed {
		t.Fatalf("short circuit: result=%q executed=%v err=%v", result, executed, err)
	}

	failing := &extensionInterceptorMiddleware{extensionID: "f", impl: &stubInterceptor{id: "f", fn: func(context.Context, extension.ToolCallInfo, extension.ToolCallNext) (extension.ToolCallOutcome, error) {
		return extension.ToolCallOutcome{}, errors.New("deny")
	}}}
	wrapped, _ = failing.WrapInvokableToolCall(context.Background(), endpoint, &adk.ToolContext{Name: "t"})
	if _, err := wrapped(context.Background(), `{}`); err == nil {
		t.Fatal("interceptor error must fail closed")
	}
}

// resolver 包装：按 Definition 声明的 ID 顺序在 compaction handler 之后追加
// 投影；未声明不执行；未知 ID / 类型不匹配 fail closed；Options 优先于全集。
func TestNewExtensionPreModelHandlerResolver(t *testing.T) {
	base := agentruntime.EinoPreModelHandlerResolverFunc(func(context.Context, agentruntime.EinoPreModelHandlerRequest) ([]adk.ChatModelAgentMiddleware, error) {
		return []adk.ChatModelAgentMiddleware{&adk.BaseChatModelAgentMiddleware{}}, nil
	})
	catalog, err := kernel.NewExtensionCatalog([]kernel.ExtensionEntry{
		{ID: "rw", Kind: kernel.ExtBeforeModelHook, Implementation: &stubHook{id: "rw", fn: func(req extension.BeforeModelRequest) (extension.BeforeModelResult, error) {
			return extension.BeforeModelResult{Messages: req.Messages}, nil
		}}},
		{ID: "ic", Kind: kernel.ExtToolCallInterceptor, Implementation: &stubInterceptor{id: "ic", fn: func(ctx context.Context, call extension.ToolCallInfo, next extension.ToolCallNext) (extension.ToolCallOutcome, error) {
			return next(ctx, call.Arguments)
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver := newExtensionPreModelHandlerResolver(base, catalog, kernel.TurnEnvironment{Environment: "local"})
	boundRequest := agentruntime.EinoPreModelHandlerRequest{Definition: agentruntime.AgentDefinition{
		AgentID:              "agent_bound",
		BeforeModelHooks:     []string{"rw"},
		ToolCallInterceptors: []string{"ic"},
	}}
	handlers, err := resolver.Resolve(context.Background(), boundRequest)
	if err != nil {
		t.Fatal(err)
	}
	if len(handlers) != 3 {
		t.Fatalf("handlers = %d, want compaction + transformer + interceptor", len(handlers))
	}
	if _, ok := handlers[1].(*extensionBeforeModelMiddleware); !ok {
		t.Fatalf("handlers[1] = %T", handlers[1])
	}
	if _, ok := handlers[2].(*extensionInterceptorMiddleware); !ok {
		t.Fatalf("handlers[2] = %T", handlers[2])
	}

	// 未声明的 agent：零扩展直通，只剩 inner handler。
	unbound, err := resolver.Resolve(context.Background(), agentruntime.EinoPreModelHandlerRequest{Definition: agentruntime.AgentDefinition{AgentID: "agent_unbound"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(unbound) != 1 {
		t.Fatalf("undeclared agent handlers = %d, want inner only", len(unbound))
	}

	// 未知 ID fail closed（Options 与全集都没有）。
	_, err = resolver.Resolve(context.Background(), agentruntime.EinoPreModelHandlerRequest{Definition: agentruntime.AgentDefinition{
		AgentID: "agent_bad", BeforeModelHooks: []string{"missing_transformer"},
	}})
	if err == nil || !strings.Contains(err.Error(), "missing_transformer") {
		t.Fatalf("unknown id must fail closed, got %v", err)
	}

	// 全集兜底：catalog 为 nil 时仍能解析全集注册的实现。
	extension.RegisterBeforeModelHooks(&stubHook{id: "test.app.global_only", fn: func(req extension.BeforeModelRequest) (extension.BeforeModelResult, error) {
		return extension.BeforeModelResult{Messages: req.Messages}, nil
	}})
	globalResolver := newExtensionPreModelHandlerResolver(base, nil, kernel.TurnEnvironment{})
	globalHandlers, err := globalResolver.Resolve(context.Background(), agentruntime.EinoPreModelHandlerRequest{Definition: agentruntime.AgentDefinition{
		AgentID: "agent_global", BeforeModelHooks: []string{"test.app.global_only"},
	}})
	if err != nil {
		t.Fatalf("global fallback resolve: %v", err)
	}
	if len(globalHandlers) != 2 {
		t.Fatalf("global fallback handlers = %d, want inner + transformer", len(globalHandlers))
	}

	// Options 优先于全集：同 ID 时命中 catalog 条目。
	optionsMarker := false
	extension.RegisterBeforeModelHooks(&stubHook{id: "test.app.shadowed", fn: func(req extension.BeforeModelRequest) (extension.BeforeModelResult, error) {
		return extension.BeforeModelResult{Messages: req.Messages}, nil
	}})
	shadowCatalog, err := kernel.NewExtensionCatalog([]kernel.ExtensionEntry{
		{ID: "test.app.shadowed", Kind: kernel.ExtBeforeModelHook, Implementation: &stubHook{id: "test.app.shadowed", fn: func(req extension.BeforeModelRequest) (extension.BeforeModelResult, error) {
			optionsMarker = true
			return extension.BeforeModelResult{Messages: req.Messages}, nil
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	impl, err := resolveBeforeModelHook(shadowCatalog, "agent_shadow", "test.app.shadowed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := impl.BeforeModel(context.Background(), extension.BeforeModelRequest{}); err != nil {
		t.Fatal(err)
	}
	if !optionsMarker {
		t.Fatal("options-registered implementation must take precedence over the global registry")
	}

	// 类型不匹配 fail closed。
	bad, err := kernel.NewExtensionCatalog([]kernel.ExtensionEntry{{ID: "x", Kind: kernel.ExtBeforeModelHook, Implementation: struct{}{}}})
	if err != nil {
		t.Fatal(err)
	}
	badRequest := agentruntime.EinoPreModelHandlerRequest{Definition: agentruntime.AgentDefinition{AgentID: "agent_x", BeforeModelHooks: []string{"x"}}}
	if _, err := newExtensionPreModelHandlerResolver(base, bad, kernel.TurnEnvironment{}).Resolve(context.Background(), badRequest); err == nil {
		t.Fatal("type mismatch must fail closed")
	}
}
