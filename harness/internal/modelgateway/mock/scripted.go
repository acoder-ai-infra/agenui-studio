package mock

import (
	"context"
	"io"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

// Script 是一次 InvokeChat 要回放的确定性 chunk 列表。以 done/error 结束或让流到达末尾(io.EOF)。
type Script struct {
	Chunks []mg.NormalizedChunk
	// InvokeErr 非 nil 时,让 InvokeChat 在流式开始前失败(例如 dial/HTTP 错误)。
	InvokeErr error
}

// TokenChunk 构建一个 token delta chunk。
func TokenChunk(text string) mg.NormalizedChunk {
	return mg.NormalizedChunk{Kind: mg.ChunkToken, TextDelta: text}
}

// ThoughtChunk 构建一个 thought delta chunk。
func ThoughtChunk(text string) mg.NormalizedChunk {
	return mg.NormalizedChunk{Kind: mg.ChunkThought, ThoughtDelta: text}
}

// ToolCallChunk 构建一个 tool_call delta chunk。
func ToolCallChunk(index int, id, name, argsDelta string) mg.NormalizedChunk {
	return mg.NormalizedChunk{
		Kind: mg.ChunkToolCall,
		ToolCallDelta: &mg.ToolCallDelta{
			Index:          index,
			ToolCallID:     id,
			Name:           name,
			ArgumentsDelta: argsDelta,
		},
	}
}

// UsageChunk 构建一个 usage chunk。
func UsageChunk(prompt, completion int) mg.NormalizedChunk {
	return mg.NormalizedChunk{
		Kind:  mg.ChunkUsage,
		Usage: &mg.ModelUsage{PromptTokens: prompt, CompletionTokens: completion},
	}
}

// UsageDetailChunk builds a usage chunk with optional reasoning/cache tokens.
func UsageDetailChunk(prompt, completion, reasoning, cacheRead, cacheWrite int) mg.NormalizedChunk {
	return mg.NormalizedChunk{
		Kind: mg.ChunkUsage,
		Usage: &mg.ModelUsage{
			PromptTokens:     prompt,
			CompletionTokens: completion,
			ReasoningTokens:  reasoning,
			CacheReadTokens:  cacheRead,
			CacheWriteTokens: cacheWrite,
		},
	}
}

// ErrorChunk 构建一个流内错误 chunk。
func ErrorChunk(message string, retryable bool) mg.NormalizedChunk {
	return mg.NormalizedChunk{Kind: mg.ChunkError, Err: &mg.AdapterError{Message: message, Retryable: retryable}}
}

// scriptStream 回放一个 Script。
type scriptStream struct {
	chunks []mg.NormalizedChunk
	pos    int
}

func (s *scriptStream) Next(_ context.Context) (mg.NormalizedChunk, error) {
	if s.pos >= len(s.chunks) {
		return mg.NormalizedChunk{}, io.EOF
	}
	c := s.chunks[s.pos]
	s.pos++
	return c, nil
}

func (s *scriptStream) Close() error { return nil }
