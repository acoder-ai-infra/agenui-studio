package modelgateway_test

import (
	"testing"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway/mock"
)

// Agent-declared fallback models (ModelFallbackHints) must degrade to the next
// declared model on a retryable primary failure, replace the tenant fallback
// order, skip models the tenant does not serve, and never rescue a fail-closed
// primary.
func TestFacadeModelFallbackHints(t *testing.T) {
	// Route configures three (provider, model) targets. The agent declarations
	// select an ordered subset of these.
	newGW := func() (*mg.Facade, map[string]*mock.MockProvider) {
		providers := map[string]*mock.MockProvider{
			"pa": mock.New("pa", mock.Script{Chunks: []mg.NormalizedChunk{mock.ErrorChunk("retry", true)}}),
			"pb": mock.New("pb", mock.Script{Chunks: []mg.NormalizedChunk{mock.TokenChunk("b"), mock.UsageChunk(1, 1)}}),
			"pc": mock.New("pc", mock.Script{Chunks: []mg.NormalizedChunk{mock.TokenChunk("c"), mock.UsageChunk(1, 1)}}),
		}
		gw := &mg.Facade{
			Router: mg.NewStaticRouter(mg.Route{
				Primary: mg.ModelTarget{Model: "model-a", Provider: "pa"},
				Fallback: []mg.ModelTarget{
					{Model: "model-b", Provider: "pb"},
					{Model: "model-c", Provider: "pc"},
				},
			}, nil),
			Providers: map[string]mg.ChatProvider{"pa": providers["pa"], "pb": providers["pb"], "pc": providers["pc"]},
		}
		return gw, providers
	}

	t.Run("degrades to declared fallback in order, skipping tenant fallbacks", func(t *testing.T) {
		gw, providers := newGW()
		// Primary model-a fails (retryable); agent declares fallback = [model-c].
		// It must jump straight to model-c, never touching the tenant's model-b.
		_, resp := collect(t, gw, mg.ModelRequest{
			RequestID: "r1", Streaming: true,
			ModelHint:          "model-a",
			ModelFallbackHints: []string{"model-c"},
		})
		if resp.Status != mg.StatusSuccess || resp.Model != "model-c" {
			t.Fatalf("want success on model-c, got status=%s model=%s", resp.Status, resp.Model)
		}
		if providers["pb"].Calls() != 0 {
			t.Fatalf("tenant fallback model-b must not be attempted, calls=%d", providers["pb"].Calls())
		}
		if providers["pc"].Calls() != 1 {
			t.Fatalf("declared fallback model-c must be attempted once, calls=%d", providers["pc"].Calls())
		}
	})

	t.Run("skips a fallback the tenant does not serve", func(t *testing.T) {
		gw, _ := newGW()
		// fallback = [model-x (unserved), model-b]. model-x is skipped, model-b runs.
		_, resp := collect(t, gw, mg.ModelRequest{
			RequestID: "r2", Streaming: true,
			ModelHint:          "model-a",
			ModelFallbackHints: []string{"model-x", "model-b"},
		})
		if resp.Status != mg.StatusSuccess || resp.Model != "model-b" {
			t.Fatalf("want success on model-b after skipping model-x, got status=%s model=%s", resp.Status, resp.Model)
		}
	})

	t.Run("a missing fallback does not rescue a failing primary", func(t *testing.T) {
		gw, _ := newGW()
		// Primary fails, and the only declared fallback is unserved → overall failure.
		_, resp := collect(t, gw, mg.ModelRequest{
			RequestID: "r3", Streaming: true,
			ModelHint:          "model-a",
			ModelFallbackHints: []string{"model-x"},
		})
		if resp.Status != mg.StatusFailed {
			t.Fatalf("want failure when primary fails and fallback is unserved, got %+v", resp)
		}
	})
}
