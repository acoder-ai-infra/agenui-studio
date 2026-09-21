package agentruntime

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestEinoInternalAgentDecoratorWrapsInternalAgentOnce(t *testing.T) {
	decorator := EinoInternalAgentDecorator{}
	agent := observedCompletedEinoInternalAgent{}

	wrapped, err := decorator.Wrap(agent)
	if err != nil {
		t.Fatalf("wrap internal agent: %v", err)
	}
	wrappedAgain, err := decorator.Wrap(wrapped)
	if err != nil {
		t.Fatalf("wrap internal agent twice: %v", err)
	}
	if wrappedAgain != wrapped {
		t.Fatal("internal agent decorator must be idempotent")
	}

	emitter := &capturingRuntimeEmitter{}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TraceID: "trace_1", RunID: "run_1", RequestID: "request_1"})
	ctx = WithRuntimeEventEmitter(ctx, emitter)
	for range collectEinoIterator(wrapped.Run(ctx, &adk.AgentInput{})) {
	}
	if len(emitter.events) != 2 || emitter.events[0].EventType != EventRuntimeStepStarted || emitter.events[1].EventType != EventRuntimeStepCompleted {
		t.Fatalf("decorated lifecycle is invalid: %#v", emitter.events)
	}
}

func TestEinoInternalAgentDecoratorIsSafeForConcurrentFactoryAssembly(t *testing.T) {
	decorator := EinoInternalAgentDecorator{}
	const workers = 32
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := decorator.Wrap(observedCompletedEinoInternalAgent{})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent wrap: %v", err)
		}
	}
}

func TestEinoInternalAgentObserverPersistsOneLogicalStepAcrossInterruptResume(t *testing.T) {
	observer, err := NewEinoInternalAgentObserver(observedEinoInternalAgent{})
	if err != nil {
		t.Fatalf("build observer: %v", err)
	}
	emitter := &capturingRuntimeEmitter{}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TraceID: "trace_1", RunID: "run_1", RequestID: "request_1"})
	ctx = WithRuntimeParentStepID(ctx, "runtime_adapter_step")
	ctx = WithRuntimeEventEmitter(ctx, emitter)

	for range collectEinoIterator(observer.Run(ctx, &adk.AgentInput{})) {
	}
	if len(emitter.events) != 1 || emitter.events[0].EventType != EventRuntimeStepStarted {
		t.Fatalf("interrupted internal agent must keep its step open: %#v", emitter.events)
	}
	stepID := emitter.events[0].StepID
	if stepID == "" {
		t.Fatal("internal agent step_id missing")
	}

	for range collectEinoIterator(observer.Resume(ctx, &adk.ResumeInfo{})) {
	}
	if len(emitter.events) != 3 || emitter.events[1].EventType != EventRuntimeStepStarted || emitter.events[2].EventType != EventRuntimeStepCompleted {
		t.Fatalf("resumed internal agent lifecycle is invalid: %#v", emitter.events)
	}
	if emitter.events[1].StepID != stepID || emitter.events[2].StepID != stepID {
		t.Fatalf("resume created a different logical step: %#v", emitter.events)
	}
	var payload struct {
		Kind         StepKind          `json:"kind"`
		Name         string            `json:"name"`
		ParentStepID string            `json:"parent_step_id"`
		Metadata     map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(emitter.events[0].Payload, &payload); err != nil {
		t.Fatalf("decode internal step payload: %v", err)
	}
	if payload.Kind != StepKindRuntimeInternalAgent || payload.Name != "planner" || payload.ParentStepID != "runtime_adapter_step" || payload.Metadata["scope"] != string(SubAgentScopeRuntimeInternal) {
		t.Fatalf("internal step identity is incomplete: %#v", payload)
	}
}

func TestEinoInternalAgentObserverEmitsCancelledTerminal(t *testing.T) {
	observer, err := NewEinoInternalAgentObserver(cancelledEinoInternalAgent{})
	if err != nil {
		t.Fatalf("build observer: %v", err)
	}
	emitter := &capturingRuntimeEmitter{}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TraceID: "trace_1", RunID: "run_1", RequestID: "request_1"})
	ctx = WithRuntimeEventEmitter(ctx, emitter)

	for range collectEinoIterator(observer.Run(ctx, &adk.AgentInput{})) {
	}
	if len(emitter.events) != 2 || emitter.events[1].EventType != EventRuntimeStepCancelled {
		t.Fatalf("cancelled internal agent lifecycle is invalid: %#v", emitter.events)
	}
}

type observedEinoInternalAgent struct{}

type cancelledEinoInternalAgent struct{}

type observedCompletedEinoInternalAgent struct{}

func (observedCompletedEinoInternalAgent) Name(context.Context) string { return "critic" }
func (observedCompletedEinoInternalAgent) Description(context.Context) string {
	return "runtime critic"
}
func (observedCompletedEinoInternalAgent) Run(context.Context, *adk.AgentInput, ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	return observedEinoIterator(adk.EventFromMessage(schema.AssistantMessage("done", nil), nil, schema.Assistant, ""))
}
func (observedCompletedEinoInternalAgent) Resume(context.Context, *adk.ResumeInfo, ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	return observedEinoIterator(adk.EventFromMessage(schema.AssistantMessage("done", nil), nil, schema.Assistant, ""))
}

func (cancelledEinoInternalAgent) Name(context.Context) string        { return "planner" }
func (cancelledEinoInternalAgent) Description(context.Context) string { return "runtime planner" }
func (cancelledEinoInternalAgent) Run(context.Context, *adk.AgentInput, ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	return observedEinoIterator(&adk.AgentEvent{Err: context.Canceled})
}
func (cancelledEinoInternalAgent) Resume(context.Context, *adk.ResumeInfo, ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	return observedEinoIterator(&adk.AgentEvent{Err: context.Canceled})
}

func (observedEinoInternalAgent) Name(context.Context) string        { return "planner" }
func (observedEinoInternalAgent) Description(context.Context) string { return "runtime planner" }

func (observedEinoInternalAgent) Run(ctx context.Context, _ *adk.AgentInput, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	return observedEinoIterator(adk.Interrupt(ctx, "approval required"))
}

func (observedEinoInternalAgent) Resume(context.Context, *adk.ResumeInfo, ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	return observedEinoIterator(adk.EventFromMessage(schema.AssistantMessage("approved", nil), nil, schema.Assistant, ""))
}

func observedEinoIterator(events ...*adk.AgentEvent) *adk.AsyncIterator[*adk.AgentEvent] {
	iterator, generator := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	for _, event := range events {
		generator.Send(event)
	}
	generator.Close()
	return iterator
}

func collectEinoIterator(iterator *adk.AsyncIterator[*adk.AgentEvent]) []*adk.AgentEvent {
	var events []*adk.AgentEvent
	for {
		event, ok := iterator.Next()
		if !ok {
			return events
		}
		events = append(events, event)
	}
}
