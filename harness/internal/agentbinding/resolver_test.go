package agentbinding

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/executionmode"
)

type configResolverFunc func(context.Context, ConfigResolveRequest) (ResolvedConfig, error)

func (f configResolverFunc) ResolveConfig(ctx context.Context, req ConfigResolveRequest) (ResolvedConfig, error) {
	return f(ctx, req)
}

func TestResolverFinalizesRegistryResult(t *testing.T) {
	t.Parallel()
	resolved := resolvedAgent("agent-a", "v7", executionmode.SingleAgent)
	manifestRef := resolved.ConfigSnapshotRef + "#capabilities"
	resolved.CapabilitySnapshotRefs = []string{manifestRef}
	var captured ConfigResolveRequest
	resolver := NewResolver(configResolverFunc(func(_ context.Context, req ConfigResolveRequest) (ResolvedConfig, error) {
		captured = req
		return resolved, nil
	}))
	resolver.Clock = func() time.Time { return time.Unix(123, 0) }

	req := baseRequest(Selection{AgentID: "agent-a"})
	req.SchemaVersion = BindingSchemaVersion
	req.CreatedAt = time.Time{}
	result, err := resolver.Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	binding := result.Binding
	if captured.Selection.AgentID != "agent-a" || binding.AgentVersion != "v7" || binding.ExecutionMode != executionmode.SingleAgent {
		t.Fatalf("unexpected resolution: captured=%#v binding=%#v", captured, binding)
	}
	if binding.Target != (Target{Kind: TargetAgent, Ref: "agent-a", Version: "v7"}) {
		t.Fatalf("target = %#v", binding.Target)
	}
	if got := strings.Join(binding.CapabilitySnapshotRefs, ","); got != manifestRef {
		t.Fatalf("capability refs = %q", got)
	}
	if binding.SchemaVersion != BindingSchemaVersion || binding.Source != SourceRequestParam || binding.CreatedAt != time.Unix(123, 0).UTC() || binding.BindingHash == "" {
		t.Fatalf("binding facts incomplete: %#v", binding)
	}
	if err := binding.Validate(); err != nil {
		t.Fatalf("effective binding invalid: %v", err)
	}

	// Registry 返回值和调用结果不能共享可变引用，避免并发 Run 串包。
	resolved.Definition.Metadata["owner"] = "mutated"
	resolved.Definition.ToolRefs[0] = "mutated"
	if result.Definition.Metadata["owner"] != "team-a" || result.Definition.ToolRefs[0] != "tool-a@v1" {
		t.Fatalf("definition was not isolated: %#v", result.Definition)
	}
}

func TestResolverRequiresFrozenCapabilityManifest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*ResolvedConfig)
		want   ErrorCode
	}{
		{name: "exact tool ref with manifest"},
		{name: "tool manifest missing", mutate: func(resolved *ResolvedConfig) {
			resolved.CapabilitySnapshotRefs = nil
		}, want: CodeCapabilitySnapshotFailed},
		{name: "manifest does not match config snapshot", mutate: func(resolved *ResolvedConfig) {
			resolved.CapabilitySnapshotRefs = []string{"agent-config://other/v1/hash#capabilities"}
		}, want: CodeCapabilitySnapshotFailed},
		{name: "extra capability ref", mutate: func(resolved *ResolvedConfig) {
			resolved.CapabilitySnapshotRefs = append(resolved.CapabilitySnapshotRefs, "tool://legacy")
		}, want: CodeCapabilitySnapshotFailed},
		{name: "whitespace manifest", mutate: func(resolved *ResolvedConfig) {
			resolved.CapabilitySnapshotRefs[0] = " " + resolved.CapabilitySnapshotRefs[0]
		}, want: CodeCapabilitySnapshotFailed},
		{name: "unversioned tool", mutate: func(resolved *ResolvedConfig) {
			resolved.Definition.ToolRefs = []string{"tool-a"}
		}, want: CodeCapabilitySnapshotFailed},
		{name: "tool ref with multiple separators", mutate: func(resolved *ResolvedConfig) {
			resolved.Definition.ToolRefs = []string{"tool-a@alias@v1"}
		}, want: CodeCapabilitySnapshotFailed},
		{name: "skill with manifest", mutate: func(resolved *ResolvedConfig) {
			resolved.Definition.ToolRefs = nil
			resolved.Definition.Metadata["skill_refs"] = `["route@v1"]`
		}},
		{name: "skill manifest missing", mutate: func(resolved *ResolvedConfig) {
			resolved.Definition.ToolRefs = nil
			resolved.Definition.Metadata["skill_refs"] = `["route@v1"]`
			resolved.CapabilitySnapshotRefs = nil
		}, want: CodeCapabilitySnapshotFailed},
		{name: "mcp manifest missing", mutate: func(resolved *ResolvedConfig) {
			resolved.Definition.ToolRefs = nil
			resolved.Definition.Metadata["mcp_servers"] = `["maps"]`
			resolved.CapabilitySnapshotRefs = nil
		}, want: CodeCapabilitySnapshotFailed},
		{name: "empty metadata lists keep canonical manifest", mutate: func(resolved *ResolvedConfig) {
			resolved.Definition.ToolRefs = nil
			resolved.Definition.Metadata["skill_refs"] = `[]`
			resolved.Definition.Metadata["mcp_servers"] = `null`
		}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resolved := resolvedAgent("agent-a", "v1", executionmode.SingleAgent)
			if tt.mutate != nil {
				tt.mutate(&resolved)
			}
			resolver := NewResolver(configResolverFunc(func(context.Context, ConfigResolveRequest) (ResolvedConfig, error) {
				return resolved, nil
			}))

			_, err := resolver.Resolve(context.Background(), baseRequest(Selection{AgentID: "agent-a", Mode: executionmode.SingleAgent}))
			if CodeOf(err) != tt.want {
				t.Fatalf("Resolve() code = %q, want %q (err=%v)", CodeOf(err), tt.want, err)
			}
			if tt.want != "" && (StageOf(err) != StageFinalize || RetryableOf(err)) {
				t.Fatalf("Resolve() stage=%q retryable=%t", StageOf(err), RetryableOf(err))
			}
		})
	}
}

