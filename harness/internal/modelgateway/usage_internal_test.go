package modelgateway

import (
	"strings"
	"testing"
)

func TestEstimateCostDoesNotInventPriceOrCurrency(t *testing.T) {
	cost := estimateCost(ModelUsage{PromptTokens: 100, CompletionTokens: 20}, ModelCostTable{})
	if cost.Currency != "" || cost.Estimated != 0 {
		t.Fatalf("missing price table must remain unavailable, got %#v", cost)
	}
}

func TestEstimatePromptInputDoesNotCountImageBase64AsText(t *testing.T) {
	dataURL := "data:image/png;base64," + strings.Repeat("A", 400_000)
	messages := []ChatMessage{{
		Role: "user",
		Parts: []ContentPart{
			{Type: "text", Text: "build this card"},
			{Type: "image_url", ImageURL: &ImageURL{URL: dataURL}},
		},
	}}
	chars, tokens := estimatePromptInput(messages)
	if chars >= len(dataURL) {
		t.Fatalf("binary transport leaked into prompt chars: chars=%d data=%d", chars, len(dataURL))
	}
	if tokens < 1024 || tokens > 1100 {
		t.Fatalf("image estimate=%d, want fixed visual cost plus text", tokens)
	}
}

func TestEstimatePromptInputUsesSameVisualCostForURLAndDataURI(t *testing.T) {
	build := func(url string) []ChatMessage {
		return []ChatMessage{{Role: "user", Parts: []ContentPart{{
			Type: "image_url", ImageURL: &ImageURL{URL: url, Detail: "high"},
		}}}}
	}
	_, remoteTokens := estimatePromptInput(build("https://example.com/reference.png"))
	_, inlineTokens := estimatePromptInput(build("data:image/png;base64," + strings.Repeat("A", 400_000)))
	if remoteTokens != inlineTokens || remoteTokens < 2048 {
		t.Fatalf("remote=%d inline=%d, want equal high-detail visual cost", remoteTokens, inlineTokens)
	}
}
