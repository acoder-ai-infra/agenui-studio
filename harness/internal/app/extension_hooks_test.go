package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// stubOutputValidator 记录入参并返回预设结果。
type stubOutputValidator struct {
	id      string
	lastReq extension.OutputValidateRequest
	result  extension.OutputValidateResult
	err     error
	calls   int
}

func (v *stubOutputValidator) ID() string { return v.id }

func (v *stubOutputValidator) Validate(_ context.Context, req extension.OutputValidateRequest) (extension.OutputValidateResult, error) {
	v.calls++
	v.lastReq = req
	return v.result, v.err
}

func outputValidatorCatalog(t *testing.T, entries ...kernel.ExtensionEntry) *kernel.ExtensionCatalog {
	t.Helper()
	catalog, err := kernel.NewExtensionCatalog(entries)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	return catalog
}

func responseHookInput(text string, validatorIDs ...string) agentruntime.RuntimeHookInput {
	return agentruntime.RuntimeHookInput{
		Point: agentruntime.HookBeforeResponse,
		Run: agentruntime.RunRequest{
			RunID: "run_1", TenantID: "t1",
			Definition: agentruntime.AgentDefinition{AgentID: "agent_1", OutputValidators: validatorIDs},
		},
		Response: &agentruntime.RuntimeHookResponse{Text: text},
	}
}

