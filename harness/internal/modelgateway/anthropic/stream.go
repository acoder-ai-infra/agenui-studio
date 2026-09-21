package anthropic

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

type streamEvent struct {
	Type         string        `json:"type"`
	Index        int           `json:"index"`
	Message      streamMessage `json:"message"`
	ContentBlock responseBlock `json:"content_block"`
	Delta        streamDelta   `json:"delta"`
	Usage        responseUsage `json:"usage"`
	Error        *wireError    `json:"error,omitempty"`
}

type streamMessage struct {
	Usage responseUsage `json:"usage"`
}

type streamDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Thinking    string `json:"thinking"`
	Signature   string `json:"signature"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}

type anthropicStream struct {
	body         io.ReadCloser
	scanner      *bufio.Scanner
	queue        []mg.NormalizedChunk
	toolInputs   map[int]string
	toolArgDelta map[int]bool
	usage        responseUsage
	usageEmitted bool
	done         bool
}

func newStream(body io.ReadCloser) *anthropicStream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return &anthropicStream{
		body: body, scanner: scanner,
		toolInputs: make(map[int]string), toolArgDelta: make(map[int]bool),
	}
}

func (s *anthropicStream) Next(ctx context.Context) (mg.NormalizedChunk, error) {
	if len(s.queue) > 0 {
		return s.pop(), nil
	}
	if s.done {
		return mg.NormalizedChunk{}, io.EOF
	}
	for {
		select {
		case <-ctx.Done():
			return mg.NormalizedChunk{}, ctx.Err()
		default:
		}
		if !s.scanner.Scan() {
			if err := s.scanner.Err(); err != nil {
				return mg.NormalizedChunk{}, err
			}
			return mg.NormalizedChunk{}, &mg.AdapterError{
				Message: "anthropic stream ended before message_stop", Class: mg.ErrorStreamInterrupted, Retryable: true,
			}
		}
		line := strings.TrimSpace(s.scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		var event streamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return mg.NormalizedChunk{}, &mg.AdapterError{
				Message: "decode anthropic stream event: " + err.Error(), Class: mg.ErrorStreamInterrupted, Retryable: true,
			}
		}
		s.enqueue(event)
		if len(s.queue) > 0 {
			return s.pop(), nil
		}
		if s.done {
			return mg.NormalizedChunk{}, io.EOF
		}
	}
}

func (s *anthropicStream) enqueue(event streamEvent) {
	switch event.Type {
	case "message_start":
		s.mergeUsage(event.Message.Usage)
	case "content_block_start":
		s.enqueueBlockStart(event.Index, event.ContentBlock)
	case "content_block_delta":
		s.enqueueBlockDelta(event.Index, event.Delta)
	case "content_block_stop":
		s.enqueueBlockStop(event.Index)
	case "message_delta":
		s.mergeUsage(event.Usage)
		if usagePresent(s.usage) && !s.usageEmitted {
			s.queue = append(s.queue, usageChunk(s.usage))
			s.usageEmitted = true
		}
	case "message_stop":
		if usagePresent(s.usage) && !s.usageEmitted {
			s.queue = append(s.queue, usageChunk(s.usage))
			s.usageEmitted = true
		}
		s.queue = append(s.queue, mg.NormalizedChunk{Kind: mg.ChunkDone})
		s.done = true
	case "error":
		s.queue = append(s.queue, mg.NormalizedChunk{Kind: mg.ChunkError, Err: classifyWireError(event.Error, 0)})
		s.done = true
	}
}

func (s *anthropicStream) enqueueBlockStart(index int, block responseBlock) {
	switch block.Type {
	case "text":
		if block.Text != "" {
			s.queue = append(s.queue, mg.NormalizedChunk{Kind: mg.ChunkToken, TextDelta: block.Text})
		}
	case "thinking":
		if block.Thinking != "" || block.Signature != "" {
			s.queue = append(s.queue, mg.NormalizedChunk{Kind: mg.ChunkThought, ThoughtDelta: block.Thinking, ReasoningSignature: block.Signature})
		}
	case "tool_use":
		initial := string(block.Input)
		if initial == "" || initial == "null" {
			initial = "{}"
		}
		s.toolInputs[index] = initial
		s.queue = append(s.queue, mg.NormalizedChunk{Kind: mg.ChunkToolCall, ToolCallDelta: &mg.ToolCallDelta{
			Index: index, ToolCallID: block.ID, Name: block.Name,
		}})
	}
}

func (s *anthropicStream) enqueueBlockDelta(index int, delta streamDelta) {
	switch delta.Type {
	case "text_delta":
		if delta.Text != "" {
			s.queue = append(s.queue, mg.NormalizedChunk{Kind: mg.ChunkToken, TextDelta: delta.Text})
		}
	case "thinking_delta":
		if delta.Thinking != "" {
			s.queue = append(s.queue, mg.NormalizedChunk{Kind: mg.ChunkThought, ThoughtDelta: delta.Thinking})
		}
	case "signature_delta":
		if delta.Signature != "" {
			s.queue = append(s.queue, mg.NormalizedChunk{Kind: mg.ChunkThought, ReasoningSignature: delta.Signature})
		}
	case "input_json_delta":
		if delta.PartialJSON != "" {
			s.toolArgDelta[index] = true
			s.queue = append(s.queue, mg.NormalizedChunk{Kind: mg.ChunkToolCall, ToolCallDelta: &mg.ToolCallDelta{
				Index: index, ArgumentsDelta: delta.PartialJSON,
			}})
		}
	}
}

// Anthropic streams non-empty tool inputs through input_json_delta, but an
// empty object exists only on content_block_start and produces no delta. Emit
// that start value at block stop only when no argument delta arrived; emitting
// it eagerly would corrupt non-empty inputs by prefixing "{}".
func (s *anthropicStream) enqueueBlockStop(index int) {
	initial, toolBlock := s.toolInputs[index]
	if !toolBlock {
		return
	}
	if !s.toolArgDelta[index] {
		s.queue = append(s.queue, mg.NormalizedChunk{Kind: mg.ChunkToolCall, ToolCallDelta: &mg.ToolCallDelta{
			Index: index, ArgumentsDelta: initial,
		}})
	}
	delete(s.toolInputs, index)
	delete(s.toolArgDelta, index)
}

func (s *anthropicStream) mergeUsage(next responseUsage) {
	if next.InputTokens != 0 {
		s.usage.InputTokens = next.InputTokens
	}
	if next.OutputTokens != 0 {
		s.usage.OutputTokens = next.OutputTokens
	}
	if next.CacheCreationInputTokens != 0 {
		s.usage.CacheCreationInputTokens = next.CacheCreationInputTokens
	}
	if next.CacheReadInputTokens != 0 {
		s.usage.CacheReadInputTokens = next.CacheReadInputTokens
	}
}

func (s *anthropicStream) pop() mg.NormalizedChunk {
	chunk := s.queue[0]
	s.queue = s.queue[1:]
	return chunk
}

func (s *anthropicStream) Close() error {
	if s.body != nil {
		return s.body.Close()
	}
	return nil
}
