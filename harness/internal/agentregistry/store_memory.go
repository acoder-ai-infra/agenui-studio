package agentregistry

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
)

// StoreRecord 是可持久化的 Registry 记录，供内存和共享 SQL backend 复用。
type StoreRecord struct {
	Config    AgentConfig
	Card      CapabilityCard
	Effective EffectiveConfig
	// ConfigSnapshots 只属于 Create/Replace 写入聚合；读取统一走快照接口，避免 backend 返回形态不一致。
	ConfigSnapshots []ConfigSnapshotRecord
	RegisteredAt    time.Time
	ActivatedAtMS   int64
	GrayPercent     int
	Revision        int64
}

type compiledAgent = StoreRecord

// ConfigSnapshotRecord 是按 execution mode 冻结、可按引用恢复的最终配置。
type ConfigSnapshotRecord struct {
	Ref           string
	AgentID       string
	Version       string
	ExecutionMode ExecutionMode
	ConfigHash    string
	Effective     EffectiveConfig
	CreatedAt     time.Time
}

type configSnapshotModeKey struct {
	agentID string
	version string
	mode    ExecutionMode
}

// MemoryStore 是确定性的本地 backend，不应作为多实例线上事实源。
type MemoryStore struct {
	mu              sync.RWMutex
	agents          map[string]map[string]*StoreRecord
	snapshotsByRef  map[string]*ConfigSnapshotRecord
	snapshotsByMode map[configSnapshotModeKey]string
	auditEvents     []RegistryEvent
	auditIDs        map[string]struct{}
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		agents:          make(map[string]map[string]*StoreRecord),
		snapshotsByRef:  make(map[string]*ConfigSnapshotRecord),
		snapshotsByMode: make(map[configSnapshotModeKey]string),
		auditIDs:        make(map[string]struct{}),
	}
}

