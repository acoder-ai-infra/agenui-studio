package agentregistry

import (
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
)

// validation_extensions_test.go 覆盖 agent 级扩展绑定声明（extensions 字段）
// 的校验与 ToAgentDefinition 透传。

// extensionsBaseConfig 构造一份可通过基础校验的 eino deep_agent 配置。
func extensionsBaseConfig() AgentConfig {
	return AgentConfig{
		AgentID:   "ext_agent",
		AgentType: "assistant",
		Version:   "1.0.0",
		Runtime:   agentruntime.RuntimeSpec{Type: agentruntime.RuntimeTypeEino, Mode: agentruntime.RuntimeModeDeepAgent},
		PromptRef: "prompt://ext_agent/system",
		Orchestration: OrchestrationConfig{
			DefaultMode: ExecutionModeDeepAgent,
		},
	}
}

func TestValidateExtensionsConfigAcceptsOrderedIDs(t *testing.T) {
	cfg := extensionsBaseConfig()
	cfg.Extensions = ExtensionsConfig{
		BeforeModelHooks:     []string{"biz.a", "biz.b"},
		ToolCallInterceptors: []string{"biz.audit"},
		OutputValidators:     []string{"biz.check"},
	}
	if err := ValidateAgentConfig(cfg); err != nil {
		t.Fatalf("valid extensions must pass: %v", err)
	}
}

func TestValidateExtensionsConfigRejectsBadIDs(t *testing.T) {
	cases := []struct {
		name string
		ext  ExtensionsConfig
		want string
	}{
		{name: "empty id", ext: ExtensionsConfig{BeforeModelHooks: []string{" "}}, want: "must not be empty"},
		{name: "control char", ext: ExtensionsConfig{OutputValidators: []string{"bad\nid"}}, want: "control characters"},
		{name: "duplicate", ext: ExtensionsConfig{ToolCallInterceptors: []string{"dup", "dup"}}, want: "duplicate extension id"},
	}
	for _, tc := range cases {
		cfg := extensionsBaseConfig()
		cfg.Extensions = tc.ext
		err := ValidateAgentConfig(cfg)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want error containing %q, got %v", tc.name, tc.want, err)
		}
	}
}

// 纯 native runtime 声明 tool_call_interceptors 直接拒绝（native 无工具环绕
// 挂载点，避免静默失效）。
func TestValidateExtensionsConfigRejectsInterceptorOnNativeRuntime(t *testing.T) {
	cfg := extensionsBaseConfig()
	cfg.Runtime = agentruntime.RuntimeSpec{Type: agentruntime.RuntimeTypeNative, Mode: agentruntime.RuntimeModeDirect}
	cfg.Orchestration = OrchestrationConfig{DefaultMode: ExecutionModeDirectAction}
	cfg.Extensions = ExtensionsConfig{ToolCallInterceptors: []string{"biz.audit"}}
	err := ValidateAgentConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "eino-capable") {
		t.Fatalf("native runtime with interceptors must be rejected, got %v", err)
	}
	// transformer / validator 不受 runtime 限制。
	cfg.Extensions = ExtensionsConfig{BeforeModelHooks: []string{"biz.a"}, OutputValidators: []string{"biz.check"}}
	if err := ValidateAgentConfig(cfg); err != nil {
		t.Fatalf("native runtime with transformer/validator must pass: %v", err)
	}
}

// ToAgentDefinition 把三个列表按声明顺序透传为一等字段（不排序）。
func TestToAgentDefinitionCarriesExtensionBindings(t *testing.T) {
	cfg := extensionsBaseConfig()
	cfg.Extensions = ExtensionsConfig{
		BeforeModelHooks:     []string{"z.second", "a.first"},
		ToolCallInterceptors: []string{"outer", "inner"},
		OutputValidators:     []string{"v1"},
	}
	definition := cfg.ToAgentDefinition()
	if got := definition.BeforeModelHooks; len(got) != 2 || got[0] != "z.second" || got[1] != "a.first" {
		t.Fatalf("transformers must keep declaration order: %v", got)
	}
	if got := definition.ToolCallInterceptors; len(got) != 2 || got[0] != "outer" || got[1] != "inner" {
		t.Fatalf("interceptors must keep declaration order: %v", got)
	}
	if got := definition.OutputValidators; len(got) != 1 || got[0] != "v1" {
		t.Fatalf("validators mismatch: %v", got)
	}
}
