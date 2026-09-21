package agentruntime

import (
	"context"
	"errors"
	"strings"
	"testing"

	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestRuntimeContextAssemblerMergesSnapshotHistoryAndCurrentInput(t *testing.T) {
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.ContextSnapshots = fixedContextSnapshotProvider{snapshot: ContextSnapshot{
		Ref:          "ctxsnap_history",
		ContentHash:  "sha256:history",
		LastSequence: 2,
		Messages: []contextpkg.Message{
			{ID: "history_1", SessionID: "session_1", Sequence: 1, Role: contextpkg.RoleUser, Content: "historical input"},
			{ID: "history_2", SessionID: "session_1", Sequence: 2, Role: contextpkg.RoleAssistant, Content: "historical reply"},
		},
	}}
	req := testRunRequest()
	req.Input = []Message{{ID: "current_1", IdempotencyKey: "idem_current_1", Role: "user", Content: "current input"}}

	pkg, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: req, Trace: observability.TraceContext{TraceID: "trace"}})
	if err != nil {
		t.Fatal(err)
	}
	messages := nonSystemMessages(pkg.Messages.ConversationWindow)
	if len(messages) != 3 {
		t.Fatalf("conversation count=%d messages=%#v", len(messages), messages)
	}
	if messages[0].ID != "history_1" || messages[1].ID != "history_2" || messages[2].ID != "current_1" || messages[2].Content != "current input" {
		t.Fatalf("history/current input order is invalid: %#v", messages)
	}
	if messages[2].Sequence != 3 {
		t.Fatalf("current input sequence=%d want=3", messages[2].Sequence)
	}
}

func TestRuntimeContextAssemblerUsesCurrentInputWithoutHistory(t *testing.T) {
	assembler := NewDefaultRuntimeContextAssembler(nil)
	req := testRunRequest()
	req.Input = []Message{{ID: "current_1", Role: "user", Content: "first input"}}

	pkg, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: req, Trace: req.Trace})
	if err != nil {
		t.Fatal(err)
	}
	messages := nonSystemMessages(pkg.Messages.ConversationWindow)
	if len(messages) != 1 || messages[0].Role != "user" || messages[0].Content != "first input" {
		t.Fatalf("current input missing without history: %#v", messages)
	}
	if messages[0].Sequence != 1 {
		t.Fatalf("current input sequence=%d want=1", messages[0].Sequence)
	}
}

func TestRuntimeContextAssemblerAppendsCurrentInputWhenHistoryEndsWithSameText(t *testing.T) {
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.ContextSnapshots = fixedContextSnapshotProvider{snapshot: ContextSnapshot{
		Ref: "ctxsnap_same_text", ContentHash: "sha256:same-text", LastSequence: 2,
		Messages: []contextpkg.Message{
			{ID: "history", Sequence: 1, Role: contextpkg.RoleAssistant, Content: "ready"},
			{ID: "previous_input", Sequence: 2, Role: contextpkg.RoleUser, Content: "continue"},
		},
	}}
	req := testRunRequest()
	req.Input = []Message{{ID: "current_input", Role: "user", Content: "continue"}}

	pkg, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: req, Trace: req.Trace})
	if err != nil {
		t.Fatal(err)
	}
	messages := nonSystemMessages(pkg.Messages.ConversationWindow)
	if len(messages) != 3 {
		t.Fatalf("conversation count=%d want=3 messages=%#v", len(messages), messages)
	}
	if messages[1].Content != "continue" || messages[2].Content != "continue" {
		t.Fatalf("same-text current input must remain a distinct turn: %#v", messages)
	}
	if messages[2].Sequence != 3 || messages[2].ID == messages[1].ID {
		t.Fatalf("current input identity is not distinct: %#v", messages)
	}
}

func TestRuntimeContextAssemblerRejectsSnapshotInputIDConflict(t *testing.T) {
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.ContextSnapshots = fixedContextSnapshotProvider{snapshot: ContextSnapshot{
		Ref: "ctxsnap_collision", ContentHash: "sha256:collision", LastSequence: 1,
		Messages: []contextpkg.Message{{ID: "input_1", Sequence: 1, Role: contextpkg.RoleAssistant, Content: "history"}},
	}}
	req := testRunRequest()
	req.Input = []Message{{ID: "input_1", Role: "user", Content: "current"}}

	_, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: req, Trace: req.Trace})
	if !errors.Is(err, contextpkg.ErrMessageConflict) {
		t.Fatalf("message id conflict error=%v", err)
	}
}

