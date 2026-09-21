package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// stubFunctionTool 是一款记录调用的宿主 function 工具实现。
type stubFunctionTool struct {
	name     string
	lastCall extension.FunctionCall
	result   *extension.FunctionResult
	err      error
}

func (t *stubFunctionTool) Name() string { return t.name }

func (t *stubFunctionTool) Invoke(_ context.Context, call extension.FunctionCall) (*extension.FunctionResult, error) {
	t.lastCall = call
	return t.result, t.err
}

func TestExtensionFunctionHandlerClassifiesLocalFailures(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantType  toolgateway.ErrorType
		retryable bool
	}{
		{
			name: "typed business validation",
			err: extension.NewFunctionError(
				extension.FunctionErrorInvalidArgument, "target does not exist", true, nil,
			),
			wantType: toolgateway.ErrorTypeInvalidArgument,
		},
		{
			name: "typed dependency failure",
			err: extension.NewFunctionError(
				extension.FunctionErrorUnavailable, "provider unavailable", true, nil,
			),
			wantType: toolgateway.ErrorTypeUpstreamError, retryable: true,
		},
		{name: "untyped local failure", err: errors.New("local validation leaked"), wantType: toolgateway.ErrorTypeInternal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newExtensionFunctionHandler(&stubFunctionTool{name: "biz.failure", err: test.err}, kernel.TurnEnvironment{})
			_, err := handler(context.Background(), toolgateway.FunctionCall{Trace: observability.TraceContext{TenantID: "t1"}})
			var toolErr *toolgateway.ToolError
			if !errors.As(err, &toolErr) || toolErr.Type != test.wantType || toolErr.Retryable != test.retryable {
				t.Fatalf("error=%#v, want type=%s retryable=%t", err, test.wantType, test.retryable)
			}
		})
	}
}

// stubToolProvider 打包一组 function 工具实现（不声明任何工具定义）。
type stubToolProvider struct {
	id    string
	tools []extension.FunctionTool
}

func (p *stubToolProvider) ID() string                              { return p.id }
func (p *stubToolProvider) FunctionTools() []extension.FunctionTool { return p.tools }

func toolProviderCatalog(t *testing.T, entries ...kernel.ExtensionEntry) *kernel.ExtensionCatalog {
	t.Helper()
	catalog, err := kernel.NewExtensionCatalog(entries)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	return catalog
}

