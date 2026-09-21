package agentgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/gatewaycontract"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestServiceLocalProjectsScopedDataAndEmitsLifecycle(t *testing.T) {
	provider := &capturingLocalProvider{}
	service := newServiceForTest(t, provider)
	sink := &recordingSubAgentSink{}
	result, err := service.Invoke(context.Background(), validSubAgentRequest(), sink)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Content != "child:book a hotel" || result.ChildRunID == "" {
		t.Fatalf("unexpected result: %+v", result)
	}
	provider.mu.Lock()
	captured := provider.requests[0]
	provider.mu.Unlock()
	if len(captured.ScopedData.Run) != 2 || string(captured.ScopedData.Run["locale"].Value) != `"zh-CN"` || string(captured.ScopedData.Run["target_only"].Value) != `"yes"` {
		t.Fatalf("scoped projection mismatch: %+v", captured.ScopedData)
	}
	if _, leaked := captured.ScopedData.Run["secret"]; leaked || len(captured.ScopedData.Agents) != 0 {
		t.Fatalf("unapproved scoped data leaked: %+v", captured.ScopedData)
	}
	if got := sink.types(); len(got) != 2 || got[0] != observability.EventSubAgentStarted || got[1] != observability.EventSubAgentCompleted {
		t.Fatalf("unexpected lifecycle: %v", got)
	}
}

func TestServiceAllowsLargeLocalInputAndOutput(t *testing.T) {
	large := strings.Repeat("x", 128*1024)
	provider := &capturingLocalProvider{result: &agentruntime.SubAgentInvocationResult{ChildRunID: "child-large", Content: large}}
	service := newServiceForTest(t, provider)
	req := validSubAgentRequest()
	req.Description = large
	result, err := service.Invoke(context.Background(), req, &recordingSubAgentSink{})
	if err != nil {
		t.Fatalf("Invoke() rejected local payload above removed Service limits: %v", err)
	}
	if len(result.Content) != len(large) || provider.count() != 1 {
		t.Fatalf("result bytes=%d provider calls=%d", len(result.Content), provider.count())
	}
}

func TestServiceCoreResultAndUTF8Invariants(t *testing.T) {
	t.Run("invalid input utf8", func(t *testing.T) {
		provider := &capturingLocalProvider{}
		service := newServiceForTest(t, provider)
		req := validSubAgentRequest()
		req.Description = string([]byte{0xff})
		sink := &recordingSubAgentSink{}
		_, err := service.Invoke(context.Background(), req, sink)
		if !errors.Is(err, ErrTaskInputInvalid) || provider.count() != 0 {
			t.Fatalf("err=%v provider calls=%d", err, provider.count())
		}
		assertFailedLifecycle(t, sink)
	})

	for _, tc := range []struct {
		name   string
		result agentruntime.SubAgentInvocationResult
	}{
		{name: "empty", result: agentruntime.SubAgentInvocationResult{ChildRunID: "child"}},
		{name: "invalid content utf8", result: agentruntime.SubAgentInvocationResult{ChildRunID: "child", Content: string([]byte{0xff})}},
		{name: "invalid content ref utf8", result: agentruntime.SubAgentInvocationResult{ChildRunID: "child", ContentRef: string([]byte{0xff})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &capturingLocalProvider{result: &tc.result}
			service := newServiceForTest(t, provider)
			sink := &recordingSubAgentSink{}
			_, err := service.Invoke(context.Background(), validSubAgentRequest(), sink)
			if !errors.Is(err, ErrTaskOutputInvalid) || provider.count() != 1 {
				t.Fatalf("err=%v provider calls=%d", err, provider.count())
			}
			assertFailedLifecycle(t, sink)
		})
	}

	t.Run("content ref only", func(t *testing.T) {
		provider := &capturingLocalProvider{result: &agentruntime.SubAgentInvocationResult{ChildRunID: "child", ContentRef: "artifact://result"}}
		service := newServiceForTest(t, provider)
		result, err := service.Invoke(context.Background(), validSubAgentRequest(), &recordingSubAgentSink{})
		if err != nil || result.ContentRef != "artifact://result" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})
}

func TestServiceFailsClosedOnDepthBeforeProvider(t *testing.T) {
	provider := &capturingLocalProvider{}
	service := newServiceForTest(t, provider)
	req := validSubAgentRequest()
	req.Depth = 1
	sink := &recordingSubAgentSink{}
	if _, err := service.Invoke(context.Background(), req, sink); !errors.Is(err, ErrNestingLimit) || provider.count() != 0 {
		t.Fatalf("depth gate failed: %v", err)
	}
	assertFailedLifecycle(t, sink)

	service.targets = failingTargetResolver{err: ErrNestingLimit}
	sink = &recordingSubAgentSink{}
	if _, err := service.Invoke(context.Background(), validSubAgentRequest(), sink); !errors.Is(err, ErrNestingLimit) || provider.count() != 0 {
		t.Fatalf("persisted parent depth gate failed: %v", err)
	}
	assertFailedLifecycle(t, sink)
}

