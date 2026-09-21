package agentregistry

import (
	"context"
	"errors"
	"fmt"
	"reflect"
)

type bootstrapCandidate struct {
	record     *StoreRecord
	validation *ValidationResult
}

// Bootstrap 将 Loader 中尚未注册的不可变版本写入共享 Store。
// 重复启动只跳过完全相同的版本，不覆盖线上启停和灰度状态。
func (s *Service) Bootstrap(ctx context.Context) (*BootstrapResult, error) {
	if s.loader == nil {
		return nil, newError(CodeLoadFailed, "loader", "loader is not configured")
	}
	inputs, err := s.loader.Load(ctx)
	if err != nil {
		return nil, err
	}
	result := &BootstrapResult{Loaded: len(inputs)}
	configs := make([]AgentConfig, 0, len(inputs))
	seenIdentity := make(map[string]struct{}, len(inputs))
	seenTypeVersion := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		cfg := normalizeConfig(input)
		identity := identityTupleKey(cfg.AgentID, cfg.Version)
		typeVersion := identityTupleKey(cfg.AgentType, cfg.Version)
		if _, exists := seenIdentity[identity]; exists {
			result.Issues = append(result.Issues, ValidationIssue{Code: CodeConflict, Field: "agent_id+version", Message: "duplicate agent version in bootstrap batch"})
			continue
		}
		if _, exists := seenTypeVersion[typeVersion]; exists {
			result.Issues = append(result.Issues, ValidationIssue{Code: CodeConflict, Field: "agent_type+version", Message: "duplicate agent type version in bootstrap batch"})
			continue
		}
		seenIdentity[identity] = struct{}{}
		seenTypeVersion[typeVersion] = struct{}{}
		configs = append(configs, cfg)
	}
	if len(result.Issues) > 0 {
		return result, newError(CodeLoadFailed, "agents", "bootstrap batch contains duplicate versions")
	}

	// 先完成整批静态校验、依赖解析和编译，再判断新增或跳过，避免旧版本因
	// 依赖变化被误判为“配置未变”。发布门禁只对真正待注册的版本执行。
	compiled := make([]bootstrapCandidate, 0, len(configs))
	for _, cfg := range configs {
		validation, validateErr := s.ValidateAgent(ctx, cfg)
		if validateErr != nil {
			result.Issues = append(result.Issues, issueFromError(validateErr))
			continue
		}
		record, compileErr := s.compile(cfg, validation.ResolvedDeps, validation.ResolvedPrompt)
		if compileErr != nil {
			result.Issues = append(result.Issues, issueFromError(compileErr))
			continue
		}
		compiled = append(compiled, bootstrapCandidate{record: record, validation: validation})
	}
	if len(result.Issues) > 0 {
		return result, newError(CodeLoadFailed, "agents", "some agent configs failed bootstrap validation")
	}

	pending := make([]bootstrapCandidate, 0, len(compiled))
	for _, candidate := range compiled {
		record := candidate.record
		existing, getErr := s.store.Get(ctx, StoreLookup{AgentID: record.Config.AgentID, Version: record.Config.Version})
		switch {
		case getErr == nil:
			equal, compareErr := s.bootstrapStoredRecordEqual(ctx, existing, record)
			if compareErr != nil {
				result.Issues = append(result.Issues, issueFromError(mapStoreError(compareErr)))
				continue
			}
			if equal {
				result.Unchanged++
				continue
			}
			result.Issues = append(result.Issues, ValidationIssue{
				Code: CodeConflict, Field: "agent_id+version",
				Message: fmt.Sprintf(
					"registered agent %s@%s is immutable but differs from bootstrap config or resolved dependencies (stored_config_hash=%s candidate_config_hash=%s); publish a new agent version",
					record.Config.AgentID, record.Config.Version, existing.Effective.ConfigHash, record.Effective.ConfigHash,
				),
			})
		case errors.Is(getErr, ErrAgentNotFound):
			byType, typeErr := s.store.Get(ctx, StoreLookup{AgentType: record.Config.AgentType, Version: record.Config.Version})
			if typeErr == nil && byType.Config.AgentID != record.Config.AgentID {
				result.Issues = append(result.Issues, ValidationIssue{
					Code: CodeConflict, Field: "agent_type+version",
					Message: fmt.Sprintf(
						"agent type %s@%s is already owned by agent %s; publish a new version",
						record.Config.AgentType, record.Config.Version, byType.Config.AgentID,
					),
				})
				continue
			}
			if typeErr != nil && !errors.Is(typeErr, ErrAgentNotFound) {
				return result, mapStoreError(typeErr)
			}
			pending = append(pending, candidate)
		default:
			return result, mapStoreError(getErr)
		}
	}
	if len(result.Issues) > 0 {
		return result, newError(CodeLoadFailed, "agents", "bootstrap conflicts with registered versions")
	}

	// 整批待注册版本先通过门禁，任一失败都不会留下部分 Registry 配置写入。
	for _, candidate := range pending {
		if gateErr := s.runReleaseGates(ctx, candidate.record.Config, candidate.validation); gateErr != nil {
			result.Issues = append(result.Issues, issueFromError(gateErr))
		}
	}
	if len(result.Issues) > 0 {
		return result, newError(CodeLoadFailed, "agents", "some agent configs failed bootstrap release gates")
	}

	for _, candidate := range pending {
		record := candidate.record
		audit := s.prepareRegistryEvent(ctx, RegistryEvent{
			Type: EventAgentRegistered, AgentID: record.Config.AgentID, Version: record.Config.Version,
			ConfigHash: record.Effective.ConfigHash, Reason: "bootstrap",
		})
		if createErr := s.store.Create(ctx, record, audit); createErr != nil {
			if errors.Is(createErr, ErrDuplicateAgent) {
				existing, readErr := s.store.Get(ctx, StoreLookup{AgentID: record.Config.AgentID, Version: record.Config.Version})
				if readErr == nil {
					equal, compareErr := s.bootstrapStoredRecordEqual(ctx, existing, record)
					if compareErr == nil && equal {
						result.Unchanged++
						continue
					}
					if compareErr != nil {
						createErr = compareErr
					}
				}
			}
			result.Issues = append(result.Issues, issueFromError(mapStoreError(createErr)))
			continue
		}
		result.Registered++
		s.logRegistryEvent(ctx, audit)
	}
	if len(result.Issues) > 0 {
		return result, newError(CodeLoadFailed, "store", "some agent configs failed to bootstrap")
	}
	return result, nil
}

