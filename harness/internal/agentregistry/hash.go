package agentregistry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
)

func computeEffectiveConfigHash(cfg EffectiveConfig, source AgentConfig) (string, error) {
	payload := struct {
		Definition       agentruntime.AgentDefinition `json:"definition"`
		ExecutionConfig  AgentConfig                  `json:"execution_config"`
		Gateway          *EffectiveGatewayTarget      `json:"gateway,omitempty"`
		PromptHash       string                       `json:"prompt_hash,omitempty"`
		SchemaHash       string                       `json:"schema_hash,omitempty"`
		PolicyHash       string                       `json:"policy_hash,omitempty"`
		ResolvedDeps     ResolvedDependencies         `json:"resolved_deps,omitempty"`
		SourceVersionIDs []string                     `json:"source_version_ids,omitempty"`
	}{
		Definition:       cfg.Definition,
		ExecutionConfig:  executionConfigForHash(source),
		Gateway:          cloneEffectiveGatewayTarget(cfg.Gateway),
		PromptHash:       cfg.PromptHash,
		SchemaHash:       cfg.SchemaHash,
		PolicyHash:       cfg.PolicyHash,
		ResolvedDeps:     cfg.ResolvedDeps,
		SourceVersionIDs: cfg.SourceVersionIDs,
	}
	return hashJSON("effective_config", payload)
}

func executionConfigForHash(cfg AgentConfig) AgentConfig {
	cfg = normalizeConfig(cfg)
	// 发布状态和展示字段由 CapabilityCard / lifecycle 管理，不改变一次 Run 的执行语义。
	cfg.Status = ""
	cfg.Name = ""
	cfg.Description = ""
	cfg.Owner = ""
	cfg.Tags = nil
	cfg.Capability.Intents = nil
	cfg.Release = ReleaseConfig{}
	// Gateway author config is replaced by the normalized, frozen effective
	// target above, so provider/remote config participates exactly once.
	cfg.Gateway = nil
	return cfg
}

func computeCardHash(card CapabilityCard) (string, error) {
	card.ConfigHash = ""
	card.CardHash = ""
	return hashJSON("capability_card", card)
}

func hashJSON(field string, value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", wrapError(CodeInvalidConfig, field, "marshal canonical value", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
