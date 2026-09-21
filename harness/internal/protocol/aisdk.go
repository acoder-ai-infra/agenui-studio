package protocol

import (
	"encoding/json"
	"net/http"
)

// Vercel AI SDK v5 "UI Message Stream" 协议适配。
//
// 传输:SSE,每帧 `data: {json}\n\n`,每个 json 带 `type` 字段;流以 `data: [DONE]` 结束。
// 响应头需带 `x-vercel-ai-ui-message-stream: v1`,前端 @ai-sdk/react 的 useChat 据此消费。
//
// 本文件只做"我方 canonical AgentEvent -> v5 part"的映射与写帧;Event Store 仍是事实源,
// v5 流只是又一种端侧投影(与 SSEAdapter 平行)。

// AISDKStreamHeader 是 v5 UI message stream 的标识响应头。
const AISDKStreamHeader = "x-vercel-ai-ui-message-stream"

// AISDKPart 是一个 v5 UI message stream part(序列化为 data 帧)。
type AISDKPart map[string]any

// v5 part 构造器 -----------------------------------------------------------

func AISDKStart(messageID string) AISDKPart {
	p := AISDKPart{"type": "start"}
	if messageID != "" {
		p["messageId"] = messageID
	}
	return p
}

func AISDKStartStep() AISDKPart  { return AISDKPart{"type": "start-step"} }
func AISDKFinishStep() AISDKPart { return AISDKPart{"type": "finish-step"} }
func AISDKFinish() AISDKPart     { return AISDKPart{"type": "finish"} }

func AISDKTextStart(id string) AISDKPart { return AISDKPart{"type": "text-start", "id": id} }
func AISDKTextDelta(id, delta string) AISDKPart {
	return AISDKPart{"type": "text-delta", "id": id, "delta": delta}
}
func AISDKTextEnd(id string) AISDKPart { return AISDKPart{"type": "text-end", "id": id} }

func AISDKReasoningStart(id string) AISDKPart { return AISDKPart{"type": "reasoning-start", "id": id} }
func AISDKReasoningDelta(id, delta string) AISDKPart {
	return AISDKPart{"type": "reasoning-delta", "id": id, "delta": delta}
}
func AISDKReasoningEnd(id string) AISDKPart { return AISDKPart{"type": "reasoning-end", "id": id} }

func AISDKToolInputStart(toolCallID, toolName string) AISDKPart {
	return AISDKPart{"type": "tool-input-start", "toolCallId": toolCallID, "toolName": toolName}
}
func AISDKToolInputAvailable(toolCallID, toolName string, input any) AISDKPart {
	return AISDKPart{"type": "tool-input-available", "toolCallId": toolCallID, "toolName": toolName, "input": input}
}
func AISDKToolOutputAvailable(toolCallID string, output any) AISDKPart {
	return AISDKPart{"type": "tool-output-available", "toolCallId": toolCallID, "output": output}
}

func AISDKError(text string) AISDKPart { return AISDKPart{"type": "error", "errorText": text} }

// AISDKDataPart 是自定义 data-* part(调试/透传我方 debug 事件用)。
func AISDKDataPart(name string, data any) AISDKPart {
	return AISDKPart{"type": "data-" + name, "data": data}
}

// AISDKWriter 把 v5 part 写为 SSE 帧并 flush。
type AISDKWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

// NewAISDKWriter 设置 v5 响应头并返回写入器;若不支持 flush 返回 ok=false。
func NewAISDKWriter(w http.ResponseWriter) (*AISDKWriter, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set(AISDKStreamHeader, "v1")
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return &AISDKWriter{w: w, f: f}, true
}

// Part 写一个 part 帧。
func (a *AISDKWriter) Part(p AISDKPart) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if _, err := a.w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := a.w.Write(b); err != nil {
		return err
	}
	if _, err := a.w.Write([]byte("\n\n")); err != nil {
		return err
	}
	a.f.Flush()
	return nil
}

// Done 写终止帧 `data: [DONE]`。
func (a *AISDKWriter) Done() error {
	if _, err := a.w.Write([]byte("data: [DONE]\n\n")); err != nil {
		return err
	}
	a.f.Flush()
	return nil
}
