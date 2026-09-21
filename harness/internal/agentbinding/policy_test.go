package agentbinding

import (
	"context"
	"errors"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/executionmode"
)

func TestSourcePolicyPriority(t *testing.T) {
	t.Parallel()
	request := Selection{AgentID: "request", Mode: executionmode.SingleAgent}
	session := Selection{AgentID: "session", Mode: executionmode.SingleAgent}
	config := Selection{AgentID: "config", Mode: executionmode.SingleAgent}
	control := Selection{AgentID: "control", Mode: executionmode.SingleAgent}
	tests := []struct {
		name       string
		req        BindingRequest
		wantAgent  string
		wantSource Source
	}{
		{
			name: "force overrides every source",
			req: BindingRequest{
				Request: &request, SessionDefault: &session, ConfigDefault: &config,
				Control: &ControlRule{Action: ControlForce, Selection: &control, Ref: "rule://force", Revision: "7"},
			},
			wantAgent: "control", wantSource: SourceControlPlane,
		},
		{
			name: "request beats defaults",
			req: BindingRequest{
				Request: &request, SessionDefault: &session, ConfigDefault: &config,
				Control: &ControlRule{Action: ControlDefault, Selection: &control, Ref: "rule://default", Revision: "3"},
			},
			wantAgent: "request", wantSource: SourceRequestParam,
		},
		{name: "session beats config", req: BindingRequest{SessionDefault: &session, ConfigDefault: &config}, wantAgent: "session", wantSource: SourceSessionDefault},
		{name: "config beats control default", req: BindingRequest{ConfigDefault: &config, Control: &ControlRule{Action: ControlDefault, Selection: &control, Ref: "rule://default", Revision: "3"}}, wantAgent: "config", wantSource: SourceConfigDefault},
		{name: "control default is last", req: BindingRequest{Control: &ControlRule{Action: ControlDefault, Selection: &control, Ref: "rule://default", Revision: "3"}}, wantAgent: "control", wantSource: SourceControlPlane},
	}
	for _, tt := range tests {
		tc := tt
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := (SourcePolicy{}).Select(tc.req)
			if err != nil {
				t.Fatalf("Select() error = %v", err)
			}
			if got.Selection.AgentID != tc.wantAgent || got.Source != tc.wantSource {
				t.Fatalf("Select() = %#v, want agent=%q source=%q", got, tc.wantAgent, tc.wantSource)
			}
		})
	}
}

func TestSourcePolicyDeny(t *testing.T) {
	t.Parallel()
	request := Selection{AgentID: "agent-a", Mode: executionmode.SingleAgent}

	_, err := (SourcePolicy{}).Select(BindingRequest{
		Request: &request,
		Control: &ControlRule{Action: ControlDeny, Selection: &Selection{AgentID: "agent-a"}, Ref: "rule://deny", Revision: "2"},
	})
	if CodeOf(err) != CodeControlDenied {
		t.Fatalf("deny error code = %q, want %q", CodeOf(err), CodeControlDenied)
	}

	got, err := (SourcePolicy{}).Select(BindingRequest{
		Request: &request,
		Control: &ControlRule{Action: ControlDeny, Selection: &Selection{AgentID: "agent-b"}, Ref: "rule://deny", Revision: "2"},
	})
	if err != nil {
		t.Fatalf("non-matching deny failed: %v", err)
	}
	if got.ControlRuleRef != "rule://deny" || got.ControlRuleRevision != "2" {
		t.Fatalf("evaluated control rule was not retained: %#v", got)
	}
}

func TestResolverAppliesDenyAfterConfigDefaultsMode(t *testing.T) {
	t.Parallel()
	request := Selection{AgentID: "agent-a"}
	resolver := NewResolver(configResolverFunc(func(context.Context, ConfigResolveRequest) (ResolvedConfig, error) {
		return resolvedAgent("agent-a", "v1", executionmode.SingleAgent), nil
	}))
	req := baseRequest(request)
	req.Control = &ControlRule{
		Action: ControlDeny, Selection: &Selection{Mode: executionmode.SingleAgent},
		Ref: "rule://deny-mode", Revision: "1",
	}
	if _, err := resolver.Resolve(context.Background(), req); CodeOf(err) != CodeControlDenied {
		t.Fatalf("late deny code = %q", CodeOf(err))
	}
}

func TestSourcePolicyMissingSelectionAsksUser(t *testing.T) {
	t.Parallel()
	_, err := (SourcePolicy{}).Select(BindingRequest{})
	if !errors.Is(err, ErrAskUserRequired) || CodeOf(err) != CodeAskUserRequired {
		t.Fatalf("Select() error = %v, code=%q", err, CodeOf(err))
	}
	var ask *AskUserError
	if !errors.As(err, &ask) || len(ask.Clarification.MissingFields) != 1 || ask.Clarification.MissingFields[0] != "agent_id" {
		t.Fatalf("unexpected AskUser error: %#v", err)
	}
}

func TestSourcePolicyRejectsLegacyModeAndInvalidControl(t *testing.T) {
	t.Parallel()
	legacy := Selection{AgentID: "agent", Mode: "workflow_graph"}
	if _, err := (SourcePolicy{}).Select(BindingRequest{Request: &legacy}); CodeOf(err) != CodeExecutionModeUnsupported {
		t.Fatalf("legacy mode code = %q", CodeOf(err))
	}
	valid := Selection{AgentID: "agent", Mode: executionmode.SingleAgent}
	if _, err := (SourcePolicy{}).Select(BindingRequest{Request: &valid, Control: &ControlRule{Action: ControlForce, Selection: &valid}}); CodeOf(err) != CodeControlRuleInvalid {
		t.Fatalf("invalid control code = %q", CodeOf(err))
	}
}
