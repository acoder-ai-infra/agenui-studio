package harness

import (
	"context"

	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// CollectContextFragments runs every registered ContextContributor in
// registration order and concatenates their emitted fragments. It is a
// caller-driven helper for hosts that need to consult contributor output
// alongside their own logic (Agent Registry pre-fetch, testkit fixtures, etc.).
//
// Contract (the public SDK contract):
//
//   - ContextContributor runs after AgentBinding + CapabilitySnapshot and
//     before InputNormalizer. Callers that skip runtime dispatch (e.g. unit
//     tests) can still invoke it via this helper.
//   - Contributors are order-preserving; downstream truncation uses
//     Priority + TokenBudget from each ContextFragment.
//   - A contributor failure returns the error; callers apply their own
//     FailurePolicy (fail-closed for security-critical contributors, fail-open
//     for convenience ones).
func CollectContextFragments(engine Engine, ctx context.Context, req extension.ContribRequest) ([]extension.ContextFragment, error) {
	impl, ok := engine.(*engineImpl)
	if !ok || impl == nil || impl.kernel == nil || impl.kernel.Extensions == nil {
		return nil, nil
	}
	entries := impl.kernel.Extensions.ByKind(kernel.ExtContextContributor)
	if len(entries) == 0 {
		return nil, nil
	}
	var fragments []extension.ContextFragment
	for _, entry := range entries {
		contributor, ok := entry.Implementation.(extension.ContextContributor)
		if !ok {
			continue
		}
		reqCtx := ctx
		if entry.Timeout > 0 {
			var cancel context.CancelFunc
			reqCtx, cancel = context.WithTimeout(ctx, entry.Timeout)
			defer cancel()
		}
		out, err := contributor.Contribute(reqCtx, req)
		if err != nil {
			return fragments, err
		}
		fragments = append(fragments, out...)
	}
	return fragments, nil
}

// NormalizeInput runs all registered InputNormalizers as an ordered chain
// (Engine-level semantics, not bound to agent_id): each normalizer receives
// the previous output as RawInput; SourceRefs accumulate across the chain.
// Returns the caller's raw input unchanged if no normalizer is bound.
//
// Callers use this helper when they orchestrate their own input
// canonicalization pass; runtime-integrated invocation happens automatically
// inside the kernel TurnPipeline.
func NormalizeInput(engine Engine, ctx context.Context, req extension.NormalizeRequest) (extension.NormalizedInput, error) {
	impl, ok := engine.(*engineImpl)
	if !ok || impl == nil || impl.kernel == nil || impl.kernel.Extensions == nil {
		return extension.NormalizedInput{Message: req.RawInput}, nil
	}
	entries := impl.kernel.Extensions.ByKind(kernel.ExtInputNormalizer)
	if len(entries) == 0 {
		return extension.NormalizedInput{Message: req.RawInput}, nil
	}
	out := extension.NormalizedInput{Message: req.RawInput}
	var sourceRefs []extension.SourceRef
	for _, entry := range entries {
		normalizer, ok := entry.Implementation.(extension.InputNormalizer)
		if !ok {
			return extension.NormalizedInput{}, wrapInvalidRequest(
				"extension " + entry.ID + " does not implement extension.InputNormalizer",
			)
		}
		stepReq := req
		stepReq.RawInput = out.Message
		reqCtx := ctx
		var cancel context.CancelFunc
		if entry.Timeout > 0 {
			reqCtx, cancel = context.WithTimeout(ctx, entry.Timeout)
		}
		stepOut, err := normalizer.Normalize(reqCtx, stepReq)
		if cancel != nil {
			cancel()
		}
		if err != nil {
			return extension.NormalizedInput{}, err
		}
		out.Message = stepOut.Message
		sourceRefs = append(sourceRefs, stepOut.SourceRefs...)
	}
	out.SourceRefs = sourceRefs
	return out, nil
}
