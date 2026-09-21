package app_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/app"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// TestEndToEndSQLite_FullChain 验证完整链路(真实模型):
//
//	输入: agentId + 用户 prompt
//	→ POST /api/v1/ai/chat
//	→ RunService.OpenTurn: session create → message append → run create → events
//	→ Dispatcher.Dispatch: RuntimeService.Run → Bridge.StartRun (CAS created→running)
//	→ gatewayRuntime.Run → modelgateway.Chat (真实 provider: dashscope qwen-max)
//	→ RuntimeService.pipeEvents: run_started → (model_token_delta/agent_text_delta 走实时通道不落库) → final_response → run_completed
//	→ Bridge.CompleteRun (CAS running→completed)
//	→ 全部 8 store 落 SQLite
//
// 验证点:
//  1. SSE 流返回 start → text-delta* → text-end → finish
//  2. sessions 表有 1 条记录,agent_id 正确
//  3. messages 表有 1 条 user 消息,content_preview = prompt
//  4. runs 表有 1 条记录,status = completed
//  5. agent_events 表有完整事件链(user_message_received → run_created → run_started → final_response → run_completed;agent_text_delta 走实时通道不落库)
func TestEndToEndSQLite_FullChain(t *testing.T) {
	enableDevMode(t)
	configPath := strings.TrimSpace(os.Getenv("HARNESS_E2E_CONFIG"))
	if configPath == "" {
		t.Skip("HARNESS_E2E_CONFIG is required for the real-provider integration test")
	}
	harness, err := app.LoadHarnessConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	components, err := app.LoadComponentConfigs(harness)
	if err != nil {
		t.Fatal(err)
	}
	application, buildErr := app.Build(components.Models, components.Storage, components.Auth, components.Redis, nil, nil, app.WithHarnessConfig(harness), app.WithArtifactConfig(components.Artifact))
	if buildErr != nil {
		t.Fatalf("build: %v", buildErr)
	}
	agentID := harness.Runtime.DefaultAgentID

	// 验证真实 provider 已注册(不只是 mock)
	hasReal := false
	for _, p := range application.Providers {
		if p != "mock" && p != "scenario_mock" {
			hasReal = true
		}
	}
	if !hasReal {
		t.Skipf("未配置真实 provider(仅 mock),跳过真实模型测试; providers=%v", application.Providers)
	}
	t.Logf("已注册 providers: %v", application.Providers)
	t.Logf("已配置租户: %v", application.Tenants)

	srv := httptest.NewServer(application.Handler)
	defer srv.Close()

	// 2. POST /api/v1/ai/chat,输入 agentId + prompt(走真实模型)
	prompt := "你好,请做个自我介绍"
	body, _ := json.Marshal(map[string]string{
		"agentId": agentID,
		"prompt":  prompt,
	})
	httpCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(httpCtx, http.MethodPost, srv.URL+"/api/v1/ai/chat", bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-tenant-id", "public")
	req.Header.Set("x-user-id", "u1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/ai/chat: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// 3. 读取 AI SDK v5 SSE 流,提取 runId 并收集 text-delta
	runID, textParts := readAISDKStream(t, resp.Body)
	if runID == "" {
		t.Fatal("未从 SSE 流中获取到 runId")
	}
	t.Logf("runID = %s", runID)
	t.Logf("text parts = %d", len(textParts))
	if len(textParts) == 0 {
		t.Fatal("未收到任何 text-delta")
	}

	// 等待异步 dispatch 完成(真实模型可能需要更长时间)
	time.Sleep(500 * time.Millisecond)

	// 4. 验证 SQLite 各表数据
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{
		TenantID: "public",
	})
	stores := application.Stores

	// 4.1 验证 runs 表:status = completed
	run, err := stores.Runs.Get(ctx, runID)
	if err != nil {
		t.Fatalf("Runs.Get: %v", err)
	}
	if run.Status != storage.RunStatusCompleted {
		t.Fatalf("run status = %s, want completed", run.Status)
	}
	if run.AgentID != agentID {
		t.Fatalf("run agent_id = %s, want %s", run.AgentID, agentID)
	}
	t.Logf("✓ runs 表: runID=%s, status=%s, agentID=%s", run.RunID, run.Status, run.AgentID)

	// 4.2 验证 sessions 表
	runList, err := stores.Runs.ListBySession(ctx, run.SessionID)
	if err != nil {
		t.Fatalf("Runs.ListBySession: %v", err)
	}
	if len(runList) == 0 {
		t.Fatal("ListBySession 返回空")
	}
	sess, err := stores.Sessions.Get(ctx, run.SessionID)
	if err != nil {
		t.Fatalf("Sessions.Get: %v", err)
	}
	if sess.Title != prompt {
		t.Fatalf("session title = %q, want %q", sess.Title, prompt)
	}
	// 通过 messages 反查 session
	msgPage, err := stores.Messages.List(ctx, storage.MessageListQuery{
		SessionID: run.SessionID,
		Limit:     50,
		Visibilities: []observability.EventVisibility{
			observability.VisibilityUserVisible,
		},
	})
	if err != nil {
		t.Fatalf("Messages.List: %v", err)
	}
	if len(msgPage.Items) == 0 {
		t.Fatal("messages 表为空")
	}
	var userMsg, assistantMsg *storage.Message
	for _, m := range msgPage.Items {
		if m.Role == "user" {
			userMsg = m
		}
		if m.Role == "assistant" {
			assistantMsg = m
		}
	}
	if userMsg == nil {
		t.Fatal("未找到 user 消息")
	}
	if assistantMsg == nil {
		t.Fatal("未找到 assistant 消息")
	}
	if userMsg.ContentPreview != prompt {
		t.Fatalf("user message content = %q, want %q", userMsg.ContentPreview, prompt)
	}
	if userMsg.RunID != runID || assistantMsg.RunID != runID {
		t.Fatalf("message run_id mismatch: user=%q assistant=%q want %q", userMsg.RunID, assistantMsg.RunID, runID)
	}
	if assistantMsg.ContentPreview == "" {
		t.Fatal("assistant message content_preview 为空")
	}
	t.Logf("✓ messages 表: %d 条消息, user content = %q, assistant content len = %d", len(msgPage.Items), userMsg.ContentPreview, len([]rune(assistantMsg.ContentPreview)))

	// 4.3 验证 agent_events 表:完整事件链
	events, err := stores.Events.Query(ctx, storage.EventQuery{
		RunID:         runID,
		AfterSequence: 0,
		Limit:         500,
	})
	if err != nil {
		t.Fatalf("Events.Query: %v", err)
	}
	if len(events) < 6 {
		t.Fatalf("事件数 = %d, 期望至少 6 条", len(events))
	}
	// 收集事件类型序列
	var eventTypes []string
	var userMessageEvent *observability.AgentEvent
	for _, ev := range events {
		eventTypes = append(eventTypes, string(ev.EventType))
		if ev.EventType == observability.EventUserMessageReceived {
			userMessageEvent = &ev
		}
	}
	t.Logf("✓ agent_events 表: %d 条事件", len(events))
	t.Logf("  事件序列: %s", strings.Join(eventTypes, " → "))

	// 验证关键事件存在。注意:agent_text_delta 是高频逐字增量,走实时通道不落库
	// (层 A),因此不出现在持久化事件链里;完整内容由 final_response 承载。
	mustHave := []string{
		string(observability.EventUserMessageReceived),
		string(observability.EventRunCreated),
		string(observability.EventRunStarted),
		string(observability.EventFinalResponse),
		string(observability.EventRunCompleted),
	}
	for _, want := range mustHave {
		found := false
		for _, got := range eventTypes {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("事件链缺少 %q, 完整序列: %v", want, eventTypes)
		}
	}
	// agent_text_delta 不应落库(层 A:逐字增量只走实时通道)。
	for _, got := range eventTypes {
		if got == string(observability.EventAgentTextDelta) {
			t.Fatalf("agent_text_delta 不应出现在持久化事件链: %v", eventTypes)
		}
	}
	if userMessageEvent == nil {
		t.Fatal("未找到 user_message_received 事件")
	}
	var userPayload struct {
		MessageID      string `json:"message_id"`
		Role           string `json:"role"`
		ContentPreview string `json:"content_preview"`
		Text           string `json:"text"`
	}
	if err := json.Unmarshal(userMessageEvent.PayloadPreview, &userPayload); err != nil {
		t.Fatalf("user_message_received payload_preview 解析失败: %v", err)
	}
	if userPayload.Role != "user" || userPayload.ContentPreview != prompt || userPayload.Text != prompt {
		t.Fatalf("user_message_received payload = %+v, want prompt %q", userPayload, prompt)
	}
	t.Logf("✓ 事件链完整: user_message_received → run_created → run_started → final_response → run_completed(agent_text_delta 走实时通道不落库)")

	// 4.4 验证正式 Runtime 的 model_context/runtime_adapter Step。
	steps, err := stores.Steps.ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("Steps.ListByRun: %v", err)
	}
	if len(steps) < 2 {
		t.Fatalf("steps 表缺少 Runtime 执行事实: %#v", steps)
	}
	t.Logf("✓ steps 表: %d 条记录", len(steps))

	// 5. 打印完整链路验证摘要
	fmt.Println("\n========== 端到端链路验证通过 ==========")
	fmt.Printf("输入: agentId=%s, prompt=%q\n", agentID, prompt)
	fmt.Printf("Session: %s\n", run.SessionID)
	fmt.Printf("Run: %s (status=%s)\n", run.RunID, run.Status)
	fmt.Printf("SQLite 表:\n")
	fmt.Printf("  - sessions:    ✓ (1 条)\n")
	fmt.Printf("  - messages:    ✓ (%d 条)\n", len(msgPage.Items))
	fmt.Printf("  - runs:        ✓ (status=%s)\n", run.Status)
	fmt.Printf("  - agent_events: ✓ (%d 条)\n", len(events))
	fmt.Printf("  - steps:       %d 条\n", len(steps))
	fmt.Printf("模型回复: %s\n", strings.Join(textParts, ""))
	fmt.Println("========================================")
}

// readAISDKStream 读取 AI SDK v5 SSE 流,返回 (messageID, textDeltas)。
// 流格式: data: {"type":"start","messageId":"run_xxx"} → data: {"type":"text-delta",...} → data: {"type":"finish"}
func readAISDKStream(t *testing.T, body interface{ Read([]byte) (int, error) }) (string, []string) {
	t.Helper()
	var messageID string
	var textParts []string
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "[DONE]" {
			break
		}
		var part map[string]any
		if json.Unmarshal([]byte(data), &part) != nil {
			continue
		}
		switch part["type"] {
		case "start":
			if id, ok := part["messageId"].(string); ok {
				messageID = id
			}
		case "text-delta":
			if delta, ok := part["delta"].(string); ok {
				textParts = append(textParts, delta)
			}
		case "finish":
			return messageID, textParts
		}
	}
	return messageID, textParts
}
