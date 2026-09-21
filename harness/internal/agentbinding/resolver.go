package agentbinding

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/executionmode"
)

type Resolver struct {
	Configs ConfigResolver
	Policy  SourcePolicy
	Clock   func() time.Time
}

func NewResolver(configs ConfigResolver) *Resolver {
	return &Resolver{Configs: configs, Clock: time.Now}
}

func (r *Resolver) Resolve(ctx context.Context, req BindingRequest) (Result, error) {
	if err := validateRequest(req, r); err != nil {
		return Result{}, err
	}
	decision, err := r.Policy.Select(req)
	if err != nil {
		return Result{}, err
	}

	resolved, err := r.resolveConfig(ctx, req, decision.Selection)
	fallback := FallbackFact{}
	if err != nil {
		primaryErr := err
		// 控制面 force 是强制执行事实，不能被调用方 fallback 绕过。
		if decision.forceActive || req.Fallback == nil || !IsAgentUnavailable(err) {
			return Result{}, err
		}
		if *req.Fallback == decision.Selection {
			return Result{}, err
		}
		if fallbackErr := validateSelectionInput(*req.Fallback); fallbackErr != nil {
			return Result{}, fallbackErr
		}
		fallbackSelection := *req.Fallback
		resolved, err = r.resolveConfig(ctx, req, fallbackSelection)
		if err != nil {
			return Result{}, err
		}
		fallback = FallbackFact{
			Applied:    true,
			From:       decision.Selection,
			To:         fallbackSelection,
			ReasonCode: CodeOf(primaryErr),
		}
	}

	return r.finalize(req, decision, resolved, fallback)
}

func validateRequest(req BindingRequest, resolver *Resolver) error {
	if resolver == nil || resolver.Configs == nil || req.BindingID == "" || req.SessionID == "" || req.RunID == "" {
		return NewError(StageSourceSelection, CodeInvalidRequest, false, nil)
	}
	if req.SchemaVersion != "" && req.SchemaVersion != BindingSchemaVersion {
		return NewError(StageSourceSelection, CodeInvalidRequest, false, nil)
	}
	return nil
}

func (r *Resolver) resolveConfig(ctx context.Context, req BindingRequest, selection Selection) (ResolvedConfig, error) {
	resolved, err := r.Configs.ResolveConfig(ctx, ConfigResolveRequest{
		Selection: selection,
		SessionID: req.SessionID,
		RunID:     req.RunID,
	})
	if err == nil {
		return resolved, nil
	}
	var coded codedError
	if errors.As(err, &coded) {
		return ResolvedConfig{}, NewError(StageConfigResolve, coded.BindingErrorCode(), coded.BindingRetryable(), err)
	}
	return ResolvedConfig{}, NewError(StageConfigResolve, CodeConfigResolveFailed, true, err)
}

