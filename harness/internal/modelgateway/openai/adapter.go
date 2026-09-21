package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

// Adapter 是 OpenAI 兼容的 ChatProvider(裸 net/http 流式)。
type Adapter struct {
	id     string
	client *Client

	schemaProfileMu sync.RWMutex
	// schemaProfiles are learned from authoritative provider responses.
	// OpenAI-compatible is a transport contract, not one uniform JSON-Schema
	// dialect: permissive gateways keep the canonical union while a deployment
	// that rejects it is lowered on subsequent requests.
	schemaProfiles map[string]toolSchemaProfile
}

type toolSchemaProfile uint8

const (
	toolSchemaProfileUnknown toolSchemaProfile = iota
	toolSchemaProfileNative
	toolSchemaProfileRestricted
)

var _ mg.ChatProvider = (*Adapter)(nil)

// New 构建一个指向 baseURL、使用 apiKey 的 OpenAI 兼容适配器。
func New(id, baseURL, apiKey string) *Adapter {
	if id == "" {
		id = "openai_provider"
	}
	return &Adapter{id: id, client: NewClient(baseURL, apiKey), schemaProfiles: map[string]toolSchemaProfile{}}
}

func (a *Adapter) ID() string { return a.id }

func (a *Adapter) InvokeChat(ctx context.Context, req mg.AdapterRequest) (mg.AdapterStream, error) {
	profile := a.toolSchemaProfile(req.Model)
	restrictedSchema := profile == toolSchemaProfileRestricted
	negotiatesToolSchema := profile == toolSchemaProfileUnknown && canonicalToolsContainRootUnion(req.ToolsSchema)
	body := map[string]any{
		"model":    req.Model,
		"messages": openAIMessages(req.Messages, req.Options.ImageDetail),
		"stream":   req.Streaming,
	}
	if req.Streaming {
		// OpenAI-compatible providers only include authoritative token usage in
		// streaming responses when explicitly requested.
		body["stream_options"] = map[string]bool{"include_usage": true}
	}
	if req.Options.Temperature != nil {
		body["temperature"] = *req.Options.Temperature
	}
	if req.Options.TopP != nil {
		body["top_p"] = *req.Options.TopP
	}
	if req.Options.MaxTokens != nil {
		body["max_tokens"] = *req.Options.MaxTokens
	}
	if len(req.Options.Stop) > 0 {
		body["stop"] = req.Options.Stop
	}
	if req.Options.ToolChoice != "" {
		body["tool_choice"] = req.Options.ToolChoice
	}
	if req.Options.ParallelToolCalls != nil {
		body["parallel_tool_calls"] = *req.Options.ParallelToolCalls
	}
	// G-A：约束解码输出格式。取值已在 agentregistry 白名单校验，此处
	// 非空即按 OpenAI 兼容形状序列化。
	if req.Options.ResponseFormat != "" {
		body["response_format"] = map[string]string{"type": req.Options.ResponseFormat}
	}
	switch req.Options.ReasoningMode {
	case mg.ReasoningEnabled:
		body["enable_thinking"] = true
	case mg.ReasoningDisabled:
		body["enable_thinking"] = false
	}
	if req.Options.ReasoningBudget > 0 {
		body["thinking_budget"] = req.Options.ReasoningBudget
	}
	if req.Options.ReasoningEffort != "" {
		body["reasoning_effort"] = req.Options.ReasoningEffort
	}
	if len(req.ToolsSchema) > 0 {
		tools, err := openAIToolsWithCompiler(req.ToolsSchema, openAIToolCompiler(restrictedSchema))
		if err != nil {
			return nil, &mg.AdapterError{Message: err.Error(), Class: mg.ErrorProvider4xx, Retryable: false}
		}
		body["tools"] = tools
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	// 流式走 SSE(Accept: text/event-stream);非流式走单条 JSON chat.completion
	// (Accept: application/json)。两条路径共用错误处理,但用不同的响应解析器。
	var resp *http.Response
	if req.Streaming {
		resp, err = a.client.PostChatCompletions(ctx, raw)
	} else {
		resp, err = a.client.PostChatCompletionsJSON(ctx, raw)
	}
	if err != nil {
		return nil, &mg.AdapterError{Message: err.Error(), Class: mg.ErrorNetwork, Retryable: true}
	}
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		_ = resp.Body.Close()
		if len(req.ToolsSchema) > 0 && !restrictedSchema && isUnsupportedToolSchemaResponse(resp.StatusCode, errBody) {
			tools, compileErr := openAIToolsWithCompiler(req.ToolsSchema, openAIToolCompiler(true))
			if compileErr != nil {
				return nil, &mg.AdapterError{Message: compileErr.Error(), Class: mg.ErrorProvider4xx, Retryable: false}
			}
			body["tools"] = tools
			retryRaw, marshalErr := json.Marshal(body)
			if marshalErr != nil {
				return nil, marshalErr
			}
			if req.Streaming {
				resp, err = a.client.PostChatCompletions(ctx, retryRaw)
			} else {
				resp, err = a.client.PostChatCompletionsJSON(ctx, retryRaw)
			}
			if err != nil {
				return nil, &mg.AdapterError{Message: err.Error(), Class: mg.ErrorNetwork, Retryable: true}
			}
			if resp.StatusCode == http.StatusOK {
				a.rememberToolSchemaProfile(req.Model, toolSchemaProfileRestricted)
				restrictedSchema = true
				negotiatesToolSchema = false
			} else {
				retryBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
				_ = resp.Body.Close()
				return nil, openAIHTTPError(resp.StatusCode, retryBody)
			}
		} else {
			return nil, openAIHTTPError(resp.StatusCode, errBody)
		}
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		// 429 / 5xx 视为可重试(触发 fallback);其余 4xx 不重试。
		return nil, openAIHTTPError(resp.StatusCode, errBody)
	}
	if !req.Streaming {
		// 非流式:整段 JSON 一次读完并归一化成 chunk 队列(token[+thought/tool_call]
		// + usage),复用与 SSE 路径相同的 Chunk/ModelUsage 类型,下游计费一致。
		stream, perr := newNonStream(resp.Body)
		if perr != nil {
			return nil, &mg.AdapterError{Message: perr.Error(), Class: mg.ErrorProvider5xx, Retryable: false}
		}
		if negotiatesToolSchema {
			return newToolSchemaNegotiationStream(stream, a.restrictedRetry(req, body), func() {
				a.rememberToolSchemaProfile(req.Model, toolSchemaProfileNative)
			}), nil
		}
		return stream, nil
	}
	stream := newStream(resp.Body)
	if negotiatesToolSchema {
		return newToolSchemaNegotiationStream(stream, a.restrictedRetry(req, body), func() {
			a.rememberToolSchemaProfile(req.Model, toolSchemaProfileNative)
		}), nil
	}
	return stream, nil
}

