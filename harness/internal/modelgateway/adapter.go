package modelgateway

import (
	"context"
	"encoding/json"
)

// ProviderAdapter 是单个下游 provider 端点的基接口(能力子接口的公共基)。
// 保留基接口是为了给未来的 image/video 等能力子接口共享;当前只有 ChatProvider。
type ProviderAdapter interface {
	ID() string
}

// ChatProvider 是 chat/completions 能力子接口。适配器把 provider 的流式响应
// 归一为与协议无关的 NormalizedChunk;线路协议(openai/anthropic)是实现内部细节。
type ChatProvider interface {
	ProviderAdapter
	InvokeChat(ctx context.Context, req AdapterRequest) (AdapterStream, error)
}

// AdapterRequest 是已物化的具体请求(内联 messages,不走 artifact ref)。
type AdapterRequest struct {
	Model                  string
	Messages               []ChatMessage
	ToolsSchema            json.RawMessage // 可选:tool schema 透传
	Streaming              bool
	TimeoutMS              int
	Options                ModelOptions
	PromptCacheAffinityKey string // Anthropic company-gateway routing only.
}

// AdapterStream 持续拉取归一化 chunk 直到 io.EOF。
type AdapterStream interface {
	Next(ctx context.Context) (NormalizedChunk, error)
	Close() error
}

// ChunkKind 对归一化 chunk 分类。
type ChunkKind string

const (
	ChunkToken    ChunkKind = "token"
	ChunkThought  ChunkKind = "thought"
	ChunkToolCall ChunkKind = "tool_call_delta"
	ChunkUsage    ChunkKind = "usage"
	ChunkDone     ChunkKind = "done"
	ChunkError    ChunkKind = "error"
)

// NormalizedChunk 是适配器产出的、与协议无关的流式 chunk。
type NormalizedChunk struct {
	Kind               ChunkKind
	TextDelta          string
	ThoughtDelta       string
	ReasoningSignature string
	ToolCallDelta      *ToolCallDelta
	Usage              *ModelUsage
	Err                *AdapterError
}

// ToolCallDelta 是来自 provider 流的增量 tool call 片段。
type ToolCallDelta struct {
	Index          int
	ToolCallID     string
	Name           string
	ArgumentsDelta string
}

// AdapterError 是适配器/流内错误。Retryable 决定 Facade 是否换下一个 fallback target
// (例如 HTTP 429/5xx、SSE 内嵌 throttling 均为可重试)。
type AdapterError struct {
	Message    string
	Retryable  bool
	Class      ModelErrorClass
	HTTPStatus int
	Latency    ModelLatency
}

func (e *AdapterError) Error() string { return e.Message }
