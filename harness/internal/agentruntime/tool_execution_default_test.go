package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/prebuilt/deep"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// tool_execution_default_test.go 覆盖 ADR-014 的缺省语义与 ScopedData 在
// native direct 路径的透传。

// 未显式声明 tool_policy.execution 的 Agent 必须串行执行本轮 N 个工具调用；
// 并行只在配置显式选入时启用。缺省归一在 agentregistry 完成，工厂侧反向
// 判定作为双保险，因此此处直接以 Definition 取值驱动断言。
func TestDeepAgentToolExecutionDefaultsToSequential(t *testing.T) {
	cases := []struct {
		execution      string
		wantSequential bool
	}{
		{"", true},
		{ToolExecutionSequential, true},
		{ToolExecutionParallel, false},
	}
	for _, tc := range cases {
		var captured *deep.Config
		factory := newEinoDeepAgentFactory(func(_ context.Context, cfg *deep.Config) (adk.ResumableAgent, error) {
			captured = cfg
			return observedCompletedEinoInternalAgent{}, nil
		})
		_, err := factory.Build(context.Background(), EinoManagedAgentBuildRequest{
			Definition: AgentDefinition{
				AgentID:       "deep_agent",
				AgentType:     "assistant",
				Runtime:       RuntimeSpec{Type: RuntimeTypeEino, Mode: RuntimeModeDeepAgent},
				ToolExecution: tc.execution,
			},
			ChatModel:      stubEinoToolCallingModel{},
			InternalAgents: EinoInternalAgentDecorator{},
		})
		if err != nil {
			t.Fatalf("execution=%q build: %v", tc.execution, err)
		}
		if captured == nil {
			t.Fatalf("execution=%q: deep builder was not invoked", tc.execution)
		}
		if got := captured.ToolsConfig.ToolsNodeConfig.ExecuteSequentially; got != tc.wantSequential {
			t.Fatalf("execution=%q: ExecuteSequentially = %v, want %v", tc.execution, got, tc.wantSequential)
		}
	}
}

// scopedDataProbeInvoker 代替真实模型调用：记下 transform 塑形后的请求并
// 以错误终止本轮（不需要 provider 与事件流）。
type scopedDataProbeInvoker struct {
	seen ModelInvokeRequest
}

func (i *scopedDataProbeInvoker) Invoke(_ context.Context, req ModelInvokeRequest) (<-chan ModelStreamItem, error) {
	i.seen = req
	return nil, errors.New("probe: stop after transform")
}

// native direct 路径：本 Run 冻结的 ScopedData 必须随模型调用下传给
// ModelInputTransform（BeforeModelHook 的挂载点），且是深拷贝——变换实现
// 改写它不得回写 RunRequest 快照。
func TestNativeDirectPassesScopedDataToModelTransform(t *testing.T) {
	var seen ScopedData
	probe := &scopedDataProbeInvoker{}
	runtime := &NativeDirectRuntime{
		Models: probe,
		ModelInputTransform: func(_ context.Context, _ AgentDefinition, req ModelInvokeRequest) (ModelInvokeRequest, error) {
			seen = req.ScopedData
			if req.ScopedData.Run != nil {
				entry := req.ScopedData.Run["device"]
				entry.Source = "tampered"
				req.ScopedData.Run["device"] = entry
			}
			return req, nil
		},
	}
	req := RunRequest{
		Definition: AgentDefinition{AgentID: "agent_1"},
		ScopedData: ScopedData{Run: map[string]ScopedDataItem{
			"device": {Source: "client", Value: json.RawMessage(`{"tools":["nav"]}`)},
		}},
	}
	events := make(chan observability.AgentEvent, 8)
	// probe invoker 在 transform 之后以错误终止；断言只关心 transform 与
	// invoker 看到的 ScopedData 视图。
	if _, runtimeErr := runtime.runModelRound(context.Background(), req, ModelContextPackage{}, 1, false, events); runtimeErr == nil {
		t.Fatal("probe invoker must surface a runtime error")
	}

	entry, ok := seen.Run["device"]
	if !ok {
		t.Fatal("scoped data must be threaded into the model invoke request")
	}
	if string(entry.Value) != `{"tools":["nav"]}` {
		t.Fatalf("scoped value = %s", entry.Value)
	}
	if req.ScopedData.Run["device"].Source != "client" {
		t.Fatalf("transform rewrite leaked into the run snapshot: %q", req.ScopedData.Run["device"].Source)
	}
	if got := probe.seen.ScopedData.Run["device"].Source; got != "tampered" {
		t.Fatalf("transform output must reach the invoker, source=%q", got)
	}
}