func TestResolverFallbackFailsClosedOnInvalidCapabilityManifest(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	resolver := NewResolver(configResolverFunc(func(_ context.Context, req ConfigResolveRequest) (ResolvedConfig, error) {
		calls.Add(1)
		if req.Selection.AgentID == "primary" {
			return ResolvedConfig{}, NewError(StageConfigResolve, CodeAgentUnavailable, true, nil)
		}
		resolved := resolvedAgent("fallback", "v1", executionmode.SingleAgent)
		resolved.CapabilitySnapshotRefs = nil
		return resolved, nil
	}))
	fallback := Selection{AgentID: "fallback", Mode: executionmode.SingleAgent}
	req := baseRequest(Selection{AgentID: "primary", Mode: executionmode.SingleAgent})
	req.Fallback = &fallback

	_, err := resolver.Resolve(context.Background(), req)
	if CodeOf(err) != CodeCapabilitySnapshotFailed || StageOf(err) != StageFinalize || RetryableOf(err) || calls.Load() != 2 {
		t.Fatalf("Resolve() code=%q stage=%q retryable=%t calls=%d", CodeOf(err), StageOf(err), RetryableOf(err), calls.Load())
	}
}

func TestResolverFallbackOnlyForUnavailableAgent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		primaryError  error
		wantFallback  bool
		wantCalls     int32
		wantErrorCode ErrorCode
	}{
		{"disabled falls back", NewError(StageConfigResolve, CodeAgentDisabled, false, nil), true, 2, ""},
		{"not found falls back", NewError(StageConfigResolve, CodeAgentNotFound, false, nil), true, 2, ""},
		{"version unavailable falls back", NewError(StageConfigResolve, CodeAgentVersionUnavailable, false, nil), true, 2, ""},
		{"agent unavailable falls back", NewError(StageConfigResolve, CodeAgentUnavailable, true, nil), true, 2, ""},
		{"invalid config fails closed", NewError(StageConfigResolve, CodeConfigInvalid, false, nil), false, 1, CodeConfigInvalid},
		{"unknown backend failure fails closed", errors.New("secret database endpoint"), false, 1, CodeConfigResolveFailed},
	}
	for _, tt := range tests {
		tc := tt
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			resolver := NewResolver(configResolverFunc(func(_ context.Context, req ConfigResolveRequest) (ResolvedConfig, error) {
				calls.Add(1)
				if req.Selection.AgentID == "primary" {
					return ResolvedConfig{}, tc.primaryError
				}
				return resolvedAgent(req.Selection.AgentID, "v2", executionmode.DeepAgent), nil
			}))
			req := baseRequest(Selection{AgentID: "primary", Mode: executionmode.DeepAgent})
			fallback := Selection{AgentID: "fallback", Mode: executionmode.DeepAgent}
			req.Fallback = &fallback
			result, err := resolver.Resolve(context.Background(), req)
			if tc.wantErrorCode != "" {
				if CodeOf(err) != tc.wantErrorCode {
					t.Fatalf("error code = %q, want %q", CodeOf(err), tc.wantErrorCode)
				}
			} else if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if calls.Load() != tc.wantCalls {
				t.Fatalf("resolver calls = %d, want %d", calls.Load(), tc.wantCalls)
			}
			if tc.wantFallback {
				if !result.Binding.Fallback.Applied || result.Binding.AgentID != "fallback" || result.Binding.Fallback.From.AgentID != "primary" || result.Binding.Fallback.To.AgentVersion != "v2" {
					t.Fatalf("fallback fact = %#v", result.Binding.Fallback)
				}
			}
		})
	}
}

