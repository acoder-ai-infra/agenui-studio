package agentruntime

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

const ContextBudgetReportSchemaVersion = "harness.context_budget_report.v1"

const (
	contextBudgetPreviewRunes = 800
	contextBudgetMaxItems     = 80
)

type ContextBudgetCategory string

const (
	ContextBudgetInstructions    ContextBudgetCategory = "instructions"
	ContextBudgetCurrentInput    ContextBudgetCategory = "current_input"
	ContextBudgetHistory         ContextBudgetCategory = "history"
	ContextBudgetToolDefinitions ContextBudgetCategory = "tool_definitions"
	ContextBudgetToolResults     ContextBudgetCategory = "tool_results"
	ContextBudgetOther           ContextBudgetCategory = "other"
)

type ContextBudgetPressure string

const (
	ContextBudgetPressureNormal   ContextBudgetPressure = "normal"
	ContextBudgetPressureElevated ContextBudgetPressure = "elevated"
	ContextBudgetPressureHigh     ContextBudgetPressure = "high"
	ContextBudgetPressureCritical ContextBudgetPressure = "critical"
)

type ContextBudgetReport struct {
	SchemaVersion        string                 `json:"schema_version"`
	Round                int                    `json:"round"`
	MaxInputTokens       int                    `json:"max_input_tokens"`
	ReservedOutputTokens int                    `json:"reserved_output_tokens,omitempty"`
	OriginalTokens       int                    `json:"original_tokens"`
	UsedTokens           int                    `json:"used_tokens"`
	AvailableTokens      int                    `json:"available_tokens"`
	ReclaimedTokens      int                    `json:"reclaimed_tokens,omitempty"`
	UsageRatio           float64                `json:"usage_ratio"`
	Pressure             ContextBudgetPressure  `json:"pressure"`
	AppliedStrategies    []string               `json:"applied_strategies,omitempty"`
	Segments             []ContextBudgetSegment `json:"segments"`
}

type ContextBudgetSegment struct {
	Category  ContextBudgetCategory `json:"category"`
	Tokens    int                   `json:"tokens"`
	Ratio     float64               `json:"ratio"`
	ItemCount int                   `json:"item_count"`
	Protected bool                  `json:"protected,omitempty"`
	Truncated bool                  `json:"truncated,omitempty"`
	Items     []ContextBudgetItem   `json:"items,omitempty"`
}

type ContextBudgetItem struct {
	Label          string `json:"label"`
	Role           string `json:"role,omitempty"`
	SourceRef      string `json:"source_ref,omitempty"`
	Tokens         int    `json:"tokens"`
	Protected      bool   `json:"protected,omitempty"`
	ContentPreview string `json:"content_preview,omitempty"`
	Truncated      bool   `json:"truncated,omitempty"`
}

type weightedContextBudgetItem struct {
	category ContextBudgetCategory
	weight   int
	item     ContextBudgetItem
}

