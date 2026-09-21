package harness_test

import (
	"context"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk"
)

// 基线隔离探针：把验收基线的关键变量（多 Part 输入 / task 子 Agent /
// 单个新扩展）各自单独回归，基线变红时用于快速二分定位。

func TestBaselineProbeMultiPartOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	engine := buildHITLEngine(t, ctx, t.TempDir())
	defer func() {
		c, cc := context.WithTimeout(context.Background(), 10*time.Second)
		defer cc()
		_ = engine.Close(c)
	}()
	execution, err := engine.Start(ctx, harness.StartRequest{
		Identity: harness.Identity{TenantID: "public", UserID: "probe", AgentID: "control_agent"},
		Input: harness.Message{
			Role: harness.RoleUser,
			Parts: []harness.MessagePart{
				{Kind: harness.PartKindText, Text: "多 Part 纯回复探针"},
				{Kind: harness.PartKindImageRef, MIME: "image/png", Ref: harness.ArtifactRef{ID: tinyPNGDataURI(), MIME: "image/png", Hash: "sha256:probe"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	terminal, _ := drainUntilTerminal(t, ctx, execution)
	if terminal != harness.EventRunCompleted {
		t.Fatalf("terminal = %s", terminal)
	}
}

func TestBaselineProbeSubAgentOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	engine := buildHITLEngine(t, ctx, t.TempDir())
	defer func() {
		c, cc := context.WithTimeout(context.Background(), 10*time.Second)
		defer cc()
		_ = engine.Close(c)
	}()
	execution := hitlStart(t, ctx, engine, "[[harness:subagent:control_child]] 子 Agent 探针")
	terminal, _ := drainUntilTerminal(t, ctx, execution)
	if terminal != harness.EventRunCompleted {
		t.Fatalf("terminal = %s", terminal)
	}
}

func TestBaselineProbeTransformerOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tempRoot := t.TempDir()
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "probe-fake-token")
	engine := buildHITLEngineWithOptions(t, ctx, tempRoot,
		"extensions:\n  before_model_hooks:\n    - baseline.transformer\n",
		harness.WithBeforeModelHookProvider(&baselineTransformer{}),
	)
	defer func() {
		c, cc := context.WithTimeout(context.Background(), 10*time.Second)
		defer cc()
		_ = engine.Close(c)
	}()
	execution := hitlStart(t, ctx, engine, "transformer 探针纯回复")
	terminal, _ := drainUntilTerminal(t, ctx, execution)
	if terminal != harness.EventRunCompleted {
		t.Fatalf("terminal = %s", terminal)
	}
}

func TestBaselineProbeInterceptorOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tempRoot := t.TempDir()
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "probe-fake-token")
	engine := buildHITLEngineWithOptions(t, ctx, tempRoot,
		"extensions:\n  tool_call_interceptors:\n    - baseline.task_store\n",
		harness.WithToolCallInterceptorProvider(&baselineInterceptor{}),
	)
	defer func() {
		c, cc := context.WithTimeout(context.Background(), 10*time.Second)
		defer cc()
		_ = engine.Close(c)
	}()
	execution := hitlStart(t, ctx, engine, "[[harness:subagent:control_child]] interceptor 探针")
	terminal, _ := drainUntilTerminal(t, ctx, execution)
	if terminal != harness.EventRunCompleted {
		t.Fatalf("terminal = %s", terminal)
	}
}