func (a *Adapter) toolSchemaProfile(model string) toolSchemaProfile {
	if a == nil {
		return toolSchemaProfileUnknown
	}
	a.schemaProfileMu.RLock()
	profile := a.schemaProfiles[strings.TrimSpace(model)]
	a.schemaProfileMu.RUnlock()
	return profile
}

func (a *Adapter) rememberToolSchemaProfile(model string, profile toolSchemaProfile) {
	if a == nil {
		return
	}
	model = strings.TrimSpace(model)
	a.schemaProfileMu.Lock()
	if a.schemaProfiles == nil {
		a.schemaProfiles = map[string]toolSchemaProfile{}
	}
	a.schemaProfiles[model] = profile
	a.schemaProfileMu.Unlock()
}

func (a *Adapter) restrictedRetry(req mg.AdapterRequest, body map[string]any) func(context.Context) (mg.AdapterStream, error) {
	return func(ctx context.Context) (mg.AdapterStream, error) {
		tools, err := openAIToolsWithCompiler(req.ToolsSchema, openAIToolCompiler(true))
		if err != nil {
			return nil, &mg.AdapterError{Message: err.Error(), Class: mg.ErrorProvider4xx, Retryable: false}
		}
		retryBody := make(map[string]any, len(body))
		for key, value := range body {
			retryBody[key] = value
		}
		retryBody["tools"] = tools
		raw, err := json.Marshal(retryBody)
		if err != nil {
			return nil, err
		}
		var resp *http.Response
		if req.Streaming {
			resp, err = a.client.PostChatCompletions(ctx, raw)
		} else {
			resp, err = a.client.PostChatCompletionsJSON(ctx, raw)
		}
		if err != nil {
			return nil, &mg.AdapterError{Message: err.Error(), Class: mg.ErrorNetwork, Retryable: true}
		}
		if resp.StatusCode != http.StatusOK {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			_ = resp.Body.Close()
			return nil, openAIHTTPError(resp.StatusCode, errBody)
		}
		a.rememberToolSchemaProfile(req.Model, toolSchemaProfileRestricted)
		if req.Streaming {
			return newStream(resp.Body), nil
		}
		return newNonStream(resp.Body)
	}
}

func openAIToolCompiler(restricted bool) mg.ToolSchemaCompiler {
	if restricted {
		return NewToolSchemaCompiler()
	}
	return mg.NewIdentityToolSchemaCompiler("openai")
}

func isUnsupportedToolSchemaResponse(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	return isUnsupportedToolSchemaPayload(body)
}

