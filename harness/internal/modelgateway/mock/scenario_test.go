package mock_test

import (
	"context"
	"errors"
	"io"
	"testing"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway/mock"
)

func TestScenarioProviderBuildsExplicitSubAgentTaskCall(t *testing.T) {
	provider := mock.NewScenario("scenario")
	stream, err := provider.InvokeChat(context.Background(), mg.AdapterRequest{Messages: []mg.ChatMessage{
		mg.TextMessage("user", "[[harness:subagent:deep_researcher]]"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	chunk, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if chunk.Kind != mg.ChunkToolCall || chunk.ToolCallDelta == nil || chunk.ToolCallDelta.Name != "task" ||
		chunk.ToolCallDelta.ArgumentsDelta != `{"subagent_type":"deep_researcher","description":"return the deterministic child research proof"}` {
		t.Fatalf("unexpected sub-agent task chunk: %#v", chunk)
	}
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("stream terminal error = %v, want EOF", err)
	}
}

func TestScenarioProviderBuildsExplicitDeepAgentChildTaskCall(t *testing.T) {
	provider := mock.NewScenario("scenario")
	stream, err := provider.InvokeChat(context.Background(), mg.AdapterRequest{Messages: []mg.ChatMessage{
		mg.TextMessage("user", "[[harness:subagent:deep_child_agent]]"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	chunk, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if chunk.Kind != mg.ChunkToolCall || chunk.ToolCallDelta == nil || chunk.ToolCallDelta.Name != "task" ||
		chunk.ToolCallDelta.ArgumentsDelta != `{"subagent_type":"deep_child_agent","description":"return the deterministic DeepAgent child proof"}` {
		t.Fatalf("unexpected DeepAgent child task chunk: %#v", chunk)
	}
}
