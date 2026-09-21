// Package agentgateway implements the Agent Gateway: the SubAgentInvoker port
// for platform Agent calls. It routes an authorized invocation to either the
// local scheduled provider (local_agent) or the remote A2A provider
// (remote_a2a) through the fixed core plugins followed by target-configured
// plugins.
//
// A2A SDK types are confined to the remote provider; they never appear in this
// package's public plugin contracts, in agentruntime, or in the
// Registry.
package agentgateway

import (
	"context"
	"encoding/json"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/gatewaycontract"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

const (
	GatewayTaskSchemaVersion = "harness.gateway_task.v2"
	ProviderBindingSchemaV2  = "harness.provider_binding.v2"
)

type ProviderBinding struct {
	SchemaVersion     string                               `json:"schema_version"`
	BindingID         string                               `json:"binding_id"`
	ProviderKind      gatewaycontract.SubAgentProviderKind `json:"provider_kind"`
	TargetAgentID     string                               `json:"target_agent_id"`
	TargetVersion     string                               `json:"target_version"`
	ConfigSnapshotRef string                               `json:"config_snapshot_ref"`
	ConfigHash        string                               `json:"config_hash"`
	RemoteCardHash    string                               `json:"remote_card_hash,omitempty"`
	CreatedAt         time.Time                            `json:"created_at"`
}

// ResolvedTarget is produced by the Registry-owned resolver. It proves both
// that the parent snapshot authorized the reference and that the target and
// provider binding come from one immutable configuration view.
type ResolvedTarget struct {
	Definition agentruntime.AgentDefinition
	Provider   gatewaycontract.SubAgentProviderKind
	Plugins    []gatewaycontract.GatewayPluginConfig
	Binding    ProviderBinding
	Remote     *RemoteAgent
}

type ResolveTargetRequest struct {
	TenantID                string
	SessionID               string
	ParentRunID             string
	ParentAgentID           string
	ParentAgentVersion      string
	ParentConfigSnapshotRef string
	ParentConfigHash        string
	SubAgentRef             string
}

type TargetResolver interface {
	Resolve(ctx context.Context, req ResolveTargetRequest) (ResolvedTarget, error)
}

type LocalProvider interface {
	Invoke(ctx context.Context, req LocalInvocationRequest) (agentruntime.SubAgentInvocationResult, error)
}

type LocalInvocationRequest struct {
	Invocation AuthorizedInvocation
	Target     ResolvedTarget
	ScopedData agentruntime.ScopedData
	Depth      int
}

// GatewayProviderPhase records how far a provider invocation progressed. Only
// the Service (via provider adapters) advances it; plugins can only read it.
type GatewayProviderPhase string

const (
	GatewayProviderNotStarted     GatewayProviderPhase = "not_started"
	GatewayProviderEntered        GatewayProviderPhase = "entered"
	GatewayProviderRequestWritten GatewayProviderPhase = "request_written"
	GatewayProviderOutcomeKnown   GatewayProviderPhase = "outcome_known"
)

// GatewayProviderOutcome is the read-only provider result view exposed to
// plugins, distinct from the Gateway's final outcome.
type GatewayProviderOutcome struct {
	Phase     GatewayProviderPhase
	Succeeded bool
	ErrorCode string
}

// AuthorizedInvocation is the read-only, already-authorized invocation view
// handed to providers and plugins. It cannot be used to change routing, tenant,
// credentials, or the frozen target binding.
type AuthorizedInvocation struct {
	TenantID      string
	SessionID     string
	ParentRunID   string
	ParentAgentID string
	SubAgentRef   string
	ProviderKind  gatewaycontract.SubAgentProviderKind
	InvocationID  string // SubAgentInvocationRequest.TaskID
	Description   string
	AttemptID     string
	Binding       ProviderBinding
	InputParts    []contextpkg.ContentPart
	Trace         observability.TraceContext
}

// GatewayInvocationFacts is implemented by the Service. Plugins may only append
// allowlist audit fields (before the provider is entered) and read the provider
// outcome; they cannot rewrite the authorized request, credentials, or outcome.
type GatewayInvocationFacts interface {
	AddAuditField(key, value string) error
	ProviderOutcome() GatewayProviderOutcome
}

// GatewayNext continues the current request through the remaining plugins to
// the provider. A plugin must call it synchronously at most once.
type GatewayNext func(ctx context.Context) (agentruntime.SubAgentInvocationResult, error)

// GatewayPlugin combines the trusted implementation and its request binding.
// Templates live in the process catalog; pluginChain copies a template and
// binds one frozen config object for each request.
type GatewayPlugin struct {
	id       string
	config   json.RawMessage
	policy   pluginFailurePolicy
	validate func(json.RawMessage) error
	invoke   func(
		context.Context,
		json.RawMessage,
		AuthorizedInvocation,
		GatewayInvocationFacts,
		GatewayNext,
	) (agentruntime.SubAgentInvocationResult, error)
}

func (p GatewayPlugin) Invoke(
	ctx context.Context,
	req AuthorizedInvocation,
	facts GatewayInvocationFacts,
	next GatewayNext,
) (agentruntime.SubAgentInvocationResult, error) {
	if p.invoke == nil {
		return agentruntime.SubAgentInvocationResult{}, newGatewayError(CodePluginFailed, "nil plugin implementation", nil)
	}
	return p.invoke(ctx, cloneRawMessage(p.config), req, facts, next)
}
