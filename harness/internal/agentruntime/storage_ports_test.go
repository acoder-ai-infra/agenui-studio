package agentruntime

import (
	"context"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/storagewrite"
)

func TestRuntimeRunStorePortUsesLegacyErrorWhenTypedErrorIsNil(t *testing.T) {
	tests := []struct {
		name       string
		action     RunStoreAction
		wantStatus RunStatus
	}{
		{name: "fail", action: RunStoreActionFail, wantStatus: RunStatusFailed},
		{name: "expire", action: RunStoreActionExpire, wantStatus: RunStatusExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := NewInMemoryStateManager()
			req := testRunRequest()
			if _, err := state.StartRun(context.Background(), req); err != nil {
				t.Fatalf("start run: %v", err)
			}
			port := runtimeRunStorePort{state: state}
			_, err := port.Write(context.Background(), storagewrite.Write{
				Store: storagewrite.StoreRun,
				RunID: req.RunID,
				Payload: RunStoreWrite{
					Action: tt.action,
					Error:  "legacy failure",
				},
			})
			if err != nil {
				t.Fatalf("write run state: %v", err)
			}
			run, ok := state.Run(req.RunID)
			if !ok || run.Status != tt.wantStatus || run.ErrorMessage != "legacy failure" {
				t.Fatalf("legacy error was not preserved: %#v", run)
			}
		})
	}
}
