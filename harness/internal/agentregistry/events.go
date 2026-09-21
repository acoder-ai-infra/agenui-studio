package agentregistry

import (
	"context"
	"fmt"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type RegistryEvent struct {
	EventID       string `json:"event_id"`
	TraceID       string `json:"trace_id,omitempty"`
	SpanID        string `json:"span_id,omitempty"`
	ParentSpanID  string `json:"parent_span_id,omitempty"`
	RequestID     string `json:"request_id,omitempty"`
	RunID         string `json:"run_id,omitempty"`
	Tenant        string `json:"tenant,omitempty"`
	Type          string `json:"type"`
	AgentID       string `json:"agent_id,omitempty"`
	Version       string `json:"version,omitempty"`
	SelectionHash string `json:"selection_hash,omitempty"`
	// BindingHash 仅保留早期审计数据的反序列化兼容。
	BindingHash string    `json:"binding_hash,omitempty"`
	ConfigHash  string    `json:"config_hash,omitempty"`
	Actor       string    `json:"actor,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
	DiffSummary string    `json:"diff_summary,omitempty"`
}

const (
	EventConfigCommitted         = "config_committed"
	EventSchemaValidated         = "schema_validated"
	EventDependencyResolved      = "dependency_resolved"
	EventPolicyValidated         = "policy_validated"
	EventRuntimeDryRunPassed     = "runtime_dry_run_passed"
	EventRuntimeDryRunSkipped    = "runtime_dry_run_skipped"
	EventEvalGatePassed          = "eval_gate_passed"
	EventEvalGateSkipped         = "eval_gate_skipped"
	EventAgentRegistered         = "agent_registered"
	EventAgentEnabled            = "agent_enabled"
	EventAgentDisabled           = "agent_disabled"
	EventGrayPercentChanged      = "gray_percent_changed"
	EventRollbackTriggered       = "rollback_triggered"
	EventEffectiveConfigResolved = "effective_config_resolved"
	EventConfigDriftDetected     = "config_drift_detected"
)

func (s *Service) emitRegistryEvent(ctx context.Context, event RegistryEvent) error {
	event = s.prepareRegistryEvent(ctx, event)
	// 解析类事件属于热路径 App Log；只有发布治理事实进入 durable audit。
	if !requiresDurableRegistryAudit(event.Type) {
		s.logRegistryEvent(ctx, event)
		return nil
	}
	if err := s.store.AppendRegistryEvent(ctx, event); err != nil {
		s.logger.Error(ctx, "registry audit write failed", err,
			observability.String("event_id", event.EventID),
			observability.String("event_type", event.Type),
		)
		return fmt.Errorf("%w: %v", ErrAuditWrite, err)
	}
	s.logRegistryEvent(ctx, event)
	return nil
}

func requiresDurableRegistryAudit(eventType string) bool {
	switch eventType {
	case EventConfigCommitted, EventEvalGatePassed, EventEvalGateSkipped, EventAgentRegistered,
		EventAgentEnabled, EventAgentDisabled, EventGrayPercentChanged, EventRollbackTriggered:
		return true
	default:
		return false
	}
}

func (s *Service) prepareRegistryEvent(ctx context.Context, event RegistryEvent) RegistryEvent {
	if event.Timestamp.IsZero() {
		event.Timestamp = s.now()
	}
	tc := observability.MustTraceContext(ctx)
	if event.TraceID == "" {
		event.TraceID = tc.TraceID
	}
	if event.SpanID == "" {
		event.SpanID = tc.SpanID
	}
	if event.ParentSpanID == "" {
		event.ParentSpanID = tc.ParentSpanID
	}
	if event.RequestID == "" {
		event.RequestID = tc.RequestID
	}
	if event.RunID == "" {
		event.RunID = tc.RunID
	}
	if event.Tenant == "" {
		event.Tenant = tc.TenantID
	}
	if event.AgentID == "" {
		event.AgentID = tc.AgentID
	}
	if event.Version == "" {
		event.Version = tc.AgentVersion
	}
	if event.EventID == "" {
		event.EventID = s.ids.NewEventID()
	}
	return event
}

func (s *Service) logRegistryEvent(ctx context.Context, event RegistryEvent) {
	selectionHash := event.SelectionHash
	if selectionHash == "" {
		selectionHash = event.BindingHash
	}
	s.logger.Info(ctx, "registry event",
		observability.String("logger", "registry_event"),
		observability.String("event_id", event.EventID),
		observability.String("event_type", event.Type),
		observability.String("agent_id", event.AgentID),
		observability.String("version", event.Version),
		observability.String("selection_hash", selectionHash),
		observability.String("config_hash", event.ConfigHash),
		observability.String("actor", event.Actor),
		observability.String("reason", event.Reason),
	)
}
