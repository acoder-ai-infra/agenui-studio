package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/control/controlticket"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

func TestParseAgentChatCursorMap(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet,
		`/chat-stream?cursor={"schemaVersion":"harness.agent_chat_cursor.v1","sequences":{"run-root":12,"run-child":7}}`, nil)
	cursor, err := parseAgentChatCursor(req)
	if err != nil {
		t.Fatal(err)
	}
	if cursor["run-root"] != 12 || cursor["run-child"] != 7 {
		t.Fatalf("cursor=%#v", cursor)
	}

	bad := httptest.NewRequest(http.MethodGet,
		`/chat-stream?cursor={"schemaVersion":"harness.agent_chat_cursor.v1","sequences":{"run-root":-1}}`, nil)
	if _, err := parseAgentChatCursor(bad); err == nil {
		t.Fatal("negative cursor sequence accepted")
	}
}

func TestAgentChatRunTerminalRecognizesPersistedTerminalState(t *testing.T) {
	for _, status := range []storage.RunStatus{
		storage.RunStatusCompleted,
		storage.RunStatusFailed,
		storage.RunStatusCancelled,
		storage.RunStatusExpired,
	} {
		if !agentChatRunTerminal(status) {
			t.Fatalf("status %q was not terminal", status)
		}
	}
	if agentChatRunTerminal(storage.RunStatusRunning) {
		t.Fatal("running status was terminal")
	}
}

func TestAgentChatContextProjectionIsDebugOnlyAndRedactsProtectedItems(t *testing.T) {
	event := observability.AgentEvent{
		EventID: "context-1", RunID: "run-1", EventType: observability.EventModelContextBuilt,
		Visibility: observability.VisibilityDebug, CreatedAt: time.Unix(100, 0).UTC(),
		Payload: json.RawMessage(`{"context_budget_report":{"schema_version":"harness.context_budget_report.v1","round":2,"max_input_tokens":1000,"reserved_output_tokens":100,"used_tokens":420,"usage_ratio":0.42,"pressure":"normal","segments":[{"category":"instructions","tokens":120,"ratio":0.12,"protected":true,"items":[{"label":"system","tokens":120,"protected":true,"content_preview":"must not leak"}]},{"category":"current_input","tokens":80,"ratio":0.08,"items":[{"label":"current question","tokens":80,"content_preview":"请继续 token=projection-secret"}]}]}}`),
	}
	if view, ok := agentChatContextProjection(event, false); ok || view != nil {
		t.Fatalf("ordinary projection widened debug context: %#v", view)
	}
	view, ok := agentChatContextProjection(event, true)
	if !ok || view["schemaVersion"] != agentChatContextSchemaVersion || view["runId"] != "run-1" || view["round"] != 2 {
		t.Fatalf("context projection=%#v visible=%v", view, ok)
	}
	segments, ok := view["segments"].([]any)
	if !ok || len(segments) != 2 {
		t.Fatalf("segments=%#v", view["segments"])
	}
	instructions := segments[0].(map[string]any)
	items := instructions["items"].([]any)
	if _, leaked := items[0].(map[string]any)["contentPreview"]; leaked {
		t.Fatalf("protected context leaked: %#v", items[0])
	}
	currentItems := segments[1].(map[string]any)["items"].([]any)
	if preview := currentItems[0].(map[string]any)["contentPreview"]; strings.Contains(preview.(string), "projection-secret") {
		t.Fatalf("projection leaked a secret: %#v", currentItems[0])
	}
}

