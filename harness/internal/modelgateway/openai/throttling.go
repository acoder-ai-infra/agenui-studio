package openai

import "strings"

// throttlingSignal 判断一个已解析的 SSE chunk 是否携带内嵌限流信号。
//
// 关键约束:只检查结构化错误字段(顶层 error 对象、code、message),绝不检查
// delta.content / reasoning_content —— 模型正文里可能合法地出现 "429"、
// "rate limit"、"tpm" 等字样(例如在解释 HTTP 状态码或限流概念),若扫描正文会
// 把正常回答误判为限流并错误触发 fallback/失败(design §4.3/§9, spec §2/§8)。
//
// 部分 OpenAI 兼容网关把限流以结构化对象内嵌进流而非 HTTP 429,因此仍需在错误
// 字段上识别。返回命中的错误文本供上层填充 AdapterError.Message。
func throttlingSignal(parsed sseChunk) (string, bool) {
	if parsed.Error != nil {
		if txt := parsed.Error.Message; looksThrottled(txt) || looksThrottled(parsed.Error.Code) || looksThrottled(parsed.Error.Type) {
			if txt == "" {
				txt = parsed.Error.Code
			}
			return txt, true
		}
	}
	// 少数网关不含 error 对象,直接在顶层放 code/message。仅当没有正常 delta 内容时
	// 才把顶层 message 当作错误信号,避免与正文字段混淆。
	if !hasContent(parsed) {
		if looksThrottled(parsed.Code) {
			msg := parsed.Message
			if msg == "" {
				msg = parsed.Code
			}
			return msg, true
		}
		if looksThrottled(parsed.Message) {
			return parsed.Message, true
		}
	}
	return "", false
}

// hasContent 报告该 chunk 是否携带模型正文(token/reasoning/tool_call)。用于避免把
// 承载正文的 chunk 顶层字段误当作错误信号。
func hasContent(parsed sseChunk) bool {
	for _, choice := range parsed.Choices {
		if choice.Delta.Content != "" || choice.Delta.ReasoningContent != "" || len(choice.Delta.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// looksThrottled 在给定的错误字段文本上做限流关键字匹配。
func looksThrottled(text string) bool {
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	return strings.Contains(lower, "throttling") ||
		strings.Contains(lower, "throttle") ||
		strings.Contains(lower, "tpm") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "ratelimit") ||
		strings.Contains(lower, "rate_limit") ||
		strings.Contains(lower, "429")
}
