package orchestration

import (
	"context"
	"encoding/json"
	"errors"

	agenuiextensions "github.com/AGenUI/agenui-studio/agenui-agent/internal/extensions"
	stepartifact "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/artifact"
	bindingcontract "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/binding/contract"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/materializer"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/workspace"
	"github.com/AGenUI/agenui-studio/harness/sdk"
)

type Completion struct {
	Result          string
	FieldHints      string
	Plan            string
	APIContent      string
	TemplateContent string
	Preflight       string
}

type ArtifactCompletionResolver struct{ store ArtifactContentStore }

func NewArtifactCompletionResolver(store ArtifactContentStore, _ ...string) *ArtifactCompletionResolver {
	return &ArtifactCompletionResolver{store: store}
}

func (r *ArtifactCompletionResolver) Resolve(ctx context.Context, identity harness.Identity) (Completion, error) {
	if r == nil || r.store == nil {
		return Completion{}, errors.New("artifact completion resolver unavailable")
	}
	if identity.AgentID == agenuiextensions.MainAgent {
		return r.resolveMain(ctx, identity)
	}
	return r.resolveBound(ctx, identity)
}

func (r *ArtifactCompletionResolver) resolveMain(ctx context.Context, identity harness.Identity) (Completion, error) {
	design, err := r.store.Load(ctx, identity, stepartifact.StepDesign)
	if err != nil {
		return Completion{}, err
	}
	result, decodeErr := workspace.ArtifactMessagesJSON(design)
	if decodeErr != nil || !json.Valid([]byte(result)) {
		return Completion{}, errors.New("artifact completion: invalid design")
	}
	requirements, err := r.store.Load(ctx, identity, stepartifact.StepRequirements)
	if err != nil {
		return Completion{}, err
	}
	preflight, err := r.store.Load(ctx, identity, stepartifact.StepPreflight)
	if err != nil {
		return Completion{}, err
	}
	search, searchErr := loadBindingSearch(ctx, r.store, identity)
	binding, bindingErr := r.store.Load(ctx, identity, stepartifact.StepBinding)
	if searchErr == nil && bindingErr == nil {
		return materializedCompletion(design, search, binding)
	}
	if bindingErr == nil && errors.Is(searchErr, stepartifact.ErrNotFound) {
		return materializedCompletion(design, "", binding)
	}
	if !errors.Is(searchErr, stepartifact.ErrNotFound) || !errors.Is(bindingErr, stepartifact.ErrNotFound) {
		return Completion{}, errors.New("artifact completion: incomplete binding artifacts")
	}
	fieldHints, _, _ := workspace.ArtifactSlotsJSON(design)
	return Completion{Result: result, FieldHints: fieldHints, Plan: requirements, TemplateContent: design, Preflight: preflight}, nil
}

func (r *ArtifactCompletionResolver) resolveBound(ctx context.Context, identity harness.Identity) (Completion, error) {
	design, err := r.store.Load(ctx, identity, stepartifact.StepTemplate)
	if err != nil {
		return Completion{}, err
	}
	search, err := loadBindingSearch(ctx, r.store, identity)
	if err != nil {
		return Completion{}, err
	}
	binding, err := r.store.Load(ctx, identity, stepartifact.StepBinding)
	if err != nil {
		return Completion{}, err
	}
	return materializedCompletion(design, search, binding)
}

func materializedCompletion(template, search, binding string) (Completion, error) {
	design, decodeErr := workspace.ArtifactMessagesJSON(template)
	if decodeErr != nil || !json.Valid([]byte(design)) {
		return Completion{}, errors.New("artifact completion: invalid design result")
	}
	plan := binding
	if submission, parseErr := bindingcontract.ParseSubmission(binding); parseErr == nil {
		encodedPlan, encodeErr := bindingcontract.EncodeExecutablePlan(submission.Plan.FieldMappings, submission.Plan.ActionMappings)
		if encodeErr != nil {
			return Completion{}, encodeErr
		}
		plan = encodedPlan
	}
	output, err := materializer.Materialize(design, plan)
	if err != nil {
		return Completion{}, err
	}
	fieldHints, _, _ := workspace.ArtifactSlotsJSON(template)
	return Completion{Result: output.Result, FieldHints: fieldHints, Plan: output.Plan, APIContent: search, TemplateContent: template}, nil
}

func buildCompletion(template, search, binding string) (Completion, bool) {
	completion, err := materializedCompletion(template, search, binding)
	return completion, err == nil
}

func loadBindingSearch(ctx context.Context, store ArtifactContentStore, identity harness.Identity) (string, error) {
	return store.Load(ctx, identity, stepartifact.StepSearch)
}
