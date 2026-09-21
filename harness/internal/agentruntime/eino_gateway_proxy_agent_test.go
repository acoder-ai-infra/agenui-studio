package agentruntime

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestGatewayProxyAgentInvokesPlatformChildRun(t *testing.T) {
	invoker := &capturingSubAgentInvoker{result: SubAgentInvocationResult{ChildRunID: "child_1", Content: "done"}}
	agent, err := NewGatewayProxyAgent(GatewayProxyAgentConfig{
		Name: "researcher", Description: "research tasks", Invoker: invoker,
		Request: SubAgentInvocationRequest{TenantID: "tenant_1", SessionID: "session_1", ParentRunID: "run_1", ParentAgentID: "parent_1"},
		IDs:     fixedGatewayProxyIDs{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := gatewayProxyTestContext()
	iterator := agent.Run(ctx, &adk.AgentInput{Messages: []*schema.Message{{Role: schema.User, Content: "inspect module"}}})
	event, ok := iterator.Next()
	if !ok || event.Err != nil || event.Output == nil || event.Output.MessageOutput.Message.Content != "done" {
		t.Fatalf("unexpected output: %#v", event)
	}
	if _, more := iterator.Next(); more {
		t.Fatal("expected one terminal event")
	}
	req := invoker.Requests()[0]
	if req.Scope != SubAgentScopePlatformChildRun || req.SubAgentRef != "researcher" || req.Description != "inspect module" || req.TaskID != "task_fixed" {
		t.Fatalf("unexpected gateway request: %#v", req)
	}
	if req.Trace.TraceID == "" {
		t.Fatal("trace was not propagated")
	}
}

func TestGatewayProxyAgentReturnsArtifactReference(t *testing.T) {
	invoker := &capturingSubAgentInvoker{result: SubAgentInvocationResult{ChildRunID: "child_1", ContentRef: "artifact://result"}}
	agent, err := NewGatewayProxyAgent(GatewayProxyAgentConfig{Name: "worker", Description: "work", Invoker: invoker, Request: SubAgentInvocationRequest{SessionID: "session_1", ParentRunID: "run_1", ParentAgentID: "parent_1"}})
	if err != nil {
		t.Fatal(err)
	}
	iterator := agent.Run(gatewayProxyTestContext(), &adk.AgentInput{Messages: []*schema.Message{{Role: schema.User, Content: "work"}}})
	event, _ := iterator.Next()
	if event.Err != nil || event.Output.MessageOutput.Message.Content != `{"content_ref":"artifact://result","child_run_id":"child_1"}` {
		t.Fatalf("unexpected artifact result: %#v", event)
	}
}

func TestGatewayProxyAgentPropagatesFailureAndCancellation(t *testing.T) {
	t.Run("gateway failure", func(t *testing.T) {
		agent, err := NewGatewayProxyAgent(GatewayProxyAgentConfig{Name: "worker", Description: "work", Invoker: &capturingSubAgentInvoker{err: errors.New("gateway unavailable")}, Request: SubAgentInvocationRequest{SessionID: "session_1", ParentRunID: "run_1", ParentAgentID: "parent_1"}})
		if err != nil {
			t.Fatal(err)
		}
		iterator := agent.Run(gatewayProxyTestContext(), &adk.AgentInput{Messages: []*schema.Message{{Role: schema.User, Content: "work"}}})
		event, _ := iterator.Next()
		if !errors.Is(event.Err, ErrGatewayProxyAgentInvokeFailed) {
			t.Fatalf("err=%v", event.Err)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		invoker := &blockingSubAgentInvoker{cancelled: make(chan struct{})}
		agent, err := NewGatewayProxyAgent(GatewayProxyAgentConfig{Name: "worker", Description: "work", Invoker: invoker, Request: SubAgentInvocationRequest{SessionID: "session_1", ParentRunID: "run_1", ParentAgentID: "parent_1"}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(gatewayProxyTestContext())
		iterator := agent.Run(ctx, &adk.AgentInput{Messages: []*schema.Message{{Role: schema.User, Content: "work"}}})
		cancel()
		event, _ := iterator.Next()
		if !errors.Is(event.Err, context.Canceled) {
			t.Fatalf("err=%v", event.Err)
		}
		<-invoker.cancelled
	})
}

func TestGatewayProxyAgentConcurrentCallsHaveDistinctTasks(t *testing.T) {
	invoker := &capturingSubAgentInvoker{result: SubAgentInvocationResult{Content: "ok"}}
	agent, err := NewGatewayProxyAgent(GatewayProxyAgentConfig{Name: "worker", Description: "work", Invoker: invoker, Request: SubAgentInvocationRequest{SessionID: "session_1", ParentRunID: "run_1", ParentAgentID: "parent_1"}})
	if err != nil {
		t.Fatal(err)
	}
	const workers = 32
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			iterator := agent.Run(gatewayProxyTestContext(), &adk.AgentInput{Messages: []*schema.Message{{Role: schema.User, Content: "work"}}})
			if event, _ := iterator.Next(); event.Err != nil {
				t.Errorf("run: %v", event.Err)
			}
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, req := range invoker.Requests() {
		if seen[req.TaskID] {
			t.Fatalf("duplicate task_id=%s", req.TaskID)
		}
		seen[req.TaskID] = true
	}
}

func TestEinoRuntimeBuildsOnlyAuthorizedPlatformSubAgentProxies(t *testing.T) {
	invoker := &capturingSubAgentInvoker{result: SubAgentInvocationResult{Content: "ok"}}
	runtime := NewEinoRuntime(nil, RuntimeEnvironment{SubAgents: invoker}, nil)
	proxies, err := runtime.platformSubAgentProxies(RunRequest{
		TenantID: "tenant_1", SessionID: "session_1", RunID: "parent_run", Definition: AgentDefinition{AgentID: "parent_agent"},
	}, ModelContextPackage{Capabilities: ModelContextCapabilities{SubAgents: []string{"researcher", "reviewer"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 2 || proxies[0].Name(context.Background()) != "researcher" || proxies[1].Name(context.Background()) != "reviewer" {
		t.Fatalf("unexpected proxies: %#v", proxies)
	}
	none, err := runtime.platformSubAgentProxies(RunRequest{}, ModelContextPackage{})
	if err != nil || len(none) != 0 {
		t.Fatalf("unauthorized proxies should be empty: %#v err=%v", none, err)
	}
	child, err := runtime.platformSubAgentProxies(RunRequest{ParentRunID: "parent_run"}, ModelContextPackage{
		Capabilities: ModelContextCapabilities{SubAgents: []string{"forbidden_grandchild"}},
	})
	if !errors.Is(err, ErrNestedPlatformSubAgentForbidden) || len(child) != 0 {
		t.Fatalf("child runtime accepted nested platform delegation: proxies=%#v err=%v", child, err)
	}
}

func gatewayProxyTestContext() context.Context {
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TraceID: "trace_1", SpanID: "span_1"})
	return WithRuntimeEventEmitter(ctx, &capturingRuntimeEmitter{})
}

type capturingSubAgentInvoker struct {
	mu       sync.Mutex
	requests []SubAgentInvocationRequest
	result   SubAgentInvocationResult
	err      error
}

func (i *capturingSubAgentInvoker) Invoke(_ context.Context, req SubAgentInvocationRequest, events SubAgentEventSink) (SubAgentInvocationResult, error) {
	i.mu.Lock()
	i.requests = append(i.requests, req)
	i.mu.Unlock()
	if events != nil {
		_ = events.Emit(context.Background(), observability.AgentEvent{EventType: observability.EventSubAgentStarted, Visibility: observability.VisibilityDebug})
		_ = events.Emit(context.Background(), observability.AgentEvent{EventType: observability.EventSubAgentCompleted, Visibility: observability.VisibilityDebug})
	}
	return i.result, i.err
}

func (i *capturingSubAgentInvoker) Requests() []SubAgentInvocationRequest {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]SubAgentInvocationRequest(nil), i.requests...)
}

type blockingSubAgentInvoker struct{ cancelled chan struct{} }

func (i *blockingSubAgentInvoker) Invoke(ctx context.Context, _ SubAgentInvocationRequest, _ SubAgentEventSink) (SubAgentInvocationResult, error) {
	<-ctx.Done()
	close(i.cancelled)
	return SubAgentInvocationResult{}, ctx.Err()
}

type fixedGatewayProxyIDs struct{}

func (fixedGatewayProxyIDs) NewTraceID() string   { return "trace_fixed" }
func (fixedGatewayProxyIDs) NewSpanID() string    { return "span_fixed" }
func (fixedGatewayProxyIDs) NewRunID() string     { return "run_fixed" }
func (fixedGatewayProxyIDs) NewRequestID() string { return "task_fixed" }
func (fixedGatewayProxyIDs) NewEventID() string   { return "event_fixed" }
