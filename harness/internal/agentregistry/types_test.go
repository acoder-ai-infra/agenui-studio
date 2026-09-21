package agentregistry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
)

func TestInMemoryRegistryResolveEffectiveConfig(t *testing.T) {
	registry, err := NewInMemoryRegistry(validConfig())
	if err != nil {
		t.Fatalf("registry init failed: %v", err)
	}

	effective, err := registry.ResolveEffectiveConfig(context.Background(), ResolveRequest{AgentID: "agent_1", Version: "v1"})
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if effective.Definition.AgentID != "agent_1" || effective.Definition.PromptRef != "prompt://agent_1/system" {
		t.Fatalf("unexpected definition: %#v", effective.Definition)
	}
	if effective.ConfigHash == "" || effective.ConfigSnapshotRef == "" {
		t.Fatalf("snapshot metadata missing: %#v", effective)
	}
	if got := effective.Definition.ToolRefs; len(got) != 1 || got[0] != "search@v1" {
		t.Fatalf("tool refs not compiled: %#v", got)
	}
	if effective.Definition.ContextCompaction.PolicyHash == "" || effective.Definition.ContextCompaction.PreserveRecentTurns != 2 {
		t.Fatalf("default context compaction policy not frozen: %#v", effective.Definition.ContextCompaction)
	}
}

func TestInMemoryRegistryRejectsInvalidContextCompactionPolicy(t *testing.T) {
	cfg := validConfig()
	cfg.ContextCompaction = agentruntime.DefaultContextCompactionPolicy()
	cfg.ContextCompaction.PolicyHash = "tampered"
	if _, err := NewInMemoryRegistry(cfg); !errors.Is(err, agentruntime.ErrContextCompactionPolicyInvalid) {
		t.Fatalf("expected invalid compaction policy, got %v", err)
	}
}

func TestContextCompactionPolicyParticipatesInConfigHash(t *testing.T) {
	base, err := compileEffectiveConfig(validConfig(), ResolvedDependencies{}, time.Unix(1, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	changedConfig := validConfig()
	changedConfig.ContextCompaction = agentruntime.DefaultContextCompactionPolicy()
	changedConfig.ContextCompaction.PreserveRecentTurns = 5
	changedConfig.ContextCompaction.PolicyHash = ""
	changed, err := compileEffectiveConfig(changedConfig, ResolvedDependencies{}, time.Unix(1, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if base.ConfigHash == changed.ConfigHash {
		t.Fatalf("config hash ignored context policy: %s", base.ConfigHash)
	}
}

func TestHITLToolsParticipateInPolicyHash(t *testing.T) {
	baseConfig := validConfig()
	base, err := compileEffectiveConfig(baseConfig, ResolvedDependencies{}, time.Unix(1, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	changedConfig := validConfig()
	changedConfig.ToolPolicy.HITLRequiredTools = []string{"search@v1"}
	changed, err := compileEffectiveConfig(changedConfig, ResolvedDependencies{}, time.Unix(1, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if base.PolicyHash == changed.PolicyHash {
		t.Fatalf("policy hash ignored HITL tools: %s", base.PolicyHash)
	}
}

func TestInMemoryRegistryRejectsInvalidConfig(t *testing.T) {
	cfg := validConfig()
	cfg.PromptRef = ""
	if _, err := NewInMemoryRegistry(cfg); !errors.Is(err, ErrPromptRefRequired) {
		t.Fatalf("expected ErrPromptRefRequired, got %v", err)
	}
}

func TestInMemoryRegistryRejectsDuplicateAgentVersion(t *testing.T) {
	_, err := NewInMemoryRegistry(validConfig(), validConfig())
	if !errors.Is(err, ErrDuplicateAgent) {
		t.Fatalf("expected duplicate error, got %v", err)
	}
}

func TestInMemoryRegistryDisabledAgentFailsClosed(t *testing.T) {
	cfg := validConfig()
	cfg.Status = AgentStatusDisabled
	registry, err := NewInMemoryRegistry(cfg)
	if err != nil {
		t.Fatalf("registry init failed: %v", err)
	}
	if _, err := registry.ResolveEffectiveConfig(context.Background(), ResolveRequest{AgentID: "agent_1", Version: "v1"}); !errors.Is(err, ErrAgentDisabled) {
		t.Fatalf("expected ErrAgentDisabled, got %v", err)
	}
}

func TestInMemoryRegistryCapabilityCardUsesSameHash(t *testing.T) {
	registry, err := NewInMemoryRegistry(validConfig())
	if err != nil {
		t.Fatalf("registry init failed: %v", err)
	}
	effective, err := registry.ResolveEffectiveConfig(context.Background(), ResolveRequest{AgentID: "agent_1", Version: "v1"})
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	card, err := registry.GetCapabilityCard(context.Background(), "agent_1", "v1")
	if err != nil {
		t.Fatalf("card failed: %v", err)
	}
	if card.ConfigHash != effective.ConfigHash {
		t.Fatalf("card and effective config hash drift: card=%s effective=%s", card.ConfigHash, effective.ConfigHash)
	}
}

func validConfig() AgentConfig {
	return AgentConfig{
		AgentID:        "agent_1",
		AgentType:      "assistant",
		Version:        "v1",
		Status:         AgentStatusEnabled,
		Runtime:        agentruntime.RuntimeSpec{Type: agentruntime.RuntimeTypeMock, Mode: agentruntime.RuntimeModeReact},
		PromptRef:      "prompt://agent_1/system",
		Tools:          []VersionedRef{{Name: "search", Version: "v1"}},
		SubAgents:      []string{"sub_agent_1"},
		ProtocolPolicy: ProtocolPolicy{Streaming: true, OutputFormat: "markdown"},
	}
}
