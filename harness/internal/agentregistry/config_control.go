package agentregistry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/AGenUI/agenui-studio/harness/internal/identifiercontract"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

var (
	ErrAgentConfigControlInvalid   = errors.New("agent config control input invalid")
	ErrAgentConfigControlNotFound  = errors.New("agent config control record not found")
	ErrAgentConfigControlConflict  = errors.New("agent config control revision conflict")
	ErrAgentConfigVersionImmutable = errors.New("agent config version is immutable")
)

type ConfigEnvironment string

const (
	ConfigEnvironmentLocal      ConfigEnvironment = "local"
	ConfigEnvironmentTesting    ConfigEnvironment = "testing"
	ConfigEnvironmentStaging    ConfigEnvironment = "staging"
	ConfigEnvironmentProduction ConfigEnvironment = "production"
)

func (e ConfigEnvironment) valid() bool {
	switch e {
	case ConfigEnvironmentLocal, ConfigEnvironmentTesting, ConfigEnvironmentStaging, ConfigEnvironmentProduction:
		return true
	default:
		return false
	}
}

type AgentConfigDraft struct {
	TenantID    string      `json:"tenant_id"`
	AgentID     string      `json:"agent_id"`
	Config      AgentConfig `json:"config"`
	ContentHash string      `json:"content_hash"`
	Revision    int64       `json:"revision"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
	CreatedBy   string      `json:"created_by"`
	UpdatedBy   string      `json:"updated_by"`
}

type AgentConfigVersion struct {
	TenantID            string         `json:"tenant_id"`
	AgentID             string         `json:"agent_id"`
	Version             string         `json:"version"`
	Config              AgentConfig    `json:"config"`
	ContentHash         string         `json:"content_hash"`
	SourceDraftRevision int64          `json:"source_draft_revision"`
	CreatedAt           time.Time      `json:"created_at"`
	CreatedBy           string         `json:"created_by"`
	Prepared            *PreparedAgent `json:"prepared,omitempty"`
}

type AgentConfigRelease struct {
	TenantID    string            `json:"tenant_id"`
	Environment ConfigEnvironment `json:"environment"`
	AgentID     string            `json:"agent_id"`
	Version     string            `json:"version"`
	ContentHash string            `json:"content_hash"`
	Revision    int64             `json:"revision"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	UpdatedBy   string            `json:"updated_by"`
}

type AgentConfigReleaseEvent struct {
	EventID         string            `json:"event_id"`
	TenantID        string            `json:"tenant_id"`
	Environment     ConfigEnvironment `json:"environment"`
	AgentID         string            `json:"agent_id"`
	FromVersion     string            `json:"from_version,omitempty"`
	ToVersion       string            `json:"to_version"`
	ContentHash     string            `json:"content_hash"`
	ReleaseRevision int64             `json:"release_revision"`
	Actor           string            `json:"actor"`
	Reason          string            `json:"reason,omitempty"`
	OccurredAt      time.Time         `json:"occurred_at"`
}

type SaveAgentConfigDraftRequest struct {
	TenantID         string
	Config           AgentConfig
	ExpectedRevision int64
	Actor            string
}

type PublishAgentConfigDraftRequest struct {
	TenantID                string
	AgentID                 string
	Environment             ConfigEnvironment
	ExpectedDraftRevision   int64
	ExpectedReleaseRevision int64
	Actor                   string
	Reason                  string
	Prepared                *PreparedAgent
}

type PromoteAgentConfigVersionRequest struct {
	TenantID                string
	AgentID                 string
	Version                 string
	Environment             ConfigEnvironment
	ExpectedReleaseRevision int64
	Actor                   string
	Reason                  string
}

type AgentConfigControlStore interface {
	SaveDraft(context.Context, AgentConfigDraft, int64) (AgentConfigDraft, error)
	GetDraft(context.Context, string, string) (AgentConfigDraft, error)
	ListDrafts(context.Context, string) ([]AgentConfigDraft, error)
	AgentIDManaged(context.Context, string) (bool, error)
	PublishDraft(context.Context, PublishAgentConfigDraftRequest, AgentConfigReleaseEvent) (AgentConfigVersion, AgentConfigRelease, error)
	PromoteVersion(context.Context, PromoteAgentConfigVersionRequest, AgentConfigReleaseEvent) (AgentConfigRelease, error)
	GetRelease(context.Context, string, ConfigEnvironment, string) (AgentConfigRelease, error)
	GetVersion(context.Context, string, string, string) (AgentConfigVersion, error)
	ListVersions(context.Context, string, string) ([]AgentConfigVersion, error)
}

