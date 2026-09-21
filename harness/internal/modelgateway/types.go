package modelgateway

import (
	"bytes"
	"encoding/json"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func jsonPayload(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// ChatMessage 是内联的 prompt/输出消息(不走 artifact ref)。
//
// Content 与 Parts 二选一:纯文本用 Content;多模态(图片/音频)用 Parts。
// 序列化为 OpenAI 兼容的 content 字段——纯文本时是字符串,多模态时是内容块数组
// (DashScope qwen-vl 同款形状)。因此可承接 eino schema.Message.MultiContent /
// langchain content blocks 等多模态消息。
type ChatMessage struct {
	Role       string
	Content    string        // 纯文本内容(与 Parts 二选一)
	Parts      []ContentPart // 多模态内容块(非空时优先)
	Name       string
	ToolCallID string
	ToolCalls  []ChatToolCall
	// ReasoningContent is provider-neutral assistant reasoning history. Some
	// thinking models require it to remain paired with assistant tool_calls.
	ReasoningContent string
	// ReasoningSignature is opaque provider data required to replay some
	// reasoning blocks during a tool loop. It is never user-visible content.
	ReasoningSignature string
}

// ChatToolCall is the provider-neutral assistant tool request retained in the
// next model round so the following tool result can be paired by ToolCallID.
type ChatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ChatFunctionCall `json:"function"`
}

type ChatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ContentPart 是一个多模态内容块(OpenAI content parts 形状)。
type ContentPart struct {
	Type       string      `json:"type"` // text | image_url | input_audio | file
	Text       string      `json:"text,omitempty"`
	ImageURL   *ImageURL   `json:"image_url,omitempty"`
	InputAudio *InputAudio `json:"input_audio,omitempty"`
	// File 是非图片文件内容块（G-C），供支持文件理解的模型原生读取。
	File *FilePart `json:"file,omitempty"`
}

// ImageURL 是图片内容块;URL 可为 http(s) 或 data:image/...;base64,... 。
type ImageURL struct {
	URL string `json:"url"`
	// Detail 是 provider 视觉编码预算（G-B）：空=缺省；low|high|auto。
	// 由 ModelOptions.ImageDetail 在 adapter 层统一注入。
	Detail string `json:"detail,omitempty"`
}

// FilePart 是非图片文件内容块（G-C），对齐 OpenAI 兼容 file content
// part 的线上形状：FileData 承载 data URI（data:<mime>;base64,<data>）。
type FilePart struct {
	FileData string `json:"file_data"`
	Filename string `json:"filename,omitempty"`
}

// InputAudio 是音频输入内容块。
type InputAudio struct {
	Data   string `json:"data"`   // base64
	Format string `json:"format"` // wav|mp3|...
}

// TextMessage 便捷构造一条纯文本消息。
func TextMessage(role, text string) ChatMessage { return ChatMessage{Role: role, Content: text} }

// chatMessageWire 是 ChatMessage 的线上形状:content 为 string 或 []ContentPart。
type chatMessageWire struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content,omitempty"`
	Name             string          `json:"name,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ToolCalls        []ChatToolCall  `json:"tool_calls,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
}

// MarshalJSON emits content as a string (text) or an array (multimodal parts).
func (m ChatMessage) MarshalJSON() ([]byte, error) {
	w := chatMessageWire{Role: m.Role, Name: m.Name, ToolCallID: m.ToolCallID, ToolCalls: m.ToolCalls, ReasoningContent: m.ReasoningContent}
	switch {
	case len(m.Parts) > 0:
		b, err := json.Marshal(m.Parts)
		if err != nil {
			return nil, err
		}
		w.Content = b
	case m.Content != "":
		b, err := json.Marshal(m.Content)
		if err != nil {
			return nil, err
		}
		w.Content = b
	}
	return json.Marshal(w)
}

