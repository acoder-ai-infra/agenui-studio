package agentruntime

import (
	"context"
	"errors"
	"testing"
)

func TestModelInputGovernorPreservesOpaqueReasoningSignature(t *testing.T) {
	messages := modelCallMessagesFromContext([]ModelContextMessage{{
		Role: "assistant", ReasoningContent: "inspect", ReasoningSignature: "sig-opaque",
	}})
	if len(messages) != 1 || messages[0].ReasoningContent != "inspect" || messages[0].ReasoningSignature != "sig-opaque" {
		t.Fatalf("reasoning signature was lost: %#v", messages)
	}
}

type deterministicModelInputCounter struct{}

func (deterministicModelInputCounter) Count(_ context.Context, messages []ModelCallMessage, tools []ModelToolDefinition) (int, error) {
	total := 0
	for _, message := range messages {
		total += len(message.Content) + len(message.ReasoningContent) + 1
		for _, call := range message.ToolCalls {
			total += len(call.Name) + len(call.Arguments) + 1
		}
	}
	for _, tool := range tools {
		total += len(tool.Name) + len(tool.Description) + len(tool.Schema) + 1
	}
	return total, nil
}

func TestModelInputGovernorCompactsCompleteOldTurns(t *testing.T) {
	governor := NewDefaultModelInputGovernor(deterministicModelInputCounter{})
	pkg := validEinoModelPackage()
	pkg.RuntimeConstraints.TokenBudget.MaxInputTokens = 20
	pkg.ContextHash = ComputeModelContextHash(pkg)
	req, result, err := governor.Govern(context.Background(), ModelInvokeRequest{
		Package: pkg,
		Messages: []ModelCallMessage{
			{Role: "system", Content: "rules"},
			{Role: "user", Content: "old question"},
			{Role: "assistant", ToolCalls: []ModelToolCall{{ToolCallID: "call_1", Name: "search", Arguments: []byte(`{"q":"old"}`)}}},
			{Role: "tool", ToolCallID: "call_1", ToolName: "search", Content: "old result"},
			{Role: "user", Content: "latest"},
		},
	})
	if err != nil {
		t.Fatalf("govern: %v", err)
	}
	if result.TrimmedMessages != 3 || len(req.Messages) != 2 {
		t.Fatalf("unexpected compaction result=%#v messages=%#v", result, req.Messages)
	}
	if req.Messages[0].Role != "system" || req.Messages[1].Content != "latest" {
		t.Fatalf("protected input changed: %#v", req.Messages)
	}
	if req.Package.Messages.TokenCount > 20 || req.Package.Messages.TrimmedCount != 3 {
		t.Fatalf("package budget metadata not updated: %#v", req.Package.Messages)
	}
	if err := validateModelContextIntegrity(req.Package); err != nil {
		t.Fatalf("derived package integrity: %v", err)
	}
}

func TestModelInputGovernorFailsClosedWhenProtectedInputExceedsBudget(t *testing.T) {
	governor := NewDefaultModelInputGovernor(deterministicModelInputCounter{})
	pkg := validEinoModelPackage()
	pkg.RuntimeConstraints.TokenBudget.MaxInputTokens = 5
	pkg.ContextHash = ComputeModelContextHash(pkg)
	_, result, err := governor.Govern(context.Background(), ModelInvokeRequest{
		Package: pkg,
		Messages: []ModelCallMessage{
			{Role: "system", Content: "required system prompt"},
			{Role: "user", Content: "latest input"},
		},
	})
	if !errors.Is(err, ErrModelInputBudgetExceeded) {
		t.Fatalf("error=%v", err)
	}
	if result.OriginalTokens <= 5 {
		t.Fatalf("original token count not recorded: %#v", result)
	}
}

func TestModelInputGovernorRecordsAttemptedTrimBeforeFailClosed(t *testing.T) {
	governor := NewDefaultModelInputGovernor(deterministicModelInputCounter{})
	pkg := validEinoModelPackage()
	pkg.RuntimeConstraints.TokenBudget.MaxInputTokens = 10
	pkg.ContextHash = ComputeModelContextHash(pkg)
	_, result, err := governor.Govern(context.Background(), ModelInvokeRequest{
		Package: pkg,
		Messages: []ModelCallMessage{
			{Role: "system", Content: "required"},
			{Role: "user", Content: "old"},
			{Role: "assistant", Content: "old response"},
			{Role: "user", Content: "current input remains too large"},
		},
	})
	if !errors.Is(err, ErrModelInputBudgetExceeded) || result.TrimmedMessages == 0 || len(result.CompactionRecords) != 1 {
		t.Fatalf("failed trim provenance missing: result=%#v error=%v", result, err)
	}
}

func TestModelInputGovernorNeverReordersLateSystemInstruction(t *testing.T) {
	strategy := CompleteOldTurnCompressionStrategy{}
	request := ModelInvokeRequest{
		Package: validEinoModelPackage(),
		Messages: []ModelCallMessage{
			{Role: "system", Content: "base"},
			{Role: "user", Content: "old removable"},
			{Role: "assistant", Content: "old response"},
			{Role: "user", Content: "runtime turn"},
			{Role: "system", Content: "late runtime instruction"},
			{Role: "assistant", Content: "runtime response"},
			{Role: "user", Content: "current"},
		},
	}
	result, _, err := strategy.Compress(context.Background(), ModelInputCompressionRequest{
		Request: request, Counter: deterministicModelInputCounter{}, MaxInputTokens: 80,
	})
	if err != nil {
		t.Fatal(err)
	}
	var lateSystemIndex, currentIndex = -1, -1
	for index, message := range result.Messages {
		if message.Content == "late runtime instruction" {
			lateSystemIndex = index
		}
		if message.Content == "current" {
			currentIndex = index
		}
	}
	if lateSystemIndex <= 0 || currentIndex <= lateSystemIndex {
		t.Fatalf("late system instruction was removed or reordered: %#v", result.Messages)
	}
}

func TestModelInputGovernorCountsActualToolSchemas(t *testing.T) {
	governor := NewDefaultModelInputGovernor(deterministicModelInputCounter{})
	pkg := validEinoModelPackage()
	pkg.RuntimeConstraints.TokenBudget.MaxInputTokens = 30
	pkg.ContextHash = ComputeModelContextHash(pkg)
	_, _, err := governor.Govern(context.Background(), ModelInvokeRequest{
		Package:  pkg,
		Messages: []ModelCallMessage{{Role: "user", Content: "latest"}},
		Tools:    []ModelToolDefinition{{Name: "large", Schema: []byte(`{"type":"object","description":"a schema too large for this budget"}`)}},
	})
	if !errors.Is(err, ErrModelInputBudgetExceeded) {
		t.Fatalf("tool schema should be included in final budget, error=%v", err)
	}
}
