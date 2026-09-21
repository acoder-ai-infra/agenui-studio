package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestGovernedModelInvokerEmitsContextBudgetBreakdown(t *testing.T) {
	underlying := &capturingEinoModelInvoker{items: []ModelStreamItem{modelTextEvent("ok")}}
	emitter := &capturingRuntimeEmitter{}
	pkg := validEinoModelPackage()
	pkg.RuntimeConstraints.TokenBudget.MaxInputTokens = 1000
	pkg.RuntimeConstraints.TokenBudget.ReservedOutputTokens = 100
	pkg.ContextHash = ComputeModelContextHash(pkg)
	_, err := NewGovernedModelInvoker(underlying, NewDefaultModelInputGovernor(deterministicModelInputCounter{})).Invoke(
		WithRuntimeEventEmitter(context.Background(), emitter),
		ModelInvokeRequest{
			Package: pkg, Round: 3,
			Messages: []ModelCallMessage{
				{Role: "system", Content: "private system policy"},
				{Role: "user", Content: "old question"},
				{Role: "assistant", Content: "old answer"},
				{Role: "assistant", ToolCalls: []ModelToolCall{{ToolCallID: "call-1", Name: "search", Arguments: json.RawMessage(`{"q":"harness"}`)}}},
				{Role: "tool", ToolName: "search", ToolCallID: "call-1", Content: "search result"},
				{Role: "user", Content: "current question"},
			},
			Tools: []ModelToolDefinition{{Name: "search", Description: "Search projects", Schema: json.RawMessage(`{"type":"object"}`)}},
		},
	)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if len(emitter.events) != 1 || emitter.events[0].EventType != observability.EventModelContextBuilt {
		t.Fatalf("context events=%#v", emitter.events)
	}
	var payload struct {
		TokenCount int                 `json:"token_count"`
		Report     ContextBudgetReport `json:"context_budget_report"`
	}
	if err := json.Unmarshal(emitter.events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Report.SchemaVersion != ContextBudgetReportSchemaVersion || payload.Report.Round != 3 || payload.Report.UsedTokens != payload.TokenCount {
		t.Fatalf("report identity=%#v token_count=%d", payload.Report, payload.TokenCount)
	}
	segmentTokens := 0
	segments := map[ContextBudgetCategory]ContextBudgetSegment{}
	for _, segment := range payload.Report.Segments {
		segmentTokens += segment.Tokens
		segments[segment.Category] = segment
	}
	if segmentTokens != payload.Report.UsedTokens {
		t.Fatalf("segment total=%d used=%d segments=%#v", segmentTokens, payload.Report.UsedTokens, payload.Report.Segments)
	}
	for _, category := range []ContextBudgetCategory{ContextBudgetInstructions, ContextBudgetCurrentInput, ContextBudgetHistory, ContextBudgetToolDefinitions, ContextBudgetToolResults} {
		if segments[category].Tokens <= 0 {
			t.Fatalf("missing category %s: %#v", category, payload.Report.Segments)
		}
	}
	system := segments[ContextBudgetInstructions]
	if len(system.Items) != 1 || !system.Items[0].Protected || system.Items[0].ContentPreview != "" {
		t.Fatalf("system content was not protected: %#v", system)
	}
	current := segments[ContextBudgetCurrentInput]
	if len(current.Items) != 1 || current.Items[0].ContentPreview != "current question" {
		t.Fatalf("current input breakdown=%#v", current)
	}
}

type modelCallingConversationSummarizer struct {
	models ModelInvoker
}

func (s modelCallingConversationSummarizer) Summarize(ctx context.Context, _ []contextpkg.Message, _ int) (string, error) {
	_, err := s.models.Invoke(ctx, ModelInvokeRequest{})
	return "", err
}

func TestGovernedModelInvokerCompactsAfterFinalAssembly(t *testing.T) {
	underlying := &capturingEinoModelInvoker{items: []ModelStreamItem{modelTextEvent("ok")}}
	governor := NewDefaultModelInputGovernor(deterministicModelInputCounter{})
	pkg := validEinoModelPackage()
	pkg.RuntimeConstraints.TokenBudget.MaxInputTokens = 20
	pkg.ContextHash = ComputeModelContextHash(pkg)
	invoker := NewGovernedModelInvoker(underlying, governor)
	_, err := invoker.Invoke(context.Background(), ModelInvokeRequest{
		Package: pkg, Round: 2,
		Messages: []ModelCallMessage{
			{Role: "system", Content: "planner"},
			{Role: "user", Content: "old question"},
			{Role: "assistant", Content: "old answer"},
			{Role: "user", Content: "latest"},
		},
	})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	requests := underlying.Requests()
	if len(requests) != 1 || len(requests[0].Messages) != 2 {
		t.Fatalf("underlying gateway received ungoverned input: %#v", requests)
	}
	if requests[0].Messages[0].Content != "planner" || requests[0].Messages[1].Content != "latest" {
		t.Fatalf("protected final input changed: %#v", requests[0].Messages)
	}
}

type failingGovernanceEmitter struct{ err error }

func (e failingGovernanceEmitter) Emit(context.Context, observability.AgentEvent) error { return e.err }

func TestGovernedModelInvokerDoesNotCallModelWhenContextEventFails(t *testing.T) {
	underlying := &capturingEinoModelInvoker{items: []ModelStreamItem{modelTextEvent("must not run")}}
	pkg := validEinoModelPackage()
	pkg.RuntimeConstraints.TokenBudget.MaxInputTokens = 100
	pkg.ContextHash = ComputeModelContextHash(pkg)
	emitErr := errors.New("event store unavailable")
	ctx := WithRuntimeEventEmitter(context.Background(), failingGovernanceEmitter{err: emitErr})
	_, err := NewGovernedModelInvoker(underlying, nil).Invoke(ctx, ModelInvokeRequest{
		Package: pkg, Messages: []ModelCallMessage{{Role: "user", Content: "hello"}},
	})
	if !errors.Is(err, emitErr) {
		t.Fatalf("error=%v", err)
	}
	if len(underlying.Requests()) != 0 {
		t.Fatal("model was called without durable model_context_built")
	}
}

func TestGovernedModelInvokerIsTheFailClosedGatewayBoundary(t *testing.T) {
	underlying := &capturingEinoModelInvoker{items: []ModelStreamItem{modelTextEvent("must not run")}}
	pkg := validEinoModelPackage()
	pkg.RuntimeConstraints.TokenBudget.MaxInputTokens = 1
	pkg.ContextHash = ComputeModelContextHash(pkg)
	invoker := NewGovernedModelInvoker(underlying, NewDefaultModelInputGovernor(deterministicModelInputCounter{}))
	_, err := invoker.Invoke(context.Background(), ModelInvokeRequest{
		Package: pkg,
		Messages: []ModelCallMessage{
			{Role: "system", Content: "required planner instruction"},
			{Role: "user", Content: "latest"},
		},
	})
	if !errors.Is(err, ErrModelInputBudgetExceeded) {
		t.Fatalf("error=%v", err)
	}
	if len(underlying.Requests()) != 0 {
		t.Fatal("final input bypassed governance")
	}
}

type selectedCompressionStrategy struct{ called bool }

func (s *selectedCompressionStrategy) Name() string { return "selected" }

func (s *selectedCompressionStrategy) Compress(ctx context.Context, req ModelInputCompressionRequest) (ModelInvokeRequest, int, error) {
	s.called = true
	result := req.Request
	result.Messages = result.Messages[len(result.Messages)-1:]
	tokens, err := req.Counter.Count(ctx, result.Messages, result.Tools)
	return result, tokens, err
}

type unsafeCompressionStrategy struct{}

func (unsafeCompressionStrategy) Name() string { return "unsafe" }

func (unsafeCompressionStrategy) Compress(_ context.Context, req ModelInputCompressionRequest) (ModelInvokeRequest, int, error) {
	result := req.Request
	result.Messages = nil
	return result, 0, nil
}

func TestModelInputGovernorRejectsStrategyThatDropsPinnedInput(t *testing.T) {
	governor := &DefaultModelInputGovernor{
		Counter: deterministicModelInputCounter{}, Strategies: []ModelInputCompressionStrategy{unsafeCompressionStrategy{}},
	}
	pkg := validEinoModelPackage()
	pkg.RuntimeConstraints.TokenBudget.MaxInputTokens = 1
	pkg.ContextHash = ComputeModelContextHash(pkg)
	_, _, err := governor.Govern(context.Background(), ModelInvokeRequest{
		Package: pkg,
		Messages: []ModelCallMessage{
			{Role: "system", Content: "required"},
			{Role: "user", Content: "current"},
		},
	})
	if !errors.Is(err, ErrModelInputCompressionInvalid) {
		t.Fatalf("unsafe strategy error=%v", err)
	}
}

func TestModelInputGovernorSupportsSelectedCompressionStrategies(t *testing.T) {
	strategy := &selectedCompressionStrategy{}
	governor := &DefaultModelInputGovernor{
		Counter: deterministicModelInputCounter{}, Strategies: []ModelInputCompressionStrategy{strategy},
	}
	pkg := validEinoModelPackage()
	pkg.RuntimeConstraints.TokenBudget.MaxInputTokens = 10
	pkg.ContextHash = ComputeModelContextHash(pkg)
	req, result, err := governor.Govern(context.Background(), ModelInvokeRequest{
		Package: pkg,
		Messages: []ModelCallMessage{
			{Role: "user", Content: "old content"},
			{Role: "user", Content: "new"},
		},
	})
	if err != nil {
		t.Fatalf("govern: %v", err)
	}
	if !strategy.called || len(req.Messages) != 1 || len(result.AppliedStrategies) != 1 || result.AppliedStrategies[0] != "selected" {
		t.Fatalf("selected strategy was not applied: result=%#v messages=%#v", result, req.Messages)
	}
}

func TestGovernedModelInvokerEmitsDiagnosablePreModelFailure(t *testing.T) {
	underlying := &capturingEinoModelInvoker{items: []ModelStreamItem{modelTextEvent("must not run")}}
	pkg := validEinoModelPackage()
	pkg.RuntimeConstraints.CompactionPolicy = DefaultContextCompactionPolicy()
	pkg.ContextHash = ComputeModelContextHash(pkg)
	emitter := &capturingRuntimeEmitter{}
	ctx := WithRuntimeEventEmitter(context.Background(), emitter)
	unsafe := RuntimePreModelCompactorFunc(func(_ context.Context, req RuntimePreModelCompactionRequest) (RuntimePreModelCompactionResult, error) {
		result := req.Request
		result.Messages = []ModelCallMessage{{Role: "assistant", Content: "removed pinned input"}}
		return RuntimePreModelCompactionResult{Request: result}, nil
	})
	_, err := NewContextManagedModelInvoker(underlying, unsafe, nil).Invoke(ctx, ModelInvokeRequest{
		Package: pkg, Messages: []ModelCallMessage{{Role: "system", Content: "rules"}, {Role: "user", Content: "current"}},
	})
	if !errors.Is(err, ErrPreserveManifestInvalid) {
		t.Fatalf("error=%v", err)
	}
	if len(underlying.Requests()) != 0 {
		t.Fatal("unsafe pre-model request reached provider")
	}
	if len(emitter.events) != 1 || emitter.events[0].Error == nil || emitter.events[0].Error.Code != "MODEL_INPUT_PRESERVE_VIOLATION" {
		t.Fatalf("diagnostic event=%#v", emitter.events)
	}
}

func TestGovernedModelInvokerRejectsRuntimeThatSkippedPreModelStage(t *testing.T) {
	underlying := &capturingEinoModelInvoker{items: []ModelStreamItem{modelTextEvent("must not run")}}
	pkg := validEinoModelPackage()
	pkg.Run.RuntimeMode = string(RuntimeModeReact)
	pkg.RuntimeConstraints.CompactionPolicy = DefaultContextCompactionPolicy()
	pkg.ContextHash = ComputeModelContextHash(pkg)
	_, err := NewGovernedModelInvoker(underlying, nil).Invoke(context.Background(), ModelInvokeRequest{
		Package: pkg, Messages: []ModelCallMessage{{Role: "user", Content: "current"}},
	})
	if !errors.Is(err, ErrRuntimePreModelCompactorMissing) {
		t.Fatalf("skipped Stage B error=%v", err)
	}
	if len(underlying.Requests()) != 0 {
		t.Fatal("runtime bypassed mandatory pre-model stage")
	}
}

func TestContextSummarizerCannotRecursivelyUseNormalModelInvoker(t *testing.T) {
	underlying := &capturingEinoModelInvoker{items: []ModelStreamItem{modelTextEvent("must not run")}}
	emitter := &capturingRuntimeEmitter{}
	governed := NewGovernedModelInvoker(underlying, nil)
	compactor := contextpkg.NewRollingConversationCompactor(modelCallingConversationSummarizer{models: governed})
	_, err := compactor.Compact(WithRuntimeEventEmitter(context.Background(), emitter), contextpkg.ConversationCompactionRequest{
		Messages: []contextpkg.Message{
			{Role: contextpkg.RoleUser, Content: "old history that should be summarized"},
			{Role: contextpkg.RoleAssistant, Content: "old response that should be summarized"},
			{Role: contextpkg.RoleUser, Content: "current"},
		},
		TargetTokens: 15, PreserveRecentTurns: 1, MaxSummaryTokens: 32,
	})
	if !errors.Is(err, ErrRecursiveModelInvocationDuringCompaction) {
		t.Fatalf("recursive model invocation error=%v", err)
	}
	if len(underlying.Requests()) != 0 {
		t.Fatal("recursive summary model request reached the provider")
	}
	if len(emitter.events) != 1 || emitter.events[0].Error == nil || emitter.events[0].Error.Code != "MODEL_CONTEXT_COMPACTION_RECURSION" {
		t.Fatalf("recursive invocation diagnostic=%#v", emitter.events)
	}
}