func TestAgentChatProcessProjectionIsVersionedAndStable(t *testing.T) {
	started := observability.AgentEvent{
		EventID: "event-start", RunID: "run-1", StepID: "step-1", AgentID: "agent-a",
		EventType: observability.EventToolCallStarted, Visibility: observability.VisibilityUserVisible,
		PayloadPreview: json.RawMessage(`{"tool_call_id":"call-1","tool_name":"search","display_name":"知识检索","arguments_preview":{"q":"west lake"}}`), CreatedAt: time.Unix(100, 0).UTC(),
	}
	completed := started
	completed.EventID = "event-complete"
	completed.EventType = observability.EventToolCallCompleted
	completed.PayloadPreview = json.RawMessage(`{"tool_call_id":"call-1","tool_name":"search","display_name":"知识检索","presentation":{"title":"能力知识检索已完成","summary":"找到可用能力","details":[{"label":"候选数量","value":"3"}]},"result_preview":{"answer":"done"}}`)
	completed.CreatedAt = time.Unix(102, 0).UTC()

	startView, ok := agentChatProcessProjection(started, false)
	if !ok {
		t.Fatal("started event was not projected")
	}
	completeView, ok := agentChatProcessProjection(completed, false)
	if !ok {
		t.Fatal("completed event was not projected")
	}
	if startView["schemaVersion"] != "harness.agent_chat_process.v1" {
		t.Fatalf("schemaVersion = %#v", startView["schemaVersion"])
	}
	if startView["processId"] == "" || startView["processId"] != completeView["processId"] {
		t.Fatalf("process identity drifted: start=%#v complete=%#v", startView, completeView)
	}
	if startView["status"] != "running" || completeView["status"] != "completed" {
		t.Fatalf("statuses = %#v / %#v", startView["status"], completeView["status"])
	}
	if startView["startedAt"] == nil || completeView["endedAt"] == nil {
		t.Fatalf("process timestamps missing: start=%#v complete=%#v", startView, completeView)
	}
	if startView["title"] != "知识检索" {
		t.Fatalf("title = %#v", startView["title"])
	}
	if completeView["title"] != "能力知识检索已完成" || completeView["summary"] != "找到可用能力" {
		t.Fatalf("completed presentation = %#v", completeView)
	}
	details, ok := completeView["details"].([]map[string]string)
	if !ok || len(details) != 1 || details[0]["label"] != "候选数量" || details[0]["value"] != "3" {
		t.Fatalf("completed details = %#v", completeView["details"])
	}
	input, ok := startView["input"].(map[string]any)
	if !ok || input["q"] != "west lake" {
		t.Fatalf("tool input preview = %#v", startView["input"])
	}
	output, ok := completeView["output"].(map[string]any)
	if !ok || output["answer"] != "done" {
		t.Fatalf("tool output preview = %#v", completeView["output"])
	}
}

func TestAgentChatProcessProjectionCarriesToolInputArtifactRef(t *testing.T) {
	event := observability.AgentEvent{
		EventID: "event-input-ref", RunID: "run-1", StepID: "step-1", AgentID: "agent-a",
		EventType: observability.EventToolArtifactCreated, Visibility: observability.VisibilityDebug,
		PayloadPreview: json.RawMessage(`{"tool_name":"search","artifact_ref":"artifact://tool/args","input_size_bytes":4096}`),
		PayloadRef:     "artifact://tool/args",
		DebugRef:       "artifact://tool/args",
		CreatedAt:      time.Unix(101, 0).UTC(),
	}
	view, ok := agentChatProcessProjection(event, true)
	if !ok {
		t.Fatal("debug tool input artifact was not projected")
	}
	if view["processId"] != "run-1:step-1" || view["status"] != "running" || view["inputRef"] != "artifact://tool/args" {
		t.Fatalf("tool input artifact projection=%#v", view)
	}
}

func TestAgentChatProcessProjectionDoesNotWidenVisibility(t *testing.T) {
	event := observability.AgentEvent{
		EventID: "restricted", RunID: "run-1", EventType: observability.EventReasoningSummary,
		Visibility: observability.VisibilityRestricted, Payload: json.RawMessage(`{"text":"hidden chain"}`),
	}
	if view, ok := agentChatProcessProjection(event, false); ok || view != nil {
		t.Fatalf("ordinary projection widened restricted visibility: %#v", view)
	}
}

