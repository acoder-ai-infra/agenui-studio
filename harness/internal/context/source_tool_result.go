package context

import (
	gocontext "context"
)

// ToolResultSource produces dynamic fragments from tool execution results.
type ToolResultSource struct {
	MaxResults   int // 0 = no limit
	MaxCharsEach int // 0 = no limit
}

func NewToolResultSource(maxResults, maxCharsEach int) *ToolResultSource {
	return &ToolResultSource{MaxResults: maxResults, MaxCharsEach: maxCharsEach}
}

func (s *ToolResultSource) Kind() SourceKind { return SourceToolResult }

func (s *ToolResultSource) Collect(_ gocontext.Context, req CollectRequest) ([]ContextFragment, error) {
	type indexedMessage struct {
		message *Message
		index   int
	}
	var toolMsgs []indexedMessage
	for index, msg := range req.Messages {
		if msg.Role == RoleTool {
			toolMsgs = append(toolMsgs, indexedMessage{message: msg, index: index})
		}
	}

	if s.MaxResults > 0 && len(toolMsgs) > s.MaxResults {
		toolMsgs = toolMsgs[len(toolMsgs)-s.MaxResults:]
	}

	counter := EstimateCounter{}
	var frags []ContextFragment
	for _, indexed := range toolMsgs {
		msg := indexed.message
		content := msg.Content
		if s.MaxCharsEach > 0 {
			content = truncateRunes(content, s.MaxCharsEach)
		}
		ordinal := int64(indexed.index + 1)
		if msg.Sequence > 0 {
			ordinal = msg.Sequence
		}
		frags = append(frags, ContextFragment{
			Slot:       SlotToolResult,
			Stability:  StabilityDynamic,
			Priority:   40,
			Pinned:     false,
			Generation: req.Generation,
			Source:     string(SourceToolResult),
			Role:       RoleTool,
			Content:    content,
			Messages:   []*Message{msg},
			TokenCost:  counter.Count(content),
			Ordinal:    ordinal,
			PairingIDs: messagePairingIDs(msg),
		})
	}
	return frags, nil
}

func truncateRunes(content string, maxRunes int) string {
	if maxRunes <= 0 {
		return content
	}
	runes := []rune(content)
	if len(runes) <= maxRunes {
		return content
	}
	return string(runes[:maxRunes]) + "\n... [truncated]"
}
