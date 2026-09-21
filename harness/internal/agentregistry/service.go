package agentregistry

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type Service struct {
	store    Store
	loader   Loader
	resolver DependencyResolver
	prompts  PromptResolver
	// allowUnresolvedPrompts 只允许本地 MemoryStore/旧骨架显式开启。
	allowUnresolvedPrompts bool
	runtime                RuntimeValidator
	evalGate               EvalGate
	logger                 observability.StructuredLogger
	tracer                 observability.TraceProvider
	ids                    observability.IDGenerator
	now                    func() time.Time
}

type Option func(*Service)

func NewService(opts ...Option) *Service {
	s := &Service{
		store:    NewMemoryStore(),
		resolver: CatalogDependencyResolver{},
		runtime:  PassThroughRuntimeValidator{},
		evalGate: PassThroughEvalGate{},
		logger:   observability.NoopLogger{},
		tracer:   observability.NewNoopTracer("agentregistry"),
		ids:      observability.NewULIDGenerator("registry"),
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ValidateProduction 检查线上装配的不可省略边界。本地 NewService 继续保留
// MemoryStore 和轻量目录，只有显式进入生产装配时才执行这些门禁。
func (s *Service) ValidateProduction() error {
	if s == nil || s.store == nil {
		return ErrProductionMemoryStore
	}
	if _, memory := s.store.(*MemoryStore); memory {
		return ErrProductionMemoryStore
	}
	if s.prompts == nil {
		return ErrProductionPromptStore
	}
	if !hasToolCatalogResolver(s.resolver) {
		return ErrProductionToolCatalog
	}
	return nil
}

func WithStore(store Store) Option {
	return func(s *Service) {
		if store != nil {
			s.store = store
		}
	}
}

func WithLoader(loader Loader) Option { return func(s *Service) { s.loader = loader } }
func WithDependencyResolver(resolver DependencyResolver) Option {
	return func(s *Service) { s.resolver = resolver }
}
func WithPromptResolver(resolver PromptResolver) Option {
	return func(s *Service) {
		if resolver != nil {
			s.prompts = resolver
		}
	}
}

// WithLegacyUnresolvedPrompts 仅用于本地 demo 和兼容测试。线上注册必须注入 PromptResolver。
func WithLegacyUnresolvedPrompts() Option {
	return func(s *Service) { s.allowUnresolvedPrompts = true }
}
func WithRuntimeValidator(runtime RuntimeValidator) Option {
	return func(s *Service) { s.runtime = runtime }
}
func WithEvalGate(evalGate EvalGate) Option { return func(s *Service) { s.evalGate = evalGate } }
func WithLogger(logger observability.StructuredLogger) Option {
	return func(s *Service) { s.logger = logger }
}
func WithTracer(tracer observability.TraceProvider) Option {
	return func(s *Service) { s.tracer = tracer }
}
func WithIDGenerator(ids observability.IDGenerator) Option { return func(s *Service) { s.ids = ids } }
func WithClock(now func() time.Time) Option                { return func(s *Service) { s.now = now } }

func NewInMemoryRegistry(configs ...AgentConfig) (*InMemoryRegistry, error) {
	r := &InMemoryRegistry{service: NewService(WithLegacyUnresolvedPrompts())}
	for _, cfg := range configs {
		if err := r.Register(context.Background(), cfg); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// RuntimeSystemPromptResolver 暴露同一份冻结 Prompt 解析能力，供 composition root
// 注入 RuntimeContextAssembler；Runtime 不需要也不能反向查询 Agent Registry。
func (s *Service) RuntimeSystemPromptResolver() (agentruntime.SystemPromptResolver, error) {
	return NewRuntimePromptResolver(s.prompts)
}

func (r *InMemoryRegistry) Register(ctx context.Context, cfg AgentConfig) error {
	_, err := r.service.RegisterAgent(ctx, cfg)
	return err
}

func (r *InMemoryRegistry) ResolveEffectiveConfig(ctx context.Context, req ResolveRequest) (EffectiveConfig, error) {
	return r.service.ResolveEffectiveConfig(ctx, req)
}

func (r *InMemoryRegistry) GetCapabilityCard(ctx context.Context, agentID, version string) (CapabilityCard, error) {
	return r.service.GetCapabilityCard(ctx, agentID, version)
}

func (s *Service) ValidateAgent(ctx context.Context, input AgentConfig) (*ValidationResult, error) {
	cfg := normalizeConfig(input)
	ctx, span := s.startSpan(ctx, "registry.schema_validate",
		observability.String("agent_id", cfg.AgentID),
		observability.String("agent_version", cfg.Version),
	)
	defer span.End()
	ctx = s.withAgentTrace(ctx, cfg)
	result := &ValidationResult{Valid: true}
	if err := validateSchema(cfg); err != nil {
		result.Valid = false
		result.Issues = append(result.Issues, issueFromError(err))
		span.RecordError(err)
		return result, err
	}
	if err := s.emitRegistryEvent(ctx, RegistryEvent{Type: EventSchemaValidated, AgentID: cfg.AgentID, Version: cfg.Version}); err != nil {
		return result, mapStoreError(err)
	}
	deps, err := s.resolver.Resolve(ctx, cfg)
	if err != nil {
		result.Valid = false
		result.Issues = append(result.Issues, issueFromError(err))
		span.RecordError(err)
		return result, err
	}
	result.ResolvedDeps = deps
	if s.prompts != nil {
		prompt, resolveErr := s.prompts.Resolve(ctx, PromptKey{Ref: cfg.PromptRef, Version: cfg.PromptVersion})
		if resolveErr != nil {
			err = mapPromptResolveError(resolveErr)
			result.Valid = false
			result.Issues = append(result.Issues, issueFromError(err))
			span.RecordError(err)
			return result, err
		}
		prompt.Content = ""
		result.ResolvedPrompt = &prompt
	} else if !s.allowUnresolvedPrompts {
		err = newError(CodeDependencyMissing, "prompt_resolver", "system prompt resolver is required")
		result.Valid = false
		result.Issues = append(result.Issues, issueFromError(err))
		span.RecordError(err)
		return result, err
	}
	if err := s.emitRegistryEvent(ctx, RegistryEvent{Type: EventDependencyResolved, AgentID: cfg.AgentID, Version: cfg.Version}); err != nil {
		return result, mapStoreError(err)
	}
	if err := validatePolicy(cfg, deps); err != nil {
		result.Valid = false
		result.Issues = append(result.Issues, issueFromError(err))
		span.RecordError(err)
		return result, err
	}
	if err := s.emitRegistryEvent(ctx, RegistryEvent{Type: EventPolicyValidated, AgentID: cfg.AgentID, Version: cfg.Version}); err != nil {
		return result, mapStoreError(err)
	}
	return result, nil
}

func (s *Service) RegisterAgent(ctx context.Context, input AgentConfig) (*RegisterAgentResult, error) {
	cfg := normalizeConfig(input)
	ctx, span := s.startSpan(ctx, "registry.register",
		observability.String("agent_id", cfg.AgentID),
		observability.String("agent_version", cfg.Version),
	)
	defer span.End()
	validation, err := s.ValidateAgent(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := s.runReleaseGates(ctx, cfg, validation); err != nil {
		span.RecordError(err)
		return nil, err
	}
	compiled, err := s.compile(cfg, validation.ResolvedDeps, validation.ResolvedPrompt)
	if err != nil {
		return nil, err
	}
	audit := s.prepareRegistryEvent(ctx, RegistryEvent{
		Type: EventAgentRegistered, AgentID: cfg.AgentID, Version: cfg.Version,
		ConfigHash: compiled.Effective.ConfigHash, Reason: "register",
	})
	if err := s.store.Create(ctx, compiled, audit); err != nil {
		return nil, mapStoreError(err)
	}
	s.logRegistryEvent(ctx, audit)
	return &RegisterAgentResult{
		AgentID: cfg.AgentID, AgentType: cfg.AgentType, Version: cfg.Version,
		ConfigHash: compiled.Effective.ConfigHash, SnapshotRef: compiled.Effective.ConfigSnapshotRef,
		CapabilityCard: cloneCapabilityCard(compiled.Card),
	}, nil
}

// PrepareAgent runs the same validation, dependency resolution, runtime dry-run,
// evaluation gate, and compilation used by RegisterAgent, without mutating the
// Registry store. The configuration control plane uses this before atomically
// publishing an immutable tenant version.
func (s *Service) PrepareAgent(ctx context.Context, input AgentConfig) (*PreparedAgent, error) {
	cfg := normalizeConfig(input)
	validation, err := s.ValidateAgent(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := s.runReleaseGates(ctx, cfg, validation); err != nil {
		return nil, err
	}
	compiled, err := s.compile(cfg, validation.ResolvedDeps, validation.ResolvedPrompt)
	if err != nil {
		return nil, err
	}
	return &PreparedAgent{
		Card:            cloneCapabilityCard(compiled.Card),
		Effective:       cloneEffectiveConfig(compiled.Effective),
		ConfigSnapshots: cloneConfigSnapshotRecords(compiled.ConfigSnapshots),
	}, nil
}

func (s *Service) GetCapabilityCard(ctx context.Context, agentID, version string) (CapabilityCard, error) {
	ctx, span := s.startSpan(ctx, "registry.get_capability_card", observability.String("agent_id", agentID))
	defer span.End()
	agent, err := s.store.Get(ctx, StoreLookup{AgentID: agentID, Version: version})
	if err != nil {
		span.RecordError(err)
		return CapabilityCard{}, mapStoreError(err)
	}
	return cloneCapabilityCard(agent.Card), nil
}

func (s *Service) GetConfigSnapshot(ctx context.Context, ref string) (EffectiveConfig, error) {
	if ref == "" {
		return EffectiveConfig{}, wrapError(CodeInvalidConfig, "config_snapshot_ref", "config snapshot ref is required", ErrConfigSnapshotMissing)
	}
	snapshot, err := s.store.GetConfigSnapshot(ctx, ref)
	if err != nil {
		return EffectiveConfig{}, mapStoreError(err)
	}
	return cloneEffectiveConfig(snapshot.Effective), nil
}

func (s *Service) GetConfigSnapshotForTenant(ctx context.Context, _ string, ref string) (EffectiveConfig, error) {
	return s.GetConfigSnapshot(ctx, ref)
}

func (s *Service) GetAgentDefinition(ctx context.Context, req GetAgentRequest) (*agentruntime.AgentDefinition, error) {
	effective, err := s.ResolveEffectiveConfig(ctx, ResolveRequest{
		AgentID: req.AgentID, AgentType: req.AgentType, Version: req.Version,
		Tenant: req.TenantID, IncludeDisabled: req.IncludeDisabled,
	})
	if err != nil {
		return nil, err
	}
	definition := effective.Definition
	return &definition, nil
}

func (s *Service) ListCapabilityCards(ctx context.Context, req ListCapabilityCardsRequest) (*ListCapabilityCardsResult, error) {
	ctx, span := s.startSpan(ctx, "registry.list_capability_cards", observability.String("tenant_id", req.Tenant))
	defer span.End()
	for _, mode := range req.ExecutionModes {
		if err := mode.Validate(); err != nil {
			return nil, wrapError(CodeInvalidConfig, "execution_modes", "unsupported execution mode", err)
		}
	}
	agents, err := s.store.List(ctx)
	if err != nil {
		return nil, mapStoreError(err)
	}
	cards := make([]CapabilityCard, 0, len(agents))
	for _, agent := range agents {
		if agent.Config.Status == AgentStatusEnabled && matchesCapabilityCard(agent.Card, req) {
			cards = append(cards, cloneCapabilityCard(agent.Card))
		}
	}
	sort.Slice(cards, func(i, j int) bool {
		if cards[i].AgentID == cards[j].AgentID {
			return cards[i].Version < cards[j].Version
		}
		return cards[i].AgentID < cards[j].AgentID
	})
	return &ListCapabilityCardsResult{Cards: cards}, nil
}

// ListAgentConfigs returns immutable copies of the source configurations in
// the base Registry. The management plane uses this read-only view to seed a
// tenant draft without mutating or bypassing the YAML/SQL Registry contract.
func (s *Service) ListAgentConfigs(ctx context.Context) ([]AgentConfig, error) {
	agents, err := s.store.List(ctx)
	if err != nil {
		return nil, mapStoreError(err)
	}
	configs := make([]AgentConfig, 0, len(agents))
	for _, agent := range agents {
		if agent != nil {
			configs = append(configs, cloneAgentConfig(agent.Config))
		}
	}
	return configs, nil
}

func (s *Service) ResolveEffectiveConfig(ctx context.Context, req ResolveRequest) (EffectiveConfig, error) {
	if req.RequestID != "" {
		ctx = observability.WithRequest(ctx, req.RequestID)
	}
	if req.RunID != "" {
		tc := observability.MustTraceContext(ctx)
		ctx = observability.WithRun(ctx, firstNonEmpty(req.SessionID, tc.SessionID), req.RunID)
	}
	selectionHash := resolveSelectionHash(req)
	ctx, span := s.startSpan(ctx, "registry.resolve_effective_config",
		observability.String("agent_id", req.AgentID),
		observability.String("selection_hash", selectionHash),
	)
	defer span.End()
	agent, err := s.store.Get(ctx, StoreLookup{AgentID: req.AgentID, AgentType: req.AgentType, Version: req.Version})
	if err != nil {
		span.RecordError(err)
		return EffectiveConfig{}, mapStoreError(err)
	}
	if agent.Config.Status == AgentStatusDisabled && !req.IncludeDisabled {
		err := wrapError(CodeDisabled, "status", "agent is disabled", ErrAgentDisabled)
		span.RecordError(err)
		return EffectiveConfig{}, err
	}
	if req.CandidateConfigHash != "" && req.CandidateConfigHash != agent.Card.ConfigHash {
		auditErr := s.emitRegistryEvent(ctx, RegistryEvent{
			Type: EventConfigDriftDetected, AgentID: agent.Config.AgentID, Version: agent.Config.Version,
			SelectionHash: selectionHash, ConfigHash: agent.Card.ConfigHash, Reason: "candidate_config_hash_mismatch",
		})
		if auditErr != nil {
			s.logger.Error(ctx, "registry config drift audit failed", auditErr,
				observability.String("agent_id", agent.Config.AgentID),
				observability.String("agent_version", agent.Config.Version),
			)
		}
		err := wrapError(CodeConfigDrift, "candidate_config_hash", "candidate config hash does not match current agent configuration", ErrConfigDrift)
		span.RecordError(err)
		return EffectiveConfig{}, err
	}
	mode := agent.Config.Orchestration.DefaultMode
	if req.ExecutionMode != "" {
		if err := req.ExecutionMode.Validate(); err != nil {
			err := wrapError(CodeInvalidConfig, "execution_mode", "unsupported execution mode", err)
			span.RecordError(err)
			return EffectiveConfig{}, err
		}
		if !containsExecutionMode(agent.Card.ExecutionModes, req.ExecutionMode) {
			err := newError(CodeInvalidConfig, "execution_mode", "execution mode is not enabled for this agent")
			span.RecordError(err)
			return EffectiveConfig{}, err
		}
		mode = req.ExecutionMode
	}
	snapshot, err := s.store.GetConfigSnapshotByMode(ctx, AgentRef{AgentID: agent.Config.AgentID, Version: agent.Config.Version}, mode)
	if err != nil {
		span.RecordError(err)
		return EffectiveConfig{}, mapStoreError(err)
	}
	if err := validateConfigSnapshotForAgent(snapshot, agent.Config); err != nil {
		span.RecordError(err)
		return EffectiveConfig{}, mapStoreError(err)
	}
	effective := cloneEffectiveConfig(snapshot.Effective)
	ctx = s.withAgentTrace(ctx, agent.Config)
	if err := s.emitRegistryEvent(ctx, RegistryEvent{
		Type: EventEffectiveConfigResolved, AgentID: agent.Config.AgentID, Version: agent.Config.Version,
		SelectionHash: selectionHash, ConfigHash: effective.ConfigHash, Reason: "resolve_effective_config",
	}); err != nil {
		span.RecordError(err)
		return EffectiveConfig{}, mapStoreError(err)
	}
	s.logger.Info(ctx, "registry effective config resolved",
		observability.String("selection_hash", selectionHash),
		observability.String("config_hash", effective.ConfigHash),
		observability.String("config_snapshot_ref", effective.ConfigSnapshotRef),
	)
	return effective, nil
}

func (s *Service) EnableAgent(ctx context.Context, ref AgentRef) error {
	return s.setStatus(ctx, ref, AgentStatusEnabled, EventAgentEnabled)
}

func (s *Service) DisableAgent(ctx context.Context, ref AgentRef) error {
	return s.setStatus(ctx, ref, AgentStatusDisabled, EventAgentDisabled)
}

func (s *Service) UpdateGrayPercent(ctx context.Context, ref AgentRef, percent int) error {
	if percent < 0 || percent > 100 {
		return newError(CodeInvalidConfig, "gray_percent", "gray percent must be between 0 and 100")
	}
	audit := s.prepareRegistryEvent(ctx, RegistryEvent{
		Type: EventGrayPercentChanged, AgentID: ref.AgentID, Version: ref.Version,
		Reason: "update_gray_percent", DiffSummary: "gray_percent=" + strconv.Itoa(percent),
	})
	if err := s.store.UpdateGrayPercent(ctx, ref, percent, audit); err != nil {
		return mapStoreError(err)
	}
	s.logRegistryEvent(ctx, audit)
	return nil
}

func (s *Service) RollbackAgent(ctx context.Context, ref AgentRef, targetVersion string) error {
	if _, err := s.store.Get(ctx, StoreLookup{AgentID: ref.AgentID, Version: targetVersion}); err != nil {
		return mapStoreError(err)
	}
	target := AgentRef{AgentID: ref.AgentID, Version: targetVersion}
	audit := s.prepareRegistryEvent(ctx, RegistryEvent{
		Type: EventRollbackTriggered, AgentID: ref.AgentID, Version: targetVersion,
		Reason: "rollback", DiffSummary: "from=" + ref.Version + ";to=" + targetVersion,
	})
	if err := s.store.UpdateStatus(ctx, target, AgentStatusEnabled, s.now().UnixMilli(), audit); err != nil {
		return mapStoreError(err)
	}
	s.logRegistryEvent(ctx, audit)
	return nil
}

func (s *Service) ListAgentVersions(ctx context.Context, agentID string) ([]AgentVersion, error) {
	versions, err := s.store.ListVersions(ctx, agentID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	if len(versions) == 0 {
		return nil, wrapError(CodeNotFound, "agent_id", "agent versions not found", ErrAgentNotFound)
	}
	return versions, nil
}

func (s *Service) Reload(ctx context.Context) (*ReloadResult, error) {
	if s.loader == nil {
		return nil, newError(CodeLoadFailed, "loader", "loader is not configured")
	}
	replacer, ok := s.store.(SnapshotStore)
	if !ok {
		return nil, newError(CodeLoadFailed, "store", "store does not support atomic snapshot reload")
	}
	configs, err := s.loader.Load(ctx)
	if err != nil {
		return nil, err
	}
	result := &ReloadResult{Loaded: len(configs)}
	compiled := make([]*compiledAgent, 0, len(configs))
	seen := make(map[string]struct{}, len(configs))
	for _, input := range configs {
		cfg := normalizeConfig(input)
		key := cfg.AgentID + "\x00" + cfg.Version
		if _, ok := seen[key]; ok {
			result.Issues = append(result.Issues, ValidationIssue{Code: CodeConflict, Field: "agent_id+version", Message: "duplicate agent version in reload batch"})
			continue
		}
		seen[key] = struct{}{}
		validation, err := s.ValidateAgent(ctx, cfg)
		if err != nil {
			result.Issues = append(result.Issues, issueFromError(err))
			continue
		}
		if err := s.runReleaseGates(ctx, cfg, validation); err != nil {
			result.Issues = append(result.Issues, issueFromError(err))
			continue
		}
		agent, err := s.compile(cfg, validation.ResolvedDeps, validation.ResolvedPrompt)
		if err != nil {
			result.Issues = append(result.Issues, issueFromError(err))
			continue
		}
		compiled = append(compiled, agent)
	}
	if len(result.Issues) > 0 {
		return result, newError(CodeLoadFailed, "agents", "some agent configs failed to reload")
	}
	if err := replacer.Replace(ctx, compiled); err != nil {
		return result, mapStoreError(err)
	}
	result.Registered = len(compiled)
	return result, nil
}

func (s *Service) runReleaseGates(ctx context.Context, cfg AgentConfig, validation *ValidationResult) error {
	runtimeResult, err := s.runtime.DryRun(ctx, cfg)
	validation.RuntimeDryRun = runtimeResult
	if err != nil {
		return err
	}
	if !runtimeResult.Passed {
		return newError(CodeRuntimeDryRunFailed, "runtime", firstNonEmpty(runtimeResult.Reason, "runtime dry run failed"))
	}
	// mock/pass-through 门禁不谎报"通过":发 *_skipped 事件,审计如实记录未真正校验。
	runtimeEvent := EventRuntimeDryRunPassed
	if runtimeResult.Mock {
		runtimeEvent = EventRuntimeDryRunSkipped
	}
	if err := s.emitRegistryEvent(ctx, RegistryEvent{Type: runtimeEvent, AgentID: cfg.AgentID, Version: cfg.Version}); err != nil {
		return mapStoreError(err)
	}
	evalResult, err := s.evalGate.Evaluate(ctx, cfg)
	validation.EvalGate = evalResult
	if err != nil {
		return err
	}
	if !evalResult.Passed {
		return newError(CodeEvalGateFailed, "release.eval_gate", firstNonEmpty(evalResult.Reason, "eval gate failed"))
	}
	evalEvent := EventEvalGatePassed
	if evalResult.Mock {
		evalEvent = EventEvalGateSkipped
	}
	if err := s.emitRegistryEvent(ctx, RegistryEvent{Type: evalEvent, AgentID: cfg.AgentID, Version: cfg.Version}); err != nil {
		return mapStoreError(err)
	}
	return nil
}

func (s *Service) compile(cfg AgentConfig, deps ResolvedDependencies, prompt *PromptSnapshot) (*compiledAgent, error) {
	cfg = normalizeConfig(cfg)
	registeredAt := s.now().UTC()
	effective, err := compileEffectiveConfig(cfg, deps, registeredAt, prompt)
	if err != nil {
		return nil, err
	}
	snapshots, err := compileConfigSnapshots(cfg, *effective)
	if err != nil {
		return nil, err
	}
	for i := range snapshots {
		if snapshots[i].ExecutionMode == cfg.Orchestration.DefaultMode {
			effective = &snapshots[i].Effective
			break
		}
	}
	card, err := compileCapabilityCard(cfg, effective)
	if err != nil {
		return nil, err
	}
	return &compiledAgent{
		Config: cfg, Card: *card, Effective: *effective, ConfigSnapshots: snapshots, RegisteredAt: registeredAt,
		ActivatedAtMS: registeredAt.UnixMilli(), GrayPercent: cfg.Release.GrayPercent, Revision: 1,
	}, nil
}

func (s *Service) setStatus(ctx context.Context, ref AgentRef, status AgentStatus, eventType string) error {
	audit := s.prepareRegistryEvent(ctx, RegistryEvent{
		Type: eventType, AgentID: ref.AgentID, Version: ref.Version,
		Reason: string(status), DiffSummary: "status=" + string(status),
	})
	if err := s.store.UpdateStatus(ctx, ref, status, s.now().UnixMilli(), audit); err != nil {
		return mapStoreError(err)
	}
	s.logRegistryEvent(ctx, audit)
	return nil
}

func validateSchema(cfg AgentConfig) error {
	if err := ValidateAgentConfig(cfg); err != nil {
		var registryErr *RegistryError
		if errors.As(err, &registryErr) {
			return registryErr
		}
		field := "agent"
		switch {
		case errors.Is(err, ErrAgentIDRequired):
			field = "agent_id"
		case errors.Is(err, ErrAgentTypeRequired):
			field = "agent_type"
		case errors.Is(err, ErrAgentVersionRequired):
			field = "version"
		case errors.Is(err, ErrRuntimeRequired):
			field = "runtime.type"
		case errors.Is(err, ErrPromptRefRequired):
			field = "prompt_ref"
		}
		return wrapError(CodeInvalidConfig, field, err.Error(), err)
	}
	if cfg.Status != AgentStatusEnabled && cfg.Status != AgentStatusDisabled {
		return newError(CodeInvalidConfig, "status", "status must be enabled or disabled")
	}
	return nil
}

func validatePolicy(cfg AgentConfig, deps ResolvedDependencies) error {
	risk, err := effectiveToolRisk(cfg, deps)
	if err != nil {
		return wrapError(CodePolicyViolation, "tool_policy.risk_level", "tool risk policy cannot lower catalog risk", err)
	}
	if risk != "high" {
		return nil
	}
	hitl := make(map[string]struct{}, len(cfg.ToolPolicy.HITLRequiredTools))
	for _, ref := range cfg.ToolPolicy.HITLRequiredTools {
		hitl[ref] = struct{}{}
	}
	if len(deps.AuthoringHighRiskTools) == 0 && len(hitl) == 0 {
		return newError(CodePolicyViolation, "tool_policy.hitl_required_tools", "high risk tools require HITL policy")
	}
	for _, ref := range deps.AuthoringHighRiskTools {
		if _, ok := hitl[ref]; !ok {
			return newError(CodePolicyViolation, "tool_policy.hitl_required_tools", "high risk tool requires HITL: "+ref)
		}
	}
	return nil
}

func matchesCapabilityCard(card CapabilityCard, req ListCapabilityCardsRequest) bool {
	if req.RiskLevel != "" && !strings.EqualFold(req.RiskLevel, card.RiskLevel) {
		return false
	}
	if len(req.ExecutionModes) > 0 && !intersectsExecutionMode(card.ExecutionModes, req.ExecutionModes) {
		return false
	}
	for _, tag := range req.Tags {
		if !containsString(card.Tags, tag) {
			return false
		}
	}
	return true
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func intersectsExecutionMode(left, right []ExecutionMode) bool {
	for _, a := range left {
		for _, b := range right {
			if a == b {
				return true
			}
		}
	}
	return false
}

func containsExecutionMode(values []ExecutionMode, target ExecutionMode) bool {
	return intersectsExecutionMode(values, []ExecutionMode{target})
}

func resolveSelectionHash(req ResolveRequest) string {
	if req.SelectionHash != "" {
		return req.SelectionHash
	}
	return req.BindingHash
}

func mapStoreError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrDuplicateAgent):
		return wrapError(CodeConflict, "agent_id+version", "agent version already exists", ErrDuplicateAgent)
	case errors.Is(err, ErrAgentNotFound):
		return wrapError(CodeNotFound, "agent", "agent not found", ErrAgentNotFound)
	case errors.Is(err, ErrStoreConflict):
		return wrapError(CodeConflict, "revision", "concurrent registry update", ErrStoreConflict)
	case errors.Is(err, ErrConfigSnapshotMissing):
		return wrapError(CodeLoadFailed, "config_snapshot_ref", "registry config snapshot is missing", ErrConfigSnapshotMissing)
	case errors.Is(err, ErrStoreCorrupt):
		return wrapError(CodeLoadFailed, "store", "registry stored data is inconsistent", ErrStoreCorrupt)
	case errors.Is(err, ErrAuditWrite):
		return wrapError(CodeLoadFailed, "audit", "registry audit write failed", ErrAuditWrite)
	default:
		return wrapError(CodeLoadFailed, "store", "registry store operation failed", err)
	}
}

func mapPromptResolveError(err error) error {
	var registryErr *RegistryError
	if errors.As(err, &registryErr) {
		return err
	}
	switch {
	case errors.Is(err, ErrPromptNotFound):
		return wrapError(CodeDependencyMissing, "prompt_ref", "system prompt version does not exist", err)
	case errors.Is(err, ErrPromptInvalid):
		return wrapError(CodeInvalidConfig, "prompt_version", "system prompt requires an exact ref and version", err)
	case errors.Is(err, ErrPromptContentUnavailable), errors.Is(err, ErrPromptContentHashMismatch), errors.Is(err, ErrStoreCorrupt):
		return wrapError(CodeLoadFailed, "prompt_ref", "system prompt snapshot cannot be resolved safely", err)
	default:
		return wrapError(CodeLoadFailed, "prompt_ref", "resolve system prompt snapshot", err)
	}
}

var _ Registry = (*Service)(nil)
var _ AgentRegistry = (*Service)(nil)
var _ AgentRegistryAdmin = (*Service)(nil)
var _ ConfigSnapshotReader = (*Service)(nil)
var _ Registry = (*InMemoryRegistry)(nil)