func buildContextBudgetReport(req ModelInvokeRequest, governance ModelInputGovernance) ContextBudgetReport {
	items := contextBudgetItems(req.Messages, req.Tools)
	allocateContextBudgetTokens(items, governance.FinalTokens)

	segmentsByCategory := make(map[ContextBudgetCategory]*ContextBudgetSegment)
	for _, weighted := range items {
		segment := segmentsByCategory[weighted.category]
		if segment == nil {
			segment = &ContextBudgetSegment{Category: weighted.category}
			segmentsByCategory[weighted.category] = segment
		}
		segment.Tokens += weighted.item.Tokens
		segment.ItemCount++
		segment.Protected = segment.Protected || weighted.item.Protected
		if len(segment.Items) < contextBudgetMaxItems {
			segment.Items = append(segment.Items, weighted.item)
		} else {
			segment.Truncated = true
		}
	}

	order := []ContextBudgetCategory{
		ContextBudgetInstructions,
		ContextBudgetCurrentInput,
		ContextBudgetHistory,
		ContextBudgetToolDefinitions,
		ContextBudgetToolResults,
		ContextBudgetOther,
	}
	segments := make([]ContextBudgetSegment, 0, len(segmentsByCategory))
	for _, category := range order {
		segment := segmentsByCategory[category]
		if segment == nil || segment.Tokens == 0 {
			continue
		}
		if req.Package.RuntimeConstraints.TokenBudget.MaxInputTokens > 0 {
			segment.Ratio = float64(segment.Tokens) / float64(req.Package.RuntimeConstraints.TokenBudget.MaxInputTokens)
		}
		segments = append(segments, *segment)
	}

	maxInput := req.Package.RuntimeConstraints.TokenBudget.MaxInputTokens
	available := maxInput - governance.FinalTokens
	if available < 0 {
		available = 0
	}
	ratio := 0.0
	if maxInput > 0 {
		ratio = float64(governance.FinalTokens) / float64(maxInput)
	}
	reclaimed := governance.OriginalTokens - governance.FinalTokens
	if reclaimed < 0 {
		reclaimed = 0
	}
	return ContextBudgetReport{
		SchemaVersion:        ContextBudgetReportSchemaVersion,
		Round:                req.Round,
		MaxInputTokens:       maxInput,
		ReservedOutputTokens: req.Package.RuntimeConstraints.TokenBudget.ReservedOutputTokens,
		OriginalTokens:       governance.OriginalTokens,
		UsedTokens:           governance.FinalTokens,
		AvailableTokens:      available,
		ReclaimedTokens:      reclaimed,
		UsageRatio:           ratio,
		Pressure:             contextBudgetPressure(ratio, req.Package.RuntimeConstraints.CompactionPolicy),
		AppliedStrategies:    append([]string(nil), governance.AppliedStrategies...),
		Segments:             segments,
	}
}

func contextBudgetItems(messages []ModelCallMessage, tools []ModelToolDefinition) []*weightedContextBudgetItem {
	latestUser := -1
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == "user" {
			latestUser = index
			break
		}
	}
	items := make([]*weightedContextBudgetItem, 0, len(messages)+len(tools))
	for index, message := range messages {
		category, protected := contextBudgetMessageCategory(message, index == latestUser)
		preview, truncated := contextBudgetMessagePreview(message, protected)
		label := message.Role
		if message.ToolName != "" {
			label = message.Role + ": " + message.ToolName
		} else if len(message.ToolCalls) > 0 && message.ToolCalls[0].Name != "" {
			label = "tool call: " + message.ToolCalls[0].Name
		}
		items = append(items, &weightedContextBudgetItem{
			category: category,
			weight:   maxInt(1, estimateModelCallTokens([]ModelCallMessage{message}, nil)),
			item: ContextBudgetItem{
				Label: label, Role: message.Role, SourceRef: contextBudgetMessageRef(message, index),
				Protected: protected, ContentPreview: preview, Truncated: truncated,
			},
		})
	}
	for index, tool := range tools {
		data, _ := json.Marshal(tool)
		preview, truncated := boundedContextBudgetPreview(string(data))
		items = append(items, &weightedContextBudgetItem{
			category: ContextBudgetToolDefinitions,
			weight:   maxInt(1, estimateModelCallTokens(nil, []ModelToolDefinition{tool})),
			item: ContextBudgetItem{
				Label: tool.Name, SourceRef: fmt.Sprintf("tool-definition://%d/%s", index, tool.Name),
				ContentPreview: preview, Truncated: truncated,
			},
		})
	}
	return items
}

func contextBudgetMessageCategory(message ModelCallMessage, current bool) (ContextBudgetCategory, bool) {
	switch message.Role {
	case "system", "developer":
		return ContextBudgetInstructions, true
	case "user":
		if current {
			return ContextBudgetCurrentInput, false
		}
		return ContextBudgetHistory, false
	case "tool":
		return ContextBudgetToolResults, false
	case "assistant":
		if len(message.ToolCalls) > 0 || message.ToolCallID != "" || message.ToolName != "" {
			return ContextBudgetToolResults, false
		}
		return ContextBudgetHistory, false
	default:
		return ContextBudgetOther, true
	}
}

