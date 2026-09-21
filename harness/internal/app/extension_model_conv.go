package app

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
)

// extension_model_conv.go 承载 BeforeModelHook 的消息 / 工具 / 参数在
// eino 运行态、native ModelCall 视图与公开 extension 视图之间的双向转换，
// 以及工具可见集收窄的确定性算法。

// ── ScopedData 只读视图转换 ────────────────────────────────

// scopedDataToExtensionEntries 把 Runtime 的 Run 级 ScopedData 快照投影为
// 公开扩展视图（与 InputNormalizer / ContextContributor 收到的冻结视图同形）。
// Agents 作用域不展开：它由 agentgateway 投影给子 agent，不属于本轮 Hook 视图。
func scopedDataToExtensionEntries(data agentruntime.ScopedData) map[string]extension.ScopedDataEntry {
	if len(data.Run) == 0 {
		return nil
	}
	out := make(map[string]extension.ScopedDataEntry, len(data.Run))
	for key, item := range data.Run {
		out[key] = extension.ScopedDataEntry{
			Source:     item.Source,
			Visibility: item.Visibility,
			Value:      append(json.RawMessage(nil), item.Value...),
			Ref:        item.Ref,
			Hash:       item.Hash,
		}
	}
	return out
}

// cloneExtensionScopedData 为单次 Hook 调用复制一份独立视图，使实现无法
// 通过改写入参影响共享快照（与 extension.Context.Config 的深拷贝立场一致）。
func cloneExtensionScopedData(in map[string]extension.ScopedDataEntry) map[string]extension.ScopedDataEntry {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]extension.ScopedDataEntry, len(in))
	for key, entry := range in {
		entry.Value = append(json.RawMessage(nil), entry.Value...)
		out[key] = entry
	}
	return out
}

// ── 内容 Part 与 Hook 视图的保真互转 ─────────────────────────

// hookPartURL 把内联字节与 URL 归一为 Hook 视图的单一定位符：URL 优先；
// 否则把 Base64Data 编码为 data URI，使内联图片/文件（帧、附件）在 Hook
// 改写往返中不丢内容（G-C）。
func hookPartURL(url, base64Data, mime, fallbackMIME string) string {
	if url != "" || base64Data == "" {
		return url
	}
	if mime == "" {
		mime = fallbackMIME
	}
	return "data:" + mime + ";base64," + base64Data
}

// hookPartPayload 是 hookPartURL 的逆操作：data URI 拆回（URL=""，
// Base64Data+MIME）；其余定位符原样保留。
func hookPartPayload(url, mime string) (outURL, base64Data, outMIME string) {
	if !strings.HasPrefix(url, "data:") {
		return url, "", mime
	}
	meta, data, ok := strings.Cut(strings.TrimPrefix(url, "data:"), ",")
	if !ok || !strings.HasSuffix(meta, ";base64") {
		return url, "", mime
	}
	parsedMIME := strings.TrimSuffix(meta, ";base64")
	if parsedMIME == "" {
		parsedMIME = mime
	}
	return "", data, parsedMIME
}

// ── 消息转换（eino 路径） ──────────────────────────────────────────────

// beforeModelMessagesFromEino 把 eino 运行态消息投影为 Hook 可见的塑形视图。
func beforeModelMessagesFromEino(messages []*schema.Message) []extension.BeforeModelMessage {
	out := make([]extension.BeforeModelMessage, 0, len(messages))
	for _, message := range messages {
		if message == nil {
			continue
		}
		converted := extension.BeforeModelMessage{
			Role:       string(message.Role),
			Content:    message.Content,
			ToolCallID: message.ToolCallID,
			ToolName:   message.ToolName,
		}
		for _, call := range message.ToolCalls {
			converted.ToolCalls = append(converted.ToolCalls, extension.BeforeModelToolCall{
				ID: call.ID, Name: call.Function.Name, Arguments: json.RawMessage(call.Function.Arguments),
			})
		}
		for _, part := range message.UserInputMultiContent {
			tp := extension.BeforeModelPart{Type: string(part.Type), Text: part.Text}
			switch {
			case part.Image != nil:
				tp.MIME = part.Image.MIMEType
				url, base64Data := "", ""
				if part.Image.URL != nil {
					url = *part.Image.URL
				}
				if part.Image.Base64Data != nil {
					base64Data = *part.Image.Base64Data
				}
				tp.URL = hookPartURL(url, base64Data, tp.MIME, "image/png")
			case part.File != nil:
				// file part（G-C）投影为 Hook 可见的 file 类型，内联字节
				// 编码为 data URI。
				tp.Type = "file"
				tp.MIME = part.File.MIMEType
				tp.Filename = part.File.Name
				url, base64Data := "", ""
				if part.File.URL != nil {
					url = *part.File.URL
				}
				if part.File.Base64Data != nil {
					base64Data = *part.File.Base64Data
				}
				tp.URL = hookPartURL(url, base64Data, tp.MIME, "application/octet-stream")
			}
			converted.Parts = append(converted.Parts, tp)
		}
		out = append(out, converted)
	}
	return out
}

