package modelgateway_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway/mock"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func tokenTexts(events []observability.AgentEvent) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.EventType != observability.EventModelTokenDelta {
			continue
		}
		var p mg.ModelTokenDeltaPayload
		if err := json.Unmarshal(ev.Payload, &p); err == nil {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func TestStreamingFallbackBlockedAfterAnySemanticOutput(t *testing.T) {
	tests := []struct {
		name  string
		chunk mg.NormalizedChunk
	}{
		{name: "thought", chunk: mock.ThoughtChunk("thinking")},
		{name: "tool call", chunk: mock.ToolCallChunk(0, "call-1", "search", `{"q":"x"}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw := &mg.Facade{
				Router: mg.NewStaticRouter(mg.Route{
					Primary:  mg.ModelTarget{Model: "m", Provider: "primary"},
					Fallback: []mg.ModelTarget{{Model: "m", Provider: "backup"}},
				}, nil),
				Providers: map[string]mg.ChatProvider{
					"primary": mock.New("primary", mock.Script{Chunks: []mg.NormalizedChunk{tt.chunk, mock.ErrorChunk("boom", true)}}),
					"backup":  mock.New("backup", mock.Script{Chunks: []mg.NormalizedChunk{mock.TokenChunk("SHOULD-NOT-APPEAR")}}),
				},
			}
			events, resp := collectEvents(t, gw, mg.ModelRequest{RequestID: "r1", Streaming: true})
			if resp.Status != mg.StatusFailed {
				t.Fatalf("post-semantic-output failure must terminate, got %s", resp.Status)
			}
			if strings.Contains(tokenTexts(events), "SHOULD-NOT-APPEAR") {
				t.Fatal("fallback output leaked after semantic output")
			}
			for _, ev := range events {
				if ev.EventType == observability.EventModelFallbackApplied {
					t.Fatal("fallback must not be applied after semantic output")
				}
			}
		})
	}
}

func TestModelCallCancellationUnblocksUndrainedEventStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	gw := &mg.Facade{
		EventBuffer: 1,
		Router:      mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{Model: "m", Provider: "p"}}, nil),
		Providers: map[string]mg.ChatProvider{
			"p": mock.New("p", mock.Script{Chunks: []mg.NormalizedChunk{mock.TokenChunk("one"), mock.TokenChunk("two")}}),
		},
	}
	call, err := gw.Chat(ctx, mg.ModelRequest{RequestID: "r1", Streaming: true})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan struct{})
	go func() {
		_, _ = call.Await()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("model call remained blocked after context cancellation")
	}
}

// P0-4: once the primary provider has emitted a token, a mid-stream failure must
// NOT fall back to another provider (which would append a second provider's
// output onto the already-delivered text). The run terminates as failed and the
// fallback provider's tokens never appear.
func TestStreamingFallbackBlockedAfterFirstToken(t *testing.T) {
	gw := &mg.Facade{
		Router: mg.NewStaticRouter(mg.Route{
			Primary:  mg.ModelTarget{Model: "m", Provider: "primary"},
			Fallback: []mg.ModelTarget{{Model: "m", Provider: "backup"}},
		}, nil),
		Providers: map[string]mg.ChatProvider{
			"primary": mock.New("primary", mock.Script{Chunks: []mg.NormalizedChunk{
				mock.TokenChunk("partial "), mock.ErrorChunk("mid-stream boom", true),
			}}),
			"backup": mock.New("backup", mock.Script{Chunks: []mg.NormalizedChunk{
				mock.TokenChunk("SHOULD-NOT-APPEAR"), mock.UsageChunk(1, 1),
			}}),
		},
	}
	events, resp := collectEvents(t, gw, mg.ModelRequest{RequestID: "r1", Streaming: true})

	if resp.Status != mg.StatusFailed {
		t.Fatalf("post-token mid-stream failure must terminate as failed, got %s", resp.Status)
	}
	text := tokenTexts(events)
	if !strings.Contains(text, "partial ") {
		t.Fatalf("primary tokens should have been delivered, got %q", text)
	}
	if strings.Contains(text, "SHOULD-NOT-APPEAR") {
		t.Fatalf("fallback provider output leaked after first token: %q", text)
	}
	var types []observability.EventType
	for _, ev := range events {
		types = append(types, ev.EventType)
	}
	if has(types, observability.EventModelFallbackApplied) {
		t.Fatalf("must not emit fallback_applied after tokens streamed: %v", types)
	}
}

// Contrast: a failure BEFORE any token still falls back, and the fallback
// provider's output is used. Proves the boundary only blocks post-first-token.
func TestStreamingFallbackAllowedBeforeFirstToken(t *testing.T) {
	gw := &mg.Facade{
		Router: mg.NewStaticRouter(mg.Route{
			Primary:  mg.ModelTarget{Model: "m", Provider: "primary"},
			Fallback: []mg.ModelTarget{{Model: "m", Provider: "backup"}},
		}, nil),
		Providers: map[string]mg.ChatProvider{
			"primary": mock.New("primary", mock.Script{InvokeErr: &mg.AdapterError{Message: "dial fail", Retryable: true}}),
			"backup": mock.New("backup", mock.Script{Chunks: []mg.NormalizedChunk{
				mock.TokenChunk("recovered"), mock.UsageChunk(1, 1),
			}}),
		},
	}
	events, resp := collectEvents(t, gw, mg.ModelRequest{RequestID: "r1", Streaming: true})

	if resp.Status != mg.StatusSuccess || resp.Text != "recovered" {
		t.Fatalf("pre-token failure should fall back to backup: status=%s text=%q", resp.Status, resp.Text)
	}
	var types []observability.EventType
	for _, ev := range events {
		types = append(types, ev.EventType)
	}
	if !has(types, observability.EventModelFallbackApplied) {
		t.Fatalf("expected fallback_applied before first token: %v", types)
	}
}
