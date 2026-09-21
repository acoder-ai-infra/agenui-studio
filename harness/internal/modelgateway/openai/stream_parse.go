package openai

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

// sseChunk 是 OpenAI 兼容流式 chunk 的形状。
type sseChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	// 结构化错误字段:网关可能把限流/错误以结构化对象内嵌进流(而非 HTTP 状态或
	// delta.content)。只在这些字段上做 throttling 识别,绝不扫描 delta.content,
	// 避免模型正文里出现 "429"/"rate limit"/"tpm" 被误判为限流。
	Code    string `json:"code"`
	Message string `json:"message"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

// openaiStream 把 SSE 响应体解析成 NormalizedChunk(一行可能产出 content+tool_call+usage,
// 因此用队列缓冲)。
type openaiStream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	queue   []mg.NormalizedChunk
	done    bool
}

func newStream(body io.ReadCloser) *openaiStream {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return &openaiStream{body: body, scanner: sc}
}

func (s *openaiStream) Next(ctx context.Context) (mg.NormalizedChunk, error) {
	if len(s.queue) > 0 {
		c := s.queue[0]
		s.queue = s.queue[1:]
		return c, nil
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
			s.done = true
			if err := s.scanner.Err(); err != nil {
				return mg.NormalizedChunk{}, err
			}
			return mg.NormalizedChunk{}, io.EOF
		}
		line := strings.TrimSpace(s.scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			s.done = true
			return mg.NormalizedChunk{Kind: mg.ChunkDone}, nil
		}
		var parsed sseChunk
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			// 跳过无法解析的 keepalive/注释行。
			continue
		}
		// SSE 内嵌 throttling 识别:只在结构化错误字段(error/code/message)上检测,
		// 绝不扫描 delta.content —— 否则模型正文里出现 "429"/"rate limit"/"tpm"
		// 会被误判为限流。部分网关把限流信号以结构化对象内嵌进流而非 HTTP 状态。
		if errText, ok := throttlingSignal(parsed); ok {
			return mg.NormalizedChunk{
				Kind: mg.ChunkError,
				Err:  &mg.AdapterError{Message: errText, Class: mg.ErrorRateLimited, Retryable: true},
			}, nil
		}
		s.enqueue(parsed)
		if len(s.queue) == 0 {
			continue
		}
		c := s.queue[0]
		s.queue = s.queue[1:]
		return c, nil
	}
}

func (s *openaiStream) enqueue(parsed sseChunk) {
	for _, choice := range parsed.Choices {
		if choice.Delta.Content != "" {
			s.queue = append(s.queue, mg.NormalizedChunk{Kind: mg.ChunkToken, TextDelta: choice.Delta.Content})
		}
		if choice.Delta.ReasoningContent != "" {
			s.queue = append(s.queue, mg.NormalizedChunk{Kind: mg.ChunkThought, ThoughtDelta: choice.Delta.ReasoningContent})
		}
		for _, tc := range choice.Delta.ToolCalls {
			s.queue = append(s.queue, mg.NormalizedChunk{
				Kind: mg.ChunkToolCall,
				ToolCallDelta: &mg.ToolCallDelta{
					Index:          tc.Index,
					ToolCallID:     tc.ID,
					Name:           tc.Function.Name,
					ArgumentsDelta: tc.Function.Arguments,
				},
			})
		}
	}
	if parsed.Usage != nil {
		s.queue = append(s.queue, mg.NormalizedChunk{
			Kind: mg.ChunkUsage,
			Usage: &mg.ModelUsage{
				PromptTokens:     parsed.Usage.PromptTokens,
				CompletionTokens: parsed.Usage.CompletionTokens,
				ReasoningTokens:  parsed.Usage.CompletionTokensDetails.ReasoningTokens,
				CacheReadTokens:  parsed.Usage.PromptTokensDetails.CachedTokens,
			},
		})
	}
}

func (s *openaiStream) Close() error {
	if s.body != nil {
		return s.body.Close()
	}
	return nil
}
