package agentbinding

import "github.com/AGenUI/agenui-studio/harness/internal/executionmode"

type SourceDecision struct {
	Selection           Selection
	Source              Source
	ControlRuleRef      string
	ControlRuleRevision string
	forceActive         bool
	denyActive          bool
	denySelection       *Selection
}

// SourcePolicy 只处理显式来源：force 优先，其余依次为请求、会话默认、配置默认、控制面默认。
// 它不读取自然语言，也不做候选召回、评分或意图识别。
type SourcePolicy struct{}

func (SourcePolicy) Select(req BindingRequest) (SourceDecision, error) {
	control, err := validateControlRule(req.Control)
	if err != nil {
		return SourceDecision{}, err
	}
	if control != nil && control.Action == ControlForce {
		selection := *control.Selection
		if err := validateSelectionInput(selection); err != nil {
			return SourceDecision{}, err
		}
		return SourceDecision{
			Selection:           selection,
			Source:              SourceControlPlane,
			ControlRuleRef:      control.Ref,
			ControlRuleRevision: control.Revision,
			forceActive:         true,
		}, nil
	}

	candidates := []struct {
		selection *Selection
		source    Source
	}{
		{req.Request, SourceRequestParam},
		{req.SessionDefault, SourceSessionDefault},
		{req.ConfigDefault, SourceConfigDefault},
	}
	if control != nil && control.Action == ControlDefault {
		candidates = append(candidates, struct {
			selection *Selection
			source    Source
		}{control.Selection, SourceControlPlane})
	}

	var decision SourceDecision
	for _, candidate := range candidates {
		if candidate.selection == nil || !selectionPresent(*candidate.selection) {
			continue
		}
		if err := validateSelectionInput(*candidate.selection); err != nil {
			return SourceDecision{}, err
		}
		decision.Selection = *candidate.selection
		decision.Source = candidate.source
		break
	}
	if decision.Source == "" {
		return SourceDecision{}, NewAskUserError(StageSourceSelection, "agent_id")
	}
	if control != nil {
		decision.ControlRuleRef = control.Ref
		decision.ControlRuleRevision = control.Revision
		if control.Action == ControlDeny {
			decision.denyActive = true
			if control.Selection != nil {
				selection := *control.Selection
				decision.denySelection = &selection
			}
			if selectionMatches(control.Selection, decision.Selection) {
				return SourceDecision{}, NewError(StageSourceSelection, CodeControlDenied, false, nil)
			}
		}
	}
	return decision, nil
}

func validateControlRule(rule *ControlRule) (*ControlRule, error) {
	if rule == nil {
		return nil, nil
	}
	if rule.Ref == "" || rule.Revision == "" {
		return nil, NewError(StageSourceSelection, CodeControlRuleInvalid, false, nil)
	}
	switch rule.Action {
	case ControlForce, ControlDefault:
		if rule.Selection == nil || !selectionPresent(*rule.Selection) {
			return nil, NewError(StageSourceSelection, CodeControlRuleInvalid, false, nil)
		}
	case ControlDeny:
		if rule.Selection != nil && rule.Selection.Mode != "" {
			if err := rule.Selection.Mode.Validate(); err != nil {
				return nil, NewError(StageSourceSelection, CodeControlRuleInvalid, false, err)
			}
		}
		if rule.Selection != nil && !rule.Selection.Target.IsZero() {
			if err := validateTargetBasics(rule.Selection.Target); err != nil {
				return nil, NewError(StageSourceSelection, CodeControlRuleInvalid, false, err)
			}
		}
	default:
		return nil, NewError(StageSourceSelection, CodeControlRuleInvalid, false, nil)
	}
	copy := *rule
	if rule.Selection != nil {
		selection := *rule.Selection
		copy.Selection = &selection
	}
	return &copy, nil
}

func validateSelectionInput(selection Selection) error {
	if selection.AgentID == "" {
		return NewAskUserError(StageSourceSelection, "agent_id")
	}
	if selection.Mode != "" {
		if err := selection.Mode.Validate(); err != nil {
			return NewError(StageSourceSelection, CodeExecutionModeUnsupported, false, err)
		}
	}
	if !selection.Target.IsZero() {
		if err := validateTargetBasics(selection.Target); err != nil {
			return err
		}
	}
	return nil
}

func validateTargetBasics(target Target) error {
	switch target.Kind {
	case TargetAgent, TargetWorkflow, TargetGraph:
	default:
		return NewError(StageStaticValidate, CodeTargetInvalid, false, nil)
	}
	if target.Ref == "" {
		return NewError(StageStaticValidate, CodeTargetInvalid, false, nil)
	}
	return nil
}

func selectionPresent(selection Selection) bool {
	return selection.AgentID != "" || selection.AgentVersion != "" || selection.Mode != "" || !selection.Target.IsZero()
}

func selectionMatches(matcher *Selection, selected Selection) bool {
	if matcher == nil || !selectionPresent(*matcher) {
		return true
	}
	if matcher.AgentID != "" && matcher.AgentID != selected.AgentID {
		return false
	}
	if matcher.AgentVersion != "" && matcher.AgentVersion != selected.AgentVersion {
		return false
	}
	if matcher.Mode != "" && matcher.Mode != selected.Mode {
		return false
	}
	return targetMatches(matcher.Target, selected.Target)
}

func targetMatches(matcher, selected Target) bool {
	if matcher.IsZero() {
		return true
	}
	checks := []struct{ want, got string }{
		{string(matcher.Kind), string(selected.Kind)},
		{matcher.Ref, selected.Ref},
		{matcher.Version, selected.Version},
		{matcher.Hash, selected.Hash},
		{matcher.StateSchemaRef, selected.StateSchemaRef},
	}
	for _, check := range checks {
		if check.want != "" && check.want != check.got {
			return false
		}
	}
	return true
}

func modeError(mode executionmode.Mode) error {
	if err := mode.Validate(); err != nil {
		return NewError(StageStaticValidate, CodeExecutionModeUnsupported, false, err)
	}
	return nil
}
