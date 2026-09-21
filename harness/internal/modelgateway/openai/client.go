// Package openai is the OpenAI-compatible HTTP backend prototype for the model
// gateway (design §4.3 / §9, D5). It uses raw net/http with a stream-friendly
// client (http.Client.Timeout is NOT set so long streaming reads are not cut
// off; deadlines come from ctx). Capability level: P0 skeleton.
package openai

import (
	"bytes"
	"context"
	"net/http"
	"strings"
)

// Client is a stream-friendly OpenAI-compatible HTTP client. It deliberately
// leaves http.Client.Timeout unset (design §4.3): a global timeout would cut off
// long SSE streams. Per-request deadlines are carried by ctx.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// NewClient builds a stream-friendly client. baseURL points at a provider
// endpoint (e.g. https://dashscope.aliyuncs.com/compatible-mode/v1).
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		// NOTE: no Timeout — long streaming must not be interrupted (design §4.3).
		// Cancellation/deadlines are driven by ctx on each request.
		HTTP: &http.Client{},
	}
}

// PostChatCompletions issues a streaming POST to /chat/completions with the
// given JSON body (Accept: text/event-stream). The caller owns closing
// resp.Body. Use this for req.Streaming == true.
func (c *Client) PostChatCompletions(ctx context.Context, body []byte) (*http.Response, error) {
	return c.postChatCompletions(ctx, body, "text/event-stream")
}

// PostChatCompletionsJSON issues a non-streaming POST to /chat/completions
// (Accept: application/json). The server returns a single chat.completion JSON
// object (choices[].message.content, no SSE framing). The caller owns closing
// resp.Body. Use this for req.Streaming == false.
func (c *Client) PostChatCompletionsJSON(ctx context.Context, body []byte) (*http.Response, error) {
	return c.postChatCompletions(ctx, body, "application/json")
}

// postChatCompletions is the shared POST path; accept controls the Accept
// header (text/event-stream for streaming, application/json for a single JSON
// response).
func (c *Client) postChatCompletions(ctx context.Context, body []byte, accept string) (*http.Response, error) {
	return c.postJSON(ctx, "/chat/completions", body, accept)
}

// PostJSON 向 BaseURL+path 发起一次非流式 JSON POST，评估类端点
// （/embeddings 等）复用。调用方负责关闭 resp.Body。
func (c *Client) PostJSON(ctx context.Context, path string, body []byte) (*http.Response, error) {
	return c.postJSON(ctx, path, body, "application/json")
}

func (c *Client) postJSON(ctx context.Context, path string, body []byte, accept string) (*http.Response, error) {
	url := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	return c.HTTP.Do(req)
}
