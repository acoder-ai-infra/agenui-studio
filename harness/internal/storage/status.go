package storage

// RunStatus is the business execution status of one Agent run. It is owned by
// the Session/Run store (canonical contract §7.1) and mirrored by agentruntime
// via a type alias (D1-A). Do not confuse with scheduler.DispatchStatus.
type RunStatus string

const (
	RunStatusCreated        RunStatus = "created"
	RunStatusRunning        RunStatus = "running"
	RunStatusWaitingControl RunStatus = "waiting_control"
	RunStatusResuming       RunStatus = "resuming"
	RunStatusCompleted      RunStatus = "completed"
	RunStatusFailed         RunStatus = "failed"
	RunStatusCancelled      RunStatus = "cancelled"
	RunStatusExpired        RunStatus = "expired"
)

// IsTerminal reports whether a RunStatus is a terminal state that must not be
// transitioned away from (canonical §7.1, state-machines §1).
func (s RunStatus) IsTerminal() bool {
	switch s {
	case RunStatusCompleted, RunStatusFailed, RunStatusCancelled, RunStatusExpired:
		return true
	default:
		return false
	}
}

// BlocksNewTopLevelTurn reports whether a top-level Run still owns the Session's
// online execution slot. P0 intentionally permits one active top-level Run per
// Session so a later user message cannot enter an earlier Run's frozen context
// snapshot. Child Runs are governed by their parent and do not consume this slot.
func (s RunStatus) BlocksNewTopLevelTurn() bool {
	return !s.IsTerminal()
}

// runStatusTransitions is the legal transition table.
//
// It encodes exactly the canonical contract §7.1 / state-machines §1 diagram,
// plus resuming->failed which is required to terminalize a failed Runtime.Resume
// (canonical §10 resume_failed). That single addition is flagged in
// docs/session-run-storage-landing-design.md for confirmation.
var runStatusTransitions = map[RunStatus]map[RunStatus]bool{
	RunStatusCreated: {
		RunStatusRunning: true,
		// Pre-run terminalization: a created run that never reaches running can
		// still fail (context build failure), be cancelled, or expire before
		// dispatch (session-run-storage-design.md §6 异常链路, failure-matrix §2).
		// created->completed stays illegal (state-machines §1: must reach running).
		RunStatusFailed:    true,
		RunStatusCancelled: true,
		RunStatusExpired:   true,
	},
	RunStatusRunning: {
		RunStatusWaitingControl: true,
		RunStatusCompleted:      true,
		RunStatusFailed:         true,
		RunStatusCancelled:      true,
		RunStatusExpired:        true,
	},
	RunStatusWaitingControl: {
		RunStatusResuming:  true,
		RunStatusFailed:    true,
		RunStatusCancelled: true,
		RunStatusExpired:   true,
	},
	RunStatusResuming: {
		RunStatusRunning:        true,
		RunStatusWaitingControl: true,
		RunStatusCompleted:      true,
		RunStatusFailed:         true,
		RunStatusCancelled:      true,
		RunStatusExpired:        true,
	},
}

// CanTransitionRun reports whether from->to is a legal RunStatus transition.
func CanTransitionRun(from, to RunStatus) bool {
	return runStatusTransitions[from][to]
}

// ValidateRunTransition returns an ErrIllegalTransition error when from->to is
// not permitted.
func ValidateRunTransition(from, to RunStatus) error {
	if from == to {
		return nil
	}
	if !CanTransitionRun(from, to) {
		return errorf(ErrIllegalTransition, "illegal run status transition %s -> %s", from, to)
	}
	return nil
}
