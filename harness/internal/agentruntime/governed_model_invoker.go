package agentruntime

import (
	"context"
	"errors"
	"fmt"

	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

var ErrRecursiveModelInvocationDuringCompaction = errors.New("recursive model invocation during context compaction")

// GovernedModelInvoker is the mandatory last Harness boundary around a model
// gateway. Runtime adapters pass their fully materialized request here; no
// prompt, tool schema or planner instruction may be appended afterwards.
type GovernedModelInvoker struct {
	next     ModelInvoker
	preModel RuntimePreModelCompactor
	governor ModelInputGovernor
}

func NewGovernedModelInvoker(next ModelInvoker, governor ModelInputGovernor) ModelInvoker {
	if next == nil {
		return nil
	}
	if _, ok := next.(*GovernedModelInvoker); ok {
		return next
	}
	if governor == nil {
		governor = NewDefaultModelInputGovernor(nil)
	}
	return &GovernedModelInvoker{next: next, governor: governor}
}

// NewContextManagedModelInvoker adds the portable Stage-B compactor before
// the mandatory final governor. Native runtimes such as Eino may run Stage B
// in their own middleware and should use NewGovernedModelInvoker instead.
func NewContextManagedModelInvoker(next ModelInvoker, preModel RuntimePreModelCompactor, governor ModelInputGovernor) ModelInvoker {
	if next == nil {
		return nil
	}
	if existing, ok := next.(*GovernedModelInvoker); ok {
		next = existing.next
		if governor == nil {
			governor = existing.governor
		}
	}
	if governor == nil {
		governor = NewDefaultModelInputGovernor(nil)
	}
	return &GovernedModelInvoker{next: next, preModel: preModel, governor: governor}
}

func IsGovernedModelInvoker(invoker ModelInvoker) bool {
	_, ok := invoker.(*GovernedModelInvoker)
	return ok
}

func (i *GovernedModelInvoker) Invoke(ctx context.Context, req ModelInvokeRequest) (<-chan ModelStreamItem, error) {
	if scope, ok := contextpkg.CompactionScopeFrom(ctx); ok && scope.Depth > 0 {
		err := fmt.Errorf("%w: purpose=%s depth=%d", ErrRecursiveModelInvocationDuringCompaction, scope.Purpose, scope.Depth)
		_ = emitModelInputGovernanceFailure(ctx, req, "compaction_recursion_guard", err, nil)
		return nil, err
	}
	prepared, err := prepareRuntimePreModelRequest(ctx, req, i.preModel)
	if err != nil {
		_ = emitModelInputGovernanceFailure(ctx, req, "pre_model_compaction", err, nil)
		return nil, err
	}
	governed, result, err := i.governor.Govern(ctx, prepared)
	if err != nil {
		_ = emitModelInputGovernanceFailure(ctx, prepared, "final_governor", err, result.CompactionRecords)
		return nil, err
	}
	if err := emitModelInputGovernance(ctx, observability.EventModelContextBuilt, governed, result); err != nil {
		return nil, fmt.Errorf("emit governed model context: %w", err)
	}
	return i.next.Invoke(ctx, governed)
}

func emitModelInputGovernanceFailure(ctx context.Context, req ModelInvokeRequest, stage string, cause error, records []ContextCompactionRecord) error {
	emitter, ok := RuntimeEventEmitterFrom(ctx)
	if !ok {
		return nil
	}
	code := "MODEL_INPUT_GOVERNANCE_FAILED"
	switch {
	case errors.Is(cause, ErrPreserveManifestInvalid), errors.Is(cause, ErrModelInputCompressionInvalid):
		code = "MODEL_INPUT_PRESERVE_VIOLATION"
	case errors.Is(cause, ErrRuntimePreModelCompactorMissing):
		code = "MODEL_PRE_MODEL_COMPACTOR_MISSING"
	case errors.Is(cause, ErrContextSummaryRequired):
		code = "MODEL_CONTEXT_SUMMARY_REQUIRED"
	case errors.Is(cause, ErrModelInputBudgetExceeded):
		code = "MODEL_INPUT_BUDGET_EXCEEDED"
	case errors.Is(cause, ErrRecursiveModelInvocationDuringCompaction):
		code = "MODEL_CONTEXT_COMPACTION_RECURSION"
	}
	return emitter.Emit(ctx, observability.AgentEvent{
		EventType: observability.EventGuardrailBlocked, Visibility: observability.VisibilityInternal,
		Payload: JSONPayload(map[string]any{
			"guardrail": "model_input_governance", "stage": stage,
			"package_id": req.Package.PackageID, "round": req.Round,
			"compaction_policy_hash": req.Package.RuntimeConstraints.CompactionPolicy.PolicyHash,
			"preserve_manifest_hash": req.PreserveManifest.ManifestHash,
			"compaction_records":     records,
		}),
		Error: &observability.EventError{Code: code, Type: observability.EventErrorGuardrailBlocked, Message: cause.Error()},
	})
}

func emitModelInputGovernance(ctx context.Context, eventType observability.EventType, req ModelInvokeRequest, result ModelInputGovernance) error {
	emitter, ok := RuntimeEventEmitterFrom(ctx)
	if !ok {
		return nil
	}
	visibility := observability.VisibilityDebug
	if eventType == observability.EventGuardrailBlocked {
		visibility = observability.VisibilityInternal
	}
	budgetReport := buildContextBudgetReport(req, result)
	return emitter.Emit(ctx, observability.AgentEvent{
		EventType: eventType, Visibility: visibility,
		Payload: JSONPayload(map[string]any{
			"guardrail":                    "model_input_budget",
			"package_id":                   req.Package.PackageID,
			"context_hash":                 req.Package.ContextHash,
			"context_snapshot_ref":         req.Package.Run.ContextSnapshotRef,
			"context_compaction_records":   req.Package.Messages.CompactionRecords,
			"round":                        req.Round,
			"token_count":                  result.FinalTokens,
			"original_token_count":         result.OriginalTokens,
			"trimmed_messages":             result.TrimmedMessages,
			"applied_strategies":           result.AppliedStrategies,
			"compaction_policy_hash":       req.PreModelCompaction.PolicyHash,
			"pre_model_strategies":         req.PreModelCompaction.AppliedStrategies,
			"pre_model_observed_tokens":    req.PreModelCompaction.ObservedTokens,
			"pre_model_soft_triggered":     req.PreModelCompaction.SoftTriggered,
			"summary_refs":                 req.PreModelCompaction.SummaryRefs,
			"trim_record_ref":              req.PreModelCompaction.TrimRecordRef,
			"pre_model_compaction_records": req.PreModelCompaction.Records,
			"final_compaction_records":     result.CompactionRecords,
			"context_budget_report":        budgetReport,
			"preserve_manifest_hash":       req.PreserveManifest.ManifestHash,
			"phase":                        "model_call",
		}),
	})
}