func TestResolverDoesNotRetryIdenticalFallback(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	resolver := NewResolver(configResolverFunc(func(context.Context, ConfigResolveRequest) (ResolvedConfig, error) {
		calls.Add(1)
		return ResolvedConfig{}, NewError(StageConfigResolve, CodeAgentUnavailable, true, nil)
	}))
	selection := Selection{AgentID: "agent-a", Mode: executionmode.SingleAgent}
	req := baseRequest(selection)
	req.Fallback = &selection

	_, err := resolver.Resolve(context.Background(), req)
	if CodeOf(err) != CodeAgentUnavailable || calls.Load() != 1 {
		t.Fatalf("Resolve() code=%q calls=%d, want unavailable and one call", CodeOf(err), calls.Load())
	}
}

func TestResolverControlForceCannotBeBypassedByFallback(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	resolver := NewResolver(configResolverFunc(func(_ context.Context, req ConfigResolveRequest) (ResolvedConfig, error) {
		calls.Add(1)
		if req.Selection.AgentID == "forced" {
			return ResolvedConfig{}, NewError(StageConfigResolve, CodeAgentUnavailable, true, nil)
		}
		return resolvedAgent(req.Selection.AgentID, "v1", executionmode.SingleAgent), nil
	}))
	forced := Selection{AgentID: "forced", Mode: executionmode.SingleAgent}
	fallback := Selection{AgentID: "fallback", Mode: executionmode.SingleAgent}
	req := baseRequest(Selection{AgentID: "request", Mode: executionmode.SingleAgent})
	req.Control = &ControlRule{Action: ControlForce, Selection: &forced, Ref: "rule://force", Revision: "7"}
	req.Fallback = &fallback

	_, err := resolver.Resolve(context.Background(), req)
	if CodeOf(err) != CodeAgentUnavailable || calls.Load() != 1 {
		t.Fatalf("Resolve() code=%q calls=%d, want forced unavailable and one call", CodeOf(err), calls.Load())
	}
}

func TestResolverAskUserAndSafeFailurePayload(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	resolver := NewResolver(configResolverFunc(func(context.Context, ConfigResolveRequest) (ResolvedConfig, error) {
		calls.Add(1)
		return ResolvedConfig{}, nil
	}))
	req := baseRequest(Selection{})
	req.Request = nil
	_, err := resolver.Resolve(context.Background(), req)
	if !errors.Is(err, ErrAskUserRequired) || calls.Load() != 0 {
		t.Fatalf("Resolve() error = %v, calls=%d", err, calls.Load())
	}
	payload := NewFailurePayload(req, err)
	if payload.Clarification == nil || payload.Code != CodeAskUserRequired || payload.SafeMessage == "" {
		t.Fatalf("failure payload = %#v", payload)
	}

	raw := errors.New("postgres://user:password@secret-host")
	resolver = NewResolver(configResolverFunc(func(context.Context, ConfigResolveRequest) (ResolvedConfig, error) {
		return ResolvedConfig{}, raw
	}))
	req = baseRequest(Selection{AgentID: "agent-a", Mode: executionmode.SingleAgent})
	_, err = resolver.Resolve(context.Background(), req)
	payload = NewFailurePayload(req, err)
	if strings.Contains(err.Error(), "password") || strings.Contains(payload.SafeMessage, "password") || payload.Code != CodeConfigResolveFailed || !payload.Retryable {
		t.Fatalf("unsafe failure exposure: err=%q payload=%#v", err, payload)
	}
}

