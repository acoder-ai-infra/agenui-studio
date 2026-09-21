package modelgateway_test

import (
	"testing"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway/mock"
)

func TestFacadePreservesOpaqueReasoningSignature(t *testing.T) {
	provider := mock.New("mock", mock.Script{Chunks: []mg.NormalizedChunk{
		{Kind: mg.ChunkThought, ThoughtDelta: "plan"},
		{Kind: mg.ChunkThought, ReasoningSignature: "sig-opaque"},
		mock.ToolCallChunk(0, "tool_1", "harness.echo", `{"text":"hello"}`),
		mock.UsageChunk(10, 4),
	}})
	gateway := &mg.Facade{
		Router:    mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{Model: "m", Provider: "mock"}}, nil),
		Providers: map[string]mg.ChatProvider{"mock": provider},
	}

	_, response := collect(t, gateway, mg.ModelRequest{RequestID: "signature", Streaming: true})
	if response.Status != mg.StatusSuccess || response.ReasoningSignature != "sig-opaque" || len(response.ToolCalls) != 1 {
		t.Fatalf("reasoning signature was not preserved: %#v", response)
	}
}
