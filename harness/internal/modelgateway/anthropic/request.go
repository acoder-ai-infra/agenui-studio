package anthropic

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

const defaultMaxTokens = 4096

type requestBody struct {
	Model         string            `json:"model"`
	MaxTokens     int               `json:"max_tokens"`
	Messages      []requestMessage  `json:"messages"`
	System        []contentBlock    `json:"system,omitempty"`
	Tools         []requestTool     `json:"tools,omitempty"`
	Stream        bool              `json:"stream"`
	Temperature   *float64          `json:"temperature,omitempty"`
	TopP          *float64          `json:"top_p,omitempty"`
	StopSequences []string          `json:"stop_sequences,omitempty"`
	ToolChoice    map[string]string `json:"tool_choice,omitempty"`
	Thinking      map[string]any    `json:"thinking,omitempty"`
	OutputConfig  map[string]any    `json:"output_config,omitempty"`
}

type requestMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type         string            `json:"type"`
	Text         string            `json:"text,omitempty"`
	Thinking     string            `json:"thinking,omitempty"`
	Signature    string            `json:"signature,omitempty"`
	ID           string            `json:"id,omitempty"`
	Name         string            `json:"name,omitempty"`
	Input        any               `json:"input,omitempty"`
	ToolUseID    string            `json:"tool_use_id,omitempty"`
	Content      any               `json:"content,omitempty"`
	IsError      bool              `json:"is_error,omitempty"`
	Source       *imageSource      `json:"source,omitempty"`
	CacheControl map[string]string `json:"cache_control,omitempty"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type requestTool struct {
	Name         string            `json:"name"`
	Description  string            `json:"description,omitempty"`
	InputSchema  json.RawMessage   `json:"input_schema"`
	CacheControl map[string]string `json:"cache_control,omitempty"`
}

type canonicalTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema"`
}

var anthropicToolSchemaCompiler mg.ToolSchemaCompiler = NewToolSchemaCompiler()

func buildRequest(req mg.AdapterRequest) (requestBody, error) {
	if strings.TrimSpace(req.Model) == "" {
		return requestBody{}, fmt.Errorf("anthropic request requires model")
	}
	body := requestBody{Model: req.Model, MaxTokens: defaultMaxTokens, Stream: req.Streaming}
	if req.Options.MaxTokens != nil {
		if *req.Options.MaxTokens <= 0 {
			return requestBody{}, fmt.Errorf("anthropic max_tokens must be positive")
		}
		body.MaxTokens = *req.Options.MaxTokens
	}
	if req.Options.TopP != nil {
		body.TopP = cloneFloat(req.Options.TopP)
	}
	body.StopSequences = append([]string(nil), req.Options.Stop...)

	for _, message := range req.Messages {
		if message.Role == "system" {
			blocks, err := textAndImageBlocks(message, false)
			if err != nil {
				return requestBody{}, fmt.Errorf("anthropic system message: %w", err)
			}
			for _, block := range blocks {
				if block.Type != "text" {
					return requestBody{}, fmt.Errorf("anthropic system message only supports text")
				}
				body.System = append(body.System, block)
			}
			continue
		}
		converted, err := convertMessage(message)
		if err != nil {
			return requestBody{}, err
		}
		body.Messages = append(body.Messages, converted)
	}
	if len(body.Messages) == 0 {
		return requestBody{}, fmt.Errorf("anthropic request requires at least one non-system message")
	}

	tools, err := convertTools(req.ToolsSchema)
	if err != nil {
		return requestBody{}, err
	}
	body.Tools = tools
	if len(body.Tools) > 0 {
		body.Tools[len(body.Tools)-1].CacheControl = ephemeralCacheControl()
	}
	if len(body.System) > 0 {
		body.System[len(body.System)-1].CacheControl = ephemeralCacheControl()
	}
	if len(body.Messages) >= 2 {
		historical := &body.Messages[len(body.Messages)-2]
		if len(historical.Content) > 0 {
			historical.Content[len(historical.Content)-1].CacheControl = ephemeralCacheControl()
		}
	}

	toolChoice, forced, err := convertToolChoice(req.Options.ToolChoice, len(body.Tools) > 0)
	if err != nil {
		return requestBody{}, err
	}
	body.ToolChoice = toolChoice
	if err := applyThinking(&body, req, forced); err != nil {
		return requestBody{}, err
	}
	return body, nil
}