func (s *MemoryStore) Create(ctx context.Context, agent *StoreRecord, audit RegistryEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if agent == nil {
		return ErrAgentNotFound
	}
	if err := validateRegistryEvent(audit); err != nil {
		return err
	}
	copy := cloneCompiledAgent(agent)
	if err := validateConfigSnapshots(copy); err != nil {
		return err
	}
	if copy.Revision == 0 {
		copy.Revision = 1
	}
	if copy.ActivatedAtMS == 0 && copy.Config.Status == AgentStatusEnabled {
		copy.ActivatedAtMS = copy.RegisteredAt.UnixMilli()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	versions := s.agents[copy.Config.AgentID]
	if versions != nil && versions[copy.Config.Version] != nil {
		return ErrDuplicateAgent
	}
	if s.hasAgentTypeVersion(copy.Config.AgentType, copy.Config.Version) {
		return ErrDuplicateAgent
	}
	if _, exists := s.auditIDs[audit.EventID]; exists {
		return ErrAuditWrite
	}
	for i := range copy.ConfigSnapshots {
		snapshot := &copy.ConfigSnapshots[i]
		if _, exists := s.snapshotsByRef[snapshot.Ref]; exists {
			return ErrStoreConflict
		}
		key := configSnapshotModeKey{agentID: snapshot.AgentID, version: snapshot.Version, mode: snapshot.ExecutionMode}
		if _, exists := s.snapshotsByMode[key]; exists {
			return ErrStoreConflict
		}
	}
	if versions == nil {
		versions = make(map[string]*StoreRecord)
		s.agents[copy.Config.AgentID] = versions
	}
	versions[copy.Config.Version] = copy
	for i := range copy.ConfigSnapshots {
		snapshot := cloneConfigSnapshotRecord(&copy.ConfigSnapshots[i])
		s.snapshotsByRef[snapshot.Ref] = snapshot
		key := configSnapshotModeKey{agentID: snapshot.AgentID, version: snapshot.Version, mode: snapshot.ExecutionMode}
		s.snapshotsByMode[key] = snapshot.Ref
	}
	s.appendAuditLocked(audit)
	return nil
}

func (s *MemoryStore) Get(ctx context.Context, lookup StoreLookup) (*StoreRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if lookup.AgentID == "" && lookup.AgentType == "" {
		return nil, ErrAgentNotFound
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var selected *StoreRecord
	for _, versions := range s.agents {
		for _, candidate := range versions {
			if lookup.AgentID != "" && candidate.Config.AgentID != lookup.AgentID {
				continue
			}
			if lookup.AgentType != "" && candidate.Config.AgentType != lookup.AgentType {
				continue
			}
			if lookup.Version != "" && candidate.Config.Version != lookup.Version {
				continue
			}
			if lookup.Version == "" && candidate.Config.Status != AgentStatusEnabled {
				continue
			}
			if selected == nil || newerStoreRecord(candidate, selected) {
				selected = candidate
			}
		}
	}
	if selected == nil {
		return nil, ErrAgentNotFound
	}
	return cloneStoreReadRecord(selected), nil
}

func (s *MemoryStore) GetConfigSnapshot(ctx context.Context, ref string) (*ConfigSnapshotRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot := s.snapshotsByRef[ref]
	if snapshot == nil {
		return nil, ErrConfigSnapshotMissing
	}
	agent := s.agents[snapshot.AgentID][snapshot.Version]
	if agent == nil {
		return nil, fmt.Errorf("%w: config snapshot has no registry entry", ErrStoreCorrupt)
	}
	if err := validateConfigSnapshotForAgent(snapshot, agent.Config); err != nil {
		return nil, err
	}
	return cloneConfigSnapshotRecord(snapshot), nil
}

func (s *MemoryStore) GetConfigSnapshotByMode(ctx context.Context, ref AgentRef, mode ExecutionMode) (*ConfigSnapshotRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := configSnapshotModeKey{agentID: ref.AgentID, version: ref.Version, mode: mode}
	snapshot := s.snapshotsByRef[s.snapshotsByMode[key]]
	if snapshot == nil {
		return nil, ErrConfigSnapshotMissing
	}
	agent := s.agents[ref.AgentID][ref.Version]
	if agent == nil {
		return nil, fmt.Errorf("%w: config snapshot has no registry entry", ErrStoreCorrupt)
	}
	if err := validateConfigSnapshotForAgent(snapshot, agent.Config); err != nil {
		return nil, err
	}
	return cloneConfigSnapshotRecord(snapshot), nil
}

func (s *MemoryStore) List(ctx context.Context) ([]*StoreRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*StoreRecord, 0)
	for _, versions := range s.agents {
		for _, agent := range versions {
			out = append(out, cloneStoreReadRecord(agent))
		}
	}
	sortCompiledAgents(out)
	return out, nil
}

func (s *MemoryStore) Replace(ctx context.Context, agents []*StoreRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	newAgents := make(map[string]map[string]*StoreRecord)
	newSnapshotsByRef := make(map[string]*ConfigSnapshotRecord)
	newSnapshotsByMode := make(map[configSnapshotModeKey]string)
	for _, input := range agents {
		if input == nil {
			continue
		}
		agent := cloneCompiledAgent(input)
		versions := newAgents[agent.Config.AgentID]
		if versions == nil {
			versions = make(map[string]*StoreRecord)
			newAgents[agent.Config.AgentID] = versions
		}
		if versions[agent.Config.Version] != nil {
			return ErrDuplicateAgent
		}
		for _, existingVersions := range newAgents {
			for _, existing := range existingVersions {
				if existing.Config.AgentType == agent.Config.AgentType && existing.Config.Version == agent.Config.Version {
					return ErrDuplicateAgent
				}
			}
		}
		if err := validateConfigSnapshots(agent); err != nil {
			return err
		}
		if agent.Revision == 0 {
			agent.Revision = 1
		}
		if agent.ActivatedAtMS == 0 && agent.Config.Status == AgentStatusEnabled {
			agent.ActivatedAtMS = agent.RegisteredAt.UnixMilli()
		}
		versions[agent.Config.Version] = agent
		for i := range agent.ConfigSnapshots {
			snapshot := cloneConfigSnapshotRecord(&agent.ConfigSnapshots[i])
			if newSnapshotsByRef[snapshot.Ref] != nil {
				return ErrStoreConflict
			}
			key := configSnapshotModeKey{agentID: snapshot.AgentID, version: snapshot.Version, mode: snapshot.ExecutionMode}
			if _, exists := newSnapshotsByMode[key]; exists {
				return ErrStoreConflict
			}
			newSnapshotsByRef[snapshot.Ref] = snapshot
			newSnapshotsByMode[key] = snapshot.Ref
		}
	}
	s.mu.Lock()
	s.agents = newAgents
	s.snapshotsByRef = newSnapshotsByRef
	s.snapshotsByMode = newSnapshotsByMode
	s.mu.Unlock()
	return nil
}

func (s *MemoryStore) UpdateStatus(ctx context.Context, ref AgentRef, status AgentStatus, activatedAtMS int64, audit RegistryEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validAgentStatus(status) {
		return fmt.Errorf("%w: invalid agent status %q", ErrStoreCorrupt, status)
	}
	if err := validateRegistryEvent(audit); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	agent := s.agent(ref)
	if agent == nil {
		return ErrAgentNotFound
	}
	if _, exists := s.auditIDs[audit.EventID]; exists {
		return ErrAuditWrite
	}
	updated := cloneCompiledAgent(agent)
	updated.Config.Status = status
	updated.Card.Status = status
	if status == AgentStatusEnabled {
		updated.ActivatedAtMS = activatedAtMS
	}
	updated.Revision++
	cardHash, err := computeCardHash(updated.Card)
	if err != nil {
		return err
	}
	updated.Card.CardHash = cardHash
	s.agents[ref.AgentID][ref.Version] = updated
	s.appendAuditLocked(audit)
	return nil
}

func (s *MemoryStore) UpdateGrayPercent(ctx context.Context, ref AgentRef, percent int, audit RegistryEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if percent < 0 || percent > 100 {
		return fmt.Errorf("%w: gray percent must be between 0 and 100", ErrStoreCorrupt)
	}
	if err := validateRegistryEvent(audit); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	agent := s.agent(ref)
	if agent == nil {
		return ErrAgentNotFound
	}
	if _, exists := s.auditIDs[audit.EventID]; exists {
		return ErrAuditWrite
	}
	updated := cloneCompiledAgent(agent)
	updated.GrayPercent = percent
	updated.Config.Release.GrayPercent = percent
	updated.Revision++
	s.agents[ref.AgentID][ref.Version] = updated
	s.appendAuditLocked(audit)
	return nil
}

func (s *MemoryStore) AppendRegistryEvent(ctx context.Context, event RegistryEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateRegistryEvent(event); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.auditIDs[event.EventID]; exists {
		return ErrAuditWrite
	}
	s.appendAuditLocked(event)
	return nil
}

func (s *MemoryStore) ListVersions(ctx context.Context, agentID string) ([]AgentVersion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	versions := make([]AgentVersion, 0, len(s.agents[agentID]))
	for _, agent := range s.agents[agentID] {
		versions = append(versions, AgentVersion{
			AgentID: agent.Config.AgentID, AgentType: agent.Config.AgentType, Version: agent.Config.Version,
			Status: agent.Config.Status, ConfigHash: agent.Effective.ConfigHash, RegisteredAt: agent.RegisteredAt,
			GrayPercent: agent.GrayPercent, Revision: agent.Revision,
		})
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].Version < versions[j].Version })
	return versions, nil
}