type AgentConfigPreparer interface {
	PrepareAgent(context.Context, AgentConfig) (*PreparedAgent, error)
}

type staticAgentRelationResolver interface {
	ResolveEffectiveConfig(context.Context, ResolveRequest) (EffectiveConfig, error)
}

type AgentConfigControlService struct {
	store    AgentConfigControlStore
	now      func() time.Time
	ids      observability.IDGenerator
	preparer AgentConfigPreparer
}

func (s *AgentConfigControlService) SetPreparer(preparer AgentConfigPreparer) {
	if s != nil {
		s.preparer = preparer
	}
}

func NewAgentConfigControlService(store AgentConfigControlStore) *AgentConfigControlService {
	return &AgentConfigControlService{
		store: store,
		now:   time.Now,
		ids:   observability.NewULIDGenerator("config"),
	}
}

func (s *AgentConfigControlService) SaveDraft(ctx context.Context, req SaveAgentConfigDraftRequest) (AgentConfigDraft, error) {
	if err := s.ready(); err != nil {
		return AgentConfigDraft{}, err
	}
	if err := validateConfigControlScope(req.TenantID, req.Config.AgentID, req.Actor); err != nil {
		return AgentConfigDraft{}, err
	}
	if req.ExpectedRevision < 0 {
		return AgentConfigDraft{}, invalidConfigControl("expected_revision must not be negative")
	}
	cfg := normalizeConfig(req.Config)
	if err := validateSchema(cfg); err != nil {
		return AgentConfigDraft{}, fmt.Errorf("%w: %v", ErrAgentConfigControlInvalid, err)
	}
	_, hash, err := encodeAgentConfig(cfg)
	if err != nil {
		return AgentConfigDraft{}, err
	}
	now := s.now().UTC()
	return s.store.SaveDraft(ctx, AgentConfigDraft{
		TenantID: req.TenantID, AgentID: cfg.AgentID, Config: cfg, ContentHash: hash,
		CreatedAt: now, UpdatedAt: now, CreatedBy: req.Actor, UpdatedBy: req.Actor,
	}, req.ExpectedRevision)
}

func (s *AgentConfigControlService) GetDraft(ctx context.Context, tenantID, agentID string) (AgentConfigDraft, error) {
	if err := s.ready(); err != nil {
		return AgentConfigDraft{}, err
	}
	if err := validateTenantAgentScope(tenantID, agentID); err != nil {
		return AgentConfigDraft{}, err
	}
	return s.store.GetDraft(ctx, tenantID, agentID)
}

func (s *AgentConfigControlService) ListDrafts(ctx context.Context, tenantID string) ([]AgentConfigDraft, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if err := validateConfigControlIdentity("tenant_id", tenantID); err != nil {
		return nil, err
	}
	return s.store.ListDrafts(ctx, tenantID)
}

func (s *AgentConfigControlService) AgentIDManaged(ctx context.Context, agentID string) (bool, error) {
	if err := s.ready(); err != nil {
		return false, err
	}
	if err := validateConfigControlAgentID(agentID); err != nil {
		return false, err
	}
	return s.store.AgentIDManaged(ctx, agentID)
}

func (s *AgentConfigControlService) PublishDraft(ctx context.Context, req PublishAgentConfigDraftRequest) (AgentConfigVersion, AgentConfigRelease, error) {
	if err := s.ready(); err != nil {
		return AgentConfigVersion{}, AgentConfigRelease{}, err
	}
	if err := validateReleaseRequest(req.TenantID, req.AgentID, req.Environment, req.Actor, req.ExpectedDraftRevision, req.ExpectedReleaseRevision); err != nil {
		return AgentConfigVersion{}, AgentConfigRelease{}, err
	}
	if err := validateReleaseReason(req.Reason); err != nil {
		return AgentConfigVersion{}, AgentConfigRelease{}, err
	}
	if s.preparer != nil {
		draft, err := s.store.GetDraft(ctx, req.TenantID, req.AgentID)
		if err != nil {
			return AgentConfigVersion{}, AgentConfigRelease{}, err
		}
		if draft.Revision != req.ExpectedDraftRevision {
			return AgentConfigVersion{}, AgentConfigRelease{}, ErrAgentConfigControlConflict
		}
		subAgentVersions, err := s.validateRelations(ctx, req.TenantID, req.Environment, draft.Config)
		if err != nil {
			return AgentConfigVersion{}, AgentConfigRelease{}, err
		}
		prepared, err := s.preparer.PrepareAgent(ctx, draft.Config)
		if err != nil {
			return AgentConfigVersion{}, AgentConfigRelease{}, err
		}
		prepared.SubAgentVersions = subAgentVersions
		req.Prepared = prepared
	}
	event := AgentConfigReleaseEvent{
		EventID: s.ids.NewEventID(), TenantID: req.TenantID, Environment: req.Environment,
		AgentID: req.AgentID, Actor: req.Actor, Reason: strings.TrimSpace(req.Reason), OccurredAt: s.now().UTC(),
	}
	return s.store.PublishDraft(ctx, req, event)
}

