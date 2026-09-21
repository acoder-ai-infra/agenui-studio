package context

import (
	gocontext "context"
)

// SystemPromptSource produces stable fragments from system instructions.
type SystemPromptSource struct {
	Instruction string
	Identity    string
	Policies    string
}

func NewSystemPromptSource(instruction, identity, policies string) *SystemPromptSource {
	return &SystemPromptSource{
		Instruction: instruction,
		Identity:    identity,
		Policies:    policies,
	}
}

func (s *SystemPromptSource) Kind() SourceKind { return SourceSystemPrompt }

func (s *SystemPromptSource) Required() bool { return true }

func (s *SystemPromptSource) Collect(_ gocontext.Context, req CollectRequest) ([]ContextFragment, error) {
	var frags []ContextFragment
	counter := EstimateCounter{}

	if s.Instruction != "" {
		frags = append(frags, ContextFragment{
			Slot:       SlotSystemPrompt,
			Stability:  StabilityStable,
			Priority:   100,
			Pinned:     true,
			Generation: req.Generation,
			Source:     string(SourceSystemPrompt),
			Role:       RoleSystem,
			Content:    s.Instruction,
			TokenCost:  counter.Count(s.Instruction),
		})
	}

	if s.Identity != "" {
		frags = append(frags, ContextFragment{
			Slot:       SlotAgentIdentity,
			Stability:  StabilityStable,
			Priority:   90,
			Pinned:     true,
			Generation: req.Generation,
			Source:     string(SourceSystemPrompt),
			Role:       RoleSystem,
			Content:    s.Identity,
			TokenCost:  counter.Count(s.Identity),
		})
	}

	if s.Policies != "" {
		frags = append(frags, ContextFragment{
			Slot:       SlotPolicies,
			Stability:  StabilityStable,
			Priority:   80,
			Pinned:     true,
			Generation: req.Generation,
			Source:     string(SourceSystemPrompt),
			Role:       RoleSystem,
			Content:    s.Policies,
			TokenCost:  counter.Count(s.Policies),
		})
	}

	return frags, nil
}
