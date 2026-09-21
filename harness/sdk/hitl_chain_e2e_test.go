package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk"
)

// hitl_chain_e2e_test.go 落地方案 4.1 的 HITL 中断-恢复端到端闭环加固：
//
//	(a) 业务链路上 ask_user 工具触发中断 → control_request_created →
//	    Resume 携带用户答复 → Run 完成；
//	(b) 高风险工具审批 request_approval 的 approve / deny 两分支事件链完整
//	   （deny 在现行契约中把 approved=false 作为受控结果交回模型，Run 正常
//	    收敛而非硬失败）；
//	(c) 中断后关闭引擎、以同一持久化目录重新 Build（模拟跨进程重启），
//	    凭 checkpoint 反序列化恢复执行。
//
// 全链路走通用 control_agent（eino deep_agent）+ scenario mock 模型：
// 工具中断经 Tool Gateway suspend，再由 control 票据和 Engine.Resume 恢复。

// hitlConfigPath 返回 fixture 在 tempRoot 下的 harness.yaml 路径。
func hitlConfigPath(tempRoot string) string {
	return filepath.Join(tempRoot, "harness.yaml")
}

// prepareHITLConfig 在 tempRoot 下生成隔离的 harness.yaml + scenario mock
// models.yaml（声明 scenario mock）；已存在则复用，
// 支撑跨 Build 目录复用。
func prepareHITLConfig(t *testing.T, tempRoot string) {
	t.Helper()
	repoRoot := findRepoRoot(t)
	configPath := hitlConfigPath(tempRoot)
	if _, err := os.Stat(configPath); err == nil {
		return
	}
	modelsPath := filepath.Join(tempRoot, "models.yaml")
	mockModels := "default:\n" +
		"  providers:\n" +
		"    - name: mock\n" +
		"      protocol: scenario_mock\n" +
		"      models: [mock-model, qwen3.7-max]\n" +
		"      capability:\n" +
		"        chat: true\n" +
		"        streaming: true\n" +
		"        tool_calling: true\n" +
		"        structured_output: true\n" +
		"        json_schema: true\n" +
		"        limits: {max_context_tokens: 128000, max_output_tokens: 32768}\n" +
		"  default_provider: mock\n" +
		"  default_model: mock-model\n"
	if writeErr := os.WriteFile(modelsPath, []byte(mockModels), 0o600); writeErr != nil {
		t.Fatalf("write models.yaml: %v", writeErr)
	}
	config := renderIntegrationConfig(t, repoRoot, tempRoot)
	config = strings.Replace(config,
		"  models: {source: file, path: "+modelsPath+"}",
		"  models: {source: file, path: "+modelsPath+"}", 1)
	if writeErr := os.WriteFile(configPath, []byte(config), 0o600); writeErr != nil {
		t.Fatalf("write harness.yaml: %v", writeErr)
	}
}

// buildHITLEngine 在指定 tempRoot 上构建引擎。目录可跨 Build 复用，
// 支撑链路 (c) 的跨进程恢复模拟。migrate 幂等，重复调用安全。
// 模型指向 scenario mock，
// 不依赖真实 provider。
func buildHITLEngine(t *testing.T, ctx context.Context, tempRoot string) harness.Engine {
	t.Helper()
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "hitl-fake-token")
	prepareHITLConfig(t, tempRoot)
	configPath := hitlConfigPath(tempRoot)
	if _, err := harness.RunMigration(ctx, harness.MigrationOptions{ConfigPath: configPath, Environment: "local"}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	engine, _, err := harness.Build(ctx, harness.WithConfigPath(configPath), harness.WithEnvironment("local"))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return engine
}

type pendingControlE2E struct {
	// RequestID 是 control_request_created 事件 payload 的实际字段
	//（eino_runtime 产出 "request_id"）。
	RequestID     string `json:"request_id"`
	CheckpointID  string `json:"checkpoint_id"`
	ControlTicket string `json:"control_ticket"`
}

// isTerminalE2E 使用 SDK 公开的终态事件枚举（canonical 状态机）。
func isTerminalE2E(t harness.EventType) bool {
	switch t {
	case harness.EventRunCompleted, harness.EventRunFailed, harness.EventRunCancelled, harness.EventRunExpired:
		return true
	}
	return false
}

