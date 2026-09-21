package agentruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/prebuilt/deep"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

func TestEinoDeepAgentFactoryBuildsRestrictedDeepAgent(t *testing.T) {
	var captured *deep.Config
	factory := newEinoDeepAgentFactory(func(_ context.Context, cfg *deep.Config) (adk.ResumableAgent, error) {
		captured = cfg
		return observedCompletedEinoInternalAgent{}, nil
	})
	chatModel := stubEinoToolCallingModel{}
	platformAgent := observedCompletedEinoInternalAgent{}
	platformTool := stubEinoBaseTool{}
	preModelHandler := &stubEinoPreModelHandler{}

	agent, err := factory.Build(context.Background(), EinoManagedAgentBuildRequest{
		Definition: AgentDefinition{
			AgentID:   "deep_agent",
			AgentType: "assistant",
			Version:   "v1",
			Runtime:   RuntimeSpec{Type: RuntimeTypeEino, Mode: RuntimeModeDeepAgent},
			Metadata:  map[string]string{"description": "deep work", "max_iterations": "12"},
		},
		ChatModel:         chatModel,
		PlatformTools:     []tool.BaseTool{platformTool},
		PlatformSubAgents: []adk.Agent{platformAgent},
		PreModelHandlers:  []adk.ChatModelAgentMiddleware{preModelHandler},
		InternalAgents:    EinoInternalAgentDecorator{},
	})
	if err != nil {
		t.Fatalf("build deep agent: %v", err)
	}
	if agent == nil || captured == nil {
		t.Fatal("deep builder was not invoked")
	}
	if captured.Name != "deep_agent" || captured.Description != "deep work" || captured.MaxIteration != 12 {
		t.Fatalf("deep identity/config mismatch: %#v", captured)
	}
	if captured.Instruction != defaultHarnessDeepAgentInstruction {
		t.Fatalf("deep instruction must be explicit and Harness-owned: %q", captured.Instruction)
	}
	if captured.ChatModel != chatModel || len(captured.ToolsConfig.Tools) != 1 || len(captured.SubAgents) != 1 {
		t.Fatalf("managed dependencies were not injected: %#v", captured)
	}
	if len(captured.Handlers) != 1 || captured.Handlers[0] != preModelHandler {
		t.Fatalf("pre-model context handlers were not installed: %#v", captured.Handlers)
	}
	if !captured.WithoutGeneralSubAgent || !captured.ToolsConfig.EmitInternalEvents {
		t.Fatalf("unsafe deep defaults enabled: %#v", captured)
	}
	if captured.Backend != nil || captured.Shell != nil || captured.StreamingShell != nil {
		t.Fatal("filesystem or shell must remain disabled without Harness sandbox ports")
	}
}

