package openai

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

func drain(t *testing.T, s mg.AdapterStream) []mg.NormalizedChunk {
	t.Helper()
	var out []mg.NormalizedChunk
	for {
		c, err := s.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		out = append(out, c)
	}
}

func TestStreamParseTokensAndUsage(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"choices":[{"delta":{"content":" there"}}]}`,
		`data: {"usage":{"prompt_tokens":3,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":1},"completion_tokens_details":{"reasoning_tokens":4}}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	chunks := drain(t, newStream(io.NopCloser(strings.NewReader(body))))

	var text string
	var gotUsage, gotDone bool
	for _, c := range chunks {
		switch c.Kind {
		case mg.ChunkToken:
			text += c.TextDelta
		case mg.ChunkUsage:
			gotUsage = c.Usage != nil && c.Usage.CompletionTokens == 2 && c.Usage.CacheReadTokens == 1 && c.Usage.ReasoningTokens == 4
		case mg.ChunkDone:
			gotDone = true
		}
	}
	if text != "hi there" || !gotUsage || !gotDone {
		t.Fatalf("text=%q usage=%v done=%v", text, gotUsage, gotDone)
	}
}

func TestStreamParseThrottling(t *testing.T) {
	body := "data: {\"message\":\"Throttling.RateQuota exceeded, TPM limit\"}\n\n"
	chunks := drain(t, newStream(io.NopCloser(strings.NewReader(body))))
	if len(chunks) == 0 || chunks[0].Kind != mg.ChunkError || chunks[0].Err == nil || !chunks[0].Err.Retryable {
		t.Fatalf("expected retryable throttling error chunk, got %+v", chunks)
	}
}

// TestStreamParseThrottlingNotTriggeredByContent 确保模型正文里出现限流关键字
// (如解释 HTTP 429 / rate limit)不会被误判为内嵌限流信号。
func TestStreamParseThrottlingNotTriggeredByContent(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"HTTP 429 means rate limit exceeded; TPM throttling applies."}}]}`,
		`data: {"choices":[{"delta":{"content":" You should retry later."}}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	chunks := drain(t, newStream(io.NopCloser(strings.NewReader(body))))

	var text string
	for _, c := range chunks {
		if c.Kind == mg.ChunkError {
			t.Fatalf("content mentioning throttling keywords must not be classified as throttling: %+v", c.Err)
		}
		if c.Kind == mg.ChunkToken {
			text += c.TextDelta
		}
	}
	if !strings.Contains(text, "429") || !strings.Contains(text, "throttling") {
		t.Fatalf("expected content to pass through verbatim, got %q", text)
	}
}

// TestStreamParseThrottlingStructuredError 确保结构化 error 对象里的限流信号仍被识别。
func TestStreamParseThrottlingStructuredError(t *testing.T) {
	body := "data: {\"error\":{\"code\":\"Throttling.RateQuota\",\"message\":\"TPM limit exceeded\",\"type\":\"rate_limit\"}}\n\n"
	chunks := drain(t, newStream(io.NopCloser(strings.NewReader(body))))
	if len(chunks) == 0 || chunks[0].Kind != mg.ChunkError || chunks[0].Err == nil || !chunks[0].Err.Retryable {
		t.Fatalf("expected retryable throttling error chunk, got %+v", chunks)
	}
	if chunks[0].Err.Class != mg.ErrorRateLimited {
		t.Fatalf("expected rate_limited class, got %v", chunks[0].Err.Class)
	}
}