func (r *Resolver) finalize(req BindingRequest, decision SourceDecision, resolved ResolvedConfig, fallback FallbackFact) (Result, error) {
	definition := cloneDefinition(resolved.Definition)
	selected := decision.Selection
	if fallback.Applied {
		selected = fallback.To
	}
	if definition.AgentID == "" || definition.Version == "" || definition.AgentID != selected.AgentID || selected.AgentVersion != "" && selected.AgentVersion != definition.Version {
		return Result{}, NewError(StageFinalize, CodeDefinitionInvalid, false, nil)
	}

	mode, err := effectiveMode(selected.Mode, resolved.ExecutionMode)
	if err != nil {
		return Result{}, err
	}
	target, err := effectiveTarget(mode, definition, selected.Target, resolved.Target)
	if err != nil {
		return Result{}, err
	}
	if err := validateDefinition(mode, definition, target); err != nil {
		return Result{}, err
	}
	if resolved.ConfigSnapshotRef == "" {
		return Result{}, NewError(StageFinalize, CodeConfigSnapshotMissing, false, nil)
	}
	if resolved.ConfigHash == "" {
		return Result{}, NewError(StageFinalize, CodeConfigHashMissing, false, nil)
	}
	if err := validateCapabilityManifest(definition, resolved.ConfigSnapshotRef, resolved.CapabilitySnapshotRefs); err != nil {
		return Result{}, err
	}

	effectiveSelection := Selection{AgentID: definition.AgentID, AgentVersion: definition.Version, Mode: mode, Target: target}
	if decision.denyActive && selectionMatches(decision.denySelection, effectiveSelection) {
		return Result{}, NewError(StageFinalize, CodeControlDenied, false, nil)
	}
	if fallback.Applied {
		fallback.To = effectiveSelection
	}
	createdAt := req.CreatedAt
	if createdAt.IsZero() {
		clock := r.Clock
		if clock == nil {
			clock = time.Now
		}
		createdAt = clock()
	}
	binding := EffectiveBinding{
		SchemaVersion:          BindingSchemaVersion,
		BindingID:              req.BindingID,
		SessionID:              req.SessionID,
		RunID:                  req.RunID,
		AgentID:                definition.AgentID,
		AgentVersion:           definition.Version,
		ExecutionMode:          mode,
		Target:                 target,
		ConfigSnapshotRef:      resolved.ConfigSnapshotRef,
		ConfigHash:             resolved.ConfigHash,
		CapabilitySnapshotRefs: normalizeRefs(resolved.CapabilitySnapshotRefs),
		Source:                 decision.Source,
		ControlRuleRef:         decision.ControlRuleRef,
		ControlRuleRevision:    decision.ControlRuleRevision,
		Fallback:               fallback,
		CreatedAt:              createdAt.UTC(),
	}
	binding.BindingHash, err = ComputeBindingHash(binding)
	if err != nil {
		return Result{}, err
	}
	if err := binding.Validate(); err != nil {
		return Result{}, err
	}
	return Result{Binding: binding, Definition: definition}, nil
}

// validateCapabilityManifest 只校验 Binding 必须冻结的最小执行事实。
// Schema、租户授权和工具策略由 Runtime 的 CapabilitySnapshotProvider 解析，
// Binding 不读取 Tool Registry，更不能在这里执行工具。
func validateCapabilityManifest(definition agentruntime.AgentDefinition, configSnapshotRef string, refs []string) error {
	for _, ref := range definition.ToolRefs {
		if !isExactVersionedRef(ref) {
			return NewError(StageFinalize, CodeCapabilitySnapshotFailed, false, nil)
		}
	}
	want := configSnapshotRef + "#capabilities"
	if len(refs) != 1 || refs[0] != want {
		return NewError(StageFinalize, CodeCapabilitySnapshotFailed, false, nil)
	}
	return nil
}

func isExactVersionedRef(ref string) bool {
	if ref == "" || strings.TrimSpace(ref) != ref || strings.Count(ref, "@") != 1 {
		return false
	}
	separator := strings.IndexByte(ref, '@')
	if separator <= 0 || separator == len(ref)-1 {
		return false
	}
	return strings.TrimSpace(ref[:separator]) == ref[:separator] && strings.TrimSpace(ref[separator+1:]) == ref[separator+1:]
}

func effectiveMode(selected, resolved executionmode.Mode) (executionmode.Mode, error) {
	if selected == "" && resolved == "" {
		return "", NewAskUserError(StageFinalize, "execution_mode")
	}
	if selected != "" {
		if err := modeError(selected); err != nil {
			return "", err
		}
	}
	if resolved != "" {
		if err := modeError(resolved); err != nil {
			return "", err
		}
	}
	if selected != "" && resolved != "" && selected != resolved {
		return "", NewError(StageFinalize, CodeExecutionModeMismatch, false, nil)
	}
	if selected != "" {
		return selected, nil
	}
	return resolved, nil
}

func effectiveTarget(mode executionmode.Mode, definition agentruntime.AgentDefinition, selected, resolved Target) (Target, error) {
	if mode == executionmode.DirectAction || mode == executionmode.SingleAgent || mode == executionmode.DeepAgent {
		canonical := Target{Kind: TargetAgent, Ref: definition.AgentID, Version: definition.Version}
		if (!selected.IsZero() && !targetMatches(selected, canonical)) || (!resolved.IsZero() && !targetMatches(resolved, canonical)) {
			return Target{}, NewError(StageFinalize, CodeTargetInvalid, false, nil)
		}
		return canonical, nil
	}
	if resolved.IsZero() || (!selected.IsZero() && !targetMatches(selected, resolved)) {
		return Target{}, NewError(StageFinalize, CodeTargetInvalid, false, nil)
	}
	if err := validateEffectiveTarget(mode, definition.AgentID, definition.Version, resolved); err != nil {
		return Target{}, err
	}
	return resolved, nil
}

