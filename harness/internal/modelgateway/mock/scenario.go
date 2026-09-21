package mock

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

// ScenarioProvider is a deterministic local/CI provider. Markers are explicit
// so ordinary prompts keep normal mock behavior and fault injection can never
// be triggered accidentally in a real provider adapter.
type ScenarioProvider struct {
	id    string
	mu    sync.Mutex
	calls int
}

func NewScenario(id string) *ScenarioProvider {
	if id == "" {
		id = "scenario_mock"
	}
	return &ScenarioProvider{id: id}
}

func (p *ScenarioProvider) ID() string { return p.id }

func (p *ScenarioProvider) Calls() int { p.mu.Lock(); defer p.mu.Unlock(); return p.calls }

func (p *ScenarioProvider) InvokeChat(_ context.Context, req mg.AdapterRequest) (mg.AdapterStream, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	for _, message := range req.Messages {
		if message.Role == "tool" {
			content := message.Content
			if len(content) > 256 {
				content = content[:256]
			}
			return scenarioChunks(0, TokenChunk("tool completed: "+content), UsageChunk(32, 16)), nil
		}
	}
	prompt := lastUserContent(req.Messages)
	if strings.Contains(prompt, "[[harness:model:error:before]]") {
		return nil, errors.New("scenario model invoke failure")
	}
	if strings.Contains(prompt, "[[harness:model:error:stream]]") {
		return scenarioChunks(0, TokenChunk("partial"), ErrorChunk("scenario stream failure", false)), nil
	}
	if value, ok := markerInt(prompt, "[[harness:model:slow:"); ok {
		return scenarioChunks(time.Duration(value)*time.Millisecond, TokenChunk("slow-1"), TokenChunk("slow-2"), UsageChunk(10, 2)), nil
	}
	if value, ok := markerInt(prompt, "[[harness:model:large:"); ok {
		if value > 1<<20 {
			value = 1 << 20
		}
		return scenarioChunks(0, TokenChunk(strings.Repeat("m", value)), UsageChunk(10, value/4)), nil
	}
	if strings.Contains(prompt, "[[harness:tool:ask_user]]") {
		// HITL 链路 (a)：驱动 ask_user 内置工具触发中断 → control_request →
		// Resume 定向恢复（方案 4.1）。
		return scenarioChunks(0, ToolCallChunk(
			0, "tc_ask_user", "ask_user",
			`{"questions":[{"header":"确认","question":"是否继续？","options":[{"label":"继续","description":"继续执行"},{"label":"取消","description":"停止"}]}]}`,
		), UsageChunk(16, 4)), nil
	}
	if strings.Contains(prompt, "[[harness:tool:approval]]") {
		// HITL 链路 (b)：驱动 request_approval 高风险审批中断（方案 4.1）。
		return scenarioChunks(0, ToolCallChunk(
			0, "tc_approval", "request_approval",
			`{"action":"delete staging dataset","reason":"cleanup"}`,
		), UsageChunk(16, 4)), nil
	}
	if strings.Contains(prompt, "[[harness:child:ask_user]]") {
		// Drive a child agent to ask for input so the parent-control promotion
		// and resume routing contract can be exercised without product fixtures.
		return scenarioChunks(0, ToolCallChunk(
			0, "tc_subagent_child_askuser", "task",
			`{"subagent_type":"control_child","description":"[[harness:tool:ask_user]] Please confirm the next step"}`,
		), UsageChunk(16, 4)), nil
	}
	if strings.Contains(prompt, "[[harness:subagent:control_child]]") {
		// Drive a generic child task for the host-extension contract tests.
		return scenarioChunks(0, ToolCallChunk(
			0, "tc_subagent_scout", "task",
			`{"subagent_type":"control_child","description":"generic child probe"}`,
		), UsageChunk(16, 4)), nil
	}
	if strings.Contains(prompt, "[[harness:subagent:deep_researcher]]") {
		return scenarioChunks(0, ToolCallChunk(
			0, "tc_subagent_researcher", "task",
			`{"subagent_type":"deep_researcher","description":"return the deterministic child research proof"}`,
		), UsageChunk(16, 4)), nil
	}
	if strings.Contains(prompt, "[[harness:subagent:deep_child_agent]]") {
		return scenarioChunks(0, ToolCallChunk(
			0, "tc_subagent_deep_child", "task",
			`{"subagent_type":"deep_child_agent","description":"return the deterministic DeepAgent child proof"}`,
		), UsageChunk(16, 4)), nil
	}
	if strings.Contains(prompt, "[[harness:tool:host_fn]]") {
		// rc.6 验收：驱动模型调用宿主 ToolProvider 提供实现的 function 工具
		//（定义在 tools.yaml，执行统一走 Tool Gateway）。
		return scenarioChunks(0, ToolCallChunk(0, "tc_host_fn", "e2e_host_lookup", `{"key":"welcome_card"}`), UsageChunk(16, 4)), nil
	}
	if strings.Contains(prompt, "[[harness:tool:host_bad_schema]]") {
		// rc.6 验收：驱动返回违反 output_schema 结果的宿主工具（输出校验在场证明）。
		return scenarioChunks(0, ToolCallChunk(0, "tc_host_bad", "e2e_bad_schema", `{"key":"whatever"}`), UsageChunk(16, 4)), nil
	}
	if strings.Contains(prompt, "[[harness:tool:read_file]]") {
		// Artifact Client 验收：从 prompt 提取 artifact:// 引用并驱动
		// read_file 工具读取宿主上传的 artifact 内容。
		ref := firstArtifactRef(prompt)
		args, _ := json.Marshal(map[string]string{"artifact_ref": ref})
		return scenarioChunks(0, ToolCallChunk(0, "tc_read_file", "read_file", string(args)), UsageChunk(16, 4)), nil
	}
	if strings.Contains(prompt, "[[harness:mcp:echo]]") {
		return scenarioChunks(0, ToolCallChunk(0, "tc_mcp_echo", "harness.mcp_echo", `{"text":"echo-through-mcp"}`), UsageChunk(16, 4)), nil
	}
	return scenarioChunks(0, TokenChunk("你好，"), TokenChunk("我是 scenario mock 模型的流式回复。"), UsageChunk(12, 8)), nil
}