func TestAgentChatProcessProjectionHidesAgentReasoningSummaries(t *testing.T) {
	started := observability.AgentEvent{
		EventID: "model-start", RunID: "run-1", EventType: observability.EventModelCallStarted,
		Visibility: observability.VisibilityDebug, Payload: json.RawMessage(`{"text":"raw hidden thought"}`),
	}
	if view, ok := agentChatProcessProjection(started, true); ok || view != nil {
		t.Fatalf("debug model lifecycle became a workbench process: %#v", view)
	}

	legacy := observability.AgentEvent{
		EventID: "summary", RunID: "run-1", EventType: observability.EventReasoningSummary,
		Visibility: observability.VisibilityUserVisible, Payload: json.RawMessage(`{"text":"已完成需求与依赖分析"}`),
	}
	if view, ok := agentChatProcessProjection(legacy, false); ok || view != nil {
		t.Fatalf("legacy unscoped reasoning was replayed: %#v", view)
	}

	answerSummary := legacy
	answerSummary.EventID = "answer-summary"
	answerSummary.Payload = json.RawMessage(`{"text":"已完成需求与依赖分析","reasoning_scope":"answer_summary"}`)
	if view, ok := agentChatProcessProjection(answerSummary, false); ok || view != nil {
		t.Fatalf("answer reasoning entered the workbench process stream: %#v", view)
	}

	debug := answerSummary
	debug.EventID = "debug-summary"
	debug.Visibility = observability.VisibilityDebug
	if view, ok := agentChatProcessProjection(debug, true); ok || view != nil {
		t.Fatalf("debug reasoning entered the workbench process stream: %#v", view)
	}
}

func TestAgentChatCommentaryProjectionPublishesOnlyTaskAcknowledgement(t *testing.T) {
	event := observability.AgentEvent{
		EventID: "ack-1", RunID: "run-1", EventType: observability.EventToolCallStarted,
		Visibility:     observability.VisibilityUserVisible,
		PayloadPreview: json.RawMessage(`{"task_acknowledgement":"已收到，我会先检查现有结构，再完成修改。"}`),
	}
	view, ok := agentChatCommentaryProjection(event)
	if !ok || view["schemaVersion"] != agentChatCommentarySchemaVersion || view["runId"] != "run-1" || view["text"] != "已收到，我会先检查现有结构，再完成修改。" {
		t.Fatalf("commentary projection=%#v visible=%v", view, ok)
	}

	wrongScope := event
	wrongScope.EventID = "answer-summary"
	wrongScope.EventType = observability.EventReasoningSummary
	wrongScope.Payload = json.RawMessage(`{"text":"内部摘要","reasoning_scope":"answer_summary"}`)
	if view, ok := agentChatCommentaryProjection(wrongScope); ok || view != nil {
		t.Fatalf("non-acknowledgement reasoning became commentary: %#v", view)
	}

	restricted := event
	restricted.EventID = "restricted-ack"
	restricted.Visibility = observability.VisibilityRestricted
	if view, ok := agentChatCommentaryProjection(restricted); ok || view != nil {
		t.Fatalf("restricted acknowledgement became commentary: %#v", view)
	}
}

func TestAgentChatProcessProjectionCarriesDynamicToolPresentation(t *testing.T) {
	event := observability.AgentEvent{
		EventID: "tool-start", RunID: "run-style", StepID: "tool-step", EventType: observability.EventToolCallStarted,
		Visibility:     observability.VisibilityUserVisible,
		PayloadPreview: json.RawMessage(`{"tool_call_id":"call-1","tool_name":"agenui_workspace","display_name":"生成界面结构","process_presentation":{"stage":{"id":"ui_generation","label":"生成界面","order":20},"activity":{"key":"agenui_workspace","label":"生成界面结构","detail_level":"secondary"}}}`),
	}
	view, ok := agentChatProcessProjection(event, false)
	if !ok {
		t.Fatal("tool process was not projected")
	}
	stage, _ := view["stage"].(map[string]any)
	activity, _ := view["activity"].(map[string]any)
	if stage["id"] != "ui_generation" || stage["label"] != "生成界面" || stage["order"] != 20 {
		t.Fatalf("stage projection=%#v", stage)
	}
	if activity["key"] != "agenui_workspace" || activity["label"] != "生成界面结构" || activity["detailLevel"] != "secondary" {
		t.Fatalf("activity projection=%#v", activity)
	}
}