func TestResolverDirectActionRequiresNativeOnlyRuntime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*ResolvedConfig)
		want   ErrorCode
	}{
		{name: "native direct"},
		{name: "non-native runtime", mutate: func(resolved *ResolvedConfig) {
			resolved.Definition.Runtime.Type = agentruntime.RuntimeTypeEino
		}, want: CodeDefinitionInvalid},
		{name: "non-native preferred runtime", mutate: func(resolved *ResolvedConfig) {
			resolved.Definition.Runtime.Preferred = agentruntime.RuntimeTypeEino
		}, want: CodeDefinitionInvalid},
		{name: "non-native fallback candidate", mutate: func(resolved *ResolvedConfig) {
			resolved.Definition.Runtime.Candidates = []agentruntime.RuntimeType{agentruntime.RuntimeTypeNative, agentruntime.RuntimeTypeEino}
		}, want: CodeDefinitionInvalid},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resolved := resolvedAgent("direct-agent", "v1", executionmode.DirectAction)
			resolved.Definition.Runtime.Type = agentruntime.RuntimeTypeNative
			if tt.mutate != nil {
				tt.mutate(&resolved)
			}
			resolver := NewResolver(configResolverFunc(func(context.Context, ConfigResolveRequest) (ResolvedConfig, error) {
				return resolved, nil
			}))
			selection := Selection{AgentID: resolved.Definition.AgentID, Mode: executionmode.DirectAction}
			_, err := resolver.Resolve(context.Background(), baseRequest(selection))
			if tt.want == "" && err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if tt.want != "" && CodeOf(err) != tt.want {
				t.Fatalf("error code = %q, want %q", CodeOf(err), tt.want)
			}
		})
	}
}

func TestResolverValidatesWorkflowAndGraph(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		resolved ResolvedConfig
		mutate   func(*ResolvedConfig)
		wantCode ErrorCode
	}{
		{name: "workflow", resolved: resolvedWorkflow()},
		{name: "graph", resolved: resolvedGraph()},
		{name: "workflow target missing", resolved: resolvedWorkflow(), mutate: func(r *ResolvedConfig) { r.Target = Target{} }, wantCode: CodeTargetInvalid},
		{name: "workflow definition missing", resolved: resolvedWorkflow(), mutate: func(r *ResolvedConfig) { r.Definition.Workflow = nil }, wantCode: CodeDefinitionInvalid},
		{name: "graph state schema missing", resolved: resolvedGraph(), mutate: func(r *ResolvedConfig) { r.Target.StateSchemaRef = "" }, wantCode: CodeTargetInvalid},
		{name: "graph schema mismatch", resolved: resolvedGraph(), mutate: func(r *ResolvedConfig) { r.Definition.Graph.StateSchemaRef = "schema://other" }, wantCode: CodeDefinitionInvalid},
		{name: "runtime mode mismatch", resolved: resolvedWorkflow(), mutate: func(r *ResolvedConfig) { r.Definition.Runtime.Mode = agentruntime.RuntimeModeGraph }, wantCode: CodeExecutionModeMismatch},
	}
	for _, tt := range tests {
		tc := tt
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resolved := tc.resolved
			if tc.mutate != nil {
				tc.mutate(&resolved)
			}
			resolver := NewResolver(configResolverFunc(func(context.Context, ConfigResolveRequest) (ResolvedConfig, error) {
				return resolved, nil
			}))
			selection := Selection{AgentID: resolved.Definition.AgentID, Mode: resolved.ExecutionMode, Target: resolved.Target}
			_, err := resolver.Resolve(context.Background(), baseRequest(selection))
			if tc.wantCode == "" && err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if tc.wantCode != "" && CodeOf(err) != tc.wantCode {
				t.Fatalf("error code = %q, want %q (err=%v)", CodeOf(err), tc.wantCode, err)
			}
		})
	}
}