func TestEinoDeepAgentFactoryIgnoresUngovernedMetadataInstruction(t *testing.T) {
	var captured *deep.Config
	factory := newEinoDeepAgentFactory(func(_ context.Context, cfg *deep.Config) (adk.ResumableAgent, error) {
		captured = cfg
		return observedCompletedEinoInternalAgent{}, nil
	})
	_, err := factory.Build(context.Background(), EinoManagedAgentBuildRequest{
		Definition: AgentDefinition{
			AgentID: "agent", Version: "v1",
			Runtime:  RuntimeSpec{Type: RuntimeTypeEino, Mode: RuntimeModeDeepAgent},
			Metadata: map[string]string{"deep_agent_instruction": "custom managed instruction"},
		},
		ChatModel: stubEinoToolCallingModel{}, InternalAgents: EinoInternalAgentDecorator{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if captured == nil || captured.Instruction != defaultHarnessDeepAgentInstruction {
		t.Fatalf("instruction=%#v", captured)
	}
}

// 验证 Registry 冻结解析的 system prompt（经 EinoRuntime 提取后以
// SystemPrompt 传入）优先于默认托管指令。
func TestEinoDeepAgentFactoryUsesGovernedSystemPrompt(t *testing.T) {
	var captured *deep.Config
	factory := newEinoDeepAgentFactory(func(_ context.Context, cfg *deep.Config) (adk.ResumableAgent, error) {
		captured = cfg
		return observedCompletedEinoInternalAgent{}, nil
	})
	_, err := factory.Build(context.Background(), EinoManagedAgentBuildRequest{
		Definition: AgentDefinition{
			AgentID: "agent", Version: "v1",
			Runtime: RuntimeSpec{Type: RuntimeTypeEino, Mode: RuntimeModeDeepAgent},
		},
		ChatModel:      stubEinoToolCallingModel{},
		SystemPrompt:   "governed prompt v2",
		InternalAgents: EinoInternalAgentDecorator{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if captured == nil || captured.Instruction != "governed prompt v2" {
		t.Fatalf("governed system prompt must win over default instruction: %#v", captured)
	}
}

// 验证 einoSystemPrompt 提取与 einoMessages 排除成对生效：冻结 system prompt
// 提升为 Instruction 后不再出现在首轮输入，消除双 system 消息。
func TestEinoSystemPromptExtractionAndWindowExclusion(t *testing.T) {
	pkg := ModelContextPackage{Messages: ModelContextMessages{ConversationWindow: []ModelContextMessage{
		{ID: systemPromptMessageID, Role: string(schema.System), Content: "governed prompt"},
		{ID: "m1", Role: string(schema.User), Content: "hello"},
	}}}
	if got := einoSystemPrompt(pkg); got != "governed prompt" {
		t.Fatalf("einoSystemPrompt=%q", got)
	}
	messages := einoMessages(pkg)
	if len(messages) != 1 || messages[0].Role != schema.User || messages[0].Content != "hello" {
		t.Fatalf("frozen system prompt must be excluded from first-round input: %#v", messages)
	}
	// 未声明 PromptRef：无提取、不排除普通 system 消息（如 attachments fragment）。
	plain := ModelContextPackage{Messages: ModelContextMessages{ConversationWindow: []ModelContextMessage{
		{ID: "frag", Role: string(schema.System), Content: "attachment facts"},
		{ID: "m1", Role: string(schema.User), Content: "hello"},
	}}}
	if got := einoSystemPrompt(plain); got != "" {
		t.Fatalf("expected empty prompt, got %q", got)
	}
	if got := einoMessages(plain); len(got) != 2 {
		t.Fatalf("ordinary system messages must be preserved: %#v", got)
	}
}

func TestEinoDeepAgentFactoryFailsClosedForUnsupportedOrUnmanagedBuild(t *testing.T) {
	factory := NewEinoDeepAgentFactory()
	base := EinoManagedAgentBuildRequest{Definition: AgentDefinition{AgentID: "agent", Version: "v1", Runtime: RuntimeSpec{Type: RuntimeTypeEino, Mode: RuntimeModeDeepAgent}}, InternalAgents: EinoInternalAgentDecorator{}}

	if _, err := factory.Build(context.Background(), base); !errors.Is(err, ErrEinoManagedChatModelMissing) {
		t.Fatalf("missing managed chat model error = %v", err)
	}
	base.ChatModel = stubEinoToolCallingModel{}
	base.Definition.Runtime.Mode = RuntimeModeReact
	if _, err := factory.Build(context.Background(), base); !errors.Is(err, ErrEinoManagedModeUnsupported) {
		t.Fatalf("unsupported mode error = %v", err)
	}
	base.Definition.Runtime.Mode = RuntimeModeDeepAgent
	base.InternalAgents = nil
	if _, err := factory.Build(context.Background(), base); !errors.Is(err, ErrEinoInternalAgentDecoratorMissing) {
		t.Fatalf("missing internal decorator error = %v", err)
	}
}

func TestEinoDeepAgentFactoryRejectsInvalidIterationLimit(t *testing.T) {
	factory := newEinoDeepAgentFactory(func(context.Context, *deep.Config) (adk.ResumableAgent, error) {
		return observedCompletedEinoInternalAgent{}, nil
	})
	for _, value := range []string{"0", "-1", "not-a-number", "1001"} {
		req := EinoManagedAgentBuildRequest{
			Definition: AgentDefinition{AgentID: "agent", Version: "v1", Runtime: RuntimeSpec{Type: RuntimeTypeEino, Mode: RuntimeModeDeepAgent}, Metadata: map[string]string{"max_iterations": value}},
			ChatModel:  stubEinoToolCallingModel{}, InternalAgents: EinoInternalAgentDecorator{},
		}
		if _, err := factory.Build(context.Background(), req); !errors.Is(err, ErrEinoManagedConfigInvalid) {
			t.Fatalf("max_iterations=%q error = %v", value, err)
		}
	}
}

func TestEinoRuntimeBuildsOnlyAuthorizedVersionedToolProxies(t *testing.T) {
	resolver := EinoToolInfoResolverFunc(func(_ context.Context, refs []string) ([]EinoResolvedTool, error) {
		if len(refs) != 2 || refs[0] != "search@v2" || refs[1] != "weather" {
			t.Fatalf("resolver received refs = %#v", refs)
		}
		return []EinoResolvedTool{
			{Ref: "search@v2", Info: &schema.ToolInfo{Name: "search"}},
			{Ref: "weather", Info: &schema.ToolInfo{Name: "weather"}},
		}, nil
	})
	runtime := &EinoRuntime{Environment: RuntimeEnvironment{Tools: deepFactoryToolInvoker{}}, ToolInfos: resolver}
	proxies, err := runtime.platformToolProxies(context.Background(), RunRequest{
		SessionID: "session_1", RunID: "run_1", Definition: AgentDefinition{AgentID: "agent_1"},
	}, ModelContextPackage{Capabilities: ModelContextCapabilities{Tools: []string{"search@v2", "weather", "search@v2"}}})
	if err != nil {
		t.Fatalf("build tool proxies: %v", err)
	}
	if len(proxies) != 2 {
		t.Fatalf("tool proxy count = %d", len(proxies))
	}
	for i, want := range []string{"search", "weather"} {
		info, err := proxies[i].Info(context.Background())
		if err != nil || info.Name != want {
			t.Fatalf("proxy[%d] info = %#v err=%v", i, info, err)
		}
	}
}

func TestEinoRuntimeToolResolutionFailsClosed(t *testing.T) {
	tests := []struct {
		name     string
		resolved []EinoResolvedTool
	}{
		{name: "missing"},
		{name: "unauthorized", resolved: []EinoResolvedTool{{Ref: "other", Info: &schema.ToolInfo{Name: "other"}}}},
		{name: "duplicate", resolved: []EinoResolvedTool{{Ref: "search@v1", Info: &schema.ToolInfo{Name: "search"}}, {Ref: "search@v1", Info: &schema.ToolInfo{Name: "search"}}}},
		{name: "schema name mismatch", resolved: []EinoResolvedTool{{Ref: "search@v1", Info: &schema.ToolInfo{Name: "other"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime := &EinoRuntime{
				Environment: RuntimeEnvironment{Tools: deepFactoryToolInvoker{}},
				ToolInfos: EinoToolInfoResolverFunc(func(context.Context, []string) ([]EinoResolvedTool, error) {
					return tt.resolved, nil
				}),
			}
			_, err := runtime.platformToolProxies(context.Background(), RunRequest{}, ModelContextPackage{Capabilities: ModelContextCapabilities{Tools: []string{"search@v1"}}})
			if !errors.Is(err, ErrEinoToolProxyInfoMissing) {
				t.Fatalf("tool resolution error = %v", err)
			}
		})
	}
}

type stubEinoToolCallingModel struct{ model.ToolCallingChatModel }

type stubEinoBaseTool struct{ tool.BaseTool }

type stubEinoPreModelHandler struct{ adk.ChatModelAgentMiddleware }

type deepFactoryToolInvoker struct{}

func (deepFactoryToolInvoker) Invoke(context.Context, ToolInvocationRequest, ToolEventSink) (ToolInvocationResult, error) {
	return ToolInvocationResult{Content: "ok"}, nil
}
