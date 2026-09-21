package agentruntime

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestContextBudgetReportProtectsUnknownRolesAndBoundsUnicodePreviews(t *testing.T) {
	longInput := strings.Repeat("界", contextBudgetPreviewRunes+25)
	report := buildContextBudgetReport(ModelInvokeRequest{
		Package: ModelContextPackage{RuntimeConstraints: ModelContextRuntimeConstraints{
			TokenBudget: ModelTokenBudget{MaxInputTokens: 1000},
		}},
		Messages: []ModelCallMessage{
			{Role: "future_internal_role", Content: "must remain private"},
			{Role: "user", Content: longInput},
		},
	}, ModelInputGovernance{OriginalTokens: 200, FinalTokens: 200})

	segments := make(map[ContextBudgetCategory]ContextBudgetSegment, len(report.Segments))
	for _, segment := range report.Segments {
		segments[segment.Category] = segment
	}
	unknown := segments[ContextBudgetOther]
	if len(unknown.Items) != 1 || !unknown.Items[0].Protected || unknown.Items[0].ContentPreview != "" {
		t.Fatalf("unknown role was not protected: %#v", unknown)
	}
	current := segments[ContextBudgetCurrentInput]
	if len(current.Items) != 1 || !current.Items[0].Truncated {
		t.Fatalf("long preview was not marked truncated: %#v", current)
	}
	if got := utf8.RuneCountInString(current.Items[0].ContentPreview); got != contextBudgetPreviewRunes+3 {
		t.Fatalf("preview runes=%d want=%d", got, contextBudgetPreviewRunes+3)
	}
}

func TestContextBudgetReportHandlesEmptyInput(t *testing.T) {
	report := buildContextBudgetReport(ModelInvokeRequest{}, ModelInputGovernance{})
	if report.UsedTokens != 0 || report.UsageRatio != 0 || report.Pressure != ContextBudgetPressureNormal || len(report.Segments) != 0 {
		t.Fatalf("empty report=%#v", report)
	}
}

func TestContextBudgetReportRedactsSecretsAndOmitsToolArguments(t *testing.T) {
	items := contextBudgetItems([]ModelCallMessage{
		{Role: "assistant", ToolCalls: []ModelToolCall{{Name: "lookup", Arguments: []byte(`{"password":"tool-secret"}`)}}},
		{Role: "tool", ToolName: "lookup", Content: `{"nested":{"api_token":"result-secret"},"safe":"visible"}`},
		{Role: "user", Content: "token=current-secret safe=value"},
	}, nil)
	for _, item := range items {
		preview := item.item.ContentPreview
		if strings.Contains(preview, "tool-secret") || strings.Contains(preview, "result-secret") || strings.Contains(preview, "current-secret") {
			t.Fatalf("secret leaked in context preview: %q", preview)
		}
	}
	if items[0].item.ContentPreview != "lookup(...)" {
		t.Fatalf("tool arguments were retained: %q", items[0].item.ContentPreview)
	}
	if !strings.Contains(items[1].item.ContentPreview, "[REDACTED]") || !strings.Contains(items[1].item.ContentPreview, "visible") {
		t.Fatalf("JSON tool result was not selectively redacted: %q", items[1].item.ContentPreview)
	}
	if !strings.Contains(items[2].item.ContentPreview, "token=***") {
		t.Fatalf("plain-text secret was not redacted: %q", items[2].item.ContentPreview)
	}
}
