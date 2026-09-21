package app

import (
	"context"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/agentregistry"
	"github.com/AGenUI/agenui-studio/harness/internal/gatewaycontract"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// registryGatewayTargetResolver is the only Registry -> Agent Gateway adapter.
// Provider kind and endpoint are selected from frozen
// Registry facts; model-generated task arguments cannot influence routing.
type registryGatewayTargetResolver struct {
	registry runtimeAgentRegistry
	runs     storage.RunStore
	ids      observability.IDGenerator
	clock    func() time.Time
}

func (r registryGatewayTargetResolver) Resolve(ctx context.Context, req agentgateway.ResolveTargetRequest) (agentgateway.ResolvedTarget, error) {
	if r.registry == nil || r.runs == nil {
		return agentgateway.ResolvedTarget{}, agentgateway.ErrTargetResolveFailed
	}
	parent, err := r.runs.Get(ctx, req.ParentRunID)
	if err != nil {
		return agentgateway.ResolvedTarget{}, err
	}
	if parent.TenantID != req.TenantID || parent.SessionID != req.SessionID || parent.AgentID != req.ParentAgentID ||
		parent.ConfigSnapshotRef != req.ParentConfigSnapshotRef {
		return agentgateway.ResolvedTarget{}, agentgateway.ErrTargetUnauthorized
	}
	if parent.ParentRunID != "" {
		return agentgateway.ResolvedTarget{}, agentgateway.ErrNestingLimit
	}
	resolved, err := r.registry.ResolveGatewayTarget(ctx, agentregistry.ResolveGatewayTargetRequest{
		TenantID:      req.TenantID,
		ParentAgentID: req.ParentAgentID, ParentAgentVersion: req.ParentAgentVersion,
		ParentConfigSnapshotRef: req.ParentConfigSnapshotRef, ParentConfigHash: req.ParentConfigHash,
		SubAgentRef: req.SubAgentRef,
	})
	if err != nil {
		return agentgateway.ResolvedTarget{}, err
	}
	ids := r.ids
	if ids == nil {
		ids = observability.NewULIDGenerator("gateway_binding")
	}
	clock := r.clock
	if clock == nil {
		clock = time.Now
	}
	target := agentgateway.ResolvedTarget{
		Definition: resolved.Effective.Definition,
		Provider:   resolved.Gateway.ProviderKind,
		Plugins:    cloneGatewayPluginConfigs(resolved.Gateway.Plugins),
		Binding: agentgateway.ProviderBinding{
			SchemaVersion: agentgateway.ProviderBindingSchemaV2,
			BindingID:     ids.NewRequestID(), ProviderKind: resolved.Gateway.ProviderKind,
			TargetAgentID:     resolved.Effective.Definition.AgentID,
			TargetVersion:     resolved.Effective.Definition.Version,
			ConfigSnapshotRef: resolved.Effective.ConfigSnapshotRef,
			ConfigHash:        resolved.Effective.ConfigHash,
			CreatedAt:         clock(),
		},
	}
	if resolved.Gateway.ProviderKind == gatewaycontract.SubAgentProviderRemoteA2A && resolved.Gateway.Remote != nil {
		remote, normalizeErr := agentgateway.NormalizeRemoteAgent(agentgateway.RemoteAgent{
			AgentRef: req.SubAgentRef, Protocol: agentgateway.A2AProtocol,
			BaseURL: resolved.Gateway.Remote.BaseURL, CardPath: resolved.Gateway.Remote.CardPath,
			Transport: resolved.Gateway.Remote.Transport, AuthRef: resolved.Gateway.Remote.AuthRef,
			AllowedEndpointOrigins: append([]string(nil), resolved.Gateway.Remote.AllowedEndpointOrigins...),
			Timeout:                time.Duration(resolved.Gateway.Remote.TimeoutMS) * time.Millisecond,
		})
		if normalizeErr != nil {
			return agentgateway.ResolvedTarget{}, normalizeErr
		}
		target.Remote = &remote
	}
	return target, nil
}

func cloneGatewayPluginConfigs(input []gatewaycontract.GatewayPluginConfig) []gatewaycontract.GatewayPluginConfig {
	out := make([]gatewaycontract.GatewayPluginConfig, len(input))
	for i := range input {
		out[i] = input[i].Clone()
	}
	return out
}

var _ agentgateway.TargetResolver = registryGatewayTargetResolver{}
