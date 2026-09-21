package context

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

var ErrCompactionRequestInvalid = errors.New("context compaction request invalid")
var ErrCompactionRecursion = errors.New("context compaction recursion detected")

type compactionScopeKey struct{}

// CompactionScope marks work performed by a ConversationSummarizer. Model and
// compaction boundaries use it to prevent a summarizer from recursively
// entering the normal Agent model path or another compaction cycle.
type CompactionScope struct {
	Depth   int    `json:"depth"`
	Purpose string `json:"purpose"`
}

func CompactionScopeFrom(ctx context.Context) (CompactionScope, bool) {
	scope, ok := ctx.Value(compactionScopeKey{}).(CompactionScope)
	return scope, ok
}

func withCompactionScope(ctx context.Context, purpose string) context.Context {
	scope, _ := CompactionScopeFrom(ctx)
	scope.Depth++
	scope.Purpose = purpose
	return context.WithValue(ctx, compactionScopeKey{}, scope)
}

// ConversationCompactionRequest is deliberately runtime-neutral. Runtime
// adapters retain their native message objects and use RemovedIndices plus the
// generated Summary to rewrite only the conversation window.
type ConversationCompactionRequest struct {
	Messages            []Message
	TargetTokens        int
	PreserveRecentTurns int
	MaxSummaryTokens    int
}

type ConversationCompactionResult struct {
	RemovedIndices []int
	Summary        *Message
	SummaryRef     string
	BeforeTokens   int
	AfterTokens    int
}

type ConversationSummarizer interface {
	// Implementations must propagate ctx to every dependency. Calling the normal
	// Agent ModelInvoker from this method is forbidden and rejected by Harness.
	Summarize(ctx context.Context, messages []Message, maxTokens int) (string, error)
}

// StructuredExtractiveSummarizer is the production-safe baseline: it is
// deterministic, bounded and cannot recursively invoke the model gateway.
// Deployments may replace it with a semantic summarizer through the same port.
type StructuredExtractiveSummarizer struct{}

func (StructuredExtractiveSummarizer) Summarize(_ context.Context, messages []Message, maxTokens int) (string, error) {
	if maxTokens <= 0 || len(messages) == 0 {
		return "", nil
	}
	prefix := "[UNTRUSTED HISTORY]\nConversation summary:\n"
	var lines []string
	for _, message := range messages {
		content := strings.TrimSpace(message.Content)
		if content == "" && len(message.ToolCalls) > 0 {
			names := make([]string, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				names = append(names, call.Name)
			}
			content = "called tools: " + strings.Join(names, ", ")
		}
		if message.ToolResult != nil {
			toolContent := strings.TrimSpace(message.ToolResult.Content)
			if content == "" {
				content = "tool result: " + toolContent
			} else if toolContent != "" && toolContent != content {
				content = strings.TrimSpace(content + " tool result: " + toolContent)
			}
		}
		if content == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", message.Role, singleLine(content)))
	}
	if len(lines) == 0 {
		return "", nil
	}
	// One rune per token is intentionally conservative for CJK and mixed text.
	// Provider-final tokenization remains the hard boundary.
	return truncateSummaryRunes(prefix+strings.Join(lines, "\n"), maxTokens), nil
}

type RollingConversationCompactor struct {
	Counter    TokenCounter
	Summarizer ConversationSummarizer
}

func NewRollingConversationCompactor(summarizer ConversationSummarizer) *RollingConversationCompactor {
	if summarizer == nil {
		summarizer = StructuredExtractiveSummarizer{}
	}
	return &RollingConversationCompactor{Counter: EstimateCounter{}, Summarizer: summarizer}
}

