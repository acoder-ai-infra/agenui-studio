package agentregistry

import (
	"context"
)

type ResolveGatewayTargetRequest struct {
	TenantID                string
	ParentAgentID           string
	ParentAgentVersion      string
	ParentConfigSnapshotRef string
	ParentConfigHash        string
	SubAgentRef             string
	SubAgentVersion         string
}

type ResolvedGatewayTarget struct {
	Effective EffectiveConfig
	Gateway   EffectiveGatewayTarget
}

// ResolveGatewayTarget validates authorization against the immutable parent
// config snapshot, then resolves and freezes the target registration. It never
// lets the caller choose provider kind or remote endpoint.
func (s *Service) ResolveGatewayTarget(ctx context.Context, req ResolveGatewayTargetRequest) (ResolvedGatewayTarget, error) {
	return resolveGatewayTarget(ctx, s, req)
}

type gatewayRegistry interface {
	ResolveEffectiveConfig(context.Context, ResolveRequest) (EffectiveConfig, error)
	GetConfigSnapshotForTenant(context.Context, string, string) (EffectiveConfig, error)
}

func resolveGatewayTarget(ctx context.Context, registry gatewayRegistry, req ResolveGatewayTargetRequest) (ResolvedGatewayTarget, error) {
	if req.ParentAgentID == "" || req.ParentAgentVersion == "" || req.ParentConfigSnapshotRef == "" || req.ParentConfigHash == "" || req.SubAgentRef == "" {
		return ResolvedGatewayTarget{}, newError(CodeInvalidConfig, "gateway_target", "complete parent snapshot identity and sub_agent_ref are required")
	}
	parent, err := registry.GetConfigSnapshotForTenant(ctx, req.TenantID, req.ParentConfigSnapshotRef)
	if err != nil {
		return ResolvedGatewayTarget{}, err
	}
	if parent.Definition.AgentID != req.ParentAgentID || parent.Definition.Version != req.ParentAgentVersion ||
		parent.ConfigSnapshotRef != req.ParentConfigSnapshotRef || parent.ConfigHash != req.ParentConfigHash {
		return ResolvedGatewayTarget{}, wrapError(CodeConfigDrift, "parent_config_snapshot_ref", "parent Agent snapshot identity mismatch", ErrConfigDrift)
	}
	if !containsString(parent.Definition.SubAgentRefs, req.SubAgentRef) {
		return ResolvedGatewayTarget{}, newError(CodePolicyViolation, "sub_agent_ref", "target is not authorized by the parent Agent snapshot")
	}
	effective, err := registry.ResolveEffectiveConfig(ctx, ResolveRequest{AgentID: req.SubAgentRef, Version: req.SubAgentVersion, Tenant: req.TenantID})
	if err != nil {
		return ResolvedGatewayTarget{}, err
	}
	if effective.Gateway == nil {
		return ResolvedGatewayTarget{}, newError(CodeInvalidConfig, "gateway", "target Agent is not registered for Gateway invocation")
	}
	gateway := cloneEffectiveGatewayTarget(effective.Gateway)
	return ResolvedGatewayTarget{Effective: effective, Gateway: *gateway}, nil
}