// einoMessagesFromBeforeModel 把塑形后的视图还原为 eino 消息。工具协议
// 配对字段（ToolCalls/ToolCallID）原样还原。
func einoMessagesFromBeforeModel(messages []extension.BeforeModelMessage) ([]*schema.Message, error) {
	out := make([]*schema.Message, 0, len(messages))
	for i, message := range messages {
		switch schema.RoleType(message.Role) {
		case schema.System, schema.User, schema.Assistant, schema.Tool:
		default:
			return nil, fmt.Errorf("before model hook message[%d] has invalid role %q", i, message.Role)
		}
		converted := &schema.Message{
			Role:       schema.RoleType(message.Role),
			Content:    message.Content,
			ToolCallID: message.ToolCallID,
			ToolName:   message.ToolName,
		}
		for _, call := range message.ToolCalls {
			converted.ToolCalls = append(converted.ToolCalls, schema.ToolCall{
				ID: call.ID, Type: "function",
				Function: schema.FunctionCall{Name: call.Name, Arguments: string(call.Arguments)},
			})
		}
		for _, part := range message.Parts {
			switch part.Type {
			case "image_url":
				image := &schema.MessageInputImage{}
				url, base64Data, mime := hookPartPayload(part.URL, part.MIME)
				image.MIMEType = mime
				if url != "" {
					value := url
					image.URL = &value
				}
				if base64Data != "" {
					value := base64Data
					image.Base64Data = &value
				}
				converted.UserInputMultiContent = append(converted.UserInputMultiContent, schema.MessageInputPart{
					Type: schema.ChatMessagePartTypeImageURL, Image: image,
				})
			case "file":
				// file part（G-C）还原为 eino file part；data URI 拆回内联字节。
				url, base64Data, mime := hookPartPayload(part.URL, part.MIME)
				file := &schema.MessageInputFile{Name: part.Filename}
				file.MIMEType = mime
				if url != "" {
					value := url
					file.URL = &value
				}
				if base64Data != "" {
					value := base64Data
					file.Base64Data = &value
				}
				converted.UserInputMultiContent = append(converted.UserInputMultiContent, schema.MessageInputPart{
					Type: schema.ChatMessagePartTypeFileURL, File: file,
				})
			default:
				converted.UserInputMultiContent = append(converted.UserInputMultiContent, schema.MessageInputPart{
					Type: schema.ChatMessagePartTypeText, Text: part.Text,
				})
			}
		}
		if len(converted.UserInputMultiContent) > 0 {
			converted.Content = ""
		}
		out = append(out, converted)
	}
	return out, nil
}

// ── 消息转换（native 路径） ────────────────────────────────────────────

// beforeModelMessagesFromModelCall 把 native 路径的 ModelCallMessage 投影为
// Hook 可见的塑形视图。
func beforeModelMessagesFromModelCall(messages []agentruntime.ModelCallMessage) []extension.BeforeModelMessage {
	out := make([]extension.BeforeModelMessage, 0, len(messages))
	for _, message := range messages {
		converted := extension.BeforeModelMessage{
			Role:       message.Role,
			Content:    message.Content,
			ToolCallID: message.ToolCallID,
			ToolName:   message.ToolName,
		}
		for _, call := range message.ToolCalls {
			converted.ToolCalls = append(converted.ToolCalls, extension.BeforeModelToolCall{
				ID: call.ToolCallID, Name: call.Name, Arguments: call.Arguments,
			})
		}
		for _, part := range message.ContentParts {
			converted.Parts = append(converted.Parts, extension.BeforeModelPart{
				Type: part.Type, Text: part.Text, MIME: part.MIMEType, Filename: part.Name,
				// 内联字节编码为 data URI，保证 Hook 往返不丢内容（G-C）。
				URL: hookPartURL(part.URL, part.Base64Data, part.MIMEType, "application/octet-stream"),
			})
		}
		out = append(out, converted)
	}
	return out
}