func TestServiceRejectsIncompleteBindingBeforeProvider(t *testing.T) {
	provider := &capturingLocalProvider{}
	target := serviceTestTarget(t)
	target.Binding.ConfigHash = ""
	service, err := NewService(ServiceConfig{Targets: staticTargetResolver{target: target}, Local: provider})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	sink := &recordingSubAgentSink{}
	_, err = service.Invoke(context.Background(), validSubAgentRequest(), sink)
	if !errors.Is(err, ErrTargetResolveFailed) || provider.count() != 0 {
		t.Fatalf("err=%v provider calls=%d", err, provider.count())
	}
	if len(sink.snapshot()) != 0 {
		t.Fatalf("invalid binding emitted lifecycle events: %v", sink.types())
	}
}

func TestServiceRejectsInvalidConfiguredPluginBeforeLifecycleAndProvider(t *testing.T) {
	for _, tc := range []struct {
		name    string
		plugin  gatewaycontract.GatewayPluginConfig
		wantErr error
	}{
		{name: "unknown", plugin: gatewaycontract.GatewayPluginConfig{PluginID: "unknown"}, wantErr: ErrPluginUnavailable},
		{name: "invalid config", plugin: gatewaycontract.GatewayPluginConfig{PluginID: basicValidatorPluginID, Config: json.RawMessage(`{"unexpected":true}`)}, wantErr: ErrPluginConfigInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &capturingLocalProvider{}
			target := serviceTestTarget(t)
			target.Plugins = []gatewaycontract.GatewayPluginConfig{tc.plugin}
			service, err := NewService(ServiceConfig{Targets: staticTargetResolver{target: target}, Local: provider})
			if err != nil {
				t.Fatal(err)
			}
			sink := &recordingSubAgentSink{}
			_, err = service.Invoke(context.Background(), validSubAgentRequest(), sink)
			if !errors.Is(err, tc.wantErr) || provider.count() != 0 {
				t.Fatalf("err=%v provider calls=%d", err, provider.count())
			}
			if len(sink.snapshot()) != 0 {
				t.Fatalf("invalid plugin emitted lifecycle events: %v", sink.types())
			}
		})
	}
}