// UnmarshalJSON accepts content as a string or an array of parts.
func (m *ChatMessage) UnmarshalJSON(b []byte) error {
	var w chatMessageWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	m.Role, m.Name, m.ToolCallID, m.ReasoningContent = w.Role, w.Name, w.ToolCallID, w.ReasoningContent
	m.ToolCalls = append([]ChatToolCall(nil), w.ToolCalls...)
	m.Content, m.Parts = "", nil
	c := bytes.TrimSpace(w.Content)
	if len(c) == 0 {
		return nil
	}
	switch c[0] {
	case '[':
		return json.Unmarshal(c, &m.Parts)
	case '"':
		return json.Unmarshal(c, &m.Content)
	}
	return nil
}

// ModelRequest 是统一请求。作用域来自 TraceContext;messages 内联。
type ModelRequest struct {
	RequestID string
	Trace     observability.TraceContext
	AgentID   string
	Messages  []ChatMessage
	// ToolsSchema 可选:tool schema 透传给模型(网关不执行工具)。
	ToolsSchema       json.RawMessage
	ResponseSchemaRef string
	Streaming         bool
	TimeoutMS         int
	ModelHint         string
	// ModelFallbackHints lists agent-declared fallback model names in priority
	// order. The gateway resolves each against the configured targets and, after
	// the primary (ModelHint) target, appends them as the ordered fallback chain.
	ModelFallbackHints []string
	PromptVersion      string
	ContextHash        string
	Options            ModelOptions
	MaxPromptTokens    int
	MaxOutputTokens    int
	Cache              CachePolicy
	Required           CapabilityRequirement
}

type ReasoningMode string

const (
	ReasoningAuto     ReasoningMode = "auto"
	ReasoningEnabled  ReasoningMode = "enabled"
	ReasoningDisabled ReasoningMode = "disabled"
)

type ModelOptions struct {
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	MaxTokens   *int     `json:"max_tokens,omitempty"`
	Stop        []string `json:"stop,omitempty"`
	ToolChoice  string   `json:"tool_choice,omitempty"`
	// ParallelToolCalls 控制 provider 是否可在一次响应生成多个并行工具调用
	// （A5）。nil=不下发该参数（provider 默认）；由 models.yaml 的模型能力
	// 声明驱动，路由后写入。与 Agent 运行期的 tool_policy.execution 正交。
	ParallelToolCalls *bool         `json:"parallel_tool_calls,omitempty"`
	ReasoningMode     ReasoningMode `json:"reasoning_mode,omitempty"`
	ReasoningBudget   int           `json:"reasoning_budget_tokens,omitempty"`
	ReasoningEffort   string        `json:"reasoning_effort,omitempty"`
	// ResponseFormat 控制 provider 的约束解码输出格式（G-A）：空=不下发；
	// json_object=强制合法 JSON。由 agents.yaml 的 model.response_format
	// 静态驱动。
	ResponseFormat string `json:"response_format,omitempty"`
	// ImageDetail 控制本请求内全部图片 part 的视觉编码预算（G-B）：
	// 空=不下发（provider 缺省）；low/high/auto 逐 part 写入 image_url.detail。
	ImageDetail string `json:"image_detail,omitempty"`
}

func cloneModelOptions(input ModelOptions) ModelOptions {
	if input.Temperature != nil {
		value := *input.Temperature
		input.Temperature = &value
	}
	if input.TopP != nil {
		value := *input.TopP
		input.TopP = &value
	}
	if input.MaxTokens != nil {
		value := *input.MaxTokens
		input.MaxTokens = &value
	}
	if input.ParallelToolCalls != nil {
		value := *input.ParallelToolCalls
		input.ParallelToolCalls = &value
	}
	input.Stop = append([]string(nil), input.Stop...)
	return input
}

// CallStatus 是模型调用的终态。
type CallStatus string

const (
	StatusSuccess CallStatus = "success"
	StatusFailed  CallStatus = "failed"
)

// UsageSource marks whether usage came from the provider gateway or a local
// estimator fallback.
type UsageSource string

const (
	UsageSourceGateway   UsageSource = "gateway"
	UsageSourceEstimated UsageSource = "estimated"
)