// drainUntilControl 消费事件流直到 control_request_created，返回票据三元组。
func drainUntilControl(t *testing.T, ctx context.Context, execution harness.Execution) pendingControlE2E {
	t.Helper()
	for {
		ev, err := execution.Events().Next(ctx)
		if err != nil {
			t.Fatalf("stream ended before control_request_created: %v", err)
		}
		if ev.EventType == harness.EventControlRequestCreated {
			var pending pendingControlE2E
			if err := json.Unmarshal(ev.PayloadPreview, &pending); err != nil {
				t.Fatalf("decode control payload: %v (%s)", err, ev.PayloadPreview)
			}
			if pending.RequestID == "" || pending.CheckpointID == "" || pending.ControlTicket == "" {
				t.Fatalf("control ticket incomplete: %+v (payload=%s)", pending, ev.PayloadPreview)
			}
			return pending
		}
		if isTerminalE2E(ev.EventType) {
			detail := ""
			if ev.Error != nil {
				detail = ev.Error.Code + ": " + ev.Error.Message
			}
			t.Fatalf("run reached terminal %s before control request: %s payload=%s", ev.EventType, detail, ev.PayloadPreview)
		}
	}
}

// drainUntilTerminal 消费事件流至终态，返回终态类型与可观察 payload 集合
// （tool_call_completed + final_response + agent 文本，供决定验证）。
func drainUntilTerminal(t *testing.T, ctx context.Context, execution harness.Execution) (harness.EventType, []string) {
	t.Helper()
	var payloads []string
	for {
		ev, err := execution.Events().Next(ctx)
		if errors.Is(err, io.EOF) {
			t.Fatal("stream ended without terminal event")
		}
		if err != nil {
			t.Fatalf("stream next: %v", err)
		}
		switch ev.EventType {
		case harness.EventToolCallCompleted, harness.EventFinalResponse, harness.EventAgentCompleted, harness.EventModelCallCompleted:
			payloads = append(payloads, string(ev.PayloadPreview))
		}
		if isTerminalE2E(ev.EventType) {
			return ev.EventType, payloads
		}
	}
}

func hitlStart(t *testing.T, ctx context.Context, engine harness.Engine, prompt string) harness.Execution {
	t.Helper()
	execution, err := engine.Start(ctx, harness.StartRequest{
		Identity: harness.Identity{TenantID: "public", UserID: "hitl-e2e", AgentID: "control_agent"},
		Input:    harness.TextMessage(prompt),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return execution
}

func hitlResume(t *testing.T, ctx context.Context, engine harness.Engine, runID string, pending pendingControlE2E, response string) harness.Execution {
	t.Helper()
	resumed, err := engine.Resume(ctx, harness.ResumeRequest{
		Identity:         harness.Identity{RunID: runID},
		ControlRequestID: pending.RequestID,
		CheckpointID:     pending.CheckpointID,
		ControlTicket:    pending.ControlTicket,
		Response:         json.RawMessage(response),
	})
	if err != nil {
		t.Fatalf("resume: %v (chain: %s)", err, dumpErrChain(err))
	}
	return resumed
}

// dumpErrChain 展开多分支错误链（safeWrappedError 对外隐藏 cause 文案，
// 诊断需递归 Unwrap）。
func dumpErrChain(err error) string {
	if err == nil {
		return ""
	}
	out := err.Error()
	switch unwrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range unwrapped.Unwrap() {
			out += " | " + dumpErrChain(child)
		}
	case interface{ Unwrap() error }:
		out += " | " + dumpErrChain(unwrapped.Unwrap())
	}
	return out
}

// 链路 (a)：ask_user 中断 → Resume 携带答复 → Run 完成。
func TestHITLAskUserInterruptAndResumeE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	engine := buildHITLEngine(t, ctx, t.TempDir())
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = engine.Close(shutdownCtx)
	}()

	execution := hitlStart(t, ctx, engine, "[[harness:tool:ask_user]] 请向我确认")
	pending := drainUntilControl(t, ctx, execution)
	resumed := hitlResume(t, ctx, engine, execution.Handle().Identity.RunID, pending, `{"确认":"继续"}`)
	terminal, _ := drainUntilTerminal(t, ctx, resumed)
	if terminal != harness.EventRunCompleted {
		t.Fatalf("terminal = %s, want run_completed", terminal)
	}
}

