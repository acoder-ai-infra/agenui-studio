package context

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

type recursiveCompactionSummarizer struct {
	compactor *RollingConversationCompactor
}

func (s *recursiveCompactionSummarizer) Summarize(ctx context.Context, messages []Message, maxTokens int) (string, error) {
	_, err := s.compactor.Compact(ctx, ConversationCompactionRequest{
		Messages: messages, TargetTokens: maxTokens, PreserveRecentTurns: 1, MaxSummaryTokens: maxTokens,
	})
	return "", err
}

func TestRollingConversationCompactorPreservesSystemCurrentTurnsAndToolPairs(t *testing.T) {
	messages := []Message{
		{Role: RoleSystem, Content: "system policy"},
		{Role: RoleUser, Content: strings.Repeat("old question ", 40)},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_1", Name: "search"}}},
		{Role: RoleTool, Content: strings.Repeat("old result ", 40), ToolResult: &ToolResult{CallID: "call_1", Name: "search", Content: strings.Repeat("old result ", 40)}},
		{Role: RoleAssistant, Content: strings.Repeat("old answer ", 40)},
		{Role: RoleUser, Content: "recent question"},
		{Role: RoleAssistant, Content: "recent answer"},
		{Role: RoleUser, Content: "current question"},
	}
	result, err := NewRollingConversationCompactor(nil).Compact(context.Background(), ConversationCompactionRequest{
		Messages: messages, TargetTokens: 100, PreserveRecentTurns: 2, MaxSummaryTokens: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary == nil || result.SummaryRef == "" || result.AfterTokens >= result.BeforeTokens {
		t.Fatalf("conversation was not compacted: %#v", result)
	}
	if len(result.RemovedIndices) != 4 {
		t.Fatalf("old complete turn must be removed atomically: %#v", result.RemovedIndices)
	}
	if !strings.Contains(result.Summary.Content, "UNTRUSTED HISTORY") {
		t.Fatalf("summary trust boundary missing: %q", result.Summary.Content)
	}
}

func TestRollingConversationCompactorNeverRemovesMarkedCurrentInput(t *testing.T) {
	messages := []Message{
		{ID: "old_user", Role: RoleUser, Content: strings.Repeat("old question ", 40)},
		{ID: "old_reply", Role: RoleAssistant, Content: strings.Repeat("old reply ", 40)},
		{ID: "current_1", Role: RoleUser, Content: "first", CurrentInput: true},
		{ID: "current_2", Role: RoleUser, Content: "second", CurrentInput: true},
	}
	result, err := NewRollingConversationCompactor(nil).Compact(context.Background(), ConversationCompactionRequest{
		Messages: messages, TargetTokens: 20, PreserveRecentTurns: 1, MaxSummaryTokens: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range result.RemovedIndices {
		if messages[index].CurrentInput {
			t.Fatalf("current input was removed: index=%d result=%#v", index, result)
		}
	}
}

func TestRollingConversationCompactorDoesNotSplitCrossBoundaryToolPair(t *testing.T) {
	messages := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_1", Name: "search"}}},
		{Role: RoleUser, Content: "current"},
		{Role: RoleTool, ToolResult: &ToolResult{CallID: "call_1", Name: "search", Content: "result"}},
	}
	result, err := NewRollingConversationCompactor(nil).Compact(context.Background(), ConversationCompactionRequest{
		Messages: messages, TargetTokens: 1, PreserveRecentTurns: 1, MaxSummaryTokens: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RemovedIndices) != 0 {
		t.Fatalf("cross-boundary pair was split: %#v", result.RemovedIndices)
	}
}

func TestRollingConversationCompactorKeepsAllResultsWhenMultiCallMessageCrossesBoundary(t *testing.T) {
	messages := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_1", Name: "a"}, {ID: "call_2", Name: "b"}}},
		{Role: RoleTool, ToolResult: &ToolResult{CallID: "call_1", Content: "first"}},
		{Role: RoleUser, Content: "current"},
		{Role: RoleTool, ToolResult: &ToolResult{CallID: "call_2", Content: "second"}},
	}
	result, err := NewRollingConversationCompactor(nil).Compact(context.Background(), ConversationCompactionRequest{
		Messages: messages, TargetTokens: 1, PreserveRecentTurns: 1, MaxSummaryTokens: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RemovedIndices) != 0 {
		t.Fatalf("multi-call atomic group was split: %#v", result.RemovedIndices)
	}
}

func TestRollingConversationCompactorSupportsRuntimeInternalHistoryWithoutUserRole(t *testing.T) {
	messages := []Message{
		{Role: RoleSystem, Content: "planner policy"},
		{Role: RoleAssistant, Content: strings.Repeat("old plan ", 100)},
		{Role: RoleAssistant, Content: strings.Repeat("old critique ", 100)},
		{Role: RoleAssistant, Content: "current planner state"},
	}
	result, err := NewRollingConversationCompactor(nil).Compact(context.Background(), ConversationCompactionRequest{
		Messages: messages, TargetTokens: 50, PreserveRecentTurns: 1, MaxSummaryTokens: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RemovedIndices) != 2 || result.RemovedIndices[0] != 1 || result.RemovedIndices[1] != 2 {
		t.Fatalf("runtime-internal history was not compacted: %#v", result.RemovedIndices)
	}
}

func TestRollingConversationCompactorIsConcurrentAndDeterministic(t *testing.T) {
	compactor := NewRollingConversationCompactor(nil)
	request := ConversationCompactionRequest{
		Messages: []Message{
			{Role: RoleUser, Content: strings.Repeat("old ", 100)},
			{Role: RoleAssistant, Content: strings.Repeat("answer ", 100)},
			{Role: RoleUser, Content: "recent"},
			{Role: RoleAssistant, Content: "answer"},
			{Role: RoleUser, Content: "current"},
		},
		TargetTokens: 40, PreserveRecentTurns: 2, MaxSummaryTokens: 32,
	}
	const workers = 32
	var wg sync.WaitGroup
	refs := make(chan string, workers)
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := compactor.Compact(context.Background(), request)
			if err != nil {
				errs <- err
				return
			}
			refs <- result.SummaryRef
		}()
	}
	wg.Wait()
	close(refs)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var expected string
	for ref := range refs {
		if expected == "" {
			expected = ref
		}
		if ref == "" || ref != expected {
			t.Fatalf("non-deterministic summary refs: expected=%q got=%q", expected, ref)
		}
	}
}

func TestRollingConversationCompactorRejectsRecursiveCompaction(t *testing.T) {
	summarizer := &recursiveCompactionSummarizer{}
	compactor := NewRollingConversationCompactor(summarizer)
	summarizer.compactor = compactor
	_, err := compactor.Compact(context.Background(), ConversationCompactionRequest{
		Messages: []Message{
			{Role: RoleUser, Content: strings.Repeat("old ", 100)},
			{Role: RoleAssistant, Content: strings.Repeat("answer ", 100)},
			{Role: RoleUser, Content: "current"},
		},
		TargetTokens: 40, PreserveRecentTurns: 1, MaxSummaryTokens: 32,
	})
	if !errors.Is(err, ErrCompactionRecursion) {
		t.Fatalf("recursive compaction error=%v", err)
	}
}
