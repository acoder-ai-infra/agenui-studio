package anthropic

import (
	"context"
	"encoding/json"
	"io"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

type responseEnvelope struct {
	Type    string          `json:"type"`
	Content []responseBlock `json:"content"`
	Usage   responseUsage   `json:"usage"`
	Error   *wireError      `json:"error,omitempty"`
}

type responseBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

type responseUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type bufferedStream struct {
	body  io.ReadCloser
	queue []mg.NormalizedChunk
}

func newNonStream(body io.ReadCloser) (*bufferedStream, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	var response responseEnvelope
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	if response.Error != nil || response.Type == "error" {
		return nil, classifyWireError(response.Error, 0)
	}
	stream := &bufferedStream{body: body}
	for index, block := range response.Content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				stream.queue = append(stream.queue, mg.NormalizedChunk{Kind: mg.ChunkToken, TextDelta: block.Text})
			}
		case "thinking":
			if block.Thinking != "" || block.Signature != "" {
				stream.queue = append(stream.queue, mg.NormalizedChunk{Kind: mg.ChunkThought, ThoughtDelta: block.Thinking, ReasoningSignature: block.Signature})
			}
		case "tool_use":
			arguments := string(block.Input)
			if arguments == "" || arguments == "null" {
				arguments = "{}"
			}
			stream.queue = append(stream.queue, mg.NormalizedChunk{Kind: mg.ChunkToolCall, ToolCallDelta: &mg.ToolCallDelta{
				Index: index, ToolCallID: block.ID, Name: block.Name, ArgumentsDelta: arguments,
			}})
		}
	}
	if usagePresent(response.Usage) {
		stream.queue = append(stream.queue, usageChunk(response.Usage))
	}
	return stream, nil
}

func (s *bufferedStream) Next(ctx context.Context) (mg.NormalizedChunk, error) {
	select {
	case <-ctx.Done():
		return mg.NormalizedChunk{}, ctx.Err()
	default:
	}
	if len(s.queue) == 0 {
		return mg.NormalizedChunk{}, io.EOF
	}
	chunk := s.queue[0]
	s.queue = s.queue[1:]
	return chunk, nil
}

func (s *bufferedStream) Close() error {
	if s.body != nil {
		return s.body.Close()
	}
	return nil
}

func usagePresent(usage responseUsage) bool {
	return usage.InputTokens != 0 || usage.OutputTokens != 0 || usage.CacheCreationInputTokens != 0 || usage.CacheReadInputTokens != 0
}

func usageChunk(usage responseUsage) mg.NormalizedChunk {
	return mg.NormalizedChunk{Kind: mg.ChunkUsage, Usage: &mg.ModelUsage{
		PromptTokens: usage.InputTokens, CompletionTokens: usage.OutputTokens,
		CacheWriteTokens: usage.CacheCreationInputTokens, CacheReadTokens: usage.CacheReadInputTokens,
		Source: mg.UsageSourceGateway,
	}}
}