// TestOutputValidatorHookRegistration 验证派发 hook 注册在 before_response
// 切点（agent 级绑定：无条件注册单个派发 hook）。
func TestOutputValidatorHookRegistration(t *testing.T) {
	service := agentruntime.NewRuntimeService(agentruntime.NewMockRuntime(), agentruntime.NewInMemoryStateManager(), nil, nil)
	if err := registerExtensionRuntimeHooks(service, nil, kernel.TurnEnvironment{Environment: "local"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	registrations := service.Hooks.Registrations(agentruntime.HookBeforeResponse)
	if len(registrations) != 1 {
		t.Fatalf("registrations = %d; want 1", len(registrations))
	}
	if registrations[0].ID != outputValidatorDispatchHookID {
		t.Fatalf("hook id = %q", registrations[0].ID)
	}
	if registrations[0].FailurePolicy != agentruntime.HookFailClosed {
		t.Fatalf("default failure policy = %q; want fail_closed", registrations[0].FailurePolicy)
	}
}

// TestOutputValidatorHookAcceptWritesScopedData 验证声明绑定的 agent 执行
// validator，Accept 放行并把结果写入派生 ID 的 ScopedData key。
func TestOutputValidatorHookAcceptWritesScopedData(t *testing.T) {
	validator := &stubOutputValidator{id: "myapp.guard", result: extension.OutputValidateResult{Action: extension.OutputAccept}}
	catalog := outputValidatorCatalog(t, kernel.ExtensionEntry{
		ID: "myapp.guard", Kind: kernel.ExtOutputValidator, Implementation: validator,
	})
	hook := newOutputValidatorDispatchHook(catalog, kernel.TurnEnvironment{Environment: "local"})
	output, err := hook.Execute(context.Background(), responseHookInput("final text", "myapp.guard"))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if validator.calls != 1 || validator.lastReq.Text != "final text" || validator.lastReq.Ctx.RunID != "run_1" {
		t.Fatalf("validator request wrong: %+v", validator.lastReq)
	}
	item, ok := output.ScopedData.Run["extension:output_validator:myapp.guard"]
	if !ok {
		t.Fatalf("scoped data result missing: %+v", output.ScopedData)
	}
	var record map[string]string
	if err := json.Unmarshal(item.Value, &record); err != nil || record["action"] != "accept" {
		t.Fatalf("scoped record wrong: %s err=%v", item.Value, err)
	}
}

// TestOutputValidatorHookUndeclaredAgentSkips 验证未声明绑定的 agent 直接
// 放行且不调用任何 validator（含 Options 已注册的实现）。
func TestOutputValidatorHookUndeclaredAgentSkips(t *testing.T) {
	validator := &stubOutputValidator{id: "myapp.guard", result: extension.OutputValidateResult{Action: extension.OutputFail}}
	catalog := outputValidatorCatalog(t, kernel.ExtensionEntry{
		ID: "myapp.guard", Kind: kernel.ExtOutputValidator, Implementation: validator,
	})
	hook := newOutputValidatorDispatchHook(catalog, kernel.TurnEnvironment{Environment: "local"})
	if _, err := hook.Execute(context.Background(), responseHookInput("final text")); err != nil {
		t.Fatalf("undeclared agent must pass through: %v", err)
	}
	if validator.calls != 0 {
		t.Fatal("undeclared agent must not invoke validators")
	}
}

// TestOutputValidatorHookUnknownIDFailsClosed 验证引用未注册 ID 时 fail-closed。
func TestOutputValidatorHookUnknownIDFailsClosed(t *testing.T) {
	hook := newOutputValidatorDispatchHook(nil, kernel.TurnEnvironment{Environment: "local"})
	_, err := hook.Execute(context.Background(), responseHookInput("final text", "missing.validator"))
	if err == nil || !strings.Contains(err.Error(), "missing.validator") {
		t.Fatalf("unknown validator id must fail closed, got %v", err)
	}
}

// TestOutputValidatorHookGlobalFallback 验证全集注册的 validator 可被绑定
// 解析（catalog 未命中时兜底）。
func TestOutputValidatorHookGlobalFallback(t *testing.T) {
	extension.RegisterOutputValidators(&globalRecordingValidator{id: "test.app.global_validator"})
	hook := newOutputValidatorDispatchHook(nil, kernel.TurnEnvironment{Environment: "local"})
	output, err := hook.Execute(context.Background(), responseHookInput("final text", "test.app.global_validator"))
	if err != nil {
		t.Fatalf("global fallback execute: %v", err)
	}
	if _, ok := output.ScopedData.Run["extension:output_validator:test.app.global_validator"]; !ok {
		t.Fatalf("scoped data result missing: %+v", output.ScopedData)
	}
}

// TestOutputValidatorHookOptionsShadowGlobal 验证同 ID 时 Options（catalog）
// 实现优先于全集实现。
func TestOutputValidatorHookOptionsShadowGlobal(t *testing.T) {
	extension.RegisterOutputValidators(&globalRecordingValidator{id: "test.app.shadowed_validator"})
	optionsValidator := &stubOutputValidator{id: "test.app.shadowed_validator", result: extension.OutputValidateResult{Action: extension.OutputAccept}}
	catalog := outputValidatorCatalog(t, kernel.ExtensionEntry{
		ID: "test.app.shadowed_validator", Kind: kernel.ExtOutputValidator, Implementation: optionsValidator,
	})
	hook := newOutputValidatorDispatchHook(catalog, kernel.TurnEnvironment{Environment: "local"})
	if _, err := hook.Execute(context.Background(), responseHookInput("final text", "test.app.shadowed_validator")); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if optionsValidator.calls != 1 {
		t.Fatal("options-registered validator must take precedence over the global registry")
	}
}

// globalRecordingValidator 是注册进全集的 Accept validator（全集不可撤销，
// 独立类型避免与其他用例共享状态）。
type globalRecordingValidator struct{ id string }

func (v *globalRecordingValidator) ID() string { return v.id }
func (v *globalRecordingValidator) Validate(context.Context, extension.OutputValidateRequest) (extension.OutputValidateResult, error) {
	return extension.OutputValidateResult{Action: extension.OutputAccept}, nil
}

// TestOutputValidatorHookRejectFailsRun 验证非 Accept 结果在 fail-closed 策略
// 下使 Run 在 final_response 前失败（端到端穿过 RuntimeService）。
func TestOutputValidatorHookRejectFailsRun(t *testing.T) {
	validator := &stubOutputValidator{id: "myapp.guard", result: extension.OutputValidateResult{
		Action: extension.OutputFail, Reason: "policy_violation",
	}}
	catalog := outputValidatorCatalog(t, kernel.ExtensionEntry{
		ID: "myapp.guard", Kind: kernel.ExtOutputValidator, Implementation: validator,
	})
	runtime := agentruntime.NewMockRuntime()
	service := agentruntime.NewRuntimeService(runtime, agentruntime.NewInMemoryStateManager(), nil, nil)
	if err := registerExtensionRuntimeHooks(service, catalog, kernel.TurnEnvironment{Environment: "local"}); err != nil {
		t.Fatalf("register: %v", err)
	}

	events, err := service.Run(context.Background(), agentruntime.RunRequest{
		SessionID: "sess_1", RunID: "run_1", TenantID: "t1",
		Definition: agentruntime.AgentDefinition{
			AgentID: "agent_1", AgentType: "assistant", Version: "v1",
			Runtime:          agentruntime.RuntimeSpec{Type: agentruntime.RuntimeTypeMock, Mode: agentruntime.RuntimeModeReact},
			OutputValidators: []string{"myapp.guard"},
		},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	sawRunFailed := false
	sawFinalResponse := false
	for event := range events {
		switch string(event.EventType) {
		case "run_failed":
			sawRunFailed = true
		case "final_response":
			sawFinalResponse = true
		}
	}
	if !sawRunFailed {
		t.Fatal("expected run_failed after validator rejection")
	}
	if sawFinalResponse {
		t.Fatal("final_response must not be emitted when validator rejects fail-closed")
	}
	if validator.calls == 0 {
		t.Fatal("validator was not invoked")
	}
	if !strings.Contains(validator.lastReq.Ctx.Environment, "local") {
		t.Fatalf("validator env missing: %+v", validator.lastReq.Ctx)
	}
}

func TestOutputValidatorHookRetryCarriesValidatorFeedback(t *testing.T) {
	validator := &stubOutputValidator{id: "myapp.guard", result: extension.OutputValidateResult{
		Action: extension.OutputRetry, RetryBudget: 1, Reason: "invalid_contract", RepairFeedback: "return the required JSON array",
	}}
	catalog := outputValidatorCatalog(t, kernel.ExtensionEntry{
		ID: "myapp.guard", Kind: kernel.ExtOutputValidator, Implementation: validator,
	})
	hook := newOutputValidatorDispatchHook(catalog, kernel.TurnEnvironment{Environment: "local"})
	_, err := hook.Execute(context.Background(), responseHookInput("invalid output", "myapp.guard"))
	var retry *agentruntime.OutputRetryError
	if !errors.As(err, &retry) {
		t.Fatalf("expected typed output retry, got %v", err)
	}
	if retry.RetryBudget != 1 || retry.RepairFeedback != "return the required JSON array" {
		t.Fatalf("retry transport drift: %#v", retry)
	}
}
