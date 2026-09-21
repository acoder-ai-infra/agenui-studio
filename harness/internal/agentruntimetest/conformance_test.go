package agentruntimetest

import (
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestValidateModelRequestsRequiresPolicyAndPreserveManifest(t *testing.T) {
	pkg := agentruntime.ModelContextPackage{
		SchemaVersion: agentruntime.ModelContextPackageSchemaVersion,
		Run:           agentruntime.ModelContextRun{RunID: "run_1", AgentID: "agent_1"},
		RuntimeConstraints: agentruntime.ModelContextRuntimeConstraints{
			TokenBudget:      agentruntime.ModelTokenBudget{MaxInputTokens: 100},
			CompactionPolicy: agentruntime.DefaultContextCompactionPolicy(),
		},
	}
	req := agentruntime.ModelInvokeRequest{Package: pkg, Messages: []agentruntime.ModelCallMessage{{Role: "system", Content: "rules"}, {Role: "user", Content: "current"}}}
	manifest, err := agentruntime.BuildPreserveManifest(req)
	if err != nil {
		t.Fatal(err)
	}
	req.PreserveManifest = manifest
	req.PreModelCompaction = agentruntime.ModelPreModelCompaction{Completed: true, PolicyHash: pkg.RuntimeConstraints.CompactionPolicy.PolicyHash}
	if err := ValidateModelRequests([]agentruntime.ModelInvokeRequest{req}); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	req.Messages[0].Content = "changed"
	if err := ValidateModelRequests([]agentruntime.ModelInvokeRequest{req}); err == nil {
		t.Fatal("tampered request must fail model conformance")
	}
}

func TestValidateModelRequestsRejectsToolModeWithoutSchemas(t *testing.T) {
	pkg := agentruntime.ModelContextPackage{RuntimeConstraints: agentruntime.ModelContextRuntimeConstraints{
		TokenBudget: agentruntime.ModelTokenBudget{MaxInputTokens: 100}, CompactionPolicy: agentruntime.DefaultContextCompactionPolicy(),
	}}
	req := agentruntime.ModelInvokeRequest{Package: pkg, AllowTools: true, Messages: []agentruntime.ModelCallMessage{{Role: "user", Content: "current"}}}
	manifest, err := agentruntime.BuildPreserveManifest(req)
	if err != nil {
		t.Fatal(err)
	}
	req.PreserveManifest = manifest
	req.PreModelCompaction = agentruntime.ModelPreModelCompaction{Completed: true, PolicyHash: pkg.RuntimeConstraints.CompactionPolicy.PolicyHash}
	if err := ValidateModelRequests([]agentruntime.ModelInvokeRequest{req}); err == nil {
		t.Fatal("tool-enabled request without schemas must fail conformance")
	}
}

func TestValidateStreamAcceptsSequentialModelRoundsAndTerminalRun(t *testing.T) {
	events := []observability.AgentEvent{
		{EventType: observability.EventAgentStarted},
		{EventType: observability.EventModelCallStarted},
		{EventType: observability.EventModelTokenDelta},
		{EventType: observability.EventModelCallCompleted},
		{EventType: observability.EventModelCallStarted},
		{EventType: observability.EventModelCallCompleted},
		{EventType: observability.EventAgentCompleted},
		{EventType: observability.EventRunCompleted},
	}
	if err := ValidateStream(events); err != nil {
		t.Fatalf("valid stream rejected: %v", err)
	}
}

func TestValidateStreamRejectsLifecycleViolations(t *testing.T) {
	tests := []struct {
		name   string
		events []observability.AgentEvent
		want   string
	}{
		{name: "delta before model start", events: []observability.AgentEvent{{EventType: observability.EventModelTokenDelta}}, want: "no active model call"},
		{name: "duplicate model start", events: []observability.AgentEvent{{EventType: observability.EventModelCallStarted}, {EventType: observability.EventModelCallStarted}}, want: "previous call terminated"},
		{name: "incomplete model", events: []observability.AgentEvent{{EventType: observability.EventModelCallStarted}}, want: "incomplete"},
		{name: "agent terminal without start", events: []observability.AgentEvent{{EventType: observability.EventAgentCompleted}}, want: "was not started"},
		{name: "run terminal not last", events: []observability.AgentEvent{{EventType: observability.EventRunFailed}, {EventType: observability.EventAgentTextDelta}}, want: "not the final event"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateStream(tt.events)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateStream() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}
