package agentregistry

import (
	"context"
	"errors"
	"strings"
)

// ReleaseRegistry overlays tenant/environment release pointers on the static
// Registry. It only affects fresh resolution. Frozen snapshot reads use the
// immutable prepared projection stored with the selected version.
type ReleaseRegistry struct {
	base        *Service
	control     AgentConfigControlStore
	environment ConfigEnvironment
}

func NewReleaseRegistry(base *Service, control AgentConfigControlStore, environment ConfigEnvironment) (*ReleaseRegistry, error) {
	if base == nil || control == nil || !environment.valid() {
		return nil, invalidConfigControl("base registry, control store and environment are required")
	}
	return &ReleaseRegistry{base: base, control: control, environment: environment}, nil
}

func (r *ReleaseRegistry) ResolveEffectiveConfig(ctx context.Context, req ResolveRequest) (EffectiveConfig, error) {
	if req.Tenant == "" {
		return r.base.ResolveEffectiveConfig(ctx, req)
	}
	explicitVersion := req.Version != ""
	versionID := req.Version
	if versionID == "" {
		release, err := r.control.GetRelease(ctx, req.Tenant, r.environment, req.AgentID)
		if errors.Is(err, ErrAgentConfigControlNotFound) {
			return r.base.ResolveEffectiveConfig(ctx, req)
		}
		if err != nil {
			return EffectiveConfig{}, err
		}
		versionID = release.Version
	}
	version, err := r.control.GetVersion(ctx, req.Tenant, req.AgentID, versionID)
	if explicitVersion && errors.Is(err, ErrAgentConfigControlNotFound) {
		return r.base.ResolveEffectiveConfig(ctx, req)
	}
	if err != nil {
		return EffectiveConfig{}, err
	}
	if version.Config.Status == AgentStatusDisabled && !req.IncludeDisabled {
		return EffectiveConfig{}, wrapError(CodeDisabled, "status", "agent is disabled", ErrAgentDisabled)
	}
	if version.Prepared == nil {
		return EffectiveConfig{}, invalidConfigControl("released Agent version has no prepared runtime projection")
	}
	if req.ExecutionMode != "" {
		for i := range version.Prepared.ConfigSnapshots {
			snapshot := version.Prepared.ConfigSnapshots[i]
			if snapshot.ExecutionMode == req.ExecutionMode {
				return cloneEffectiveConfig(snapshot.Effective), nil
			}
		}
		return EffectiveConfig{}, newError(CodeInvalidConfig, "execution_mode", "released Agent does not allow the requested execution mode")
	}
	return cloneEffectiveConfig(version.Prepared.Effective), nil
}

func (r *ReleaseRegistry) GetCapabilityCard(ctx context.Context, agentID, version string) (CapabilityCard, error) {
	return r.base.GetCapabilityCard(ctx, agentID, version)
}

func (r *ReleaseRegistry) GetConfigSnapshotForTenant(ctx context.Context, tenantID, ref string) (EffectiveConfig, error) {
	agentID, version, ok := parseConfigSnapshotRef(ref)
	if tenantID == "" || !ok {
		return r.base.GetConfigSnapshot(ctx, ref)
	}
	record, err := r.control.GetVersion(ctx, tenantID, agentID, version)
	if errors.Is(err, ErrAgentConfigControlNotFound) {
		return r.base.GetConfigSnapshot(ctx, ref)
	}
	if err != nil {
		return EffectiveConfig{}, err
	}
	if record.Prepared == nil {
		return EffectiveConfig{}, ErrConfigSnapshotMissing
	}
	for i := range record.Prepared.ConfigSnapshots {
		if record.Prepared.ConfigSnapshots[i].Ref == ref {
			return cloneEffectiveConfig(record.Prepared.ConfigSnapshots[i].Effective), nil
		}
	}
	if record.Prepared.Effective.ConfigSnapshotRef == ref {
		return cloneEffectiveConfig(record.Prepared.Effective), nil
	}
	return EffectiveConfig{}, ErrConfigSnapshotMissing
}

func (r *ReleaseRegistry) ResolveGatewayTarget(ctx context.Context, req ResolveGatewayTargetRequest) (ResolvedGatewayTarget, error) {
	if req.SubAgentVersion == "" && req.TenantID != "" {
		agentID, versionID, ok := parseConfigSnapshotRef(req.ParentConfigSnapshotRef)
		if ok {
			parent, err := r.control.GetVersion(ctx, req.TenantID, agentID, versionID)
			if errors.Is(err, ErrAgentConfigControlNotFound) {
				return resolveGatewayTarget(ctx, r, req)
			}
			if err != nil {
				return ResolvedGatewayTarget{}, err
			}
			if parent.Config.Status == AgentStatusDisabled {
				return ResolvedGatewayTarget{}, wrapError(CodeDisabled, "status", "agent is disabled", ErrAgentDisabled)
			}
			if parent.Prepared != nil {
				req.SubAgentVersion = parent.Prepared.SubAgentVersions[req.SubAgentRef]
			}
		}
	}
	return resolveGatewayTarget(ctx, r, req)
}

func parseConfigSnapshotRef(ref string) (agentID, version string, ok bool) {
	const prefix = "agent-config://"
	parts := strings.Split(strings.TrimPrefix(ref, prefix), "/")
	if !strings.HasPrefix(ref, prefix) || len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

var _ Registry = (*ReleaseRegistry)(nil)
