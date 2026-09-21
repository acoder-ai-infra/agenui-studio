package agentregistry

import (
	"fmt"

	"github.com/AGenUI/agenui-studio/harness/internal/executionmode"
)

func validateRegistryEvent(event RegistryEvent) error {
	if event.EventID == "" || event.Type == "" || event.Timestamp.IsZero() {
		return fmt.Errorf("%w: event_id, event_type and timestamp are required", ErrAuditWrite)
	}
	return nil
}

func validateConfigSnapshots(agent *StoreRecord) error {
	if agent == nil || len(agent.ConfigSnapshots) == 0 {
		return ErrConfigSnapshotMissing
	}
	if !validAgentStatus(agent.Config.Status) || agent.Card.Status != agent.Config.Status {
		return fmt.Errorf("%w: invalid registry status projection", ErrStoreCorrupt)
	}
	if agent.GrayPercent < 0 || agent.GrayPercent > 100 || agent.Config.Release.GrayPercent != agent.GrayPercent {
		return fmt.Errorf("%w: invalid registry gray projection", ErrStoreCorrupt)
	}
	if agent.Card.AgentID != agent.Config.AgentID || agent.Card.AgentType != agent.Config.AgentType ||
		agent.Card.Version != agent.Config.Version || agent.Card.ConfigHash != agent.Effective.ConfigHash {
		return fmt.Errorf("%w: invalid capability card projection", ErrStoreCorrupt)
	}
	cardHash, err := computeCardHash(agent.Card)
	if err != nil || cardHash != agent.Card.CardHash {
		return fmt.Errorf("%w: invalid capability card hash", ErrStoreCorrupt)
	}
	refs := make(map[string]struct{}, len(agent.ConfigSnapshots))
	modes := make(map[ExecutionMode]struct{}, len(agent.ConfigSnapshots))
	var defaultSnapshot *ConfigSnapshotRecord
	for i := range agent.ConfigSnapshots {
		snapshot := &agent.ConfigSnapshots[i]
		if err := validateConfigSnapshotForAgent(snapshot, agent.Config); err != nil {
			return err
		}
		if _, exists := refs[snapshot.Ref]; exists {
			return fmt.Errorf("%w: duplicate config snapshot ref %q", ErrStoreCorrupt, snapshot.Ref)
		}
		if _, exists := modes[snapshot.ExecutionMode]; exists {
			return fmt.Errorf("%w: duplicate config snapshot mode %q", ErrStoreCorrupt, snapshot.ExecutionMode)
		}
		refs[snapshot.Ref] = struct{}{}
		modes[snapshot.ExecutionMode] = struct{}{}
		if snapshot.ExecutionMode == agent.Config.Orchestration.DefaultMode {
			defaultSnapshot = snapshot
		}
	}
	defaultMode := agent.Config.Orchestration.DefaultMode
	if _, exists := modes[defaultMode]; !exists || defaultSnapshot == nil {
		return fmt.Errorf("%w: default execution mode snapshot is missing", ErrConfigSnapshotMissing)
	}
	if len(modes) != len(agent.Config.Capability.ExecutionModes) {
		return fmt.Errorf("%w: config snapshot modes do not match registered capabilities", ErrConfigSnapshotMissing)
	}
	for _, mode := range agent.Config.Capability.ExecutionModes {
		if _, exists := modes[mode]; !exists {
			return fmt.Errorf("%w: config snapshot for mode %q is missing", ErrConfigSnapshotMissing, mode)
		}
	}
	if agent.Effective.ConfigSnapshotRef != defaultSnapshot.Ref || agent.Effective.ConfigHash != defaultSnapshot.ConfigHash {
		return fmt.Errorf("%w: default config snapshot projection mismatch", ErrStoreCorrupt)
	}
	return nil
}

func validAgentStatus(status AgentStatus) bool {
	return status == AgentStatusEnabled || status == AgentStatusDisabled
}

func validateConfigSnapshotRecord(snapshot *ConfigSnapshotRecord) error {
	if snapshot == nil || snapshot.Ref == "" || snapshot.AgentID == "" || snapshot.Version == "" || snapshot.ConfigHash == "" {
		return ErrConfigSnapshotMissing
	}
	if snapshot.Ref != configSnapshotRef(snapshot.AgentID, snapshot.Version, snapshot.ConfigHash) {
		return fmt.Errorf("%w: config snapshot ref does not match its content hash", ErrStoreCorrupt)
	}
	if err := snapshot.ExecutionMode.Validate(); err != nil {
		return fmt.Errorf("%w: invalid snapshot execution mode: %v", ErrStoreCorrupt, err)
	}
	if snapshot.CreatedAt.IsZero() || snapshot.CreatedAt.UnixMilli() < 0 {
		return fmt.Errorf("%w: config snapshot created_at is required", ErrStoreCorrupt)
	}
	effective := snapshot.Effective
	if effective.ConfigSnapshotRef != snapshot.Ref || effective.ConfigHash != snapshot.ConfigHash ||
		effective.Definition.AgentID != snapshot.AgentID || effective.Definition.Version != snapshot.Version {
		return fmt.Errorf("%w: config snapshot projection mismatch", ErrStoreCorrupt)
	}
	if effective.EffectiveAt.IsZero() || effective.EffectiveAt.UnixMilli() != snapshot.CreatedAt.UnixMilli() {
		return fmt.Errorf("%w: config snapshot timestamp mismatch", ErrStoreCorrupt)
	}
	mode, err := executionmode.FromRuntimeMode(effective.Definition.Runtime.Mode)
	if err != nil || mode != snapshot.ExecutionMode {
		return fmt.Errorf("%w: config snapshot runtime mode mismatch", ErrStoreCorrupt)
	}
	return nil
}

func validateConfigSnapshotForAgent(snapshot *ConfigSnapshotRecord, cfg AgentConfig) error {
	if err := validateConfigSnapshotRecord(snapshot); err != nil {
		return err
	}
	cfg = normalizeConfig(cfg)
	if snapshot.AgentID != cfg.AgentID || snapshot.Version != cfg.Version {
		return fmt.Errorf("%w: snapshot identity does not match registry entry", ErrStoreCorrupt)
	}
	hash, err := computeEffectiveConfigHash(snapshot.Effective, cfg)
	if err != nil || hash != snapshot.ConfigHash {
		return fmt.Errorf("%w: config snapshot payload hash mismatch", ErrStoreCorrupt)
	}
	return nil
}