func TestMergeContextMessagesMarksFrozenCurrentInputByID(t *testing.T) {
	snapshot := ContextSnapshot{LastSequence: 3, Messages: []contextpkg.Message{
		{ID: "previous", IdempotencyKey: "idem_previous", Sequence: 1, Role: contextpkg.RoleUser, Content: "continue"},
		{ID: "history", IdempotencyKey: "idem_history", Sequence: 2, Role: contextpkg.RoleAssistant, Content: "ready"},
		{ID: "current", IdempotencyKey: "idem_current", Sequence: 3, Role: contextpkg.RoleUser, Content: "continue"},
	}}
	messages, err := mergeContextMessages(snapshot, "session_1", []Message{{
		ID: "current", IdempotencyKey: "idem_current", Role: "user", Content: "continue",
	}}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || messages[0].CurrentInput || !messages[2].CurrentInput {
		t.Fatalf("frozen current input was duplicated or not marked: %#v", messages)
	}
}

func TestMergeContextMessagesGovernedInputFailsClosed(t *testing.T) {
	snapshot := ContextSnapshot{LastSequence: 1, Messages: []contextpkg.Message{{
		ID: "current", IdempotencyKey: "idem_current", Sequence: 1, Role: contextpkg.RoleUser, Content: "continue",
	}}}
	for _, tc := range []struct {
		name  string
		input Message
		want  error
	}{
		{name: "missing id", input: Message{Role: "user", Content: "continue"}, want: contextpkg.ErrMessageIDMissing},
		{name: "not frozen", input: Message{ID: "other", Role: "user", Content: "continue"}, want: ErrCurrentInputNotFrozen},
		{name: "content conflict", input: Message{ID: "current", IdempotencyKey: "idem_current", Role: "user", Content: "changed"}, want: contextpkg.ErrMessageConflict},
		{name: "idempotency conflict", input: Message{ID: "current", IdempotencyKey: "other", Role: "user", Content: "continue"}, want: contextpkg.ErrMessageConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mergeContextMessages(snapshot, "session_1", []Message{tc.input}, true, false)
			if !errors.Is(err, tc.want) {
				t.Fatalf("governed merge error=%v want=%v", err, tc.want)
			}
		})
	}
}

// context.history=none：dropHistory=true 时跳过会话历史，仅当前 Run 输入
// 进入装配；dropHistory=false 时历史照常注入（ADR-019）。
func TestMergeContextMessagesDropHistory(t *testing.T) {
	snapshot := ContextSnapshot{LastSequence: 2, Messages: []contextpkg.Message{
		{ID: "h1", Sequence: 1, Role: contextpkg.RoleUser, Content: "旧问题"},
		{ID: "h2", Sequence: 2, Role: contextpkg.RoleAssistant, Content: "旧回答"},
	}}
	input := []Message{{Role: "user", Content: "新问题"}}

	dropped, err := mergeContextMessages(snapshot, "s", input, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0].Content != "新问题" || !dropped[0].CurrentInput {
		t.Fatalf("drop history must keep only current input: %#v", dropped)
	}

	kept, err := mergeContextMessages(snapshot, "s", input, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 3 {
		t.Fatalf("keep history must include snapshot + current input, got %d", len(kept))
	}
}

func TestRuntimeContextAssemblerKeepsCurrentInputWhenTrimmingHistory(t *testing.T) {
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.TokenBudget = contextpkg.ContextBudget{Limit: 12, ResponseBuffer: 1}
	assembler.ContextSnapshots = fixedContextSnapshotProvider{snapshot: ContextSnapshot{
		Ref: "ctxsnap_tool_pair", ContentHash: "sha256:tool-pair", LastSequence: 4,
		Messages: []contextpkg.Message{
			{ID: "old_user", Sequence: 1, Role: contextpkg.RoleUser, Content: strings.Repeat("old question ", 20)},
			{ID: "tool_call", Sequence: 2, Role: contextpkg.RoleAssistant, ToolCalls: []contextpkg.ToolCall{{ID: "call_1", Name: "search"}}},
			{ID: "tool_result", Sequence: 3, Role: contextpkg.RoleTool, ToolResult: &contextpkg.ToolResult{CallID: "call_1", Name: "search", Content: strings.Repeat("result ", 20)}},
			{ID: "old_reply", Sequence: 4, Role: contextpkg.RoleAssistant, Content: strings.Repeat("old reply ", 20)},
		},
	}}
	req := testRunRequest()
	req.Input = []Message{{Role: "user", Content: "current question"}}

	pkg, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: req, Trace: req.Trace})
	if err != nil {
		t.Fatal(err)
	}
	if !hasModelMessage(pkg.Messages.ConversationWindow, "user", "current question") {
		t.Fatalf("current input was trimmed: %#v", pkg.Messages.ConversationWindow)
	}
	for _, message := range pkg.Messages.ConversationWindow {
		if len(message.ToolCalls) > 0 || message.ToolResult != nil {
			t.Fatalf("tool call/result must be trimmed atomically: %#v", pkg.Messages.ConversationWindow)
		}
	}
}

