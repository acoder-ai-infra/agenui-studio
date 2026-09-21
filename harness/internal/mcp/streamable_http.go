package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const streamableHTTPProtocolVersion = "2025-03-26"

type StreamableHTTPClientOption func(*StreamableHTTPClient)

func WithStreamableHTTPClient(client *http.Client) StreamableHTTPClientOption {
	return func(c *StreamableHTTPClient) {
		if client != nil {
			c.httpClient = client
		}
	}
}

func WithStreamableHTTPHeaders(headers map[string]string) StreamableHTTPClientOption {
	return func(c *StreamableHTTPClient) {
		c.headers = cloneStringMap(headers)
	}
}

func WithStreamableHTTPProtocolVersion(version string) StreamableHTTPClientOption {
	return func(c *StreamableHTTPClient) {
		if strings.TrimSpace(version) != "" {
			c.protocolVersion = strings.TrimSpace(version)
		}
	}
}

func WithStreamableHTTPDeclaredTools(tools []Tool) StreamableHTTPClientOption {
	return func(c *StreamableHTTPClient) {
		c.tools = cloneTools(tools)
	}
}

type StreamableHTTPClient struct {
	endpoint        string
	httpClient      *http.Client
	headers         map[string]string
	tools           []Tool
	protocolVersion string
}

var _ Client = (*StreamableHTTPClient)(nil)

func NewStreamableHTTPClient(endpoint string, opts ...StreamableHTTPClientOption) (*StreamableHTTPClient, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, errorsConfiguration("streamable-http endpoint is required")
	}
	client := &StreamableHTTPClient{
		endpoint:        endpoint,
		httpClient:      &http.Client{Timeout: 30 * time.Second},
		protocolVersion: streamableHTTPProtocolVersion,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(client)
		}
	}
	return client, nil
}

func (c *StreamableHTTPClient) ListTools(ctx context.Context) ([]Tool, error) {
	if len(c.tools) > 0 {
		return cloneTools(c.tools), nil
	}
	var result struct {
		Tools []remoteTool `json:"tools"`
	}
	if err := c.callRPC(ctx, "tools/list", nil, &result); err != nil {
		return nil, err
	}
	tools := make([]Tool, 0, len(result.Tools))
	for _, tool := range result.Tools {
		schema := tool.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		tools = append(tools, Tool{Name: tool.Name, Description: tool.Description, InputSchema: append(json.RawMessage(nil), schema...)})
	}
	return tools, nil
}

func (c *StreamableHTTPClient) CallTool(ctx context.Context, name string, arguments json.RawMessage, _ CallOptions) (ToolResult, error) {
	var args any = map[string]any{}
	if len(arguments) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(arguments))
		decoder.UseNumber()
		if err := decoder.Decode(&args); err != nil {
			return ToolResult{}, err
		}
	}
	var result remoteCallToolResult
	if err := c.callRPC(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &result); err != nil {
		return ToolResult{}, err
	}
	content, err := result.modelContent()
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Content: content, IsError: result.IsError}, nil
}

func (c *StreamableHTTPClient) callRPC(ctx context.Context, method string, params any, result any) error {
	sessionID, err := c.initialize(ctx)
	if err != nil {
		return err
	}
	req := jsonRPCRequest{JSONRPC: "2.0", ID: 2, Method: method, Params: params}
	payload, err := c.post(ctx, req, sessionID)
	if err != nil {
		return err
	}
	var resp jsonRPCResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return fmt.Errorf("decode mcp %s response: %w", method, err)
	}
	if resp.Error != nil {
		return fmt.Errorf("mcp %s failed: %s", method, resp.Error.Message)
	}
	if result == nil {
		return nil
	}
	if len(resp.Result) == 0 {
		return fmt.Errorf("mcp %s response missing result", method)
	}
	if err := json.Unmarshal(resp.Result, result); err != nil {
		return fmt.Errorf("decode mcp %s result: %w", method, err)
	}
	return nil
}

func (c *StreamableHTTPClient) initialize(ctx context.Context) (string, error) {
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params: map[string]any{
			"protocolVersion": c.protocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "harness-harness", "version": "1.0"},
		},
	}
	_, headers, status, err := c.doPost(ctx, req, "")
	if err != nil {
		return "", err
	}
	sessionID := headers.Get("Mcp-Session-Id")
	if sessionID == "" {
		return "", fmt.Errorf("mcp initialize status=%d missing Mcp-Session-Id header", status)
	}
	if err := c.notify(ctx, "notifications/initialized", nil, sessionID); err != nil {
		return "", err
	}
	return sessionID, nil
}

func (c *StreamableHTTPClient) post(ctx context.Context, req jsonRPCRequest, sessionID string) ([]byte, error) {
	payload, _, _, err := c.doPost(ctx, req, sessionID)
	return payload, err
}

func (c *StreamableHTTPClient) doPost(ctx context.Context, rpcReq jsonRPCRequest, sessionID string) ([]byte, http.Header, int, error) {
	body, err := json.Marshal(rpcReq)
	if err != nil {
		return nil, nil, 0, err
	}
	data, headers, status, err := c.doRawPost(ctx, body, sessionID)
	if err != nil {
		return nil, headers, status, err
	}
	payload, err := extractJSONRPCPayload(data)
	if err != nil {
		return nil, headers, status, err
	}
	return payload, headers, status, nil
}

func (c *StreamableHTTPClient) notify(ctx context.Context, method string, params any, sessionID string) error {
	body, err := json.Marshal(jsonRPCNotification{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	_, _, _, err = c.doRawPost(ctx, body, sessionID)
	if err != nil {
		return err
	}
	return nil
}

func (c *StreamableHTTPClient) doRawPost(ctx context.Context, body []byte, sessionID string) ([]byte, http.Header, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	for key, value := range c.headers {
		req.Header.Set(key, value)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.Header.Clone(), resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return data, resp.Header.Clone(), resp.StatusCode, fmt.Errorf("mcp http status=%d body=%s", resp.StatusCode, string(data))
	}
	return data, resp.Header.Clone(), resp.StatusCode, nil
}

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      int              `json:"id"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *jsonRPCErrorObj `json:"error,omitempty"`
}

type jsonRPCErrorObj struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type remoteTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

type remoteCallToolResult struct {
	Content           []remoteContent `json:"content,omitempty"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

type remoteContent struct {
	Type string          `json:"type"`
	Text string          `json:"text,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

func (r remoteCallToolResult) modelContent() ([]byte, error) {
	if len(r.StructuredContent) > 0 {
		return append([]byte(nil), r.StructuredContent...), nil
	}
	for _, item := range r.Content {
		if item.Type == "text" && strings.TrimSpace(item.Text) != "" {
			if json.Valid([]byte(item.Text)) {
				return []byte(item.Text), nil
			}
			return json.Marshal(map[string]string{"text": item.Text})
		}
	}
	return json.Marshal(struct {
		Content []remoteContent `json:"content,omitempty"`
		IsError bool            `json:"is_error,omitempty"`
	}{Content: r.Content, IsError: r.IsError})
}

func extractJSONRPCPayload(data []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty mcp response")
	}
	if json.Valid(trimmed) {
		return append([]byte(nil), trimmed...), nil
	}
	lines := strings.Split(string(trimmed), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if json.Valid([]byte(payload)) {
			return []byte(payload), nil
		}
	}
	return nil, fmt.Errorf("mcp response is neither json nor event-stream json")
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}
