package agentgateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

const (
	defaultTracingPluginID       = "default_tracer"
	defaultAuditPluginID         = "canonical_event_auditor"
	defaultObservabilityPluginID = "canonical_observer"
	basicValidatorPluginID       = "basic_validator"
)

// fixedCorePluginIDs is the non-configurable outer chain. Configured plugins
// are always appended after these entries in their author-written order.
var fixedCorePluginIDs = [...]string{
	defaultTracingPluginID,
	defaultAuditPluginID,
	defaultObservabilityPluginID,
}

func isFixedCorePlugin(id string) bool {
	for _, coreID := range fixedCorePluginIDs {
		if id == coreID {
			return true
		}
	}
	return false
}

// defaultPluginCatalog constructs the trusted process-lifetime plugin
// templates. Only their immutable functions and policy are shared; config is
// copied into a request-scoped GatewayPlugin by pluginChain.
func defaultPluginCatalog(tracer observability.TraceProvider) map[string]GatewayPlugin {
	if tracer == nil {
		tracer = observability.NewNoopTracer("agentgateway")
	}
	return map[string]GatewayPlugin{
		defaultTracingPluginID: {
			id: defaultTracingPluginID, policy: pluginFailOpen,
			validate: validateEmptyPluginConfig, invoke: tracePlugin(tracer),
		},
		defaultAuditPluginID: {
			id: defaultAuditPluginID, policy: pluginFailClosed,
			validate: validateEmptyPluginConfig, invoke: invokeCanonicalEventAuditor,
		},
		defaultObservabilityPluginID: {
			id: defaultObservabilityPluginID, policy: pluginFailOpen,
			validate: validateEmptyPluginConfig, invoke: invokeCanonicalObserver,
		},
		basicValidatorPluginID: {
			id: basicValidatorPluginID, policy: pluginFailClosed,
			validate: validateEmptyPluginConfig, invoke: invokeBasicValidator,
		},
	}
}

func validateEmptyPluginConfig(config json.RawMessage) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(config, &object); err != nil || object == nil {
		return errors.New("config must be an object")
	}
	if len(object) != 0 {
		return errors.New("config must be an empty object")
	}
	return nil
}

func invokeBasicValidator(
	ctx context.Context,
	_ json.RawMessage,
	req AuthorizedInvocation,
	_ GatewayInvocationFacts,
	next GatewayNext,
) (agentruntime.SubAgentInvocationResult, error) {
	if strings.TrimSpace(req.SubAgentRef) == "" || strings.TrimSpace(req.TenantID) == "" ||
		strings.TrimSpace(req.SessionID) == "" || strings.TrimSpace(req.InvocationID) == "" {
		return agentruntime.SubAgentInvocationResult{}, newGatewayError(CodePluginFailed, "invalid invocation identity", nil)
	}
	if !utf8.ValidString(req.Description) {
		return agentruntime.SubAgentInvocationResult{}, newGatewayError(CodePluginFailed, "description not valid utf-8", nil)
	}
	result, err := next(ctx)
	if err != nil {
		return result, err
	}
	if result.Content != "" && !utf8.ValidString(result.Content) {
		return agentruntime.SubAgentInvocationResult{}, newGatewayError(CodePluginPostCallFailed, "result content not valid utf-8", nil)
	}
	return result, nil
}

func invokeCanonicalObserver(
	ctx context.Context,
	_ json.RawMessage,
	req AuthorizedInvocation,
	facts GatewayInvocationFacts,
	next GatewayNext,
) (agentruntime.SubAgentInvocationResult, error) {
	logger := observability.LoggerFrom(ctx, observability.NoopLogger{})
	start := time.Now()
	result, err := next(ctx)
	outcome := facts.ProviderOutcome()
	fields := []observability.Field{
		observability.String("provider_kind", string(req.ProviderKind)),
		observability.String("sub_agent_ref", req.SubAgentRef),
		observability.String("invocation_id", req.InvocationID),
		observability.String("gateway_config_hash", req.Binding.ConfigHash),
		observability.String("provider_phase", string(outcome.Phase)),
		observability.String("duration", time.Since(start).String()),
	}
	if err != nil {
		logger.Warn(ctx, "gateway invoke failed", append(fields, observability.String("gateway_error_code", SafeErrorCode(err)))...)
	} else {
		logger.Debug(ctx, "gateway invoke completed", fields...)
	}
	return result, err
}

func tracePlugin(tracer observability.TraceProvider) func(
	context.Context,
	json.RawMessage,
	AuthorizedInvocation,
	GatewayInvocationFacts,
	GatewayNext,
) (agentruntime.SubAgentInvocationResult, error) {
	return func(
		ctx context.Context,
		_ json.RawMessage,
		req AuthorizedInvocation,
		_ GatewayInvocationFacts,
		next GatewayNext,
	) (agentruntime.SubAgentInvocationResult, error) {
		ctx, span := tracer.Start(ctx, "agentgateway.invoke",
			observability.String("provider_kind", string(req.ProviderKind)),
			observability.String("sub_agent_ref", req.SubAgentRef),
			observability.String("gateway_config_hash", req.Binding.ConfigHash),
		)
		defer span.End()
		result, err := next(ctx)
		if err != nil {
			span.RecordError(err, observability.String("gateway_error_code", SafeErrorCode(err)))
		}
		return result, err
	}
}

func invokeCanonicalEventAuditor(
	ctx context.Context,
	_ json.RawMessage,
	_ AuthorizedInvocation,
	facts GatewayInvocationFacts,
	next GatewayNext,
) (agentruntime.SubAgentInvocationResult, error) {
	if err := facts.AddAuditField("audit_recorded", defaultAuditPluginID); err != nil {
		return agentruntime.SubAgentInvocationResult{}, newGatewayError(CodePluginFailed, "audit field rejected", err)
	}
	return next(ctx)
}
