package protocol

import "github.com/AGenUI/agenui-studio/harness/internal/observability"

// VisibilityFilter decides whether an event may be delivered to a target. It
// only enforces observability visibility; it never redefines it (canonical §5).
type VisibilityFilter interface {
	Allow(ev observability.AgentEvent, target ClientTarget) bool
}

// DefaultVisibilityFilter delivers user_visible to everyone; other visibilities
// require the target to be explicitly authorized (P-001).
type DefaultVisibilityFilter struct{}

// Allow reports whether ev may be delivered to target.
func (DefaultVisibilityFilter) Allow(ev observability.AgentEvent, target ClientTarget) bool {
	return target.Allows(ev.Visibility)
}

var _ VisibilityFilter = DefaultVisibilityFilter{}

// VisibilitiesForTarget returns the visibility set to pass to
// storage.EventQuery.Visibilities so replay is filtered at the store. Ordinary
// clients get [user_visible].
func VisibilitiesForTarget(target ClientTarget) []observability.EventVisibility {
	return target.Visibilities()
}
