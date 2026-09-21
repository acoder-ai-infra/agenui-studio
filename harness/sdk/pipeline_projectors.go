package harness

import (
	"context"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
)

// extensionProjectorBinding pairs a registered ProtocolProjector with its
// ExtensionEntry metadata so the stream fan-out can honour timeout and record
// projected frames on the run's frame cell.
type extensionProjectorBinding struct {
	id        string
	timeout   time.Duration
	projector extension.ProtocolProjector
}

// eventProjectors builds per-run projector bindings from the Kernel catalog.
func (e *engineImpl) eventProjectors() []extensionProjectorBinding {
	if e == nil || e.kernel == nil || e.kernel.Extensions == nil {
		return nil
	}
	entries := e.kernel.Extensions.ByKind(kernel.ExtProtocolProjector)
	if len(entries) == 0 {
		return nil
	}
	out := make([]extensionProjectorBinding, 0, len(entries))
	for _, entry := range entries {
		p, ok := entry.Implementation.(extension.ProtocolProjector)
		if !ok {
			continue
		}
		out = append(out, extensionProjectorBinding{
			id: entry.ID, timeout: entry.Timeout, projector: p,
		})
	}
	return out
}

// fanEventToProjectors runs each registered projector for ev in registration
// order, appending emitted frames to cell in stream order. Projector errors
// and panics are swallowed here (projectors are read-only per canonical
// §7.1; a broken projector must not stop the run).
func fanEventToProjectors(ctx context.Context, projectors []extensionProjectorBinding, ev Event, cell *executionFrames) {
	if len(projectors) == 0 || cell == nil {
		return
	}
	proto := toProtocolEvent(ev)
	for _, p := range projectors {
		reqCtx := ctx
		if p.timeout > 0 {
			var cancel context.CancelFunc
			reqCtx, cancel = context.WithTimeout(ctx, p.timeout)
			defer cancel()
		}
		frames := invokeProjectorSafely(reqCtx, p.projector, proto)
		cell.append(frames...)
	}
}

// invokeProjectorSafely wraps a projector call in defer/recover; any panic
// is treated as no frames emitted for that projector, preserving the
// projector's fail-open contract.
func invokeProjectorSafely(ctx context.Context, projector extension.ProtocolProjector, ev extension.ProtocolEvent) []extension.Frame {
	defer func() {
		_ = recover()
	}()
	frames, err := projector.Project(ctx, ev)
	if err != nil {
		return nil
	}
	return frames
}
