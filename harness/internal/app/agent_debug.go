package app

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/AGenUI/agenui-studio/harness/internal/agentregistry"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// AgentSummary is the lightweight per-agent entry listed by
// GET /api/v1/debug/agents (the debug console's agent catalog).
type AgentSummary struct {
	AgentID        string   `json:"agent_id"`
	AgentType      string   `json:"agent_type,omitempty"`
	Version        string   `json:"version,omitempty"`
	Status         string   `json:"status,omitempty"`
	Description    string   `json:"description,omitempty"`
	ExecutionModes []string `json:"execution_modes,omitempty"`
	Tags           []string `json:"tags,omitempty"`
}

// agentDebugConfig is the per-agent detail served by GET /api/v1/debug/agents/{id}.
// It pairs the capability card with the resolved effective config so the console
// can render prompt ref, tools, sub-agents, skills and model in one payload.
type agentDebugConfig struct {
	Card      agentregistry.CapabilityCard  `json:"card"`
	Effective agentregistry.EffectiveConfig `json:"effective"`
}

// buildAgentDebugCatalog resolves every enabled agent's effective config at
// startup into read-only JSON for the debug console. registry is nil in the
// fallback (non-harness) composition — that yields an empty catalog, so the
// endpoints simply report no agents rather than failing.
func buildAgentDebugCatalog(ctx context.Context, registry *agentregistry.Service, logger observability.StructuredLogger) (json.RawMessage, map[string]json.RawMessage) {
	if registry == nil {
		return nil, nil
	}
	list, err := registry.ListCapabilityCards(ctx, agentregistry.ListCapabilityCardsRequest{})
	if err != nil {
		logger.Info(ctx, "agent debug catalog not built", observability.String("error", err.Error()))
		return nil, nil
	}
	summaries := make([]AgentSummary, 0, len(list.Cards))
	configs := make(map[string]json.RawMessage, len(list.Cards))
	for _, card := range list.Cards {
		modes := make([]string, 0, len(card.ExecutionModes))
		for _, m := range card.ExecutionModes {
			modes = append(modes, string(m))
		}
		summaries = append(summaries, AgentSummary{
			AgentID:        card.AgentID,
			AgentType:      card.AgentType,
			Version:        card.Version,
			Status:         string(card.Status),
			Description:    card.Description,
			ExecutionModes: modes,
			Tags:           card.Tags,
		})
		eff, resolveErr := registry.ResolveEffectiveConfig(ctx, agentregistry.ResolveRequest{AgentID: card.AgentID, Version: card.Version})
		if resolveErr != nil {
			logger.Info(ctx, "agent effective config resolve failed",
				observability.String("agent_id", card.AgentID), observability.String("error", resolveErr.Error()))
			continue
		}
		if raw, mErr := json.Marshal(agentDebugConfig{Card: card, Effective: eff}); mErr == nil {
			configs[card.AgentID] = raw
		}
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].AgentID < summaries[j].AgentID })
	list0, _ := json.Marshal(map[string]any{"agents": summaries})
	logger.Info(ctx, "agent debug catalog built", observability.Int("agents", len(configs)))
	return list0, configs
}
