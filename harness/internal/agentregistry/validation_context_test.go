package agentregistry

import (
	"testing"
)

// validation_context_test.go 覆盖 A2 context.history 与 A4 tool_policy.execution
// 的取值校验与 ToAgentDefinition 透传。

func TestValidateContextHistoryAndToolExecution(t *testing.T) {
	valid := []struct {
		history   string
		execution string
	}{
		{"", ""},
		{"session", "sequential"},
		{"none", "parallel"},
	}
	for _, tc := range valid {
		cfg := extensionsBaseConfig()
		cfg.Context.History = tc.history
		cfg.ToolPolicy.Execution = tc.execution
		if err := ValidateAgentConfig(cfg); err != nil {
			t.Fatalf("history=%q execution=%q must pass: %v", tc.history, tc.execution, err)
		}
	}

	badHistory := extensionsBaseConfig()
	badHistory.Context.History = "forever"
	if err := ValidateAgentConfig(badHistory); err == nil {
		t.Fatal("invalid context.history must fail closed")
	}

	badExec := extensionsBaseConfig()
	badExec.ToolPolicy.Execution = "concurrent"
	if err := ValidateAgentConfig(badExec); err == nil {
		t.Fatal("invalid tool_policy.execution must fail closed")
	}
}

func TestContextAndToolPolicyThreadIntoDefinition(t *testing.T) {
	cfg := extensionsBaseConfig()
	cfg.Context.History = "none"
	cfg.ToolPolicy.Execution = "parallel"
	def := cfg.ToAgentDefinition()
	if def.ContextHistory != "none" {
		t.Fatalf("context.history must thread into definition, got %q", def.ContextHistory)
	}
	if def.ToolExecution != "parallel" {
		t.Fatalf("tool_policy.execution must thread into definition, got %q", def.ToolExecution)
	}
}

// 缺省归一（ADR-014）：未声明 tool_policy.execution 的 Agent 必须得到
// sequential，不得隐式获得并发工具副作用；并行只能显式选入。
func TestToolExecutionDefaultsToSequential(t *testing.T) {
	cfg := extensionsBaseConfig()
	cfg.ToolPolicy.Execution = ""
	if got := cfg.ToAgentDefinition().ToolExecution; got != ToolExecutionSequential {
		t.Fatalf("absent tool_policy.execution must default to sequential, got %q", got)
	}
	cfg.ToolPolicy.Execution = ToolExecutionSequential
	if got := cfg.ToAgentDefinition().ToolExecution; got != ToolExecutionSequential {
		t.Fatalf("explicit sequential must stay sequential, got %q", got)
	}
	cfg.ToolPolicy.Execution = ToolExecutionParallel
	if got := cfg.ToAgentDefinition().ToolExecution; got != ToolExecutionParallel {
		t.Fatalf("explicit parallel must stay parallel, got %q", got)
	}
}
