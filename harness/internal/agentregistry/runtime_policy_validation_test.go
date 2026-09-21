package agentregistry

import (
	"context"
	"errors"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
)

func TestRuntimeMaxTurnsValidation(t *testing.T) {
	tests := []struct {
		name        string
		runtime     agentruntime.RuntimeSpec
		maxTurns    int
		wantInvalid bool
	}{
		{
			name: "negative",
			runtime: agentruntime.RuntimeSpec{
				Type: agentruntime.RuntimeTypeEino, Mode: agentruntime.RuntimeModeDeepAgent,
			},
			maxTurns: -1, wantInvalid: true,
		},
		{
			name: "negative is invalid for every runtime",
			runtime: agentruntime.RuntimeSpec{
				Type: agentruntime.RuntimeTypeMock, Mode: agentruntime.RuntimeModeDeepAgent,
			},
			maxTurns: -1, wantInvalid: true,
		},
		{
			name: "zero means unset",
			runtime: agentruntime.RuntimeSpec{
				Type: agentruntime.RuntimeTypeEino, Mode: agentruntime.RuntimeModeDeepAgent,
			},
			maxTurns: 0,
		},
		{
			name: "Eino limit",
			runtime: agentruntime.RuntimeSpec{
				Type: agentruntime.RuntimeTypeEino, Mode: agentruntime.RuntimeModeDeepAgent,
			},
			maxTurns: maxEinoDeepAgentIterations,
		},
		{
			name: "Eino over limit",
			runtime: agentruntime.RuntimeSpec{
				Type: agentruntime.RuntimeTypeEino, Mode: agentruntime.RuntimeModeDeepAgent,
			},
			maxTurns: maxEinoDeepAgentIterations + 1, wantInvalid: true,
		},
		{
			name: "auto can select Eino",
			runtime: agentruntime.RuntimeSpec{
				Type: agentruntime.RuntimeTypeAuto, Preferred: agentruntime.RuntimeTypeEino, Mode: agentruntime.RuntimeModeDeepAgent,
			},
			maxTurns: maxEinoDeepAgentIterations + 1, wantInvalid: true,
		},
		{
			name: "non-Eino runtime owns its limit",
			runtime: agentruntime.RuntimeSpec{
				Type: agentruntime.RuntimeTypeMock, Mode: agentruntime.RuntimeModeDeepAgent,
			},
			maxTurns: maxEinoDeepAgentIterations + 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := runtimePolicyTestConfig(tt.runtime, tt.maxTurns)
			err := ValidateAgentConfig(cfg)
			if tt.wantInvalid {
				var registryErr *RegistryError
				if !errors.As(err, &registryErr) || registryErr.Code != CodeInvalidConfig || registryErr.Field != "runtime.max_turns" {
					t.Fatalf("max_turns=%d error=%v", tt.maxTurns, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("max_turns=%d was rejected: %v", tt.maxTurns, err)
			}
			metadata := cfg.ToAgentDefinition().Metadata
			if tt.maxTurns == 0 {
				if _, exists := metadata["max_iterations"]; exists {
					t.Fatalf("zero max_turns must remain unset: %#v", metadata)
				}
			} else if metadata["max_iterations"] == "" {
				t.Fatalf("positive max_turns was not compiled: %#v", metadata)
			}
		})
	}
}

func TestRegisterAgentRejectsNegativeMaxTurns(t *testing.T) {
	cfg := runtimePolicyTestConfig(
		agentruntime.RuntimeSpec{Type: agentruntime.RuntimeTypeEino, Mode: agentruntime.RuntimeModeDeepAgent},
		-1,
	)
	_, err := NewService(WithLegacyUnresolvedPrompts()).RegisterAgent(context.Background(), cfg)
	var registryErr *RegistryError
	if !errors.As(err, &registryErr) || registryErr.Field != "runtime.max_turns" {
		t.Fatalf("registry accepted negative max_turns: %v", err)
	}
}

func TestRegistryFreezesReasoningOptionsIntoAgentDefinition(t *testing.T) {
	cfg := runtimePolicyTestConfig(agentruntime.RuntimeSpec{Type: agentruntime.RuntimeTypeEino, Mode: agentruntime.RuntimeModeDeepAgent}, 8)
	cfg.Model.ReasoningMode = agentruntime.ModelReasoningEnabled
	cfg.Model.ReasoningBudget = 256
	cfg.Model.ReasoningEffort = "low"
	temperature, topP, maxTokens := 0.0, 0.8, 512
	cfg.Model.Temperature = &temperature
	cfg.Model.TopP = &topP
	cfg.Model.MaxTokens = &maxTokens
	cfg.Model.Stop = []string{"END"}
	cfg.Model.ToolChoice = "auto"
	definition := cfg.ToAgentDefinition()
	if definition.ModelOptions.ReasoningMode != agentruntime.ModelReasoningEnabled || definition.ModelOptions.ReasoningBudget != 256 || definition.ModelOptions.ReasoningEffort != "low" || definition.ModelOptions.Temperature == nil || *definition.ModelOptions.Temperature != 0 || definition.ModelOptions.TopP == nil || *definition.ModelOptions.TopP != 0.8 || definition.ModelOptions.MaxTokens == nil || *definition.ModelOptions.MaxTokens != 512 || definition.ModelOptions.ToolChoice != "auto" || len(definition.ModelOptions.Stop) != 1 {
		t.Fatalf("reasoning options were not frozen: %#v", definition.ModelOptions)
	}
}

func TestRegistryRejectsInvalidReasoningOptions(t *testing.T) {
	tests := []struct {
		name   string
		mode   agentruntime.ModelReasoningMode
		budget int
		effort string
	}{
		{name: "unknown mode", mode: "deep"},
		{name: "negative budget", mode: agentruntime.ModelReasoningEnabled, budget: -1},
		{name: "disabled with budget", mode: agentruntime.ModelReasoningDisabled, budget: 10},
		{name: "unknown effort", mode: agentruntime.ModelReasoningEnabled, effort: "extreme"},
		{name: "disabled with effort", mode: agentruntime.ModelReasoningDisabled, effort: "low"},
		{name: "auto with effort", mode: agentruntime.ModelReasoningAuto, effort: "low"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := runtimePolicyTestConfig(agentruntime.RuntimeSpec{Type: agentruntime.RuntimeTypeEino, Mode: agentruntime.RuntimeModeDeepAgent}, 8)
			cfg.Model.ReasoningMode, cfg.Model.ReasoningBudget, cfg.Model.ReasoningEffort = tt.mode, tt.budget, tt.effort
			if err := ValidateAgentConfig(cfg); err == nil {
				t.Fatalf("invalid reasoning config was accepted: %#v", cfg.Model)
			}
		})
	}
}