// TestExtensionFunctionHandlersReachProvider 验证 provider 的实现被收集为
// gateway handler，并在调用时收到 tools.yaml 的工具名与 trace 身份。
func TestExtensionFunctionHandlersReachProvider(t *testing.T) {
	tool := &stubFunctionTool{
		name: "biz.echo",
		result: &extension.FunctionResult{
			Data: json.RawMessage(`{"ok":true}`), MimeType: "application/json",
			Presentation: &extension.ResultPresentation{
				Title: "已完成", Details: []extension.ResultPresentationDetail{{Label: "状态", Value: "正常"}},
			},
		},
	}
	catalog := toolProviderCatalog(t, kernel.ExtensionEntry{
		ID: "myapp.tools", Kind: kernel.ExtToolProvider, Implementation: &stubToolProvider{tools: []extension.FunctionTool{tool}},
	})
	handlers, err := collectExtensionFunctionHandlers(catalog, kernel.TurnEnvironment{
		Environment: "local", SDKVersion: kernel.SDKContractVersion,
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	handler := handlers["biz.echo"]
	if handler == nil {
		t.Fatalf("handler not registered: %v", handlers)
	}
	res, err := handler(context.Background(), toolgateway.FunctionCall{
		ToolCallID: "tc1", ToolName: "get_template", ToolVersion: "1.0.0",
		Arguments: json.RawMessage(`{"q":"hi"}`),
		Trace:     observability.TraceContext{TenantID: "t1", RunID: "run1", AgentID: "demo"},
	})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if string(res.Data) != `{"ok":true}` || res.MimeType != "application/json" {
		t.Fatalf("result wrong: %+v", res)
	}
	if res.Presentation == nil || res.Presentation.Title != "已完成" || len(res.Presentation.Details) != 1 {
		t.Fatalf("result presentation not propagated: %+v", res.Presentation)
	}
	if tool.lastCall.Name != "get_template" || tool.lastCall.Version != "1.0.0" {
		t.Fatalf("tool name/version not propagated: %+v", tool.lastCall)
	}
	if tool.lastCall.Ctx.TenantID != "t1" || tool.lastCall.Ctx.RunID != "run1" || tool.lastCall.Ctx.AgentID != "demo" {
		t.Fatalf("identity not propagated: %+v", tool.lastCall.Ctx)
	}
	if tool.lastCall.Ctx.Environment != "local" {
		t.Fatalf("turn environment not propagated: %+v", tool.lastCall.Ctx)
	}
}

// TestExtensionFunctionHandlersFailClosed 验证实现名的三条约束：非空、
// 禁用 harness. 保留前缀、跨 provider 不得重名。
func TestExtensionFunctionHandlersFailClosed(t *testing.T) {
	empty := toolProviderCatalog(t, kernel.ExtensionEntry{
		ID: "myapp.a", Kind: kernel.ExtToolProvider,
		Implementation: &stubToolProvider{tools: []extension.FunctionTool{&stubFunctionTool{name: "  "}}},
	})
	if _, err := collectExtensionFunctionHandlers(empty, kernel.TurnEnvironment{}); err == nil || !strings.Contains(err.Error(), "without a name") {
		t.Fatalf("empty handler name must fail closed, got %v", err)
	}

	reserved := toolProviderCatalog(t, kernel.ExtensionEntry{
		ID: "myapp.b", Kind: kernel.ExtToolProvider,
		Implementation: &stubToolProvider{tools: []extension.FunctionTool{&stubFunctionTool{name: "harness.echo"}}},
	})
	if _, err := collectExtensionFunctionHandlers(reserved, kernel.TurnEnvironment{}); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved prefix must fail closed, got %v", err)
	}

	duplicate := toolProviderCatalog(t,
		kernel.ExtensionEntry{
			ID: "myapp.c", Kind: kernel.ExtToolProvider, Order: 1,
			Implementation: &stubToolProvider{tools: []extension.FunctionTool{&stubFunctionTool{name: "biz.dup"}}},
		},
		kernel.ExtensionEntry{
			ID: "myapp.d", Kind: kernel.ExtToolProvider, Order: 2,
			Implementation: &stubToolProvider{tools: []extension.FunctionTool{&stubFunctionTool{name: "biz.dup"}}},
		},
	)
	if _, err := collectExtensionFunctionHandlers(duplicate, kernel.TurnEnvironment{}); err == nil || !strings.Contains(err.Error(), "declared by both") {
		t.Fatalf("duplicate handler name must fail closed, got %v", err)
	}
}

// TestExtensionHandlerConflictsWithBuiltin 验证扩展实现与内置 handler 重名时
// buildConfiguredTools fail-closed（harness. 前缀禁令之外的兜底）。
func TestExtensionHandlerConflictsWithBuiltin(t *testing.T) {
	handlers := map[string]toolgateway.FunctionTool{
		"harness.ask_user": func(context.Context, toolgateway.FunctionCall) (*toolgateway.FunctionResult, error) {
			return nil, nil
		},
	}
	_, _, err := buildConfiguredTools(toolCatalogConfig{}, nil, handlers)
	if err == nil || !strings.Contains(err.Error(), "conflicts with a builtin handler") {
		t.Fatalf("builtin conflict must fail closed, got %v", err)
	}
}

// TestExtensionToolDefinitionReferencesRegisteredHandler 验证 tools.yaml 的
// function 工具引用宿主实现时组装成功，且治理属性来自 yaml。
func TestExtensionToolDefinitionReferencesRegisteredHandler(t *testing.T) {
	handlers := map[string]toolgateway.FunctionTool{
		"biz.lookup": func(context.Context, toolgateway.FunctionCall) (*toolgateway.FunctionResult, error) {
			return &toolgateway.FunctionResult{Data: json.RawMessage(`{}`)}, nil
		},
	}
	catalog := toolCatalogConfig{
		SchemaVersion: "harness.capability-catalog.v1",
		Enabled:       []string{"biz_lookup@1.0.0"},
		Definitions: []configuredTool{{
			Name: "biz_lookup", Version: "1.0.0", Type: string(toolgateway.ToolTypeFunction),
			Handler: "biz.lookup", AllowedAgents: []string{"demo"}, TimeoutMS: 3000,
			InputSchema: map[string]any{"type": "object"},
		}},
	}
	definitions, merged, err := buildConfiguredTools(catalog, nil, handlers)
	if err != nil {
		t.Fatalf("buildConfiguredTools: %v", err)
	}
	if len(definitions) != 1 {
		t.Fatalf("definitions = %d; want 1", len(definitions))
	}
	def := definitions[0]
	if def.Function == nil || def.Function.HandlerName != "biz.lookup" {
		t.Fatalf("handler binding wrong: %+v", def.Function)
	}
	// 治理属性来自 yaml：allowed_agents 非空是 policy 放行的前提。
	if len(def.Permissions.AllowedAgents) != 1 || def.Permissions.AllowedAgents[0] != "demo" {
		t.Fatalf("allowed_agents must come from tools.yaml: %+v", def.Permissions)
	}
	if merged["biz.lookup"] == nil {
		t.Fatal("extension handler missing from merged handler table")
	}

	// 引用未注册实现时 fail-closed。
	catalog.Definitions[0].Handler = "biz.missing"
	if _, _, err := buildConfiguredTools(catalog, nil, handlers); err == nil || !strings.Contains(err.Error(), "unknown function handler") {
		t.Fatalf("unknown handler must fail closed, got %v", err)
	}
}

// globalFallbackTool 是注册进全集的 FunctionTool（全集不可撤销，用专属
// 名字避免跨用例冲突）。
type globalFallbackTool struct {
	name   string
	called *bool
}

func (t globalFallbackTool) Name() string { return t.name }
func (t globalFallbackTool) Invoke(context.Context, extension.FunctionCall) (*extension.FunctionResult, error) {
	if t.called != nil {
		*t.called = true
	}
	return &extension.FunctionResult{Data: json.RawMessage(`{"from":"global"}`)}, nil
}

// TestMergeGlobalFunctionHandlersFallback 验证 tools.yaml handler 走全集
// 兜底：Options 已占用的名字优先、未占用的名字从全集补齐。
func TestMergeGlobalFunctionHandlersFallback(t *testing.T) {
	globalCalled := false
	extension.RegisterTools(
		globalFallbackTool{name: "test.app.global_fallback", called: &globalCalled},
		globalFallbackTool{name: "test.app.shadowed_tool"},
	)
	optionsCalled := false
	optionsHandlers := map[string]toolgateway.FunctionTool{
		"test.app.shadowed_tool": func(context.Context, toolgateway.FunctionCall) (*toolgateway.FunctionResult, error) {
			optionsCalled = true
			return &toolgateway.FunctionResult{Data: json.RawMessage(`{"from":"options"}`)}, nil
		},
	}
	merged, err := mergeGlobalFunctionHandlers(optionsHandlers, kernel.TurnEnvironment{Environment: "local"})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	// 全集补齐未占用的名字，且可经 gateway handler 形状调通全集实现。
	fallback := merged["test.app.global_fallback"]
	if fallback == nil {
		t.Fatal("global tool must be merged as fallback handler")
	}
	trace := observability.TraceContext{TenantID: "t1", RunID: "run_1", AgentID: "agent_1"}
	if _, err := fallback(context.Background(), toolgateway.FunctionCall{Trace: trace, ToolName: "global_tool", Arguments: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("invoke fallback handler: %v", err)
	}
	if !globalCalled {
		t.Fatal("fallback handler must reach the globally registered tool")
	}
	// Options 已占用的名字优先：同名全集实现被遮蔽。
	shadowed := merged["test.app.shadowed_tool"]
	result, err := shadowed(context.Background(), toolgateway.FunctionCall{Trace: trace, Arguments: json.RawMessage(`{}`)})
	if err != nil || !optionsCalled || !strings.Contains(string(result.Data), "options") {
		t.Fatalf("options handler must take precedence: result=%v err=%v optionsCalled=%v", result, err, optionsCalled)
	}

	// tools.yaml 的 function 工具可引用全集兜底的 handler（Build 不再
	// fail-closed）。
	catalog := toolCatalogConfig{
		SchemaVersion: "harness.capability-catalog.v1",
		Enabled:       []string{"global_tool@1.0.0"},
		Definitions: []configuredTool{{
			Name: "global_tool", Version: "1.0.0", Type: string(toolgateway.ToolTypeFunction),
			Handler: "test.app.global_fallback", AllowedAgents: []string{"demo"}, TimeoutMS: 3000,
			InputSchema: map[string]any{"type": "object"},
		}},
	}
	definitions, _, err := buildConfiguredTools(catalog, nil, merged)
	if err != nil {
		t.Fatalf("buildConfiguredTools with global fallback: %v", err)
	}
	if len(definitions) != 1 || definitions[0].Function == nil || definitions[0].Function.HandlerName != "test.app.global_fallback" {
		t.Fatalf("definition binding wrong: %+v", definitions)
	}
}

// TestRegisterToolsRejectsInvalidNameAtRegistration 验证非法工具名在全集
// 注册入口就被 panic 拒绝（无法进入全集）。"harness." 保留前缀的门禁在
// mergeGlobalFunctionHandlers 内部实现；由于全集是进程级不可撤销状态，
// 真实注册一个 harness. 前缀工具会让同进程内所有后续 merge 调用持续
// 报错（连累其他用例），故不在单测中触发该分支。
func TestRegisterToolsRejectsInvalidNameAtRegistration(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("empty tool name must panic at registration")
		}
	}()
	extension.RegisterTools(globalFallbackTool{name: ""})
}
