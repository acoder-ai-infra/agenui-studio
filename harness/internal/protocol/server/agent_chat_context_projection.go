package server

import (
	"encoding/json"
	"strings"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

const agentChatContextSchemaVersion = "harness.agent_chat_context.v1"

const (
	agentChatContextReportSchemaVersion = "harness.context_budget_report.v1"
	agentChatContextMaxSegments         = 32
	agentChatContextMaxItems            = 80
	agentChatContextPreviewRunes        = 800
)

type agentChatContextEventPayload struct {
	Report agentChatContextBudgetReport `json:"context_budget_report"`
}

type agentChatContextBudgetReport struct {
	SchemaVersion        string                          `json:"schema_version"`
	Round                int                             `json:"round"`
	MaxInputTokens       int                             `json:"max_input_tokens"`
	ReservedOutputTokens int                             `json:"reserved_output_tokens"`
	OriginalTokens       int                             `json:"original_tokens"`
	UsedTokens           int                             `json:"used_tokens"`
	AvailableTokens      int                             `json:"available_tokens"`
	ReclaimedTokens      int                             `json:"reclaimed_tokens"`
	UsageRatio           float64                         `json:"usage_ratio"`
	Pressure             string                          `json:"pressure"`
	AppliedStrategies    []string                        `json:"applied_strategies"`
	Segments             []agentChatContextBudgetSegment `json:"segments"`
}

type agentChatContextBudgetSegment struct {
	Category  string                       `json:"category"`
	Tokens    int                          `json:"tokens"`
	Ratio     float64                      `json:"ratio"`
	ItemCount int                          `json:"item_count"`
	Protected bool                         `json:"protected"`
	Truncated bool                         `json:"truncated"`
	Items     []agentChatContextBudgetItem `json:"items"`
}

type agentChatContextBudgetItem struct {
	Label          string `json:"label"`
	Role           string `json:"role"`
	SourceRef      string `json:"source_ref"`
	Tokens         int    `json:"tokens"`
	Protected      bool   `json:"protected"`
	ContentPreview string `json:"content_preview"`
	Truncated      bool   `json:"truncated"`
}

func agentChatContextProjection(event observability.AgentEvent, debugEnabled bool) (map[string]any, bool) {
	if event.EventType != observability.EventModelContextBuilt || !debugEnabled || event.Visibility == observability.VisibilityRestricted {
		return nil, false
	}
	var payload agentChatContextEventPayload
	if json.Unmarshal(event.Payload, &payload) != nil || payload.Report.SchemaVersion != agentChatContextReportSchemaVersion {
		return nil, false
	}
	report := payload.Report
	segments := make([]any, 0, minIntValue(len(report.Segments), agentChatContextMaxSegments))
	for index, segment := range report.Segments {
		if index >= agentChatContextMaxSegments {
			break
		}
		if strings.TrimSpace(segment.Category) == "" || segment.Tokens < 0 {
			continue
		}
		items := make([]any, 0, minIntValue(len(segment.Items), agentChatContextMaxItems))
		for itemIndex, item := range segment.Items {
			if itemIndex >= agentChatContextMaxItems {
				break
			}
			protected := segment.Protected || item.Protected
			itemView := map[string]any{
				"label":     boundedAgentChatText(item.Label, 200),
				"role":      boundedAgentChatText(item.Role, 40),
				"sourceRef": boundedAgentChatText(item.SourceRef, 500),
				"tokens":    maxIntValue(0, item.Tokens),
				"protected": protected,
				"truncated": item.Truncated,
			}
			if !protected {
				if preview := boundedAgentChatText(observability.RedactBasic(item.ContentPreview), agentChatContextPreviewRunes); preview != "" {
					itemView["contentPreview"] = preview
				}
			}
			items = append(items, itemView)
		}
		segments = append(segments, map[string]any{
			"category":  boundedAgentChatText(segment.Category, 80),
			"tokens":    maxIntValue(0, segment.Tokens),
			"ratio":     clampContextRatio(segment.Ratio),
			"itemCount": maxIntValue(segment.ItemCount, len(segment.Items)),
			"protected": segment.Protected,
			"truncated": segment.Truncated || len(segment.Items) > agentChatContextMaxItems,
			"items":     items,
		})
	}
	return map[string]any{
		"schemaVersion":        agentChatContextSchemaVersion,
		"eventId":              event.EventID,
		"runId":                event.RunID,
		"round":                report.Round,
		"maxInputTokens":       maxIntValue(0, report.MaxInputTokens),
		"reservedOutputTokens": maxIntValue(0, report.ReservedOutputTokens),
		"originalTokens":       maxIntValue(0, report.OriginalTokens),
		"usedTokens":           maxIntValue(0, report.UsedTokens),
		"availableTokens":      maxIntValue(0, report.AvailableTokens),
		"reclaimedTokens":      maxIntValue(0, report.ReclaimedTokens),
		"usageRatio":           clampContextRatio(report.UsageRatio),
		"pressure":             boundedAgentChatText(report.Pressure, 40),
		"appliedStrategies":    boundedContextStrings(report.AppliedStrategies, 20, 100),
		"segments":             segments,
		"createdAt":            event.CreatedAt,
	}, true
}

func decorateAgentChatContext(view map[string]any, run *storage.Run) {
	if run == nil {
		return
	}
	view["runId"] = run.RunID
	view["parentRunId"] = run.ParentRunID
	view["agentId"] = run.AgentID
}

func boundedContextStrings(values []string, limit, runeLimit int) []string {
	if len(values) > limit {
		values = values[:limit]
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = boundedAgentChatText(value, runeLimit); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func clampContextRatio(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func maxIntValue(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func minIntValue(left, right int) int {
	if left < right {
		return left
	}
	return right
}