func contextBudgetMessagePreview(message ModelCallMessage, protected bool) (string, bool) {
	if protected {
		return "", false
	}
	parts := make([]string, 0, 1+len(message.ContentParts)+len(message.ToolCalls))
	if strings.TrimSpace(message.Content) != "" {
		parts = append(parts, message.Content)
	}
	for _, part := range message.ContentParts {
		if strings.TrimSpace(part.Text) != "" {
			parts = append(parts, part.Text)
		} else if part.Type != "" {
			parts = append(parts, "["+part.Type+"]")
		}
	}
	for _, call := range message.ToolCalls {
		parts = append(parts, call.Name+"(...)")
	}
	return boundedContextBudgetPreview(strings.Join(parts, "\n"))
}

func contextBudgetMessageRef(message ModelCallMessage, index int) string {
	if message.ToolCallID != "" {
		return "tool-call://" + message.ToolCallID
	}
	return fmt.Sprintf("model-message://%d", index)
}

func boundedContextBudgetPreview(value string) (string, bool) {
	value = strings.TrimSpace(redactContextBudgetPreview(value))
	if value == "" {
		return "", false
	}
	if utf8.RuneCountInString(value) <= contextBudgetPreviewRunes {
		return value, false
	}
	runes := []rune(value)
	return string(runes[:contextBudgetPreviewRunes]) + "...", true
}

func redactContextBudgetPreview(value string) string {
	var decoded any
	if json.Unmarshal([]byte(value), &decoded) == nil {
		encoded, err := json.Marshal(redactContextBudgetValue(decoded))
		if err == nil {
			return string(encoded)
		}
	}
	return observability.RedactBasic(value)
}

func redactContextBudgetValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		redacted := make(map[string]any, len(typed))
		for key, item := range typed {
			if contextBudgetSensitiveKey(key) {
				redacted[key] = "[REDACTED]"
				continue
			}
			redacted[key] = redactContextBudgetValue(item)
		}
		return redacted
	case []any:
		redacted := make([]any, len(typed))
		for index, item := range typed {
			redacted[index] = redactContextBudgetValue(item)
		}
		return redacted
	default:
		return value
	}
}

func contextBudgetSensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	for _, marker := range []string{"password", "passwd", "secret", "token", "api_key", "apikey", "authorization", "credential"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func allocateContextBudgetTokens(items []*weightedContextBudgetItem, total int) {
	if len(items) == 0 || total <= 0 {
		return
	}
	weightTotal := 0
	for _, item := range items {
		weightTotal += maxInt(1, item.weight)
	}
	remaining := total
	if total >= len(items) {
		for _, item := range items {
			item.item.Tokens = 1
		}
		remaining -= len(items)
	}
	type remainder struct {
		index int
		value int
	}
	remainders := make([]remainder, 0, len(items))
	for index, item := range items {
		numerator := remaining * maxInt(1, item.weight)
		allocated := 0
		if weightTotal > 0 {
			allocated = numerator / weightTotal
		}
		item.item.Tokens += allocated
		remainders = append(remainders, remainder{index: index, value: numerator % weightTotal})
	}
	allocated := 0
	for _, item := range items {
		allocated += item.item.Tokens
	}
	sort.SliceStable(remainders, func(i, j int) bool { return remainders[i].value > remainders[j].value })
	for index := 0; allocated < total; index++ {
		items[remainders[index%len(remainders)].index].item.Tokens++
		allocated++
	}
}

func contextBudgetPressure(ratio float64, policy ContextCompactionPolicy) ContextBudgetPressure {
	soft := policy.SoftTriggerRatio
	if soft <= 0 {
		soft = 0.75
	}
	compact := policy.CompactTriggerRatio
	if compact <= soft {
		compact = 0.85
	}
	switch {
	case ratio >= 0.95:
		return ContextBudgetPressureCritical
	case ratio >= compact:
		return ContextBudgetPressureHigh
	case ratio >= soft:
		return ContextBudgetPressureElevated
	default:
		return ContextBudgetPressureNormal
	}
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