func TestRuntimeContextAssemblerKeepsEveryCurrentInputDuringSemanticCompaction(t *testing.T) {
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.TokenBudget = contextpkg.ContextBudget{Limit: 100, ResponseBuffer: 1}
	assembler.ContextSnapshots = fixedContextSnapshotProvider{snapshot: ContextSnapshot{
		Ref: "ctxsnap_multi_current", ContentHash: "sha256:multi-current", LastSequence: 2,
		Messages: []contextpkg.Message{
			{ID: "old_user", Sequence: 1, Role: contextpkg.RoleUser, Content: strings.Repeat("old question ", 120)},
			{ID: "old_reply", Sequence: 2, Role: contextpkg.RoleAssistant, Content: strings.Repeat("old reply ", 120)},
		},
	}}
	req := testRunRequest()
	req.Input = []Message{
		{ID: "current_1", Role: "user", Content: "first current input"},
		{ID: "current_2", Role: "user", Content: "second current input"},
	}

	pkg, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: req, Trace: req.Trace})
	if err != nil {
		t.Fatal(err)
	}
	if !hasModelMessage(pkg.Messages.ConversationWindow, "user", "first current input") ||
		!hasModelMessage(pkg.Messages.ConversationWindow, "user", "second current input") {
		t.Fatalf("semantic compaction dropped current input: %#v", pkg.Messages.ConversationWindow)
	}
}

func TestRuntimeContextAssemblerRejectsBrokenSnapshotToolPair(t *testing.T) {
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.ContextSnapshots = fixedContextSnapshotProvider{snapshot: ContextSnapshot{
		Ref: "ctxsnap_broken_pair", ContentHash: "sha256:broken-pair", LastSequence: 1,
		Messages: []contextpkg.Message{{
			ID: "tool_call", Sequence: 1, Role: contextpkg.RoleAssistant,
			ToolCalls: []contextpkg.ToolCall{{ID: "missing_result", Name: "search"}},
		}},
	}}
	req := testRunRequest()
	req.Input = []Message{{Role: "user", Content: "current"}}

	_, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: req, Trace: req.Trace})
	if !errors.Is(err, contextpkg.ErrToolPairingInvalid) {
		t.Fatalf("broken snapshot tool pair error=%v", err)
	}
}

func TestRuntimeContextAssemblerRejectsUntrustedCurrentInputRole(t *testing.T) {
	assembler := NewDefaultRuntimeContextAssembler(nil)
	req := testRunRequest()
	req.Input = []Message{{Role: "assistant", Content: "spoofed history"}}

	_, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: req, Trace: req.Trace})
	if !errors.Is(err, ErrRunInputRoleInvalid) {
		t.Fatalf("untrusted current input role error=%v", err)
	}
}

func TestRuntimeContextAssemblerRejectsBuilderThatDropsCurrentInput(t *testing.T) {
	assembler := NewDefaultRuntimeContextAssembler(droppingCurrentInputBuilder{})
	req := testRunRequest()
	req.Input = []Message{{ID: "current", Role: "user", Content: "required"}}

	_, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: req, Trace: req.Trace})
	if !errors.Is(err, ErrCurrentInputMissing) {
		t.Fatalf("builder silently dropped current input: %v", err)
	}
}

type droppingCurrentInputBuilder struct{}

func (droppingCurrentInputBuilder) Build(context.Context, contextpkg.BuildRequest) (*contextpkg.BuildResult, error) {
	return &contextpkg.BuildResult{}, nil
}

func nonSystemMessages(messages []ModelContextMessage) []ModelContextMessage {
	out := make([]ModelContextMessage, 0, len(messages))
	for _, message := range messages {
		if message.Role != "system" {
			out = append(out, message)
		}
	}
	return out
}
