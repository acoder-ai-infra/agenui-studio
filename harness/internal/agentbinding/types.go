package agentbinding

import (
	"context"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/executionmode"
)

const BindingSchemaVersion = "harness.agent_binding.v1"

type Source string

const (
	SourceRequestParam   Source = "request_param"
	SourceSessionDefault Source = "session_default"
	SourceConfigDefault  Source = "config_default"
	SourceControlPlane   Source = "control_plane"
)

func (s Source) Validate() error {
	switch s {
	case SourceRequestParam, SourceSessionDefault, SourceConfigDefault, SourceControlPlane:
		return nil
	default:
		return NewError(StageSourceSelection, CodeSourceInvalid, false, nil)
	}
}

type TargetKind string

const (
	TargetAgent    TargetKind = "agent"
	TargetWorkflow TargetKind = "workflow"
	TargetGraph    TargetKind = "graph"
)

// Target 只描述不可变执行目标；真实定义仍由 Registry 快照提供。
type Target struct {
	Kind           TargetKind `json:"kind"`
	Ref            string     `json:"ref"`
	Version        string     `json:"version,omitempty"`
	Hash           string     `json:"hash,omitempty"`
	StateSchemaRef string     `json:"state_schema_ref,omitempty"`
}

func (t Target) IsZero() bool {
	return t == (Target{})
}

type Selection struct {
	AgentID      string             `json:"agent_id"`
	AgentVersion string             `json:"agent_version,omitempty"`
	Mode         executionmode.Mode `json:"execution_mode,omitempty"`
	Target       Target             `json:"target,omitempty"`
}

type ControlAction string

const (
	ControlForce   ControlAction = "force"
	ControlDeny    ControlAction = "deny"
	ControlDefault ControlAction = "default"
)

// ControlRule 是控制面的确定性规则，不承载模型分类或候选排序。
// Deny 的 Selection 为空时表示拒绝所有候选；非空字段按精确条件匹配。
type ControlRule struct {
	Action    ControlAction `json:"action"`
	Selection *Selection    `json:"selection,omitempty"`
	Ref       string        `json:"ref"`
	Revision  string        `json:"revision"`
}

type BindingRequest struct {
	SchemaVersion  string       `json:"schema_version,omitempty"`
	BindingID      string       `json:"binding_id"`
	SessionID      string       `json:"session_id"`
	RunID          string       `json:"run_id"`
	Request        *Selection   `json:"request_selection,omitempty"`
	SessionDefault *Selection   `json:"session_default,omitempty"`
	ConfigDefault  *Selection   `json:"config_default,omitempty"`
	Control        *ControlRule `json:"control_rule,omitempty"`
	Fallback       *Selection   `json:"fallback_selection,omitempty"`
	CreatedAt      time.Time    `json:"created_at,omitempty"`
}

type FallbackFact struct {
	Applied    bool      `json:"applied"`
	From       Selection `json:"from,omitempty"`
	To         Selection `json:"to,omitempty"`
	ReasonCode ErrorCode `json:"reason_code,omitempty"`
}

type EffectiveBinding struct {
	SchemaVersion          string             `json:"schema_version"`
	BindingID              string             `json:"binding_id"`
	SessionID              string             `json:"session_id"`
	RunID                  string             `json:"run_id"`
	AgentID                string             `json:"agent_id"`
	AgentVersion           string             `json:"agent_version"`
	ExecutionMode          executionmode.Mode `json:"execution_mode"`
	Target                 Target             `json:"target"`
	ConfigSnapshotRef      string             `json:"config_snapshot_ref"`
	ConfigHash             string             `json:"config_hash"`
	CapabilitySnapshotRefs []string           `json:"capability_snapshot_refs,omitempty"`
	Source                 Source             `json:"source"`
	ControlRuleRef         string             `json:"control_rule_ref,omitempty"`
	ControlRuleRevision    string             `json:"control_rule_revision,omitempty"`
	Fallback               FallbackFact       `json:"fallback"`
	BindingHash            string             `json:"binding_hash"`
	CreatedAt              time.Time          `json:"created_at"`
}

type ConfigResolveRequest struct {
	Selection Selection `json:"selection"`
	SessionID string    `json:"session_id"`
	RunID     string    `json:"run_id"`
}

type ResolvedConfig struct {
	Definition             agentruntime.AgentDefinition `json:"definition"`
	ExecutionMode          executionmode.Mode           `json:"execution_mode"`
	Target                 Target                       `json:"target"`
	ConfigSnapshotRef      string                       `json:"config_snapshot_ref"`
	ConfigHash             string                       `json:"config_hash"`
	CapabilitySnapshotRefs []string                     `json:"capability_snapshot_refs,omitempty"`
}

// ConfigResolver 是 Registry 面向 Binding 的窄口；Binding 不依赖具体存储或 Registry 实现。
type ConfigResolver interface {
	ResolveConfig(ctx context.Context, req ConfigResolveRequest) (ResolvedConfig, error)
}

type Result struct {
	Binding    EffectiveBinding             `json:"binding"`
	Definition agentruntime.AgentDefinition `json:"definition"`
}
