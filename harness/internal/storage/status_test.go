package storage

import "testing"

func TestRunTransitionsMatchRuntimeStateMachine(t *testing.T) {
	tests := []struct {
		from RunStatus
		to   RunStatus
	}{
		{RunStatusWaitingControl, RunStatusFailed},
		{RunStatusWaitingControl, RunStatusCancelled},
		{RunStatusResuming, RunStatusCompleted},
		{RunStatusResuming, RunStatusCancelled},
		{RunStatusResuming, RunStatusExpired},
	}
	for _, test := range tests {
		if !CanTransitionRun(test.from, test.to) {
			t.Errorf("missing canonical transition %s -> %s", test.from, test.to)
		}
	}
}