func (c *RollingConversationCompactor) Compact(ctx context.Context, req ConversationCompactionRequest) (ConversationCompactionResult, error) {
	if scope, ok := CompactionScopeFrom(ctx); ok && scope.Depth > 0 {
		return ConversationCompactionResult{}, ErrCompactionRecursion
	}
	if req.TargetTokens <= 0 || req.PreserveRecentTurns <= 0 || req.MaxSummaryTokens <= 0 {
		return ConversationCompactionResult{}, ErrCompactionRequestInvalid
	}
	counter := c.Counter
	if counter == nil {
		counter = EstimateCounter{}
	}
	summarizer := c.Summarizer
	if summarizer == nil {
		summarizer = StructuredExtractiveSummarizer{}
	}
	result := ConversationCompactionResult{BeforeTokens: countMessages(counter, req.Messages)}
	if result.BeforeTokens <= req.TargetTokens {
		result.AfterTokens = result.BeforeTokens
		return result, nil
	}

	boundary := recentTurnBoundary(req.Messages, req.PreserveRecentTurns)
	removable := removablePrefix(req.Messages, boundary)
	if len(removable) == 0 {
		result.AfterTokens = result.BeforeTokens
		return result, nil
	}
	removed := make([]Message, 0, len(removable))
	removedSet := make(map[int]struct{}, len(removable))
	for _, index := range removable {
		removedSet[index] = struct{}{}
		removed = append(removed, req.Messages[index])
	}
	keptTokens := 0
	for index := range req.Messages {
		if _, drop := removedSet[index]; !drop {
			keptTokens += counter.CountMessage(&req.Messages[index])
		}
	}
	maxSummaryTokens := req.TargetTokens - keptTokens
	if maxSummaryTokens > req.MaxSummaryTokens {
		maxSummaryTokens = req.MaxSummaryTokens
	}
	if maxSummaryTokens > 8 {
		content, err := summarizer.Summarize(withCompactionScope(ctx, "conversation_summary"), removed, maxSummaryTokens)
		if err != nil {
			return ConversationCompactionResult{}, fmt.Errorf("summarize conversation: %w", err)
		}
		if content != "" {
			result.Summary = &Message{Role: RoleAssistant, Content: content}
			result.SummaryRef = summaryRef(content)
			keptTokens += counter.CountMessage(result.Summary)
		}
	}
	result.RemovedIndices = removable
	result.AfterTokens = keptTokens
	return result, nil
}

func recentTurnBoundary(messages []Message, turns int) int {
	seen := 0
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role != RoleUser {
			continue
		}
		seen++
		if seen == turns {
			return index
		}
	}
	if seen == 0 {
		boundary := len(messages) - turns
		if boundary > 0 {
			return boundary
		}
	}
	return 0
}

func removablePrefix(messages []Message, boundary int) []int {
	if boundary <= 0 {
		return nil
	}
	calls := make(map[string]int)
	results := make(map[string]int)
	for index, message := range messages {
		for _, call := range message.ToolCalls {
			calls[call.ID] = index
		}
		if message.ToolResult != nil {
			results[message.ToolResult.CallID] = index
		}
	}
	candidates := make(map[int]struct{}, boundary)
	for index := 0; index < boundary; index++ {
		if messages[index].Role != RoleSystem && !messages[index].CurrentInput {
			candidates[index] = struct{}{}
		}
	}
	for changed := true; changed; {
		changed = false
		for index := range candidates {
			message := messages[index]
			for _, call := range message.ToolCalls {
				resultIndex, ok := results[call.ID]
				if _, paired := candidates[resultIndex]; !ok || !paired {
					delete(candidates, index)
					changed = true
					break
				}
			}
			if _, stillCandidate := candidates[index]; !stillCandidate || message.ToolResult == nil {
				continue
			}
			callIndex, ok := calls[message.ToolResult.CallID]
			if _, paired := candidates[callIndex]; !ok || !paired {
				delete(candidates, index)
				changed = true
			}
		}
	}
	removable := make([]int, 0, len(candidates))
	for index := 0; index < boundary; index++ {
		if _, ok := candidates[index]; ok {
			removable = append(removable, index)
		}
	}
	return removable
}

func countMessages(counter TokenCounter, messages []Message) int {
	total := 0
	for index := range messages {
		total += counter.CountMessage(&messages[index])
	}
	return total
}

func singleLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func truncateSummaryRunes(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}

func summaryRef(content string) string {
	sum := sha256.Sum256([]byte(content))
	// The P0 default summary is carried inline in the governed model request.
	// This identifier is provenance, not a claim that an external object exists.
	return "context-summary-inline://sha256:" + hex.EncodeToString(sum[:])
}
