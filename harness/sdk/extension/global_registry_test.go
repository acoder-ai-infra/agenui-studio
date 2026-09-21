package extension

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// global_registry_test.go 覆盖包级全局注册表（全集）的注册 / 查询 / panic
// 契约。全集为进程级只增状态，用例统一使用 test. 前缀的唯一 ID 避免互相
// 污染。

type globalStubHook struct{ id string }

func (t globalStubHook) ID() string { return t.id }
func (globalStubHook) BeforeModel(context.Context, BeforeModelRequest) (BeforeModelResult, error) {
	return BeforeModelResult{}, nil
}

type globalStubInterceptor struct{ id string }

func (i globalStubInterceptor) ID() string { return i.id }
func (globalStubInterceptor) Intercept(ctx context.Context, call ToolCallInfo, next ToolCallNext) (ToolCallOutcome, error) {
	return next(ctx, call.Arguments)
}

type globalStubValidator struct{ id string }

func (v globalStubValidator) ID() string { return v.id }
func (globalStubValidator) Validate(context.Context, OutputValidateRequest) (OutputValidateResult, error) {
	return OutputValidateResult{Action: OutputAccept}, nil
}

type globalStubTool struct{ name string }

func (t globalStubTool) Name() string { return t.name }
func (globalStubTool) Invoke(context.Context, FunctionCall) (*FunctionResult, error) {
	return &FunctionResult{Data: json.RawMessage(`{}`)}, nil
}

func TestGlobalRegistryRegisterAndLookup(t *testing.T) {
	RegisterBeforeModelHooks(globalStubHook{id: "test.global.transformer"})
	RegisterToolCallInterceptors(globalStubInterceptor{id: "test.global.interceptor"})
	RegisterOutputValidators(globalStubValidator{id: "test.global.validator"})
	RegisterTools(globalStubTool{name: "test.global.tool"})

	if _, ok := LookupBeforeModelHook("test.global.transformer"); !ok {
		t.Fatal("transformer should be resolvable from the global registry")
	}
	if _, ok := LookupToolCallInterceptor("test.global.interceptor"); !ok {
		t.Fatal("interceptor should be resolvable from the global registry")
	}
	if _, ok := LookupOutputValidator("test.global.validator"); !ok {
		t.Fatal("validator should be resolvable from the global registry")
	}
	tools := GlobalFunctionTools()
	if _, ok := tools["test.global.tool"]; !ok {
		t.Fatal("tool should be visible in GlobalFunctionTools snapshot")
	}
	if _, ok := LookupBeforeModelHook("test.global.missing"); ok {
		t.Fatal("unknown transformer id must not resolve")
	}
	// 快照隔离：修改返回 map 不影响全集。
	delete(tools, "test.global.tool")
	if _, ok := GlobalFunctionTools()["test.global.tool"]; !ok {
		t.Fatal("GlobalFunctionTools must return an isolated snapshot")
	}
}

func TestGlobalRegistryDuplicatePanics(t *testing.T) {
	RegisterBeforeModelHooks(globalStubHook{id: "test.global.dup"})
	assertPanics(t, "duplicate transformer id", func() {
		RegisterBeforeModelHooks(globalStubHook{id: "test.global.dup"})
	})
	RegisterTools(globalStubTool{name: "test.global.dup_tool"})
	assertPanics(t, "duplicate tool name", func() {
		RegisterTools(globalStubTool{name: "test.global.dup_tool"})
	})
}

func TestGlobalRegistryInvalidRegistrationPanics(t *testing.T) {
	assertPanics(t, "nil transformer", func() {
		RegisterBeforeModelHooks(nil)
	})
	assertPanics(t, "empty transformer id", func() {
		RegisterBeforeModelHooks(globalStubHook{id: ""})
	})
	assertPanics(t, "nil interceptor", func() {
		RegisterToolCallInterceptors(nil)
	})
	assertPanics(t, "empty validator id", func() {
		RegisterOutputValidators(globalStubValidator{id: ""})
	})
	assertPanics(t, "nil tool", func() {
		RegisterTools(nil)
	})
	assertPanics(t, "empty tool name", func() {
		RegisterTools(globalStubTool{name: ""})
	})
}

func TestGlobalRegistryConcurrentLookup(t *testing.T) {
	RegisterBeforeModelHooks(globalStubHook{id: "test.global.concurrent"})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if _, ok := LookupBeforeModelHook("test.global.concurrent"); !ok {
					t.Error("concurrent lookup must succeed")
					return
				}
				_ = GlobalFunctionTools()
			}
		}()
	}
	wg.Wait()
}

func assertPanics(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s: expected panic", name)
		}
	}()
	fn()
}