func TestAgentChatProcessProjectionFallsBackForNewUnconfiguredTool(t *testing.T) {
	event := observability.AgentEvent{
		EventID: "tool-start", RunID: "run-1", StepID: "tool-step", EventType: observability.EventToolCallStarted,
		Visibility:     observability.VisibilityUserVisible,
		PayloadPreview: json.RawMessage(`{"tool_call_id":"call-1","tool_name":"future_tool"}`),
	}
	view, ok := agentChatProcessProjection(event, false)
	if !ok {
		t.Fatal("new unconfigured tool was dropped")
	}
	stage, _ := view["stage"].(map[string]any)
	activity, _ := view["activity"].(map[string]any)
	if stage["id"] != "execution" || stage["label"] != "执行处理" {
		t.Fatalf("fallback stage=%#v", stage)
	}
	if activity["key"] != "future_tool" || activity["label"] != "执行工具" {
		t.Fatalf("fallback activity=%#v", activity)
	}
}

func TestAgentChatControlRequestIsNotDuplicatedAsProcess(t *testing.T) {
	events := []observability.AgentEvent{
		{
			EventID: "control-created", RunID: "run-1", EventType: observability.EventControlRequestCreated,
			Visibility: observability.VisibilityUserVisible,
		},
		{
			EventID: "ask-user-tool", RunID: "run-1", EventType: observability.EventToolCallStarted,
			Visibility: observability.VisibilityUserVisible, PayloadPreview: json.RawMessage(`{"tool_name":"ask_user"}`),
		},
	}
	for _, event := range events {
		if view, ok := agentChatProcessProjection(event, false); ok || view != nil {
			t.Fatalf("control interaction must render only through data-control: %#v", view)
		}
	}
}

func TestAgentChatDisplayMessageTextRemovesEmbeddedAttachmentBody(t *testing.T) {
	composed := "请分析附件\n\n[Attachment: 成长阵地页 PRD v0.37.md]\n# 成长阵地页\n大量附件正文"
	if got := agentChatDisplayMessageText(composed); got != "请分析附件" {
		t.Fatalf("display text=%q", got)
	}
	if got := agentChatDisplayMessageText("[Attachment: spec.md]\n# Spec"); got != "已上传附件" {
		t.Fatalf("attachment-only display text=%q", got)
	}
}

func TestAgentChatFailureTextUsesStableCodeWithoutInternalDetail(t *testing.T) {
	event := observability.AgentEvent{Error: &observability.EventError{Code: "SUB_AGENT_TARGET_RESOLVE_FAILED", Message: "database password and stack trace"}}
	got := agentChatFailureText(event, &storage.Run{ErrorCode: "RUNTIME_ADAPTER_FAILED", ErrorMessage: "private runtime detail"})
	if got != "子 Agent 配置不可用（SUB_AGENT_TARGET_RESOLVE_FAILED）" {
		t.Fatalf("failure text=%q", got)
	}
	if strings.Contains(got, "password") || strings.Contains(got, "private") {
		t.Fatalf("failure text leaked internal detail: %q", got)
	}
}

func TestAgentChatFailureTextNamesModelProviderFailure(t *testing.T) {
	event := observability.AgentEvent{Error: &observability.EventError{Code: "MODEL_PROVIDER_4XX", Message: "incorrect api key sk-secret"}}
	got := agentChatFailureText(event, &storage.Run{ErrorCode: "RUNTIME_ADAPTER_FAILED", ErrorMessage: "private runtime detail"})
	if got != "模型服务请求失败（MODEL_PROVIDER_4XX）" {
		t.Fatalf("failure text=%q", got)
	}
	if strings.Contains(got, "sk-secret") || strings.Contains(got, "private") {
		t.Fatalf("failure text leaked internal detail: %q", got)
	}
}

