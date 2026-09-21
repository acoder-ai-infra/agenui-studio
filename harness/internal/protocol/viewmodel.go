package protocol

import "github.com/AGenUI/agenui-studio/harness/internal/observability"

// ViewModelMapper maps a canonical event_type to an end-side view_type
// (tool_call_started -> tool_start). It never changes the event_type and its
// output is never persisted (canonical §6.9, conformance P-002).
type ViewModelMapper interface {
	ViewType(t observability.EventType) string
}

// viewTypeByEventType is the canonical event_type -> view_type projection table
// (canonical §6.9). Only these four have a view_type alias; every other event
// type has no view_type (empty string).
var viewTypeByEventType = map[observability.EventType]string{
	observability.EventToolCallStarted:   ViewTypeToolStart,
	observability.EventToolCallProgress:  ViewTypeToolDelta,
	observability.EventToolCallCompleted: ViewTypeToolEnd,
	observability.EventToolCallFailed:    ViewTypeToolEnd,
	observability.EventAgentTextDelta:    ViewTypeMessageDelta,
}

// DefaultViewModelMapper implements ViewModelMapper from viewTypeByEventType.
type DefaultViewModelMapper struct{}

// ViewType returns the end-side view_type for t, or "" when t has no alias.
func (DefaultViewModelMapper) ViewType(t observability.EventType) string {
	return viewTypeByEventType[t]
}

var _ ViewModelMapper = DefaultViewModelMapper{}