func lastUserContent(messages []mg.ChatMessage) string {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role != "user" {
			continue
		}
		if messages[index].Content != "" {
			return messages[index].Content
		}
		// 多 Part 用户消息：Content 为空、文本落在 Parts 里，marker
		// 匹配需拼接 text part（与 provider 可见形状一致）。
		var b strings.Builder
		for _, part := range messages[index].Parts {
			if part.Type == "text" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(part.Text)
			}
		}
		return b.String()
	}
	return ""
}

// firstArtifactRef 从 prompt 中提取第一个 canonical artifact:// 引用。
func firstArtifactRef(prompt string) string {
	start := strings.Index(prompt, "artifact://")
	if start < 0 {
		return ""
	}
	rest := prompt[start:]
	end := strings.IndexFunc(rest, func(r rune) bool {
		switch r {
		case ' ', '\n', '\t', '"', ']', ')', '，', '。':
			return true
		}
		return false
	})
	if end < 0 {
		return rest
	}
	return rest[:end]
}

func markerInt(prompt, prefix string) (int, bool) {
	start := strings.Index(prompt, prefix)
	if start < 0 {
		return 0, false
	}
	rest := prompt[start+len(prefix):]
	end := strings.Index(rest, "]]")
	if end < 0 {
		return 0, false
	}
	value, err := strconv.Atoi(rest[:end])
	return value, err == nil && value > 0
}

func scenarioChunks(delay time.Duration, chunks ...mg.NormalizedChunk) mg.AdapterStream {
	return &scenarioStream{delay: delay, chunks: chunks}
}

type scenarioStream struct {
	delay  time.Duration
	chunks []mg.NormalizedChunk
	index  int
}

func (s *scenarioStream) Next(ctx context.Context) (mg.NormalizedChunk, error) {
	if s.index >= len(s.chunks) {
		return mg.NormalizedChunk{}, io.EOF
	}
	if s.delay > 0 {
		timer := time.NewTimer(s.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return mg.NormalizedChunk{}, ctx.Err()
		case <-timer.C:
		}
	}
	chunk := s.chunks[s.index]
	s.index++
	return chunk, nil
}
func (*scenarioStream) Close() error { return nil }

var _ mg.ChatProvider = (*ScenarioProvider)(nil)
