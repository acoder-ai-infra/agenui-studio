package agentregistry

import (
	"strings"
	"testing"
)

func TestValidateAgentConfigMatchesSQLIdentityLimits(t *testing.T) {
	t.Parallel()
	maxLength := extendedConfig(strings.Repeat("界", maxAgentIDRunes), strings.Repeat("界", maxRegistryIdentityRunes))
	maxLength.AgentType = strings.Repeat("界", maxRegistryIdentityRunes)
	if err := ValidateAgentConfig(maxLength); err != nil {
		t.Fatalf("ValidateAgentConfig() rejected SQL boundary identity: %v", err)
	}

	tests := map[string]func(*AgentConfig){
		"agent_id":   func(cfg *AgentConfig) { cfg.AgentID = strings.Repeat("界", maxAgentIDRunes+1) },
		"agent_type": func(cfg *AgentConfig) { cfg.AgentType = strings.Repeat("界", maxRegistryIdentityRunes+1) },
		"version":    func(cfg *AgentConfig) { cfg.Version = strings.Repeat("界", maxRegistryIdentityRunes+1) },
	}
	for field, mutate := range tests {
		field, mutate := field, mutate
		t.Run(field+"_too_long", func(t *testing.T) {
			t.Parallel()
			cfg := extendedConfig("agent", "v1")
			mutate(&cfg)
			assertRegistryError(t, ValidateAgentConfig(cfg), CodeInvalidConfig, field)
		})
	}
}

func TestValidateAgentConfigRejectsInvalidUTF8Identity(t *testing.T) {
	t.Parallel()

	cfg := extendedConfig("agent", "v1")
	cfg.AgentID = string([]byte{0xff})
	assertRegistryError(t, ValidateAgentConfig(cfg), CodeInvalidConfig, "agent_id")
}

func TestConfigControlRejectsAgentIDBeyondLedgerLimit(t *testing.T) {
	t.Parallel()
	if err := validateTenantAgentScope("tenant", strings.Repeat("代", maxAgentIDRunes)); err != nil {
		t.Fatalf("validateTenantAgentScope() rejected AgentID boundary: %v", err)
	}
	if err := validateTenantAgentScope("tenant", strings.Repeat("代", maxAgentIDRunes+1)); err == nil {
		t.Fatal("validateTenantAgentScope() accepted AgentID beyond Ledger limit")
	}
}

func TestValidateAgentConfigRejectsControlCharactersInIdentity(t *testing.T) {
	t.Parallel()

	for field, mutate := range map[string]func(*AgentConfig){
		"agent_id":   func(cfg *AgentConfig) { cfg.AgentID = "agent\x1fid" },
		"agent_type": func(cfg *AgentConfig) { cfg.AgentType = "agent\x00type" },
		"version":    func(cfg *AgentConfig) { cfg.Version = "v1\n" },
	} {
		field, mutate := field, mutate
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			cfg := extendedConfig("agent", "v1")
			mutate(&cfg)
			assertRegistryError(t, ValidateAgentConfig(cfg), CodeInvalidConfig, field)
		})
	}
}
