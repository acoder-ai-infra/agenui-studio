package orchestrator

import (
	"github.com/AGenUI/agenui-studio/harness/internal/agentbinding"
	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/dispatcher"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/scheduler"
)

// RunRequest 只补充 Binding 解析和 Runtime.Run 所需的入口事实。
// Agent、版本、执行范式和配置快照统一来自 EffectiveBinding。
type RunRequest struct {
	BindingRequest     agentbinding.BindingRequest `json:"binding_request"`
	ParentRunID        string                      `json:"parent_run_id,omitempty"`
	Input              []agentruntime.Message      `json:"input,omitempty"`
	ContextSnapshotRef string                      `json:"context_snapshot_ref,omitempty"`
	ScopedData         agentruntime.ScopedData     `json:"scoped_data,omitempty"`
	UserID             string                      `json:"user_id,omitempty"`
	TenantID           string                      `json:"tenant_id"`
	Trace              observability.TraceContext  `json:"trace"`
	Metadata           map[string]string           `json:"metadata,omitempty"`
	// ExternalFragments 由 dispatcher 从 OpenTurnResult 透传，最终进入
	// agentruntime.RunRequest.ExternalFragments 参与 ModelContext 装配。
	ExternalFragments []contextpkg.ContextFragment  `json:"external_fragments,omitempty"`
	ResultVisibility  observability.EventVisibility `json:"result_visibility,omitempty"`
	Policy            dispatcher.DispatchPolicy     `json:"dispatch_policy"`
	Priority          scheduler.Priority            `json:"priority,omitempty"`
}

const PreRuntimeFailureSchemaVersion = "harness.pre_runtime_failure.v1"

// PreRuntimeFailure records a terminal failure after Binding succeeded but
// before Runtime.Run accepted the request.
type PreRuntimeFailure struct {
	SchemaVersion string                   `json:"schema_version"`
	RunID         string                   `json:"run_id"`
	BindingID     string                   `json:"binding_id"`
	Stage         string                   `json:"stage"`
	Error         observability.EventError `json:"error"`
}