func convertMessage(message mg.ChatMessage) (requestMessage, error) {
	switch message.Role {
	case "tool":
		if message.ToolCallID == "" {
			return requestMessage{}, fmt.Errorf("anthropic tool message requires tool_call_id")
		}
		return requestMessage{Role: "user", Content: []contentBlock{{
			Type: "tool_result", ToolUseID: message.ToolCallID, Content: message.Content,
		}}}, nil
	case "user", "assistant":
	default:
		return requestMessage{}, fmt.Errorf("anthropic unsupported message role %q", message.Role)
	}

	blocks := make([]contentBlock, 0, len(message.Parts)+len(message.ToolCalls)+2)
	if message.Role == "assistant" && message.ReasoningContent != "" {
		if len(message.ToolCalls) > 0 && message.ReasoningSignature == "" {
			return requestMessage{}, fmt.Errorf("anthropic assistant reasoning with tool calls requires reasoning signature")
		}
		blocks = append(blocks, contentBlock{Type: "thinking", Thinking: message.ReasoningContent, Signature: message.ReasoningSignature})
	}
	content, err := textAndImageBlocks(message, message.Role == "user")
	if err != nil {
		return requestMessage{}, err
	}
	blocks = append(blocks, content...)
	if message.Role == "assistant" {
		for _, call := range message.ToolCalls {
			if call.ID == "" || call.Function.Name == "" {
				return requestMessage{}, fmt.Errorf("anthropic assistant tool call requires id and name")
			}
			var input any
			if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
				return requestMessage{}, fmt.Errorf("anthropic tool call %q has invalid JSON arguments: %w", call.Function.Name, err)
			}
			if _, ok := input.(map[string]any); !ok {
				return requestMessage{}, fmt.Errorf("anthropic tool call %q arguments must be a JSON object", call.Function.Name)
			}
			blocks = append(blocks, contentBlock{Type: "tool_use", ID: call.ID, Name: call.Function.Name, Input: input})
		}
	}
	if len(blocks) == 0 {
		return requestMessage{}, fmt.Errorf("anthropic %s message requires content", message.Role)
	}
	return requestMessage{Role: message.Role, Content: blocks}, nil
}

func textAndImageBlocks(message mg.ChatMessage, allowImages bool) ([]contentBlock, error) {
	if len(message.Parts) == 0 {
		if message.Content == "" {
			return nil, nil
		}
		return []contentBlock{{Type: "text", Text: message.Content}}, nil
	}
	blocks := make([]contentBlock, 0, len(message.Parts))
	for _, part := range message.Parts {
		switch part.Type {
		case "text":
			if part.Text != "" {
				blocks = append(blocks, contentBlock{Type: "text", Text: part.Text})
			}
		case "image_url":
			if !allowImages || part.ImageURL == nil || part.ImageURL.URL == "" {
				return nil, fmt.Errorf("anthropic image part requires a user message and image URL")
			}
			source, err := convertImageSource(part.ImageURL.URL)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, contentBlock{Type: "image", Source: &source})
		default:
			return nil, fmt.Errorf("anthropic unsupported content part type %q", part.Type)
		}
	}
	return blocks, nil
}

