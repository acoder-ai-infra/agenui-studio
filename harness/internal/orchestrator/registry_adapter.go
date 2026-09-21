package orchestrator

import (
	"context"
	"errors"

	"github.com/AGenUI/agenui-studio/harness/internal/agentbinding"
	"github.com/AGenUI/agenui-studio/harness/internal/agentregistry"
	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/executionmode"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// registryConfigResolver 将 Registry 的完整快照解析收窄为 Binding 所需事实。
// Orchestrator 因而不感知 Registry 的存储、灰度或能力解析细节。
type registryConfigResolver struct {
	registry agentregistry.Registry
}

func (r registryConfigResolver) ResolveConfig(ctx context.Context, req agentbinding.ConfigResolveRequest) (agentbinding.ResolvedConfig, error) {
	trace := observability.MustTraceContext(ctx)
	effective, err := r.registry.ResolveEffectiveConfig(ctx, agentregistry.ResolveRequest{
		AgentID:       req.Selection.AgentID,
		Version:       req.Selection.AgentVersion,
		SessionID:     req.SessionID,
		RunID:         req.RunID,
		ExecutionMode: req.Selection.Mode,
		Tenant:        trace.TenantID,
		RequestID:     trace.RequestID,
		SelectionHash: contentHash(req.Selection),
	})
	if err != nil {
		return agentbinding.ResolvedConfig{}, mapRegistryError(err, req.Selection)
	}

	mode, err := executionmode.FromRuntimeMode(effective.Definition.Runtime.Mode)
	if err != nil {
		return agentbinding.ResolvedConfig{}, agentbinding.NewError(agentbinding.StageConfigResolve, agentbinding.CodeExecutionModeUnsupported, false, err)
	}
	target, err := targetFromDefinition(mode, effective.Definition)
	if err != nil {
		return agentbinding.ResolvedConfig{}, err
	}
	return agentbinding.ResolvedConfig{
		Definition:             effective.Definition,
		ExecutionMode:          mode,
		Target:                 target,
		ConfigSnapshotRef:      effective.ConfigSnapshotRef,
		ConfigHash:             effective.ConfigHash,
		CapabilitySnapshotRefs: capabilityManifestRefs(effective),
	}, nil
}

func targetFromDefinition(mode executionmode.Mode, definition agentruntime.AgentDefinition) (agentbinding.Target, error) {
	switch mode {
	case executionmode.DirectAction, executionmode.SingleAgent, executionmode.DeepAgent:
		return agentbinding.Target{
			Kind:    agentbinding.TargetAgent,
			Ref:     definition.AgentID,
			Version: definition.Version,
		}, nil
	case executionmode.Workflow:
		if definition.Workflow == nil || definition.Workflow.WorkflowID == "" {
			return agentbinding.Target{}, agentbinding.NewError(agentbinding.StageConfigResolve, agentbinding.CodeDefinitionInvalid, false, nil)
		}
		return agentbinding.Target{
			Kind:    agentbinding.TargetWorkflow,
			Ref:     definition.Workflow.WorkflowID,
			Version: definition.Version,
			Hash:    contentHash(definition.Workflow),
		}, nil
	case executionmode.Graph:
		if definition.Graph == nil || definition.Graph.GraphID == "" || definition.Graph.StateSchemaRef == "" {
			return agentbinding.Target{}, agentbinding.NewError(agentbinding.StageConfigResolve, agentbinding.CodeDefinitionInvalid, false, nil)
		}
		return agentbinding.Target{
			Kind:           agentbinding.TargetGraph,
			Ref:            definition.Graph.GraphID,
			Version:        definition.Version,
			Hash:           contentHash(definition.Graph),
			StateSchemaRef: definition.Graph.StateSchemaRef,
		}, nil
	default:
		return agentbinding.Target{}, agentbinding.NewError(agentbinding.StageConfigResolve, agentbinding.CodeExecutionModeUnsupported, false, nil)
	}
}

func capabilityManifestRefs(effective agentregistry.EffectiveConfig) []string {
	if effective.ConfigSnapshotRef == "" {
		return nil
	}
	return []string{effective.ConfigSnapshotRef + "#capabilities"}
}

func mapRegistryError(err error, selection agentbinding.Selection) error {
	var registryErr *agentregistry.RegistryError
	if !errors.As(err, &registryErr) {
		return agentbinding.NewError(agentbinding.StageConfigResolve, agentbinding.CodeConfigResolveFailed, true, err)
	}
	code := agentbinding.CodeConfigInvalid
	retryable := false
	switch registryErr.Code {
	case agentregistry.CodeNotFound:
		code = agentbinding.CodeAgentNotFound
		if selection.AgentVersion != "" {
			code = agentbinding.CodeAgentVersionUnavailable
		}
	case agentregistry.CodeDisabled:
		code = agentbinding.CodeAgentDisabled
	case agentregistry.CodeDependencyMissing:
		code = agentbinding.CodeCapabilitySnapshotFailed
	case agentregistry.CodeConflict:
		// Registry 的 conflict 表示版本/存储并发冲突，不能触发 Agent fallback。
		code = agentbinding.CodeConfigResolveFailed
		retryable = true
	case agentregistry.CodeLoadFailed:
		code = agentbinding.CodeConfigResolveFailed
		retryable = true
	case agentregistry.CodeConfigDrift:
		// 候选配置已漂移，必须重新决策，不能降级到另一个 Agent。
		code = agentbinding.CodeConfigInvalid
	case agentregistry.CodeInvalidConfig:
		if registryErr.Field == "execution_mode" {
			code = agentbinding.CodeExecutionModeNotAllowed
			if errors.Is(registryErr, executionmode.ErrUnsupported) {
				code = agentbinding.CodeExecutionModeUnsupported
			}
		}
	case agentregistry.CodePolicyViolation, agentregistry.CodeRuntimeDryRunFailed, agentregistry.CodeEvalGateFailed:
		code = agentbinding.CodeConfigInvalid
	default:
		code = agentbinding.CodeConfigResolveFailed
		retryable = true
	}
	return agentbinding.NewError(agentbinding.StageConfigResolve, code, retryable, err)
}

var _ agentbinding.ConfigResolver = registryConfigResolver{}