// modelCallMessagesFromBeforeModel 把塑形后的视图还原为 ModelCallMessage。
func modelCallMessagesFromBeforeModel(messages []extension.BeforeModelMessage) []agentruntime.ModelCallMessage {
	out := make([]agentruntime.ModelCallMessage, 0, len(messages))
	for _, message := range messages {
		converted := agentruntime.ModelCallMessage{
			Role:       message.Role,
			Content:    message.Content,
			ToolCallID: message.ToolCallID,
			ToolName:   message.ToolName,
		}
		for _, call := range message.ToolCalls {
			converted.ToolCalls = append(converted.ToolCalls, agentruntime.ModelToolCall{
				ToolCallID: call.ID, Name: call.Name, Arguments: call.Arguments,
			})
		}
		for _, part := range message.Parts {
			url, base64Data, mime := hookPartPayload(part.URL, part.MIME)
			converted.ContentParts = append(converted.ContentParts, agentruntime.ModelContentPart{
				Type: part.Type, Text: part.Text, URL: url, Base64Data: base64Data,
				MIMEType: mime, Name: part.Filename,
			})
		}
		out = append(out, converted)
	}
	return out
}

// ── 工具描述符转换与可见集收窄 ─────────────────────────────────────────

// toolDefinitionsFromEino 把 eino ToolInfo 列表投影为 Hook 可见的只读描述符。
func toolDefinitionsFromEino(tools []*schema.ToolInfo) []extension.ToolDefinition {
	out := make([]extension.ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		out = append(out, extension.ToolDefinition{Name: tool.Name, Description: tool.Desc})
	}
	return out
}

// toolDefinitionsFromModelCall 把 native ModelToolDefinition 投影为 Hook 可见
// 的只读描述符（携带 schema 摘要）。
func toolDefinitionsFromModelCall(tools []agentruntime.ModelToolDefinition) []extension.ToolDefinition {
	out := make([]extension.ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		out = append(out, extension.ToolDefinition{
			Name: tool.Name, Description: tool.Description,
			Schema: append(json.RawMessage(nil), tool.Schema...),
		})
	}
	return out
}

// intersectToolDefinitions 按 Hook 返回的工具集从候选集合中确定性地选择
// 可见子集：nil selection 表示不收窄（返回全部候选）；非 nil 时只保留
// 存在于候选集合中的工具（按 selection 顺序），未知名收集到 dropped 用于
// 审计留痕。
func intersectToolDefinitions(candidate, selection []extension.ToolDefinition) (visible []extension.ToolDefinition, dropped []string) {
	if selection == nil {
		return candidate, nil
	}
	byName := make(map[string]extension.ToolDefinition, len(candidate))
	for _, tool := range candidate {
		byName[tool.Name] = tool
	}
	seen := make(map[string]struct{}, len(selection))
	visible = make([]extension.ToolDefinition, 0, len(selection))
	for _, tool := range selection {
		if _, duplicate := seen[tool.Name]; duplicate {
			continue
		}
		if def, ok := byName[tool.Name]; ok {
			visible = append(visible, def)
			seen[tool.Name] = struct{}{}
			continue
		}
		dropped = append(dropped, tool.Name)
	}
	return visible, dropped
}

// einoToolInfosFromDefinitions 依据可见集从原始 ToolInfo 列表挑选并重排：
// 保留完整 schema/params（可见集只携带名称/描述），按可见集顺序还原。
func einoToolInfosFromDefinitions(original []*schema.ToolInfo, visible []extension.ToolDefinition) []*schema.ToolInfo {
	byName := make(map[string]*schema.ToolInfo, len(original))
	for _, tool := range original {
		if tool != nil {
			byName[tool.Name] = tool
		}
	}
	out := make([]*schema.ToolInfo, 0, len(visible))
	for _, def := range visible {
		if info, ok := byName[def.Name]; ok {
			out = append(out, info)
		}
	}
	return out
}

// modelToolDefinitionsFromCandidate 依据可见集从原始 native 工具定义挑选并
// 重排：保留完整 schema，按可见集顺序还原。
func modelToolDefinitionsFromCandidate(original []agentruntime.ModelToolDefinition, visible []extension.ToolDefinition) []agentruntime.ModelToolDefinition {
	byName := make(map[string]agentruntime.ModelToolDefinition, len(original))
	for _, tool := range original {
		byName[tool.Name] = tool
	}
	out := make([]agentruntime.ModelToolDefinition, 0, len(visible))
	for _, def := range visible {
		if tool, ok := byName[def.Name]; ok {
			out = append(out, tool)
		}
	}
	return out
}

// validateToolChoiceVisible 校验 ToolChoice 指定的具体工具名必须属于本轮
// 可见集；auto/required/none/空为保留值不校验。违规 fail closed（ADR-007）。
func validateToolChoiceVisible(choice string, visible []extension.ToolDefinition) error {
	switch choice {
	case "", "auto", "required", "none":
		return nil
	}
	for _, tool := range visible {
		if tool.Name == choice {
			return nil
		}
	}
	return fmt.Errorf("tool_choice %q is not in the visible tool set for this round", choice)
}

// toolNames 提取工具描述符的名称列表（留痕用）。
func toolNames(tools []extension.ToolDefinition) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}