func TestAgentChatRunProjectionShowsChildIdentityWithoutRuntimeBinding(t *testing.T) {
	run := &storage.Run{
		RunID: "child-run", ParentRunID: "parent-run", AgentID: "solution_architect_scout",
		Runtime: "eino", RuntimeBinding: json.RawMessage(`{"secret":"must-not-leak"}`),
		Status: storage.RunStatusCompleted, StartedAt: time.Unix(100, 0).UTC(), EndedAt: time.Unix(102, 0).UTC(),
	}
	view := agentChatRunProjection(run)
	if view["schemaVersion"] != "harness.agent_chat_process.v1" || view["processId"] != "run:child-run" {
		t.Fatalf("child process identity=%#v", view)
	}
	if view["runId"] != "child-run" || view["parentRunId"] != "parent-run" || view["agentId"] != "solution_architect_scout" || view["status"] != "completed" {
		t.Fatalf("child process fields=%#v", view)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "must-not-leak") || strings.Contains(string(encoded), "runtimeBinding") {
		t.Fatalf("child process leaked runtime binding: %s", encoded)
	}
}

func TestAgentChatControlProjectionWhitelistsInteractivePayload(t *testing.T) {
	createdAt := time.Unix(123, 0).UTC()
	event := observability.AgentEvent{
		EventID: "control-created", RunID: "run-1", EventType: observability.EventControlRequestCreated,
		Visibility: observability.VisibilityUserVisible, CreatedAt: createdAt,
		PayloadPreview: json.RawMessage(`{
			"request_id":"ctrl-1","type":"ask_user","title":"需要你的确认","required":true,
			"control_ticket":"safe-ticket","resume_token":"must-not-leak","internal":"hidden",
			"interrupt_contexts":[{"id":"interrupt-root","is_root_cause":true,"info":{"questions":[{
				"header":"范围","question":"是否继续？","options":[
					{"label":"继续","description":"继续完成方案"},{"label":"停止","description":"结束任务"}
				]
			}]}}]
		}`),
	}
	view, ok := agentChatControlProjection(event)
	if !ok {
		t.Fatal("user-visible control request was not projected")
	}
	if view["schemaVersion"] != "harness.agent_chat_control.v1" || view["requestId"] != "ctrl-1" || view["controlTicket"] != "safe-ticket" || view["status"] != "pending" {
		t.Fatalf("control identity/status=%#v", view)
	}
	if view["createdAt"] != createdAt {
		t.Fatalf("control createdAt=%#v, want %s", view["createdAt"], createdAt)
	}
	targets, ok := view["resumeTargets"].([]string)
	if !ok || len(targets) != 1 || targets[0] != "interrupt-root" {
		t.Fatalf("resume targets=%#v", view["resumeTargets"])
	}
	questions, ok := view["questions"].([]agentChatControlQuestion)
	if !ok || len(questions) != 1 || questions[0].Header != "范围" || len(questions[0].Options) != 2 {
		t.Fatalf("questions=%#v", view["questions"])
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"resume_token", "must-not-leak", "internal", "hidden"} {
		if string(encoded) != "" && containsJSONText(encoded, forbidden) {
			t.Fatalf("control projection leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestAgentChatControlProjectionRejectsNonPublicAndUnusableEvents(t *testing.T) {
	for _, event := range []observability.AgentEvent{
		{EventType: observability.EventControlRequestCreated, Visibility: observability.VisibilityDebug, PayloadPreview: json.RawMessage(`{"request_id":"ctrl-1","control_ticket":"ticket"}`)},
		{EventType: observability.EventControlRequestCreated, Visibility: observability.VisibilityUserVisible, PayloadPreview: json.RawMessage(`{"request_id":"ctrl-1"}`)},
		{EventType: observability.EventToolCallStarted, Visibility: observability.VisibilityUserVisible, PayloadPreview: json.RawMessage(`{"request_id":"ctrl-1","control_ticket":"ticket"}`)},
	} {
		if view, ok := agentChatControlProjection(event); ok || view != nil {
			t.Fatalf("unexpected control projection: %#v", view)
		}
	}
}

func TestAgentChatControlAnswerProjectionPreservesStructuredAnswers(t *testing.T) {
	text, answers := agentChatControlAnswerProjection([]byte(`{
		"response_text":"卡片类型：单道菜品\n交互动作：查看详情",
		"targets":{
			"interrupt-root":{"answers":[
				{"question_id":"q0","selected_option":{"id":"q0_opt0","label":"单道菜品"}},
				{"question_id":"q1","selected_option":{"id":"q1_opt0","label":"查看详情"}}
			]}
		}
	}`))
	if text != "卡片类型：单道菜品\n交互动作：查看详情" {
		t.Fatalf("answer text=%q", text)
	}
	if len(answers) != 2 || answers[0].QuestionID != "q0" || answers[0].Text != "单道菜品" ||
		answers[1].QuestionID != "q1" || answers[1].Text != "查看详情" {
		t.Fatalf("structured answers=%#v", answers)
	}
}

func TestFinalizeAgentChatControlViewRejectsStaleTicketAfterRestart(t *testing.T) {
	currentCodec, err := controlticket.New([]byte("current-control-ticket-secret-32-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	oldCodec, err := controlticket.New([]byte("previous-control-ticket-secret-32-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	claims := controlticket.Claims{
		TenantID: "tenant-a", UserID: "owner", SessionID: "session-a", RunID: "run-a",
		RequestID: "control-a", ResumeToken: "resume-secret",
	}
	oldTicket, err := oldCodec.Seal(claims, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/", nil)
	request = request.WithContext(observability.WithTraceContext(request.Context(), observability.TraceContext{TenantID: "tenant-a", UserID: "owner"}))
	view := map[string]any{"requestId": "control-a", "status": "pending", "controlTicket": oldTicket}
	deps := &Deps{ControlTickets: currentCodec}
	deps.finalizeAgentChatControlView(request, &storage.Run{RunID: "run-a", SessionID: "session-a", TenantID: "tenant-a"}, view, false)
	if view["status"] != "unavailable" || view["controlTicket"] != nil {
		t.Fatalf("stale ticket remained actionable: %#v", view)
	}
}

func TestFinalizeAgentChatControlViewKeepsCurrentPendingTicket(t *testing.T) {
	codec, err := controlticket.New([]byte("current-control-ticket-secret-32-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	claims := controlticket.Claims{
		TenantID: "tenant-a", UserID: "owner", SessionID: "session-a", RunID: "run-a",
		RequestID: "control-a", ResumeToken: "resume-secret",
	}
	ticket, err := codec.Seal(claims, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/", nil)
	request = request.WithContext(observability.WithTraceContext(request.Context(), observability.TraceContext{TenantID: "tenant-a", UserID: "owner"}))
	view := map[string]any{"requestId": "control-a", "status": "pending", "controlTicket": ticket}
	deps := &Deps{ControlTickets: codec}
	deps.finalizeAgentChatControlView(request, &storage.Run{RunID: "run-a", SessionID: "session-a", TenantID: "tenant-a"}, view, false)
	if view["status"] != "pending" || view["controlTicket"] != ticket {
		t.Fatalf("valid ticket was changed: %#v", view)
	}
}

func TestFinalizeAgentChatControlViewMarksAnsweredWaitingRunAsResumeFailed(t *testing.T) {
	request := httptest.NewRequest("GET", "/", nil)
	view := map[string]any{"requestId": "control-a", "status": "answered", "controlTicket": "must-be-removed"}
	deps := &Deps{}
	deps.finalizeAgentChatControlView(request, &storage.Run{RunID: "run-a", Status: storage.RunStatusWaitingControl}, view, true)
	if view["status"] != "resume_failed" || view["controlTicket"] != nil {
		t.Fatalf("answered control did not expose safe recovery state: %#v", view)
	}
}

func TestFinalizeAgentChatControlViewKeepsHistoricalAnsweredControl(t *testing.T) {
	request := httptest.NewRequest("GET", "/", nil)
	view := map[string]any{"requestId": "control-a", "status": "answered", "controlTicket": "must-be-removed"}
	deps := &Deps{}
	deps.finalizeAgentChatControlView(request, &storage.Run{RunID: "run-a", Status: storage.RunStatusWaitingControl}, view, false)
	if view["status"] != "answered" || view["controlTicket"] != nil {
		t.Fatalf("historical answered control was mistaken for resume failure: %#v", view)
	}
}

func containsJSONText(payload []byte, value string) bool {
	return strings.Contains(string(payload), value)
}