func validateDefinition(mode executionmode.Mode, definition agentruntime.AgentDefinition, target Target) error {
	runtimeMode, err := executionmode.ToRuntimeMode(mode)
	if err != nil {
		return NewError(StageStaticValidate, CodeExecutionModeUnsupported, false, err)
	}
	if definition.Runtime.Mode != runtimeMode {
		return NewError(StageStaticValidate, CodeExecutionModeMismatch, false, nil)
	}
	switch mode {
	case executionmode.DirectAction:
		if definition.Runtime.Type != agentruntime.RuntimeTypeNative ||
			definition.Runtime.Preferred != "" && definition.Runtime.Preferred != agentruntime.RuntimeTypeNative {
			return NewError(StageStaticValidate, CodeDefinitionInvalid, false, nil)
		}
		for _, candidate := range definition.Runtime.Candidates {
			if candidate != agentruntime.RuntimeTypeNative {
				return NewError(StageStaticValidate, CodeDefinitionInvalid, false, nil)
			}
		}
	case executionmode.Workflow:
		workflow := definition.Workflow
		if workflow == nil || workflow.WorkflowID == "" || workflow.WorkflowID != target.Ref || workflow.EntryNode == "" || len(workflow.Nodes) == 0 {
			return NewError(StageStaticValidate, CodeDefinitionInvalid, false, nil)
		}
	case executionmode.Graph:
		graph := definition.Graph
		if graph == nil || graph.GraphID == "" || graph.GraphID != target.Ref || graph.EntryNode == "" || len(graph.Nodes) == 0 || graph.StateSchemaRef == "" || graph.StateSchemaRef != target.StateSchemaRef {
			return NewError(StageStaticValidate, CodeDefinitionInvalid, false, nil)
		}
	}
	return nil
}

func normalizeRefs(refs []string) []string {
	if len(refs) == 0 {
		return nil
	}
	values := append([]string(nil), refs...)
	sort.Strings(values)
	out := values[:0]
	for _, ref := range values {
		if ref == "" || len(out) > 0 && out[len(out)-1] == ref {
			continue
		}
		out = append(out, ref)
	}
	return out
}

func cloneDefinition(definition agentruntime.AgentDefinition) agentruntime.AgentDefinition {
	out := definition
	out.Runtime.Candidates = append([]agentruntime.RuntimeType(nil), definition.Runtime.Candidates...)
	out.ToolRefs = append([]string(nil), definition.ToolRefs...)
	out.SubAgentRefs = append([]string(nil), definition.SubAgentRefs...)
	out.RequiredCapabilities = append([]string(nil), definition.RequiredCapabilities...)
	out.DataPassing.ArtifactKeys = append([]string(nil), definition.DataPassing.ArtifactKeys...)
	out.DataPassing.ScopedDataKeys = append([]string(nil), definition.DataPassing.ScopedDataKeys...)
	out.Metadata = cloneMap(definition.Metadata)
	if definition.Workflow != nil {
		workflow := *definition.Workflow
		workflow.Nodes = cloneNodes(definition.Workflow.Nodes)
		workflow.Edges = append([]agentruntime.WorkflowEdge(nil), definition.Workflow.Edges...)
		out.Workflow = &workflow
	}
	if definition.Graph != nil {
		graph := *definition.Graph
		graph.Nodes = cloneNodes(definition.Graph.Nodes)
		graph.Edges = append([]agentruntime.WorkflowEdge(nil), definition.Graph.Edges...)
		out.Graph = &graph
	}
	return out
}

func cloneNodes(nodes []agentruntime.WorkflowNode) []agentruntime.WorkflowNode {
	out := append([]agentruntime.WorkflowNode(nil), nodes...)
	for i := range out {
		out[i].InputKeys = append([]string(nil), nodes[i].InputKeys...)
		out[i].OutputKeys = append([]string(nil), nodes[i].OutputKeys...)
		out[i].Config = cloneMap(nodes[i].Config)
	}
	return out
}

func cloneMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}
