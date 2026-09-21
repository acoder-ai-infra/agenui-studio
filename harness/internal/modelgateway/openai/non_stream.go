package openai

import (
	"context"
	"encoding/json"
	"io"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

// chatCompletion 是非流式 /chat/completions 响应的形状:服务端在 stream=false 时返回
// 单个 chat.completion JSON 对象(choices[].message.content,无 data: 前缀、无 delta)。
// 字段命名与 sseChunk 保持一致,便于产出与 SSE 路径完全相同的归一化 chunk。
type chatCompletion struct {
	Choices []struct {
		Message struct {
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
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
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

// bufferedStream 逐个吐出预先算好的 NormalizedChunk,吐完返回 io.EOF。用于非流式响应:
// 整段 body 一次解析成队列,满足与 SSE 路径相同的 AdapterStream 契约(Next/Close)。
type bufferedStream struct {
	body  io.ReadCloser
	queue []mg.NormalizedChunk
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
	c := s.queue[0]
	s.queue = s.queue[1:]
	return c, nil
}

func (s *bufferedStream) Close() error {
	if s.body != nil {
		return s.body.Close()
	}
	return nil
}

// newNonStream 读取整段非流式 chat.completion JSON body,归一化成 chunk 队列:
// content -> ChunkToken、reasoning_content -> ChunkThought、tool_calls -> ChunkToolCall,
// 末尾若带 usage 则追加一个 ChunkUsage(与 enqueue() 的 ModelUsage 映射一致)。
func newNonStream(body io.ReadCloser) (*bufferedStream, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	var parsed chatCompletion
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	s := &bufferedStream{body: body}
	for _, choice := range parsed.Choices {
		if choice.Message.Content != "" {
			s.queue = append(s.queue, mg.NormalizedChunk{Kind: mg.ChunkToken, TextDelta: choice.Message.Content})
		}
		if choice.Message.ReasoningContent != "" {
			s.queue = append(s.queue, mg.NormalizedChunk{Kind: mg.ChunkThought, ThoughtDelta: choice.Message.ReasoningContent})
		}
		for _, tc := range choice.Message.ToolCalls {
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
	return s, nil
}