func TestRegistryRejectsInvalidGenerationOptions(t *testing.T) {
	negative, aboveOne, zeroTokens := -0.1, 1.1, 0
	tests := []struct {
		name   string
		mutate func(*ModelConfig)
	}{
		{name: "negative temperature", mutate: func(model *ModelConfig) { model.Temperature = &negative }},
		{name: "top p above one", mutate: func(model *ModelConfig) { model.TopP = &aboveOne }},
		{name: "zero max tokens", mutate: func(model *ModelConfig) { model.MaxTokens = &zeroTokens }},
		{name: "empty stop", mutate: func(model *ModelConfig) { model.Stop = []string{""} }},
		{name: "invalid tool choice", mutate: func(model *ModelConfig) { model.ToolChoice = "sometimes" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := runtimePolicyTestConfig(agentruntime.RuntimeSpec{Type: agentruntime.RuntimeTypeEino, Mode: agentruntime.RuntimeModeDeepAgent}, 8)
			test.mutate(&cfg.Model)
			if err := ValidateAgentConfig(cfg); err == nil {
				t.Fatal("invalid generation option was accepted")
			}
		})
	}
}

func runtimePolicyTestConfig(runtime agentruntime.RuntimeSpec, maxTurns int) AgentConfig {
	return AgentConfig{
		AgentID: "deep_agent", AgentType: "assistant", Version: "v1",
		Runtime: runtime, PromptRef: "prompt://deep/system",
		RuntimePolicy: RuntimeConfig{MaxTurns: maxTurns},
		Capability:    CapabilityConfig{ExecutionModes: []ExecutionMode{ExecutionModeDeepAgent}},
		Orchestration: OrchestrationConfig{DefaultMode: ExecutionModeDeepAgent},
	}
}
