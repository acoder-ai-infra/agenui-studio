package anthropic

import (
	"context"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// TestLiveCompanyGatewayPromptCacheAffinity is a protected provider test. It
// deliberately creates a new HTTP transport for every request so a warm read
// cannot be explained by one reused client connection. The stable, opaque
// affinity header is the only routing identity shared by the two calls.
func TestLiveCompanyGatewayPromptCacheAffinity(t *testing.T) {
	if os.Getenv("HARNESS_RUN_LIVE_CACHE_AFFINITY") != "1" {
		t.Skip("set HARNESS_RUN_LIVE_CACHE_AFFINITY=1 and the protected provider env to run")
	}
	baseURL := strings.TrimSpace(os.Getenv("HARNESS_LIVE_ANTHROPIC_BASE_URL"))
	model := strings.TrimSpace(os.Getenv("HARNESS_LIVE_ANTHROPIC_MODEL"))
	apiKey := strings.TrimSpace(os.Getenv("HARNESS_MODEL_GATEWAY_TOKEN"))
	if baseURL == "" || model == "" || apiKey == "" {
		t.Fatal("HARNESS_LIVE_ANTHROPIC_BASE_URL, HARNESS_LIVE_ANTHROPIC_MODEL and HARNESS_MODEL_GATEWAY_TOKEN are required")
	}
	if !mg.PromptCacheSessionAffinityAllowed(mg.ProviderKindSessionAffinity, "anthropic", baseURL) {
		t.Fatalf("HARNESS_LIVE_ANTHROPIC_BASE_URL is not the session-affinity provider")
	}

	// The company Claude cache contract requires a stable prefix above the
	// provider threshold. A per-run nonce guarantees this test starts cold;
	// only the user tail differs between the two requests.
	nonce := strconv.FormatInt(time.Now().UnixNano(), 10)
	system := "cache-affinity-proof/" + nonce + "\n" + strings.Repeat("stable cache anchor. ", 800)
	affinity := mg.DerivePromptCacheAffinityKey(observability.TraceContext{
		TenantID:  "tenant-cache-proof",
		SessionID: "session-cache-proof-" + nonce,
	})
	if affinity == "" {
		t.Fatal("derived affinity key is empty")
	}

	cold := liveCacheCall(t, baseURL, model, apiKey, affinity, system, "Return only COLD_OK.")
	if cold.CacheWriteTokens <= 0 {
		t.Fatalf("cold request did not write a prompt cache entry: %+v", cold)
	}
	warm := liveCacheCall(t, baseURL, model, apiKey, affinity, system, "Return only WARM_OK.")
	if warm.CacheReadTokens <= 0 {
		t.Fatalf("warm request did not read the prompt cache entry: cold=%+v warm=%+v", cold, warm)
	}
	t.Logf(
		"MODEL-CACHE-AFFINITY-001 model=%s cold_write=%d cold_read=%d warm_write=%d warm_read=%d",
		model,
		cold.CacheWriteTokens,
		cold.CacheReadTokens,
		warm.CacheWriteTokens,
		warm.CacheReadTokens,
	)
}

func liveCacheCall(
	t *testing.T,
	baseURL string,
	model string,
	apiKey string,
	affinity string,
	system string,
	user string,
) mg.ModelUsage {
	t.Helper()
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	adapter := New("company-anthropic", baseURL, apiKey, WithPromptCacheSessionAffinity())
	adapter.client = &http.Client{Transport: transport}
	maxTokens := 32
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	stream, err := adapter.InvokeChat(ctx, mg.AdapterRequest{
		Model: model,
		Messages: []mg.ChatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Streaming:              false,
		PromptCacheAffinityKey: affinity,
		Options: mg.ModelOptions{
			MaxTokens:     &maxTokens,
			ReasoningMode: mg.ReasoningDisabled,
		},
	})
	if err != nil {
		t.Fatalf("invoke protected Anthropic cache request: %v", err)
	}
	defer stream.Close()
	var usage *mg.ModelUsage
	for {
		chunk, nextErr := stream.Next(ctx)
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatalf("read protected Anthropic cache response: %v", nextErr)
		}
		if chunk.Usage != nil {
			copy := *chunk.Usage
			usage = &copy
		}
	}
	if usage == nil || usage.Source != mg.UsageSourceGateway {
		t.Fatalf("protected Anthropic cache response lacks authoritative usage: %#v", usage)
	}
	return *usage
}
