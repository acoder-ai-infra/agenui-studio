package modelgateway_test

import (
	"testing"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway/mock"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// A declared model (ModelHint) must be honored exactly: a served hint routes to
// that model; an unserved hint fails closed with MODEL_UNAVAILABLE and never
// silently downgrades to the tenant default.
func TestFacadeModelHintFailsClosed(t *testing.T) {
	newGW := func() (*mg.Facade, *mock.MockProvider) {
		provider := mock.New("mock", mock.Script{
			Chunks: []mg.NormalizedChunk{mock.TokenChunk("hi"), mock.UsageChunk(1, 1)},
		})
		return &mg.Facade{
			Router: mg.NewStaticRouter(mg.Route{
				Primary:  mg.ModelTarget{Model: "model-a", Provider: "mock"},
				Fallback: []mg.ModelTarget{{Model: "model-b", Provider: "mock"}},
			}, nil),
			Providers: map[string]mg.ChatProvider{"mock": provider},
		}, provider
	}

	// No hint → tenant default (primary model-a).
	gw, _ := newGW()
	_, def := collect(t, gw, mg.ModelRequest{RequestID: "r0", Streaming: true})
	if def.Status != mg.StatusSuccess || def.Model != "model-a" {
		t.Fatalf("no-hint should use primary model-a: status=%s model=%s", def.Status, def.Model)
	}

	// Served hint (fallback model-b) → honored, not the default.
	gw, _ = newGW()
	_, ok := collect(t, gw, mg.ModelRequest{RequestID: "r1", Streaming: true, ModelHint: "model-b"})
	if ok.Status != mg.StatusSuccess {
		t.Fatalf("served hint should succeed: %+v", ok)
	}
	if ok.Model != "model-b" {
		t.Fatalf("served hint must route to model-b, got %q", ok.Model)
	}

	// Unserved hint → fail closed with MODEL_UNAVAILABLE, no silent downgrade.
	gw, provider := newGW()
	events, bad := collectEvents(t, gw, mg.ModelRequest{RequestID: "r2", Streaming: true, ModelHint: "model-z"})
	if bad.Status != mg.StatusFailed {
		t.Fatalf("unserved hint must fail, got %+v", bad)
	}
	if bad.Error == nil || bad.Error.Code != "MODEL_UNAVAILABLE" {
		t.Fatalf("want MODEL_UNAVAILABLE, got %+v", bad.Error)
	}
	if provider.Calls() != 0 {
		t.Fatalf("unserved hint must not reach provider, calls=%d", provider.Calls())
	}
	var sawFailure bool
	for _, ev := range events {
		if ev.EventType == observability.EventModelCallStarted {
			t.Fatalf("unserved hint must fail before model_call_started: %+v", events)
		}
		if ev.EventType == observability.EventModelCallFailed {
			sawFailure = true
			if ev.Error == nil || ev.Error.Code != "MODEL_UNAVAILABLE" {
				t.Fatalf("failure event code = %+v, want MODEL_UNAVAILABLE", ev.Error)
			}
		}
	}
	if !sawFailure {
		t.Fatalf("missing model_call_failed for unserved hint: %+v", events)
	}
}