func bootstrapConfigEqual(existing, candidate AgentConfig) bool {
	existing = normalizeConfig(existing)
	candidate = normalizeConfig(candidate)
	existing.Status, candidate.Status = "", ""
	existing.Release.GrayPercent, candidate.Release.GrayPercent = 0, 0
	return reflect.DeepEqual(existing, candidate)
}

func bootstrapRecordEqual(existing, candidate *StoreRecord) bool {
	return existing != nil && candidate != nil &&
		bootstrapConfigEqual(existing.Config, candidate.Config) &&
		existing.Effective.ConfigHash == candidate.Effective.ConfigHash
}

func (s *Service) bootstrapStoredRecordEqual(ctx context.Context, existing, candidate *StoreRecord) (bool, error) {
	if !bootstrapRecordEqual(existing, candidate) {
		return false, nil
	}
	ref := AgentRef{AgentID: candidate.Config.AgentID, Version: candidate.Config.Version}
	for i := range candidate.ConfigSnapshots {
		want := &candidate.ConfigSnapshots[i]
		got, err := s.store.GetConfigSnapshotByMode(ctx, ref, want.ExecutionMode)
		if err != nil {
			return false, err
		}
		if err := validateConfigSnapshotForAgent(got, existing.Config); err != nil {
			return false, err
		}
		if got.Ref != want.Ref || got.AgentID != want.AgentID || got.Version != want.Version ||
			got.ExecutionMode != want.ExecutionMode || got.ConfigHash != want.ConfigHash {
			return false, nil
		}
	}
	return true, nil
}