func (s *MemoryStore) agent(ref AgentRef) *compiledAgent {
	if s.agents[ref.AgentID] == nil {
		return nil
	}
	return s.agents[ref.AgentID][ref.Version]
}

func (s *MemoryStore) hasAgentTypeVersion(agentType, version string) bool {
	for _, versions := range s.agents {
		for _, agent := range versions {
			if agent.Config.AgentType == agentType && agent.Config.Version == version {
				return true
			}
		}
	}
	return false
}

func (s *MemoryStore) appendAuditLocked(event RegistryEvent) {
	s.auditEvents = append(s.auditEvents, event)
	s.auditIDs[event.EventID] = struct{}{}
}

func newerStoreRecord(candidate, current *StoreRecord) bool {
	if candidate.ActivatedAtMS != current.ActivatedAtMS {
		return candidate.ActivatedAtMS > current.ActivatedAtMS
	}
	if !candidate.RegisteredAt.Equal(current.RegisteredAt) {
		return candidate.RegisteredAt.After(current.RegisteredAt)
	}
	if candidate.Config.Version != current.Config.Version {
		return candidate.Config.Version > current.Config.Version
	}
	return candidate.Config.AgentID > current.Config.AgentID
}

func sortCompiledAgents(agents []*StoreRecord) {
	sort.Slice(agents, func(i, j int) bool {
		if agents[i].Config.AgentID == agents[j].Config.AgentID {
			return agents[i].Config.Version < agents[j].Config.Version
		}
		return agents[i].Config.AgentID < agents[j].Config.AgentID
	})
}