func isUnsupportedToolSchemaPayload(body []byte) bool {
	message := strings.ToLower(string(body))
	toolSchemaError := strings.Contains(message, "invalid_function_parameters") ||
		strings.Contains(message, "invalid schema for function") ||
		strings.Contains(message, "function.parameters")
	unsupportedKeyword := strings.Contains(message, "oneof") || strings.Contains(message, "one_of") ||
		strings.Contains(message, "anyof") || strings.Contains(message, "allof") ||
		strings.Contains(message, "top level")
	return toolSchemaError && unsupportedKeyword
}

func canonicalToolsContainRootUnion(raw json.RawMessage) bool {
	var tools []canonicalToolDefinition
	if json.Unmarshal(raw, &tools) != nil {
		return false
	}
	for _, tool := range tools {
		var schema map[string]any
		if json.Unmarshal(tool.Schema, &schema) == nil && schema["oneOf"] != nil {
			return true
		}
	}
	return false
}

func openAIHTTPError(status int, body []byte) *mg.AdapterError {
	retryable := status == http.StatusTooManyRequests || status >= 500
	class := mg.ErrorProvider4xx
	if status == http.StatusTooManyRequests {
		class = mg.ErrorRateLimited
	} else if status >= 500 {
		class = mg.ErrorProvider5xx
	}
	return &mg.AdapterError{
		Message: fmt.Sprintf("openai gateway http %d: %s", status, string(body)),
		Class:   class, HTTPStatus: status, Retryable: retryable,
	}
}

type openAIChatMessage struct {
	Role             string            `json:"role"`
	Content          any               `json:"content,omitempty"`
	Name             string            `json:"name,omitempty"`
	ToolCallID       string            `json:"tool_call_id,omitempty"`
	ToolCalls        []mg.ChatToolCall `json:"tool_calls,omitempty"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
}

// openAIMessages 把网关消息投影为 OpenAI 兼容线上形状。imageDetail 非空时
// 逐 part 重建 ImageURL 并写入 detail（G-B：不原地改写共享指针，避免污染
// 上游消息快照）。
func openAIMessages(messages []mg.ChatMessage, imageDetail string) []openAIChatMessage {
	out := make([]openAIChatMessage, 0, len(messages))
	for _, message := range messages {
		converted := openAIChatMessage{
			Role:             message.Role,
			Name:             message.Name,
			ToolCallID:       message.ToolCallID,
			ToolCalls:        append([]mg.ChatToolCall(nil), message.ToolCalls...),
			ReasoningContent: message.ReasoningContent,
		}
		switch {
		case len(message.Parts) > 0:
			parts := append([]mg.ContentPart(nil), message.Parts...)
			if imageDetail != "" {
				for i, part := range parts {
					if part.Type == "image_url" && part.ImageURL != nil {
						detailed := *part.ImageURL
						detailed.Detail = imageDetail
						parts[i].ImageURL = &detailed
					}
				}
			}
			converted.Content = parts
		case message.Content != "":
			converted.Content = message.Content
		case message.Role == "tool":
			converted.Content = ""
		}
		out = append(out, converted)
	}
	return out
}

type canonicalToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema"`
}

type openAIToolDefinition struct {
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// NewToolSchemaCompiler exposes the Provider compiler for repository-wide
// catalog conformance tests.
func NewToolSchemaCompiler() mg.ToolSchemaCompiler {
	return mg.NewDiscriminatedObjectUnionToolSchemaCompiler("openai")
}

// openAITools is the provider-boundary translation from the Harness canonical
// tool definition to the OpenAI-compatible function tool wire format.
func openAITools(raw json.RawMessage) ([]openAIToolDefinition, error) {
	return openAIToolsWithCompiler(raw, NewToolSchemaCompiler())
}

func openAIToolsWithCompiler(raw json.RawMessage, compiler mg.ToolSchemaCompiler) ([]openAIToolDefinition, error) {
	var canonical []canonicalToolDefinition
	if err := json.Unmarshal(raw, &canonical); err != nil {
		return nil, fmt.Errorf("decode canonical tool definitions: %w", err)
	}
	tools := make([]openAIToolDefinition, 0, len(canonical))
	for index, tool := range canonical {
		if tool.Name == "" || len(tool.Schema) == 0 || !json.Valid(tool.Schema) {
			return nil, fmt.Errorf("canonical tool definition %d requires name and valid schema", index)
		}
		compiledSchema, err := compiler.Compile(tool.Schema)
		if err != nil {
			return nil, fmt.Errorf("compile canonical tool definition %d for %s: %w", index, compiler.Provider(), err)
		}
		tools = append(tools, openAIToolDefinition{Type: "function", Function: openAIToolFunction{
			Name: tool.Name, Description: tool.Description, Parameters: compiledSchema,
		}})
	}
	return tools, nil
}
