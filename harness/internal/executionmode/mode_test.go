package executionmode

import (
	"errors"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
)

func TestToRuntimeMode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		mode Mode
		want agentruntime.RuntimeMode
	}{
		{DirectAction, agentruntime.RuntimeModeDirect},
		{SingleAgent, agentruntime.RuntimeModeReact},
		{DeepAgent, agentruntime.RuntimeModeDeepAgent},
		{Workflow, agentruntime.RuntimeModeWorkflow},
		{Graph, agentruntime.RuntimeModeGraph},
	}
	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			t.Parallel()
			got, err := ToRuntimeMode(tt.mode)
			if err != nil || got != tt.want {
				t.Fatalf("ToRuntimeMode() = %q, %v; want %q", got, err, tt.want)
			}
			roundTrip, err := FromRuntimeMode(got)
			if err != nil || roundTrip != tt.mode {
				t.Fatalf("FromRuntimeMode() = %q, %v; want %q", roundTrip, err, tt.mode)
			}
		})
	}
}

func TestFromRuntimeModeRejectsUnboundModes(t *testing.T) {
	t.Parallel()
	for _, mode := range []agentruntime.RuntimeMode{"", agentruntime.RuntimeModePlanExecute, "unknown"} {
		if _, err := FromRuntimeMode(mode); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("FromRuntimeMode(%q) error = %v, want ErrUnsupported", mode, err)
		}
	}
}

func TestValidateRejectsLegacyAndReservedModes(t *testing.T) {
	t.Parallel()
	for _, mode := range []Mode{"", "workflow_graph", "state_graph", "plan_execute", "remote_a2a", "unknown"} {
		mode := mode
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			if err := mode.Validate(); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("Validate() error = %v, want ErrUnsupported", err)
			}
			if _, err := ToRuntimeMode(mode); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("ToRuntimeMode() error = %v, want ErrUnsupported", err)
			}
		})
	}
}