func convertImageSource(raw string) (imageSource, error) {
	if strings.HasPrefix(raw, "data:") {
		meta, data, found := strings.Cut(strings.TrimPrefix(raw, "data:"), ",")
		if !found || !strings.HasSuffix(meta, ";base64") || data == "" {
			return imageSource{}, fmt.Errorf("anthropic image data URL must be base64 encoded")
		}
		return imageSource{Type: "base64", MediaType: strings.TrimSuffix(meta, ";base64"), Data: data}, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return imageSource{}, fmt.Errorf("anthropic image URL must use http, https or a base64 data URL")
	}
	return imageSource{Type: "url", URL: raw}, nil
}

func convertTools(raw json.RawMessage) ([]requestTool, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var canonical []canonicalTool
	if err := json.Unmarshal(raw, &canonical); err != nil {
		return nil, fmt.Errorf("decode canonical tool definitions: %w", err)
	}
	tools := make([]requestTool, 0, len(canonical))
	for i, tool := range canonical {
		if tool.Name == "" || len(tool.Schema) == 0 || !json.Valid(tool.Schema) {
			return nil, fmt.Errorf("canonical tool definition %d requires name and valid schema", i)
		}
		normalizedSchema, err := anthropicToolSchemaCompiler.Compile(tool.Schema)
		if err != nil {
			return nil, fmt.Errorf("compile canonical tool definition %d for %s: %w", i, anthropicToolSchemaCompiler.Provider(), err)
		}
		tools = append(tools, requestTool{Name: tool.Name, Description: tool.Description, InputSchema: normalizedSchema})
	}
	return tools, nil
}

func convertToolChoice(choice string, hasTools bool) (map[string]string, bool, error) {
	if choice == "" {
		return nil, false, nil
	}
	if !hasTools {
		return nil, false, fmt.Errorf("anthropic tool_choice requires tools")
	}
	switch choice {
	case "auto":
		return map[string]string{"type": "auto"}, false, nil
	case "none", "forbidden":
		return map[string]string{"type": "none"}, false, nil
	case "required", "any", "forced":
		return map[string]string{"type": "any"}, true, nil
	default:
		return nil, false, fmt.Errorf("anthropic unsupported tool_choice %q", choice)
	}
}

func applyThinking(body *requestBody, req mg.AdapterRequest, forcedTool bool) error {
	mode := req.Options.ReasoningMode
	if (mode == "" || mode == mg.ReasoningAuto) && req.Options.ReasoningBudget > 0 {
		mode = mg.ReasoningEnabled
	}
	if req.Options.ReasoningEffort != "" && mode != mg.ReasoningEnabled {
		return fmt.Errorf("anthropic reasoning_effort requires reasoning_mode enabled")
	}
	if forcedTool {
		mode = mg.ReasoningDisabled
	}
	switch mode {
	case "", mg.ReasoningAuto:
		body.Temperature = cloneFloat(req.Options.Temperature)
	case mg.ReasoningDisabled:
		body.Thinking = map[string]any{"type": "disabled"}
		if !isOpus4(req.Model) {
			body.Temperature = cloneFloat(req.Options.Temperature)
		}
	case mg.ReasoningEnabled:
		if isOpus4(req.Model) {
			body.Thinking = map[string]any{"type": "adaptive"}
			if req.Options.ReasoningEffort != "" {
				body.OutputConfig = map[string]any{"effort": req.Options.ReasoningEffort}
			}
			return nil
		}
		budget := req.Options.ReasoningBudget
		if budget == 0 {
			budget = 10240
		}
		if budget < 1024 || budget >= body.MaxTokens {
			return fmt.Errorf("anthropic reasoning budget must be at least 1024 and lower than max_tokens")
		}
		body.Thinking = map[string]any{"type": "enabled", "budget_tokens": budget}
		if req.Options.Temperature != nil {
			temperature := 1.0
			body.Temperature = &temperature
		}
	default:
		return fmt.Errorf("anthropic unsupported reasoning mode %q", mode)
	}
	return nil
}

func isOpus4(model string) bool {
	return strings.Contains(strings.ToLower(model), "opus-4")
}

func cloneFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func ephemeralCacheControl() map[string]string {
	return map[string]string{"type": "ephemeral"}
}
