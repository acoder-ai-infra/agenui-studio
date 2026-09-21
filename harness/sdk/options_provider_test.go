package harness

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
)

// options_provider_test.go 覆盖四个数组化 Provider 注册入口的 Build 语义：
// 变参累加、条目 ID 取 impl.ID()、nil 元素 / 空 ID / 重复 (kind, id)
// fail-closed。

type providerStubHook struct{ id string }

func (t providerStubHook) ID() string { return t.id }
func (providerStubHook) BeforeModel(context.Context, extension.BeforeModelRequest) (extension.BeforeModelResult, error) {
	return extension.BeforeModelResult{}, nil
}

type providerStubInterceptor struct{ id string }

func (i providerStubInterceptor) ID() string { return i.id }
func (providerStubInterceptor) Intercept(ctx context.Context, call extension.ToolCallInfo, next extension.ToolCallNext) (extension.ToolCallOutcome, error) {
	return next(ctx, call.Arguments)
}

type providerStubValidator struct{ id string }

func (v providerStubValidator) ID() string { return v.id }
func (providerStubValidator) Validate(context.Context, extension.OutputValidateRequest) (extension.OutputValidateResult, error) {
	return extension.OutputValidateResult{Action: extension.OutputAccept}, nil
}

type providerStubTool struct{ name string }

func (t providerStubTool) Name() string { return t.name }
func (providerStubTool) Invoke(context.Context, extension.FunctionCall) (*extension.FunctionResult, error) {
	return &extension.FunctionResult{Data: json.RawMessage(`{}`)}, nil
}

type providerStubToolProvider struct{ id string }

func (p providerStubToolProvider) ID() string { return p.id }
func (providerStubToolProvider) FunctionTools() []extension.FunctionTool {
	return []extension.FunctionTool{providerStubTool{name: "opt.echo"}}
}

// applyProviderOptions 把 Options 应用到累加器并走 buildExtensionCatalog
// （Build 的扩展冻结路径）。
func applyProviderOptions(opts ...Option) (*kernel.ExtensionCatalog, []ExtensionInfo, error) {
	settings := &buildSettings{}
	for _, opt := range opts {
		opt(settings)
	}
	return buildExtensionCatalog(settings)
}

// 变参累加：一次多个 + 多次调用叠加，条目 ID 取自 impl.ID()。
func TestProviderOptionsAccumulateWithSelfDescribedIDs(t *testing.T) {
	catalog, infos, err := applyProviderOptions(
		WithBeforeModelHookProvider(
			providerStubHook{id: "opt.t1"},
			providerStubHook{id: "opt.t2"},
		),
		WithBeforeModelHookProvider(providerStubHook{id: "opt.t3"}),
		WithToolCallInterceptorProvider(providerStubInterceptor{id: "opt.i1"}),
		WithOutputValidatorProvider(providerStubValidator{id: "opt.v1"}),
		WithToolProvider(providerStubToolProvider{id: "opt.tools"}),
	)
	if err != nil {
		t.Fatalf("build catalog: %v", err)
	}
	transformers := catalog.ByKind(kernel.ExtBeforeModelHook)
	if len(transformers) != 3 || transformers[0].ID != "opt.t1" || transformers[1].ID != "opt.t2" || transformers[2].ID != "opt.t3" {
		t.Fatalf("transformer entries = %+v", transformers)
	}
	if entries := catalog.ByKind(kernel.ExtToolCallInterceptor); len(entries) != 1 || entries[0].ID != "opt.i1" {
		t.Fatalf("interceptor entries = %+v", entries)
	}
	if entries := catalog.ByKind(kernel.ExtOutputValidator); len(entries) != 1 || entries[0].ID != "opt.v1" {
		t.Fatalf("validator entries = %+v", entries)
	}
	provider, ok := catalog.Find(kernel.ExtToolProvider, "opt.tools")
	if !ok {
		t.Fatal("tool provider entry missing")
	}
	if _, isProvider := provider.Implementation.(extension.ToolProvider); !isProvider {
		t.Fatalf("tool provider implementation type = %T", provider.Implementation)
	}
	// BuildReport 口径：HandlerNames 浮现实现目录。
	for _, info := range infos {
		if info.ID == "opt.tools" && (len(info.HandlerNames) != 1 || info.HandlerNames[0] != "opt.echo") {
			t.Fatalf("HandlerNames = %v", info.HandlerNames)
		}
	}
}

// nil 元素 / 空 ID / 重复 (kind, id) 在 Build 冻结时 fail-closed。
func TestProviderOptionsFailClosedOnInvalidRegistrations(t *testing.T) {
	cases := []struct {
		name string
		opts []Option
	}{
		{name: "nil transformer", opts: []Option{WithBeforeModelHookProvider(nil)}},
		{name: "empty transformer id", opts: []Option{WithBeforeModelHookProvider(providerStubHook{id: ""})}},
		{name: "nil interceptor", opts: []Option{WithToolCallInterceptorProvider(nil)}},
		{name: "empty validator id", opts: []Option{WithOutputValidatorProvider(providerStubValidator{id: ""})}},
		{name: "nil tool provider", opts: []Option{WithToolProvider(nil)}},
		{name: "empty tool provider id", opts: []Option{WithToolProvider(providerStubToolProvider{id: ""})}},
		{name: "duplicate kind+id", opts: []Option{
			WithBeforeModelHookProvider(providerStubHook{id: "dup"}),
			WithBeforeModelHookProvider(providerStubHook{id: "dup"}),
		}},
	}
	for _, tc := range cases {
		if _, _, err := applyProviderOptions(tc.opts...); err == nil {
			t.Fatalf("%s: expected fail-closed error", tc.name)
		} else if tc.name != "duplicate kind+id" && !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%s: want ErrInvalidRequest, got %v", tc.name, err)
		}
	}
}
