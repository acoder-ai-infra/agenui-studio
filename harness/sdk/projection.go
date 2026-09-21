// Package harness — projection & validation helpers.
package harness

import (
	"context"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
)

// ProjectEvent runs every registered ProtocolProjector against ev in
// registration order and concatenates their emitted frames. Callers with a
// bespoke wire protocol use it inside their event loop to transform a
// canonical harness.Event into zero or more business frames.
func ProjectEvent(engine Engine, ctx context.Context, ev Event) ([]extension.Frame, error) {
	impl, ok := engine.(*engineImpl)
	if !ok || impl == nil || impl.kernel == nil || impl.kernel.Extensions == nil {
		return nil, nil
	}
	entries := impl.kernel.Extensions.ByKind(kernel.ExtProtocolProjector)
	if len(entries) == 0 {
		return nil, nil
	}
	protoEvent := toProtocolEvent(ev)
	var frames []extension.Frame
	for _, entry := range entries {
		projector, ok := entry.Implementation.(extension.ProtocolProjector)
		if !ok {
			continue
		}
		reqCtx := ctx
		if entry.Timeout > 0 {
			var cancel context.CancelFunc
			reqCtx, cancel = context.WithTimeout(ctx, entry.Timeout)
			defer cancel()
		}
		out, err := projector.Project(reqCtx, protoEvent)
		if err != nil {
			return frames, err
		}
		frames = append(frames, out...)
	}
	return frames, nil
}

// ValidateOutput runs every registered OutputValidator. The first non-Accept
// result is returned; callers apply it to their retry loop or terminal state.
func ValidateOutput(engine Engine, ctx context.Context, req extension.OutputValidateRequest) (extension.OutputValidateResult, error) {
	impl, ok := engine.(*engineImpl)
	if !ok || impl == nil || impl.kernel == nil || impl.kernel.Extensions == nil {
		return extension.OutputValidateResult{Action: extension.OutputAccept}, nil
	}
	entries := impl.kernel.Extensions.ByKind(kernel.ExtOutputValidator)
	if len(entries) == 0 {
		return extension.OutputValidateResult{Action: extension.OutputAccept}, nil
	}
	for _, entry := range entries {
		validator, ok := entry.Implementation.(extension.OutputValidator)
		if !ok {
			continue
		}
		reqCtx := ctx
		if entry.Timeout > 0 {
			var cancel context.CancelFunc
			reqCtx, cancel = context.WithTimeout(ctx, entry.Timeout)
			defer cancel()
		}
		res, err := validator.Validate(reqCtx, req)
		if err != nil {
			return extension.OutputValidateResult{Action: extension.OutputFail, Reason: err.Error()}, err
		}
		if res.Action != extension.OutputAccept {
			return res, nil
		}
	}
	return extension.OutputValidateResult{Action: extension.OutputAccept}, nil
}

func toProtocolEvent(ev Event) extension.ProtocolEvent {
	pe := extension.ProtocolEvent{
		EventID:    ev.EventID,
		Sequence:   ev.Sequence,
		SessionID:  ev.SessionID,
		RunID:      ev.RunID,
		StepID:     ev.StepID,
		AgentID:    ev.AgentID,
		EventType:  string(ev.EventType),
		Visibility: string(ev.Visibility),
		Payload:    ev.PayloadPreview,
		PayloadRef: ev.PayloadRef,
		Usage:      ev.Usage,
		CreatedAt:  ev.CreatedAt,
	}
	if ev.Error != nil {
		pe.Error = &extension.ProtocolEventError{
			Code:      ev.Error.Code,
			Type:      string(ev.Error.Type),
			Message:   ev.Error.Message,
			Retryable: ev.Error.Retryable,
		}
	}
	return pe
}
