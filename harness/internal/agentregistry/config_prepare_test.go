package agentregistry

import (
	"context"
	"testing"
)

func TestPrepareAgentRunsReleaseGatesWithoutRegistering(t *testing.T) {
	service := newLegacyTestService()
	cfg := extendedConfig("prepared_agent", "v1")

	prepared, err := service.PrepareAgent(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Effective.Definition.AgentID != cfg.AgentID || prepared.Effective.ConfigSnapshotRef == "" {
		t.Fatalf("prepared effective config is incomplete: %#v", prepared.Effective)
	}
	if len(prepared.ConfigSnapshots) == 0 || prepared.Card.AgentID != cfg.AgentID {
		t.Fatalf("prepared projections are incomplete: %#v", prepared)
	}
	if _, err := service.ResolveEffectiveConfig(context.Background(), ResolveRequest{AgentID: cfg.AgentID}); err == nil {
		t.Fatal("PrepareAgent must not register the Agent")
	}
}
