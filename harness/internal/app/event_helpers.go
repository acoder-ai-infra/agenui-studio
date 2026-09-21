package app

import (
	"encoding/json"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// hotDeltaContent 提取一条逐字增量的文本载荷(优先 payload,回退 payload_preview)。
// 供 formal composition 的 HotBuffer 写入使用。
func hotDeltaContent(ev observability.AgentEvent) []byte {
	if text := eventText(ev.Payload); text != "" {
		return []byte(text)
	}
	if text := eventText(ev.PayloadPreview); text != "" {
		return []byte(text)
	}
	return nil
}

func eventText(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var p struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(payload, &p)
	return p.Text
}