// 链路 (b)：request_approval 的 approve / deny 两分支事件链完整。
func TestHITLApprovalApproveAndDenyE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	engine := buildHITLEngine(t, ctx, t.TempDir())
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = engine.Close(shutdownCtx)
	}()

	cases := []struct {
		name     string
		response string
	}{
		{"approve", `{"approved":true,"comment":"ok"}`},
		// deny 分支按现行契约把 approved=false 作为受控工具结果交回模型，
		// Run 正常收敛；事件链必须完整。
		{"deny", `{"approved":false,"comment":"denied by human"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			execution := hitlStart(t, ctx, engine, "[[harness:tool:approval]] 高风险操作")
			pending := drainUntilControl(t, ctx, execution)
			resumed := hitlResume(t, ctx, engine, execution.Handle().Identity.RunID, pending, tc.response)
			terminal, payloads := drainUntilTerminal(t, ctx, resumed)
			if terminal != harness.EventRunCompleted {
				t.Fatalf("terminal = %s, want run_completed", terminal)
			}
			// 两分支事件链完整性：恢复后工具确实重新执行并回到模型循环
			//（scenario mock 对工具结果回显 "tool completed"）。approve/deny
			// 的决定数据形状由工具级测试覆盖
			//（TestRequestApprovalInterruptAndResume）；已知观察：恢复路径上
			// 工具结果正文尚未回显进模型可见 content（待后续修复项）。
			found := false
			for _, payload := range payloads {
				if strings.Contains(payload, "tool completed") {
					found = true
				}
			}
			if !found {
				t.Fatalf("resumed stream must show the tool returning to the model loop: %v", payloads)
			}
		})
	}
}

// 链路 (c)：中断后关闭引擎，以同一持久化目录重新 Build（跨进程重启模拟），
// 凭 checkpoint 反序列化恢复执行至完成。
func TestHITLCrossProcessResumeE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	tempRoot := t.TempDir()

	first := buildHITLEngine(t, ctx, tempRoot)
	execution := hitlStart(t, ctx, first, "[[harness:tool:ask_user]] 重启前确认")
	pending := drainUntilControl(t, ctx, execution)
	runID := execution.Handle().Identity.RunID

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 15*time.Second)
	if err := first.Close(closeCtx); err != nil {
		closeCancel()
		t.Fatalf("close first engine: %v", err)
	}
	closeCancel()

	second := buildHITLEngine(t, ctx, tempRoot)
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = second.Close(shutdownCtx)
	}()
	resumed := hitlResume(t, ctx, second, runID, pending, `{"确认":"继续"}`)
	terminal, _ := drainUntilTerminal(t, ctx, resumed)
	if terminal != harness.EventRunCompleted {
		t.Fatalf("cross-process terminal = %s, want run_completed", terminal)
	}
}

// 链路 (d)：task → 子 Agent 在 child run 内 ask_user → proposal 上交为父
// ControlRequest → 父 Resume → 答复路由回 child → child 结果回父 → 完成。
func TestHITLChildAskUserPromotedToParentControlE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	engine := buildHITLEngine(t, ctx, t.TempDir())
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = engine.Close(shutdownCtx)
	}()

	execution := hitlStart(t, ctx, engine, "[[harness:child:ask_user]] 请调度子Agent完成任务")

	// 父流上等待 control_request_created，同时校验 payload 是 proposal 形状。
	var pending pendingControlE2E
	var controlEvent harness.Event
	for {
		ev, err := execution.Events().Next(ctx)
		if err != nil {
			t.Fatalf("parent stream ended before control_request_created: %v", err)
		}
		if ev.EventType == harness.EventControlRequestCreated {
			if err := json.Unmarshal(ev.PayloadPreview, &pending); err != nil {
				t.Fatalf("decode parent control payload: %v (%s)", err, ev.PayloadPreview)
			}
			controlEvent = ev
			break
		}
		if isTerminalE2E(ev.EventType) {
			t.Fatalf("parent run terminal %s before child control promoted: %s (%s)", ev.EventType, dumpErrChain(errFromEvent(ev)), ev.PayloadPreview)
		}
	}
	if pending.RequestID == "" || pending.CheckpointID == "" || pending.ControlTicket == "" {
		t.Fatalf("parent control ticket incomplete: %+v (payload=%s)", pending, controlEvent.PayloadPreview)
	}
	// 父 ControlRequest 的 payload 必须可被 ExtractInteractionProposal 解析
	//（child proposal 的 prompt/kind 已提升进 payload 顶层）。
	proposal, ok := harness.ExtractInteractionProposal(controlEvent)
	if !ok {
		t.Fatalf("parent control payload is not proposal-shaped: %s", controlEvent.PayloadPreview)
	}
	if proposal.Kind == "" || proposal.Prompt == "" {
		t.Fatalf("interaction proposal incomplete: %+v", proposal)
	}

	// 父 Resume：答复经 gateway 路由回 child，child 完成后父继续至终态。
	resumed := hitlResume(t, ctx, engine, execution.Handle().Identity.RunID, pending, `{"确认":"继续执行子任务"}`)
	terminal, payloads := drainUntilTerminal(t, ctx, resumed)
	if terminal != harness.EventRunCompleted {
		t.Fatalf("child-askuser chain terminal = %s, want run_completed", terminal)
	}
	// 恢复后的父模型循环必须看到 task 工具的完成回显（child 结果回父）。
	var sawTaskEcho bool
	for _, payload := range payloads {
		if strings.Contains(payload, "tool completed") {
			sawTaskEcho = true
			break
		}
	}
	if !sawTaskEcho {
		t.Fatalf("parent loop must observe the resumed child task result: %v", payloads)
	}
}
