package input

import (
	"context"
	"os"
	"testing"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

func TestDesignKnowledgeTransformerStopsOfferingReadAfterSuccess(t *testing.T) {
	repository, err := designknowledge.Load(
		context.Background(), os.DirFS("../../.."), "configs/design-public/revisions/demo-v1",
	)
	if err != nil {
		t.Fatal(err)
	}
	transformer, err := NewDesignKnowledgeTransformer(repository)
	if err != nil {
		t.Fatal(err)
	}
	result, err := transformer.BeforeModel(context.Background(), extension.BeforeModelRequest{
		Ctx: extension.Context{AgentID: "agenui_style"}, Round: 4,
		Messages: []extension.BeforeModelMessage{
			{Role: "user", Content: "design"},
			{Role: "tool", ToolName: "agenui_read_design_knowledge", ToolCallID: "call-1", Content: `{"receipt":{}}`},
		},
		Tools: []extension.ToolDefinition{{Name: "agenui_read_design_knowledge"}, {Name: "agenui_workspace"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tools) != 1 || result.Tools[0].Name != "agenui_workspace" {
		t.Fatalf("tools = %#v", result.Tools)
	}
	if result.Messages[0].Content != "design" {
		t.Fatalf("catalog was injected again: %q", result.Messages[0].Content)
	}
}