func TestServiceConcurrentInvocationsKeepFactsAndInputsIsolated(t *testing.T) {
	provider := &capturingLocalProvider{}
	service := newServiceForTest(t, provider)
	const calls = 64
	var wg sync.WaitGroup
	errs := make(chan error, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := validSubAgentRequest()
			req.TaskID = fmt.Sprintf("task-%d", i)
			req.Description = fmt.Sprintf("description-%d", i)
			req.ScopedData.Run["locale"] = agentruntime.ScopedDataItem{Value: json.RawMessage(fmt.Sprintf(`"locale-%d"`, i))}
			result, err := service.Invoke(context.Background(), req, &recordingSubAgentSink{})
			if err != nil || result.Content != "child:"+req.Description {
				errs <- fmt.Errorf("invoke %d: result=%+v err=%w", i, result, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if provider.count() != calls {
		t.Fatalf("provider calls=%d want=%d", provider.count(), calls)
	}
}

func TestServiceEmitsGatewayTaskV2Payload(t *testing.T) {
	service := newServiceForTest(t, &capturingLocalProvider{})
	sink := &recordingSubAgentSink{}
	if _, err := service.Invoke(context.Background(), validSubAgentRequest(), sink); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	for _, event := range sink.snapshot() {
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if payload["schema_version"] != GatewayTaskSchemaVersion {
			t.Fatalf("schema_version=%v", payload["schema_version"])
		}
		if _, exists := payload["task_contract_ref"]; exists {
			t.Fatalf("v2 payload contains task_contract_ref: %s", event.Payload)
		}
		if _, exists := payload["task_contract_hash"]; exists {
			t.Fatalf("v2 payload contains task_contract_hash: %s", event.Payload)
		}
		binding, ok := payload["provider_binding"].(map[string]any)
		if !ok || binding["schema_version"] != ProviderBindingSchemaV2 {
			t.Fatalf("provider binding=%v", payload["provider_binding"])
		}
		if _, exists := binding["profile_hash"]; exists {
			t.Fatalf("v2 provider binding contains profile_hash: %s", event.Payload)
		}
		if event.EventType == observability.EventSubAgentCompleted {
			details, ok := payload["details"].(map[string]any)
			if !ok {
				t.Fatalf("completed details=%T", payload["details"])
			}
			audit, ok := details["audit_fields"].(map[string]any)
			if !ok || audit["audit_recorded"] != defaultAuditPluginID {
				t.Fatalf("completed audit fields=%v", details["audit_fields"])
			}
			outcome, ok := details["provider_outcome"].(map[string]any)
			if !ok || outcome["Phase"] != string(GatewayProviderOutcomeKnown) || outcome["Succeeded"] != true {
				t.Fatalf("completed provider outcome=%v", details["provider_outcome"])
			}
		}
	}
}

func TestServiceProviderFailureEmitsSingleFailedTerminal(t *testing.T) {
	providerErr := errors.New("provider failed")
	service := newServiceForTest(t, &capturingLocalProvider{err: providerErr})
	sink := &recordingSubAgentSink{}
	_, err := service.Invoke(context.Background(), validSubAgentRequest(), sink)
	if !errors.Is(err, providerErr) {
		t.Fatalf("error=%v want provider failure", err)
	}
	assertFailedLifecycle(t, sink)
}

func TestServicePropagatesTerminalSinkFailure(t *testing.T) {
	service := newServiceForTest(t, &capturingLocalProvider{err: errors.New("provider failed")})
	sinkErr := errors.New("event store unavailable")
	_, err := service.Invoke(context.Background(), validSubAgentRequest(), &failingTerminalSink{terminalErr: sinkErr})
	if !errors.Is(err, sinkErr) {
		t.Fatalf("terminal persistence failure lost: %v", err)
	}
}

func newServiceForTest(t *testing.T, local LocalProvider) *Service {
	t.Helper()
	service, err := NewService(ServiceConfig{Targets: staticTargetResolver{target: serviceTestTarget(t)}, Local: local})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func serviceTestTarget(t *testing.T) ResolvedTarget {
	t.Helper()
	return ResolvedTarget{
		Definition: agentruntime.AgentDefinition{
			AgentID: "hotel", Version: "v1",
			DataPassing: agentruntime.DataPassingPolicy{ScopedDataKeys: []string{"locale", "target_only"}},
		},
		Provider: gatewaycontract.SubAgentProviderLocalAgent,
		Binding: ProviderBinding{
			SchemaVersion: ProviderBindingSchemaV2, BindingID: "binding-1",
			ProviderKind:  gatewaycontract.SubAgentProviderLocalAgent,
			TargetAgentID: "hotel", TargetVersion: "v1", ConfigSnapshotRef: "config://hotel/v1",
			ConfigHash: "sha256:config", CreatedAt: time.Now(),
		},
	}
}

func validSubAgentRequest() agentruntime.SubAgentInvocationRequest {
	return agentruntime.SubAgentInvocationRequest{
		Scope: agentruntime.SubAgentScopePlatformChildRun, TenantID: "tenant", SessionID: "session",
		ParentRunID: "parent", ParentAgentID: "planner", ParentAgentVersion: "v1",
		ParentConfigSnapshotRef: "config://planner/v1", ParentConfigHash: "sha256:parent",
		SubAgentRef: "hotel", TaskID: "task-1", Description: "book a hotel",
		ScopedData: agentruntime.ScopedData{
			Run: map[string]agentruntime.ScopedDataItem{
				"locale": {Value: json.RawMessage(`"zh-CN"`)}, "secret": {Value: json.RawMessage(`"no"`)},
			},
			Agents: map[string]map[string]agentruntime.ScopedDataItem{
				"hotel": {"target_only": {Value: json.RawMessage(`"yes"`)}},
				"other": {"target_only": {Value: json.RawMessage(`"leak"`)}},
			},
		},
	}
}

type staticTargetResolver struct{ target ResolvedTarget }

func (r staticTargetResolver) Resolve(context.Context, ResolveTargetRequest) (ResolvedTarget, error) {
	return r.target, nil
}

type failingTargetResolver struct{ err error }

func (r failingTargetResolver) Resolve(context.Context, ResolveTargetRequest) (ResolvedTarget, error) {
	return ResolvedTarget{}, r.err
}

type capturingLocalProvider struct {
	mu       sync.Mutex
	requests []LocalInvocationRequest
	result   *agentruntime.SubAgentInvocationResult
	err      error
}

func (p *capturingLocalProvider) Invoke(_ context.Context, req LocalInvocationRequest) (agentruntime.SubAgentInvocationResult, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	if p.err != nil {
		return agentruntime.SubAgentInvocationResult{}, p.err
	}
	if p.result != nil {
		return *p.result, nil
	}
	return agentruntime.SubAgentInvocationResult{ChildRunID: "child-" + req.Invocation.InvocationID, Content: "child:" + req.Invocation.Description}, nil
}

func (p *capturingLocalProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

type recordingSubAgentSink struct {
	mu     sync.Mutex
	events []observability.AgentEvent
}

func (s *recordingSubAgentSink) Emit(_ context.Context, event observability.AgentEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	return nil
}

func (s *recordingSubAgentSink) types() []observability.EventType {
	events := s.snapshot()
	out := make([]observability.EventType, len(events))
	for i := range events {
		out[i] = events[i].EventType
	}
	return out
}

func (s *recordingSubAgentSink) snapshot() []observability.AgentEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]observability.AgentEvent(nil), s.events...)
}

func assertFailedLifecycle(t *testing.T, sink *recordingSubAgentSink) {
	t.Helper()
	got := sink.types()
	if len(got) != 2 || got[0] != observability.EventSubAgentStarted || got[1] != observability.EventSubAgentFailed {
		t.Fatalf("unexpected failed lifecycle: %v", got)
	}
}

type failingTerminalSink struct{ terminalErr error }

func (s *failingTerminalSink) Emit(_ context.Context, event observability.AgentEvent) error {
	if event.EventType == observability.EventSubAgentFailed {
		return s.terminalErr
	}
	return nil
}