func TestResolverConcurrentResultsAreDeterministicAndIsolated(t *testing.T) {
	t.Parallel()
	resolved := resolvedAgent("agent-a", "v1", executionmode.SingleAgent)
	resolver := NewResolver(configResolverFunc(func(context.Context, ConfigResolveRequest) (ResolvedConfig, error) {
		return resolved, nil
	}))
	req := baseRequest(Selection{AgentID: "agent-a", Mode: executionmode.SingleAgent})
	req.CreatedAt = time.Unix(100, 0)

	const count = 64
	results := make([]Result, count)
	errs := make([]error, count)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = resolver.Resolve(context.Background(), req)
		}(i)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("result %d error = %v", i, errs[i])
		}
		if results[i].Binding.BindingHash != results[0].Binding.BindingHash {
			t.Fatalf("result %d hash differs", i)
		}
	}
	results[0].Definition.Metadata["owner"] = "changed"
	results[0].Definition.ToolRefs[0] = "changed"
	results[0].Binding.CapabilitySnapshotRefs[0] = "changed"
	if results[1].Definition.Metadata["owner"] != "team-a" ||
		results[1].Definition.ToolRefs[0] != "tool-a@v1" ||
		results[1].Binding.CapabilitySnapshotRefs[0] == "changed" ||
		resolved.Definition.ToolRefs[0] != "tool-a@v1" {
		t.Fatal("concurrent results share mutable state")
	}
}

func TestResolverRejectsModeAndVersionDrift(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		selection Selection
		resolved  ResolvedConfig
		want      ErrorCode
	}{
		{"mode drift", Selection{AgentID: "agent-a", Mode: executionmode.DeepAgent}, resolvedAgent("agent-a", "v1", executionmode.SingleAgent), CodeExecutionModeMismatch},
		{"version drift", Selection{AgentID: "agent-a", AgentVersion: "v2", Mode: executionmode.SingleAgent}, resolvedAgent("agent-a", "v1", executionmode.SingleAgent), CodeDefinitionInvalid},
		{"legacy mode", Selection{AgentID: "agent-a", Mode: "workflow_graph"}, resolvedAgent("agent-a", "v1", executionmode.SingleAgent), CodeExecutionModeUnsupported},
	}
	for _, tt := range tests {
		tc := tt
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resolver := NewResolver(configResolverFunc(func(context.Context, ConfigResolveRequest) (ResolvedConfig, error) { return tc.resolved, nil }))
			_, err := resolver.Resolve(context.Background(), baseRequest(tc.selection))
			if CodeOf(err) != tc.want {
				t.Fatalf("error code = %q, want %q", CodeOf(err), tc.want)
			}
		})
	}
}

func baseRequest(selection Selection) BindingRequest {
	return BindingRequest{
		BindingID: "bind-1",
		SessionID: "session-1",
		RunID:     "run-1",
		Request:   &selection,
		CreatedAt: time.Unix(100, 0).UTC(),
	}
}

func resolvedAgent(agentID, version string, mode executionmode.Mode) ResolvedConfig {
	runtimeMode, _ := executionmode.ToRuntimeMode(mode)
	resolved := ResolvedConfig{
		Definition: agentruntime.AgentDefinition{
			AgentID: agentID, AgentType: "test", Version: version,
			Runtime:  agentruntime.RuntimeSpec{Type: agentruntime.RuntimeTypeMock, Mode: runtimeMode},
			ToolRefs: []string{"tool-a@v1"}, Metadata: map[string]string{"owner": "team-a"},
		},
		ExecutionMode:     mode,
		ConfigSnapshotRef: "agent-config://" + agentID + "/" + version + "/hash",
		ConfigHash:        "sha256:config",
	}
	resolved.CapabilitySnapshotRefs = []string{resolved.ConfigSnapshotRef + "#capabilities"}
	return resolved
}

func resolvedWorkflow() ResolvedConfig {
	resolved := resolvedAgent("workflow-agent", "v1", executionmode.Workflow)
	resolved.Target = Target{Kind: TargetWorkflow, Ref: "travel-workflow", Version: "v3", Hash: "sha256:workflow"}
	resolved.Definition.Workflow = &agentruntime.WorkflowDefinition{
		WorkflowID: "travel-workflow", EntryNode: "start",
		Nodes: []agentruntime.WorkflowNode{{NodeID: "start", NodeType: "agent"}},
	}
	return resolved
}

func resolvedGraph() ResolvedConfig {
	resolved := resolvedAgent("graph-agent", "v1", executionmode.Graph)
	resolved.Target = Target{Kind: TargetGraph, Ref: "travel-graph", Version: "v2", Hash: "sha256:graph", StateSchemaRef: "schema://graph-state/v2"}
	resolved.Definition.Graph = &agentruntime.GraphDefinition{
		GraphID: "travel-graph", EntryNode: "start", StateSchemaRef: "schema://graph-state/v2",
		Nodes: []agentruntime.WorkflowNode{{NodeID: "start", NodeType: "agent"}},
	}
	return resolved
}