func cloneCompiledAgent(agent *StoreRecord) *StoreRecord {
	if agent == nil {
		return nil
	}
	copy := *agent
	copy.Config = cloneAgentConfig(agent.Config)
	copy.Card = cloneCapabilityCard(agent.Card)
	copy.Effective = cloneEffectiveConfig(agent.Effective)
	copy.ConfigSnapshots = make([]ConfigSnapshotRecord, len(agent.ConfigSnapshots))
	for i := range agent.ConfigSnapshots {
		copy.ConfigSnapshots[i] = *cloneConfigSnapshotRecord(&agent.ConfigSnapshots[i])
	}
	return &copy
}

func cloneStoreReadRecord(agent *StoreRecord) *StoreRecord {
	copy := cloneCompiledAgent(agent)
	if copy != nil {
		copy.ConfigSnapshots = nil
	}
	return copy
}

func cloneConfigSnapshotRecord(snapshot *ConfigSnapshotRecord) *ConfigSnapshotRecord {
	if snapshot == nil {
		return nil
	}
	copy := *snapshot
	copy.Effective = cloneEffectiveConfig(snapshot.Effective)
	return &copy
}

func cloneConfigSnapshotRecords(input []ConfigSnapshotRecord) []ConfigSnapshotRecord {
	out := make([]ConfigSnapshotRecord, len(input))
	for i := range input {
		out[i] = *cloneConfigSnapshotRecord(&input[i])
	}
	return out
}

func cloneAgentConfig(cfg AgentConfig) AgentConfig {
	cfg = normalizeConfig(cfg)
	cfg.Metadata = cloneStringMap(cfg.Metadata)
	cfg.Runtime.Candidates = append([]agentruntime.RuntimeType(nil), cfg.Runtime.Candidates...)
	return cfg
}

func cloneCapabilityCard(card CapabilityCard) CapabilityCard {
	card.Intents = append([]string(nil), card.Intents...)
	card.ExecutionModes = append([]ExecutionMode(nil), card.ExecutionModes...)
	card.Protocols = append([]string(nil), card.Protocols...)
	card.Tags = append([]string(nil), card.Tags...)
	return card
}

func cloneEffectiveConfig(effective EffectiveConfig) EffectiveConfig {
	effective.Gateway = cloneEffectiveGatewayTarget(effective.Gateway)
	effective.Definition.Workflow = cloneWorkflowDefinition(effective.Definition.Workflow)
	effective.Definition.Graph = cloneGraphDefinition(effective.Definition.Graph)
	effective.Definition.DataPassing.ArtifactKeys = append([]string(nil), effective.Definition.DataPassing.ArtifactKeys...)
	effective.Definition.DataPassing.ScopedDataKeys = append([]string(nil), effective.Definition.DataPassing.ScopedDataKeys...)
	effective.Definition.ToolRefs = append([]string(nil), effective.Definition.ToolRefs...)
	effective.Definition.SubAgentRefs = append([]string(nil), effective.Definition.SubAgentRefs...)
	effective.Definition.RequiredCapabilities = append([]string(nil), effective.Definition.RequiredCapabilities...)
	effective.Definition.Runtime.Candidates = append([]agentruntime.RuntimeType(nil), effective.Definition.Runtime.Candidates...)
	effective.Definition.Metadata = cloneStringMap(effective.Definition.Metadata)
	effective.ResolvedDeps = normalizeDeps(effective.ResolvedDeps)
	effective.SourceVersionIDs = append([]string(nil), effective.SourceVersionIDs...)
	return effective
}

var _ Store = (*MemoryStore)(nil)
var _ SnapshotStore = (*MemoryStore)(nil)