// ModelUsage is the normalized token accounting for a model call.
type ModelUsage struct {
	PromptTokens     int         `json:"prompt_tokens"`
	CompletionTokens int         `json:"completion_tokens"`
	ReasoningTokens  int         `json:"reasoning_tokens,omitempty"`
	CacheReadTokens  int         `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int         `json:"cache_write_tokens,omitempty"`
	Source           UsageSource `json:"source,omitempty"`
}

// ModelCost is a best-effort cost attribution for one model call.
type ModelCost struct {
	Currency  string  `json:"currency,omitempty"`
	Estimated float64 `json:"estimated,omitempty"`
}

// ModelLatency is provider-attempt latency. FirstTokenObserved distinguishes a
// agenuine sub-millisecond first output from attempts that produced no output
// (including cache hits, which are not provider performance samples).
type ModelLatency struct {
	TotalMS               int64   `json:"total_ms"`
	FirstTokenObserved    bool    `json:"first_token_observed"`
	FirstTokenMS          int64   `json:"first_token_ms"`
	GenerationDurationMS  int64   `json:"generation_duration_ms"`
	OutputTokensPerSecond float64 `json:"output_tokens_per_second"`
}

// ModelCostTable describes unit prices per 1K tokens. Missing prices simply
// produce zero estimated cost while preserving usage accounting.
type ModelCostTable struct {
	Currency        string  `json:"currency,omitempty"`
	InputPer1K      float64 `json:"input_per_1k,omitempty"`
	OutputPer1K     float64 `json:"output_per_1k,omitempty"`
	ReasoningPer1K  float64 `json:"reasoning_per_1k,omitempty"`
	CacheReadPer1K  float64 `json:"cache_read_per_1k,omitempty"`
	CacheWritePer1K float64 `json:"cache_write_per_1k,omitempty"`
}

type CachePolicy struct {
	Enabled bool `json:"enabled,omitempty"`
}

type CapabilityRequirement struct {
	Vision           bool `json:"vision,omitempty"`
	ToolCalling      bool `json:"tool_calling,omitempty"`
	StructuredOutput bool `json:"structured_output,omitempty"`
	JSONSchema       bool `json:"json_schema,omitempty"`
}

type ModelCapability struct {
	Model            string `json:"model,omitempty"`
	Provider         string `json:"provider,omitempty"`
	Chat             bool   `json:"chat,omitempty"`
	Streaming        bool   `json:"streaming,omitempty"`
	ToolCalling      bool   `json:"tool_calling,omitempty"`
	StructuredOutput bool   `json:"structured_output,omitempty"`
	Vision           bool   `json:"vision,omitempty"`
	JSONSchema       bool   `json:"json_schema,omitempty"`
	Reasoning        bool   `json:"reasoning,omitempty"`
	ReasoningToggle  bool   `json:"reasoning_toggle,omitempty"`
	// ParallelToolCalls 声明该模型是否支持并行工具调用生成（A5）。nil=不下发
	// 该 provider 参数；非 nil 时路由后写入 ModelOptions 并由 adapter 序列化。
	ParallelToolCalls *bool       `json:"parallel_tool_calls,omitempty"`
	Limits            ModelLimits `json:"limits,omitempty"`
}

type ModelLimits struct {
	MaxContextTokens int `json:"max_context_tokens,omitempty"`
	MaxOutputTokens  int `json:"max_output_tokens,omitempty"`
}

type GatewayDiagnostics struct {
	MessageCount    int            `json:"message_count,omitempty"`
	RoleCounts      map[string]int `json:"role_counts,omitempty"`
	PromptChars     int            `json:"prompt_chars,omitempty"`
	EstimatedTokens int            `json:"estimated_tokens,omitempty"`
	CacheHit        bool           `json:"cache_hit,omitempty"`
	OutputRef       string         `json:"output_ref,omitempty"`
	Classification  string         `json:"classification,omitempty"`
}

type ModelValidation struct {
	SchemaValid bool `json:"schema_valid,omitempty"`
}

// ModelToolCall 是模型返回的聚合后 tool call。
type ModelToolCall struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	Arguments  string `json:"arguments,omitempty"`
}

// ModelResponse 是统一响应。输出文本内联返回(不走 artifact)。
type ModelResponse struct {
	RequestID string
	Status    CallStatus
	Provider  string
	Model     string
	Text      string
	OutputRef string
	ToolCalls []ModelToolCall
	// ReasoningSignature is opaque provider state used only to continue a
	// reasoning-enabled tool loop. It is not emitted as visible model output.
	ReasoningSignature string
	Usage              ModelUsage
	Cost               ModelCost
	Latency            ModelLatency
	Diagnostics        GatewayDiagnostics
	Validation         ModelValidation
	CacheHit           bool
	FallbackApplied    bool
	Attempt            int
	Error              *observability.EventError
}

// --- model_* 事件 payload(payload owner = 本包) ---

type ModelCallStartedPayload struct {
	RequestID   string             `json:"request_id"`
	Model       string             `json:"model"`
	Provider    string             `json:"provider,omitempty"`
	Gateway     string             `json:"gateway,omitempty"`
	Attempt     int                `json:"attempt"`
	Streaming   bool               `json:"streaming"`
	Diagnostics GatewayDiagnostics `json:"diagnostics,omitempty"`
}

type ModelTokenDeltaPayload struct {
	Text string `json:"text"`
}

type ModelThoughtDeltaPayload struct {
	Text string `json:"text"`
}

type ModelToolCallDeltaPayload struct {
	Index          int    `json:"index"`
	ToolCallID     string `json:"tool_call_id,omitempty"`
	Name           string `json:"name,omitempty"`
	ArgumentsDelta string `json:"arguments_delta,omitempty"`
}

type ModelUsageDeltaPayload struct {
	Usage ModelUsage `json:"usage"`
	Cost  ModelCost  `json:"cost,omitempty"`
}

type ModelCallCompletedPayload struct {
	RequestID   string             `json:"request_id"`
	Model       string             `json:"model"`
	Provider    string             `json:"provider,omitempty"`
	Usage       ModelUsage         `json:"usage"`
	Cost        ModelCost          `json:"cost,omitempty"`
	Latency     ModelLatency       `json:"latency"`
	OutputRef   string             `json:"output_ref,omitempty"`
	CacheHit    bool               `json:"cache_hit,omitempty"`
	Diagnostics GatewayDiagnostics `json:"diagnostics,omitempty"`
}

type ModelCallFailedPayload struct {
	RequestID       string       `json:"request_id"`
	Model           string       `json:"model,omitempty"`
	Provider        string       `json:"provider,omitempty"`
	Attempt         int          `json:"attempt"`
	FallbackApplied bool         `json:"fallback_applied,omitempty"`
	Class           string       `json:"class,omitempty"`
	Message         string       `json:"message,omitempty"`
	Usage           ModelUsage   `json:"usage,omitempty"`
	Cost            ModelCost    `json:"cost,omitempty"`
	Latency         ModelLatency `json:"latency"`
}

type ModelFallbackAppliedPayload struct {
	RequestID string      `json:"request_id"`
	From      ModelTarget `json:"from"`
	To        ModelTarget `json:"to"`
	Attempt   int         `json:"attempt"`
	Reason    string      `json:"reason,omitempty"`
}

type EmbeddingRequest struct {
	RequestID string
	Trace     observability.TraceContext
	AgentID   string
	Texts     []string
	ModelHint string
}

type EmbeddingResponse struct {
	RequestID  string
	Provider   string
	Model      string
	Embeddings [][]float64
	Usage      ModelUsage
	Cost       ModelCost
}

type RerankRequest struct {
	RequestID string
	Trace     observability.TraceContext
	AgentID   string
	Query     string
	Documents []string
	ModelHint string
}

type RerankResult struct {
	Index int
	Score float64
}

type RerankResponse struct {
	RequestID string
	Provider  string
	Model     string
	Results   []RerankResult
	Usage     ModelUsage
	Cost      ModelCost
}

type JudgeRequest struct {
	RequestID string
	Trace     observability.TraceContext
	AgentID   string
	Input     string
	Output    string
	Rubric    string
	ModelHint string
}

type JudgeResponse struct {
	RequestID string
	Provider  string
	Model     string
	Score     float64
	Reason    string
	Usage     ModelUsage
	Cost      ModelCost
}
