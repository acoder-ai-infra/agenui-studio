package kernel

import (
	"context"

	"github.com/AGenUI/agenui-studio/harness/internal/control"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// Dispatcher is the shared abstraction over Composition Root's run-execution
// entry. Both cmd/harness (hosted) and harness (embedded) call Dispatcher.Dispatch
// after installing an event subscription so no hot delta is lost.
//
// The composer produces a concrete implementation (currently the formal
// RunDispatcher in internal/app/composition.go); tests may swap in a fake by
// building a Kernel whose RunEntry satisfies this interface.
type Dispatcher interface {
	// Dispatch hands a freshly-opened turn to the runtime. It is non-blocking:
	// the caller drains the returned event stream via Kernel.SubscribeRun.
	Dispatch(ctx context.Context, turn *storage.OpenTurnResult) error
	// Resume continues a run that reached waiting_control by supplying a
	// verified ControlResponse.
	Resume(ctx context.Context, req control.ResumeRequest) error
	// Cancel asks the kernel to cancel the identified run. Idempotent for
	// terminal runs.
	Cancel(ctx context.Context, sessionID, runID, reason string) error
}

// DispatchOptions bundles per-request options the SDK adapter may hand to the
// dispatcher without expanding the Dispatcher interface for every field.
// InputManifest / SourceRefs are omitted until the runtime carries
// them through OpenTurn.
type DispatchOptions struct {
	// Deadline is an optional per-turn deadline. Zero means "use kernel default".
	Deadline int64
	// Priority is a caller-supplied scheduling hint (higher = sooner). Zero
	// means default priority.
	Priority int
}

// Verify RunEntry satisfies Dispatcher; both names are kept: RunEntry is the
// stable field name on Kernel, Dispatcher is the interface tests target.
var _ Dispatcher = (RunEntry)(nil)
