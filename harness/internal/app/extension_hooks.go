package app

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// extension_hooks.go 把 OutputValidator 扩展桥接为 runtime hook（R2c）。
//
// 校验发生在 final_response 落盘之前：Composition Root 注册**单个派发
// hook** 到 `runtime.before_response` 切点，运行时按当前 Run 的
// Definition.OutputValidators 声明顺序解析并逐个执行（agent 级绑定：未
// 声明的 Run 直接放行；子 agent child run 用自己的声明，不继承）。ID 先查
// Options 冻结的 ExtensionCatalog，未命中再查全集
//（extension.LookupOutputValidator），两处都没有 fail-closed。
//
// 命名对齐约定：ScopedData 结果 key 仍为派生 ID
// extension:output_validator:<entryID>；首次非 Accept 结果返回错误，
// fail-closed 策略下 Run 以 run_failed 结束。

// outputValidatorDispatchHookID 是派发 hook 的注册 ID。
const outputValidatorDispatchHookID = "extension:output_validator:dispatch"

// registerExtensionRuntimeHooks 注册 output_validator 的派发 hook。绑定是
// per-agent 动态的（且可能只引用全集实现），因此 hook 无条件注册；未声明
// validator 的 Run 在 hook 内零成本直通。
func registerExtensionRuntimeHooks(service *agentruntime.RuntimeService, catalog *kernel.ExtensionCatalog, env kernel.TurnEnvironment) error {
	if service == nil {
		return nil
	}
	registration := agentruntime.RuntimeHookRegistration{
		ID:            outputValidatorDispatchHookID,
		Point:         agentruntime.HookBeforeResponse,
		Priority:      0,
		FailurePolicy: agentruntime.HookFailClosed,
		Hook:          newOutputValidatorDispatchHook(catalog, env),
	}
	if err := service.Hooks.Register(registration); err != nil {
		return fmt.Errorf("app: register output validator dispatch hook: %w", err)
	}
	return nil
}

// resolveOutputValidator 按声明 ID 解析实现：Options 优先，全集兜底。
// catalog 条目携带的独立 Timeout 一并返回（全集条目无独立超时）。
func resolveOutputValidator(catalog *kernel.ExtensionCatalog, agentID, id string) (extension.OutputValidator, time.Duration, error) {
	if entry, ok := catalog.Find(kernel.ExtOutputValidator, id); ok {
		impl, ok := entry.Implementation.(extension.OutputValidator)
		if !ok {
			return nil, 0, fmt.Errorf("app: extension %s does not implement extension.OutputValidator", id)
		}
		return impl, entry.Timeout, nil
	}
	if impl, ok := extension.LookupOutputValidator(id); ok {
		return impl, 0, nil
	}
	return nil, 0, fmt.Errorf("app: agent %s references unknown output validator %q (not registered via engine options or the global registry)", agentID, id)
}

// newOutputValidatorDispatchHook 把一次 before_response hook 调用适配为按
// Definition 声明顺序执行的 OutputValidator 链。
//
// 单条目语义（与既有逐条 hook 逐条对齐）：
//   - Accept：放行，校验结果写入 ScopedData（internal 可见性）供审计与
//     SDK 视图读取；
//   - Retry：返回可传输的 OutputRetryError。Harness 的子 Agent 执行器按
//     预算建立下一次模型尝试，并把 validator 提供的安全反馈附入输入；
//   - Fail：返回错误交由 hook 失败策略处置（fail_closed 使 Run 失败）。
func newOutputValidatorDispatchHook(catalog *kernel.ExtensionCatalog, env kernel.TurnEnvironment) agentruntime.RuntimeHook {
	return agentruntime.RuntimeHookFunc(func(ctx context.Context, input agentruntime.RuntimeHookInput) (agentruntime.RuntimeHookOutput, error) {
		ids := input.Run.Definition.OutputValidators
		if len(ids) == 0 {
			return agentruntime.RuntimeHookOutput{}, nil
		}
		if input.Response == nil {
			// 无响应视图（如累积超限）时不做校验；finalizeRun 会统一处置。
			return agentruntime.RuntimeHookOutput{}, nil
		}
		scoped := make(map[string]agentruntime.ScopedDataItem, len(ids))
		for _, entryID := range ids {
			validator, timeout, err := resolveOutputValidator(catalog, input.Run.Definition.AgentID, entryID)
			if err != nil {
				return agentruntime.RuntimeHookOutput{}, err
			}
			validateCtx, cancel := validatorContext(ctx, timeout)
			result, err := validator.Validate(validateCtx, extension.OutputValidateRequest{
				Ctx: extension.Context{
					InvocationKind: extension.InvocationRoot,
					TenantID:       input.Run.TenantID,
					UserID:         input.Run.UserID,
					SessionID:      input.Run.SessionID,
					RunID:          input.Run.RunID,
					ParentRunID:    input.Run.ParentRunID,
					RootRunID:      extensionRootRunID(input.Run.RunID, input.Run.ParentRunID),
					AgentID:        input.Run.Definition.AgentID,
					AgentVersion:   input.Run.Definition.Version,
					AgentBindingID: input.Run.AgentBindingID,
					TraceID:        input.Run.Trace.TraceID,
					Environment:    env.Environment,
					SDKVersion:     env.SDKVersion,
					SchemaVersions: env.SchemaVersions,
				},
				Text:    input.Response.Text,
				Attempt: 1,
			})
			cancel()
			if err != nil {
				return agentruntime.RuntimeHookOutput{}, fmt.Errorf("output validator %s: %w", entryID, err)
			}
			if result.Action == extension.OutputRetry {
				reason := result.Reason
				if reason == "" {
					reason = string(result.Action)
				}
				return agentruntime.RuntimeHookOutput{}, agentruntime.NewOutputRetryError(
					entryID, reason, result.RepairFeedback, result.RetryBudget,
				)
			}
			if result.Action != extension.OutputAccept {
				reason := result.Reason
				if reason == "" {
					reason = string(result.Action)
				}
				return agentruntime.RuntimeHookOutput{}, fmt.Errorf("output validator %s rejected final response: %s", entryID, reason)
			}
			record, err := json.Marshal(map[string]string{
				"action": string(result.Action),
				"reason": result.Reason,
			})
			if err != nil {
				return agentruntime.RuntimeHookOutput{}, err
			}
			scopedKey := "extension:" + string(kernel.ExtOutputValidator) + ":" + entryID
			scoped[scopedKey] = agentruntime.ScopedDataItem{
				Source:     scopedKey,
				Visibility: "internal",
				Value:      record,
			}
		}
		return agentruntime.RuntimeHookOutput{
			ScopedData: agentruntime.ScopedData{Run: scoped},
		}, nil
	})
}

func extensionRootRunID(runID, parentRunID string) string {
	if parentRunID != "" {
		return parentRunID
	}
	return runID
}

// validatorContext 为单个 validator 派生（可选超时的）子 context。
func validatorContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return ctx, func() {}
}
