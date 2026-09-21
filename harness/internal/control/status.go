// Package control is the ControlRequest service (D2): AskUser / permission /
// HITL review / MCP elicitation / dangerous-op confirmation and resume
// coordination. It owns the ControlRequestStatus enum and its transitions
// (canonical §7.4, state-machines §4); storage persists the status as a
// canonical string. It depends downward on storage + observability only.
package control

// Status is the ControlRequest lifecycle status. Owned here (D2).
type Status string

const (
	StatusPending   Status = "pending"
	StatusAnswered  Status = "answered"
	StatusExpired   Status = "expired"
	StatusCancelled Status = "cancelled"
	StatusFailed    Status = "failed"
)

// transitions encodes state-machines §4: pending -> terminal only.
var transitions = map[Status]map[Status]bool{
	StatusPending: {
		StatusAnswered:  true,
		StatusExpired:   true,
		StatusCancelled: true,
		StatusFailed:    true,
	},
}

// CanTransition reports whether from->to is a legal ControlRequest transition.
func CanTransition(from, to Status) bool { return transitions[from][to] }

// IsTerminal reports whether s is a terminal status.
func (s Status) IsTerminal() bool {
	switch s {
	case StatusAnswered, StatusExpired, StatusCancelled, StatusFailed:
		return true
	default:
		return false
	}
}
