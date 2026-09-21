package harness

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway/mock"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestModelCallCompletedProjectsUsageToPublicEvent(t *testing.T) {
	want := modelgateway.ModelUsage{
		PromptTokens:     12,
		CompletionTokens: 3,
		ReasoningTokens:  2,
		Source:           modelgateway.UsageSourceGateway,
	}
	provider := mock.New("mock", mock.Script{Chunks: []modelgateway.NormalizedChunk{
		mock.TokenChunk("ok"),
		{Kind: modelgateway.ChunkUsage, Usage: &want},
	}})
	gateway := &modelgateway.Facade{
		Router: modelgateway.NewStaticRouter(modelgateway.Route{
			Primary: modelgateway.ModelTarget{Model: "mock-model", Provider: "mock"},
		}, nil),
		Providers: map[string]modelgateway.ChatProvider{"mock": provider},
	}

	call, err := gateway.Chat(context.Background(), modelgateway.ModelRequest{RequestID: "sdk-usage"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var got modelgateway.ModelUsage
	var found bool
	for event := range call.Events {
		if event.EventType != observability.EventModelCallCompleted {
			continue
		}
		found = true
		publicEvent := convertEvent(event)
		if err := json.Unmarshal(publicEvent.Usage, &got); err != nil {
			t.Fatalf("decode public Event.Usage: %v (%s)", err, publicEvent.Usage)
		}
	}
	if _, err := call.Await(); err != nil {
		t.Fatalf("Await: %v", err)
	}
	if !found {
		t.Fatal("model_call_completed event not found")
	}
	if got != want {
		t.Fatalf("public Event.Usage = %+v, want %+v", got, want)
	}
}
