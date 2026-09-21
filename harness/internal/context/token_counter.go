package context

import "encoding/json"

// TokenCounter estimates token counts for text and messages.
type TokenCounter interface {
	Count(text string) int
	CountMessage(msg *Message) int
}

// EstimateCounter provides a heuristic token count (~4 chars per token).
type EstimateCounter struct{}

func (c EstimateCounter) Count(text string) int {
	if len(text) == 0 {
		return 0
	}
	n := len(text) / 4
	if n == 0 {
		n = 1
	}
	return n
}

func (c EstimateCounter) CountMessage(msg *Message) int {
	if msg == nil {
		return 0
	}
	tokens := c.Count(msg.Content) + 4 // role overhead
	if len(msg.ToolCalls) > 0 {
		if data, err := json.Marshal(msg.ToolCalls); err == nil {
			tokens += c.Count(string(data))
		}
	}
	if msg.ToolResult != nil {
		if data, err := json.Marshal(msg.ToolResult); err == nil {
			tokens += c.Count(string(data))
		}
	}
	return tokens
}
