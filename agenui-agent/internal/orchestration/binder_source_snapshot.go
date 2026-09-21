package orchestration

import (
	"context"

	stepartifact "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/artifact"
	"github.com/AGenUI/agenui-studio/harness/sdk"
)

// loadBinderContractWorkingSearch returns the immutable source receipts selected
// during this Harness Run. Workspace is the only admission layer; no alternate
// working-view or compatibility projection is derived here.
func loadBinderContractWorkingSearch(
	ctx context.Context,
	store ArtifactContentStore,
	identity harness.Identity,
) (string, error) {
	return store.Load(ctx, identity, stepartifact.StepSearch)
}