func (s *AgentConfigControlService) validateRelations(ctx context.Context, tenantID string, environment ConfigEnvironment, root AgentConfig) (map[string]string, error) {
	path := map[string]bool{}
	rootVersions := make(map[string]string, len(root.SubAgents))
	var walk func(AgentConfig) error
	walk = func(cfg AgentConfig) error {
		if path[cfg.AgentID] {
			return invalidConfigControl("sub_agents contains a cycle at " + cfg.AgentID)
		}
		path[cfg.AgentID] = true
		defer delete(path, cfg.AgentID)
		seen := make(map[string]struct{}, len(cfg.SubAgents))
		for _, childID := range cfg.SubAgents {
			if childID == cfg.AgentID {
				return invalidConfigControl("agent cannot reference itself as a sub-agent")
			}
			if _, duplicate := seen[childID]; duplicate {
				return invalidConfigControl("duplicate sub-agent reference " + childID)
			}
			seen[childID] = struct{}{}
			child, childVersion, err := s.resolveRelationChild(ctx, tenantID, environment, childID)
			if err != nil {
				return err
			}
			if cfg.AgentID == root.AgentID {
				rootVersions[childID] = childVersion
			}
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	return rootVersions, nil
}

func (s *AgentConfigControlService) resolveRelationChild(ctx context.Context, tenantID string, environment ConfigEnvironment, childID string) (AgentConfig, string, error) {
	release, err := s.store.GetRelease(ctx, tenantID, environment, childID)
	if err == nil {
		version, getErr := s.store.GetVersion(ctx, tenantID, childID, release.Version)
		if getErr != nil {
			return AgentConfig{}, "", getErr
		}
		return version.Config, release.Version, nil
	}
	if !errors.Is(err, ErrAgentConfigControlNotFound) {
		return AgentConfig{}, "", err
	}
	resolver, ok := s.preparer.(staticAgentRelationResolver)
	if !ok {
		return AgentConfig{}, "", invalidConfigControl("sub-agent " + childID + " has no release in environment " + string(environment))
	}
	effective, resolveErr := resolver.ResolveEffectiveConfig(ctx, ResolveRequest{AgentID: childID})
	if resolveErr != nil {
		if errors.Is(resolveErr, ErrAgentNotFound) {
			return AgentConfig{}, "", invalidConfigControl("sub-agent " + childID + " has no managed release or static registration")
		}
		return AgentConfig{}, "", resolveErr
	}
	return AgentConfig{
		AgentID:   effective.Definition.AgentID,
		Version:   effective.Definition.Version,
		SubAgents: append([]string(nil), effective.Definition.SubAgentRefs...),
	}, effective.Definition.Version, nil
}

func (s *AgentConfigControlService) PromoteVersion(ctx context.Context, req PromoteAgentConfigVersionRequest) (AgentConfigRelease, error) {
	if err := s.ready(); err != nil {
		return AgentConfigRelease{}, err
	}
	if err := validateReleaseRequest(req.TenantID, req.AgentID, req.Environment, req.Actor, 1, req.ExpectedReleaseRevision); err != nil {
		return AgentConfigRelease{}, err
	}
	if err := validateConfigControlIdentity("version", req.Version); err != nil {
		return AgentConfigRelease{}, err
	}
	if err := validateReleaseReason(req.Reason); err != nil {
		return AgentConfigRelease{}, err
	}
	event := AgentConfigReleaseEvent{
		EventID: s.ids.NewEventID(), TenantID: req.TenantID, Environment: req.Environment,
		AgentID: req.AgentID, ToVersion: req.Version, Actor: req.Actor,
		Reason: strings.TrimSpace(req.Reason), OccurredAt: s.now().UTC(),
	}
	return s.store.PromoteVersion(ctx, req, event)
}

func (s *AgentConfigControlService) GetRelease(ctx context.Context, tenantID string, environment ConfigEnvironment, agentID string) (AgentConfigRelease, error) {
	if err := s.ready(); err != nil {
		return AgentConfigRelease{}, err
	}
	if err := validateReleaseScope(tenantID, agentID, environment); err != nil {
		return AgentConfigRelease{}, err
	}
	return s.store.GetRelease(ctx, tenantID, environment, agentID)
}

func (s *AgentConfigControlService) ListVersions(ctx context.Context, tenantID, agentID string) ([]AgentConfigVersion, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if err := validateTenantAgentScope(tenantID, agentID); err != nil {
		return nil, err
	}
	return s.store.ListVersions(ctx, tenantID, agentID)
}

func (s *AgentConfigControlService) GetVersion(ctx context.Context, tenantID, agentID, version string) (AgentConfigVersion, error) {
	if err := s.ready(); err != nil {
		return AgentConfigVersion{}, err
	}
	if err := validateTenantAgentScope(tenantID, agentID); err != nil {
		return AgentConfigVersion{}, err
	}
	if err := validateConfigControlIdentity("version", version); err != nil {
		return AgentConfigVersion{}, err
	}
	return s.store.GetVersion(ctx, tenantID, agentID, version)
}

func validateReleaseRequest(tenantID, agentID string, environment ConfigEnvironment, actor string, draftRevision, releaseRevision int64) error {
	if err := validateReleaseScope(tenantID, agentID, environment); err != nil {
		return err
	}
	if err := validateConfigControlIdentity("actor", actor); err != nil {
		return err
	}
	if draftRevision < 1 || releaseRevision < 0 {
		return invalidConfigControl("draft revision must be positive and release revision must not be negative")
	}
	return nil
}

func (s *AgentConfigControlService) ready() error {
	if s == nil || s.store == nil || s.now == nil || s.ids == nil {
		return invalidConfigControl("config control service is not configured")
	}
	return nil
}

func validateConfigControlScope(tenantID, agentID, actor string) error {
	if err := validateTenantAgentScope(tenantID, agentID); err != nil {
		return err
	}
	return validateConfigControlIdentity("actor", actor)
}

func validateTenantAgentScope(tenantID, agentID string) error {
	if err := validateConfigControlIdentity("tenant_id", tenantID); err != nil {
		return err
	}
	return validateConfigControlAgentID(agentID)
}

func validateReleaseScope(tenantID, agentID string, environment ConfigEnvironment) error {
	if err := validateTenantAgentScope(tenantID, agentID); err != nil {
		return err
	}
	if !environment.valid() {
		return invalidConfigControl("environment must be local, testing, staging or production")
	}
	return nil
}

func validateConfigControlIdentity(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return invalidConfigControl(field + " is required")
	}
	if value != strings.TrimSpace(value) || !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxRegistryIdentityRunes || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return invalidConfigControl(field + " is not a valid control-plane identity")
	}
	return nil
}

func validateConfigControlAgentID(value string) error {
	if err := validateConfigControlIdentity("agent_id", value); err != nil {
		return err
	}
	if err := identifiercontract.Validate(identifiercontract.AgentID(value)); err != nil {
		return invalidConfigControl(err.Error())
	}
	return nil
}

func validateReleaseReason(reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil
	}
	if !utf8.ValidString(reason) || utf8.RuneCountInString(reason) > maxRegistryIdentityRunes || strings.IndexFunc(reason, unicode.IsControl) >= 0 {
		return invalidConfigControl("reason is not a valid release reason")
	}
	return nil
}

func invalidConfigControl(message string) error {
	return fmt.Errorf("%w: %s", ErrAgentConfigControlInvalid, message)
}

func encodeAgentConfig(cfg AgentConfig) ([]byte, string, error) {
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return nil, "", fmt.Errorf("%w: encode agent config: %v", ErrAgentConfigControlInvalid, err)
	}
	sum := sha256.Sum256(encoded)
	return encoded, "sha256:" + hex.EncodeToString(sum[:]), nil
}
