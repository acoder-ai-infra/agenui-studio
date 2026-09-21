package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/cloudwego/eino/adk"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/AGenUI/agenui-studio/harness/internal/agentregistry"
	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// extension_model_hooks.go 把 SDK 注册的 BeforeModelHook /
// ToolCallInterceptor 扩展投影为 eino 的 ChatModelAgentMiddleware，经
// EinoRuntime.PreModelHandlers 通道注入 DeepAgent（方案 3.3 / 3.4）：
//
//   - before_model_hook → BeforeModelRewriteState：每轮模型调用前塑形
//     请求（改写消息序列、收窄本轮可见工具集、调整 ModelCallOptions）；
//   - interceptor → WrapInvokableToolCall：环绕平台工具 / MCP 工具 / task
//     子 Agent 调用；
//   - 改写发生时以 debug 事件（runtime_step_completed + processor_hook）
//     留痕，不新增事件类型；扩展错误一律 fail closed。
//
// agent 级绑定语义：两类扩展都按 Definition 声明的 ID 列表筛选执行
//（未声明不执行），ID 先查 Options 冻结的 ExtensionCatalog，未命中再查
// 全集（extension.LookupXxx），两处都没有 fail-closed。子 agent 的 task
// 调用会以自己的 Definition 另起 child run 重新 Resolve，天然不继承父
// agent 的绑定；父 agent 的 interceptor 仍环绕 task 工具调用本身（那是
// 父 run 的工具调用事实），子 run 内部的模型/工具调用只受子 agent
// 配置约束。
//
// 工具可见层/治理层切分（ADR-006）：Agent 配置工具集是本 Run 不可变
// 能力快照（Tool Gateway ACL 依据）；BeforeModelHook 只影响可见层——本轮
// 请求携带哪些工具定义。Hook 返回不在候选集合内的工具名会被忽略并留痕；
// kernel 由既有 modelToolCallAuthorized 保证模型发出的 tool call 必须属于
// 本轮可见集，不可见即不可执行。

// resolveBeforeModelHook 按声明 ID 解析实现：Options 优先，全集兜底。
func resolveBeforeModelHook(catalog *kernel.ExtensionCatalog, agentID, id string) (extension.BeforeModelHook, error) {
	if entry, ok := catalog.Find(kernel.ExtBeforeModelHook, id); ok {
		impl, ok := entry.Implementation.(extension.BeforeModelHook)
		if !ok {
			return nil, fmt.Errorf("app: extension %s does not implement extension.BeforeModelHook", id)
		}
		return impl, nil
	}
	if impl, ok := extension.LookupBeforeModelHook(id); ok {
		return impl, nil
	}
	return nil, fmt.Errorf("app: agent %s references unknown before model hook %q (not registered via engine options or the global registry)", agentID, id)
}

// resolveToolCallInterceptor 按声明 ID 解析实现：Options 优先，全集兜底。
func resolveToolCallInterceptor(catalog *kernel.ExtensionCatalog, agentID, id string) (extension.ToolCallInterceptor, error) {
	if entry, ok := catalog.Find(kernel.ExtToolCallInterceptor, id); ok {
		impl, ok := entry.Implementation.(extension.ToolCallInterceptor)
		if !ok {
			return nil, fmt.Errorf("app: extension %s does not implement extension.ToolCallInterceptor", id)
		}
		return impl, nil
	}
	if impl, ok := extension.LookupToolCallInterceptor(id); ok {
		return impl, nil
	}
	return nil, fmt.Errorf("app: agent %s references unknown tool call interceptor %q (not registered via engine options or the global registry)", agentID, id)
}

// preflightAgentExtensionBindings 在 Build 期对文件加载的 agent 做扩展绑定
// 存在性预检（Options + 全集），失败 fail-closed。DB 动态发布的 agent 无法
// 静态预检，由运行期解析兜底。
func preflightAgentExtensionBindings(configs []agentregistry.AgentConfig, catalog *kernel.ExtensionCatalog) error {
	for _, cfg := range configs {
		for _, id := range cfg.Extensions.BeforeModelHooks {
			if _, err := resolveBeforeModelHook(catalog, cfg.AgentID, id); err != nil {
				return err
			}
		}
		for _, id := range cfg.Extensions.ToolCallInterceptors {
			if _, err := resolveToolCallInterceptor(catalog, cfg.AgentID, id); err != nil {
				return err
			}
		}
		for _, id := range cfg.Extensions.OutputValidators {
			if _, _, err := resolveOutputValidator(catalog, cfg.AgentID, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// newExtensionPreModelHandlerResolver 包装既有 resolver：compaction handler
// 之后追加扩展投影（hook 看到的是压缩后的最终输入）。每 Run resolve 时按
// Definition 声明的 ID 顺序新建 middleware 实例，轮次计数按 Run 隔离；
// 未声明的 Run 零成本直通。catalog 可为 nil（全集仍可被引用）。
func newExtensionPreModelHandlerResolver(inner agentruntime.EinoPreModelHandlerResolver, catalog *kernel.ExtensionCatalog, env kernel.TurnEnvironment) agentruntime.EinoPreModelHandlerResolver {
	return agentruntime.EinoPreModelHandlerResolverFunc(func(ctx context.Context, req agentruntime.EinoPreModelHandlerRequest) ([]adk.ChatModelAgentMiddleware, error) {
		var handlers []adk.ChatModelAgentMiddleware
		if inner != nil {
			var err error
			handlers, err = inner.Resolve(ctx, req)
			if err != nil {
				return nil, err
			}
		}
		if len(req.Definition.BeforeModelHooks) > 0 {
			hooks := make([]beforeModelHookBinding, 0, len(req.Definition.BeforeModelHooks))
			for _, id := range req.Definition.BeforeModelHooks {
				impl, err := resolveBeforeModelHook(catalog, req.Definition.AgentID, id)
				if err != nil {
					return nil, err
				}
				hooks = append(hooks, beforeModelHookBinding{id: id, impl: impl})
			}
			handlers = append(handlers, &extensionBeforeModelMiddleware{
				hooks: hooks, env: env,
				// 本 Run 冻结的 scoped-data 只读视图（入口层 ScopedData +
				// RunInitializer 产出），每轮以深拷贝交给 Hook。
				scopedData: scopedDataToExtensionEntries(req.ScopedData),
			})
		}
		for _, id := range req.Definition.ToolCallInterceptors {
			impl, err := resolveToolCallInterceptor(catalog, req.Definition.AgentID, id)
			if err != nil {
				return nil, err
			}
			handlers = append(handlers, &extensionInterceptorMiddleware{extensionID: id, impl: impl, env: env})
		}
		return handlers, nil
	})
}

// beforeModelHookBinding 是一个已解析的 Hook 及其声明 ID。
type beforeModelHookBinding struct {
	id   string
	impl extension.BeforeModelHook
}

// extensionBeforeModelMiddleware 把本 Agent 声明的 BeforeModelHook 链投影为
// eino 模型前钩子。round 计数以本 middleware 实例（即本 Run 的一次 resolve）
// 为界；首轮快照下来的 Agent 配置工具全集作为基线，每轮从该基线重新
// 开始收窄（ADR-006：轮间不单调）。
type extensionBeforeModelMiddleware struct {
	adk.BaseChatModelAgentMiddleware
	hooks []beforeModelHookBinding
	env   kernel.TurnEnvironment
	// scopedData 是本 Run 冻结的 scoped-data 视图（只读）。
	scopedData map[string]extension.ScopedDataEntry
	round      atomic.Int64
	// baselineTools 是首轮进入时的 Agent 配置工具全集快照；后续轮次不再
	// 读已被上轮收窄改写过的 state.ToolInfos，保证“每轮从全集重新开始”。
	baselineOnce  sync.Once
	baselineTools []*schema.ToolInfo
}

// PreCapturePreModelHandler 标记：hook 在 preserve capture 之前执行
// （塑形后的输入才是冻结校验基线）。
func (m *extensionBeforeModelMiddleware) PreCapturePreModelHandler() {}

func (m *extensionBeforeModelMiddleware) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, mc *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	round := int(m.round.Add(1))
	hookCtx := extensionModelHookContext(ctx, m.env)
	// 首轮快照 Agent 配置工具全集作为基线；后续轮次候选集从基线重新
	// 开始（不继承上轮写回 state.ToolInfos 的收窄结果）。
	m.baselineOnce.Do(func() {
		m.baselineTools = append([]*schema.ToolInfo(nil), state.ToolInfos...)
	})
	candidate := toolDefinitionsFromEino(m.baselineTools)
	messages := beforeModelMessagesFromEino(state.Messages)
	// ToolChoice 基线为空（沿用冻结 ModelOptions / provider 默认）；Hook 显式
	// 设置后经 ctx 覆盖下发到受治理模型代理。
	options := extension.ModelCallOptions{}
	for _, hook := range m.hooks {
		beforeMessagesHash := beforeModelFingerprint(messages)
		result, err := hook.impl.BeforeModel(ctx, extension.BeforeModelRequest{
			Ctx: hookCtx, Round: round, Messages: messages, Tools: candidate, Options: options,
			// 每个 Hook 拿到独立深拷贝，改写不影响共享快照与后续 Hook。
			ScopedData: cloneExtensionScopedData(m.scopedData),
		})
		if err != nil {
			return ctx, state, fmt.Errorf("before model hook %s: %w", hook.id, err)
		}
		if len(result.Messages) == 0 {
			return ctx, state, fmt.Errorf("before model hook %s returned empty messages", hook.id)
		}
		// 工具可见集只能从候选集合中选择：未知名忽略并留痕（容忍端上报
		// 未收录工具名），结果作为下一个 Hook 的候选集合。
		visible, dropped := intersectToolDefinitions(candidate, result.Tools)
		if err := validateToolChoiceVisible(result.Options.ToolChoice, visible); err != nil {
			return ctx, state, fmt.Errorf("before model hook %s: %w", hook.id, err)
		}
		afterMessagesHash := beforeModelFingerprint(result.Messages)
		emitBeforeModelHookEvent(ctx, hook.id, round, beforeMessagesHash, afterMessagesHash, len(messages), len(result.Messages), candidate, visible, dropped, options.ToolChoice, result.Options.ToolChoice)
		messages = result.Messages
		candidate = visible
		options = result.Options
	}
	rewritten, err := einoMessagesFromBeforeModel(messages)
	if err != nil {
		return ctx, state, err
	}
	state.Messages = rewritten
	// 收窄本轮模型可见工具集：从首轮基线全集中回选（eino 在
	// BeforeModelRewriteState 后按 state.ToolInfos 下发 model.WithTools）。
	// 下一轮仍从 baselineTools 重新开始，收窄不跨轮累积。
	state.ToolInfos = einoToolInfosFromDefinitions(m.baselineTools, candidate)
	// ToolChoice 经 ctx 覆盖下发到 EinoChatModelProxy（每轮生效）。
	if options.ToolChoice != "" {
		ctx = agentruntime.WithModelCallToolChoice(ctx, options.ToolChoice)
	}
	return ctx, state, nil
}

// extensionInterceptorMiddleware 把 ToolCallInterceptor 投影为 eino 工具环绕。
type extensionInterceptorMiddleware struct {
	adk.BaseChatModelAgentMiddleware
	extensionID string
	impl        extension.ToolCallInterceptor
	env         kernel.TurnEnvironment
}

// PreCapturePreModelHandler 标记：工具环绕不参与 preserve 三明治，与
// hook 同位排序，避免被冻结校验误判。
func (m *extensionInterceptorMiddleware) PreCapturePreModelHandler() {}

func (m *extensionInterceptorMiddleware) WrapInvokableToolCall(ctx context.Context, endpoint adk.InvokableToolCallEndpoint, tCtx *adk.ToolContext) (adk.InvokableToolCallEndpoint, error) {
	name, callID := "", ""
	if tCtx != nil {
		name, callID = tCtx.Name, tCtx.CallID
	}
	return func(callCtx context.Context, argumentsInJSON string, opts ...einotool.Option) (string, error) {
		originalResult := ""
		executed := false
		next := extension.ToolCallNext(func(nextCtx context.Context, arguments json.RawMessage) (extension.ToolCallOutcome, error) {
			raw, err := endpoint(nextCtx, string(arguments), opts...)
			if err != nil {
				return extension.ToolCallOutcome{}, err
			}
			originalResult, executed = raw, true
			return extension.ToolCallOutcome{Result: raw}, nil
		})
		outcome, err := m.impl.Intercept(callCtx, extension.ToolCallInfo{
			Ctx:       extensionModelHookContext(callCtx, m.env),
			Name:      name,
			CallID:    callID,
			Arguments: json.RawMessage(argumentsInJSON),
		}, next)
		if err != nil {
			return "", fmt.Errorf("tool call interceptor %s: %w", m.extensionID, err)
		}
		// 模型可见结果被改写（或短路合成）时留痕；Tool Gateway 落账的
		// 原始结果不受影响。
		if !executed || outcome.Result != originalResult {
			payload := map[string]any{
				"tool_name": name, "tool_call_id": callID, "executed": executed,
				"rewritten_hash": hashText(outcome.Result),
			}
			if executed {
				payload["original_hash"] = hashText(originalResult)
			}
			emitProcessorHookEvent(callCtx, "tool_call_interceptor", m.extensionID, payload)
		}
		return outcome.Result, nil
	}, nil
}

// processorHookIDs 为留痕 step 生成唯一 StepID。
var processorHookIDs = observability.NewULIDGenerator("exthook")

// emitBeforeModelHookEvent 汇总一次 BeforeModelHook 的塑形结果并留痕：仅在
// 消息、可见工具集或 ToolChoice 任一发生变化时发事件。
func emitBeforeModelHookEvent(ctx context.Context, extensionID string, round int, beforeMessagesHash, afterMessagesHash string, beforeCount, afterCount int, candidate, visible []extension.ToolDefinition, dropped []string, beforeChoice, afterChoice string) {
	messagesChanged := beforeMessagesHash != afterMessagesHash
	toolsChanged := len(candidate) != len(visible) || len(dropped) > 0
	choiceChanged := beforeChoice != afterChoice
	if !messagesChanged && !toolsChanged && !choiceChanged {
		return
	}
	detail := map[string]any{"round": round}
	if messagesChanged {
		detail["original_hash"] = beforeMessagesHash
		detail["rewritten_hash"] = afterMessagesHash
		detail["original_count"] = beforeCount
		detail["rewritten_count"] = afterCount
	}
	if toolsChanged {
		detail["visible_tools"] = toolNames(visible)
		detail["visible_count"] = len(visible)
		detail["candidate_count"] = len(candidate)
		if len(dropped) > 0 {
			detail["dropped_tools"] = dropped
		}
	}
	if choiceChanged {
		detail["tool_choice"] = afterChoice
	}
	emitProcessorHookEvent(ctx, "before_model_hook", extensionID, detail)
}

// emitProcessorHookEvent 以 debug visibility 的 runtime_step_started/completed
// 成对事件记录一次扩展改写（StepKind=processor_hook，不新增事件类型）。
// step 契约要求 StepID + 起始 Kind；缺一不可（persistAdapterEvent 对非法
// 形状 fail closed）。无 emitter（如单测直连）时静默跳过。
func emitProcessorHookEvent(ctx context.Context, hook, extensionID string, detail map[string]any) {
	emitter, ok := agentruntime.RuntimeEventEmitterFrom(ctx)
	if !ok || emitter == nil {
		return
	}
	stepID := processorHookIDs.NewRequestID()
	startPayload, err := json.Marshal(map[string]any{
		"step_id": stepID,
		"kind":    string(agentruntime.StepKindProcessorHook),
		"name":    hook + ":" + extensionID,
	})
	if err != nil {
		return
	}
	completedPayload, err := json.Marshal(map[string]any{
		"step_id": stepID,
		"kind":    string(agentruntime.StepKindProcessorHook),
		"hook":    hook, "extension_id": extensionID, "detail": detail,
	})
	if err != nil {
		return
	}
	// 留痕失败不影响主链路（事件桥满/关闭时丢弃 debug 痕迹可接受）。
	_ = emitter.Emit(ctx, observability.AgentEvent{
		EventType:      observability.EventRuntimeStepStarted,
		StepID:         stepID,
		Visibility:     observability.VisibilityDebug,
		Payload:        startPayload,
		PayloadPreview: startPayload,
	})
	_ = emitter.Emit(ctx, observability.AgentEvent{
		EventType:      observability.EventRuntimeStepCompleted,
		StepID:         stepID,
		Visibility:     observability.VisibilityDebug,
		Payload:        completedPayload,
		PayloadPreview: completedPayload,
	})
}

// newExtensionNativeModelTransform 把 before_model_hook 绑定投影为 native
// direct 循环的模型前塑形钩子（方案 3.3 native 路径）。绑定是 per-agent
// 动态的，因此钩子无条件安装：Definition 未声明 hook 的 Run 直通零成本。
func newExtensionNativeModelTransform(catalog *kernel.ExtensionCatalog, env kernel.TurnEnvironment) agentruntime.NativeModelInvokeTransform {
	return func(ctx context.Context, def agentruntime.AgentDefinition, req agentruntime.ModelInvokeRequest) (agentruntime.ModelInvokeRequest, error) {
		if len(def.BeforeModelHooks) == 0 {
			return req, nil
		}
		messages := req.Messages
		if len(messages) == 0 {
			// 与 gatewayModelInvoker 同源物化：空 Messages 时从冻结 window 取。
			messages = modelCallMessages(req.Package.Messages.ConversationWindow)
		}
		hookCtx := extensionModelHookContext(ctx, env)
		current := beforeModelMessagesFromModelCall(messages)
		// 候选工具集同样与 Messages 对称物化：native 路径 runModelRound 每轮
		// 新建 ModelInvokeRequest（Tools 本轮由 governor 稍后从冻结能力快照物化），
		// 因此 Hook 阶段需从 Package 能力快照取全集，否则候选集为空。与 eino 路径
		// 一致：每轮从 Agent 配置全集重新开始（ADR-006）。
		baseTools := req.Tools
		if len(baseTools) == 0 {
			baseTools = req.Package.Capabilities.ToolDefinitions
		}
		candidate := toolDefinitionsFromModelCall(baseTools)
		options := extension.ModelCallOptions{ToolChoice: req.Options.ToolChoice}
		// 本 Run 冻结的 scoped-data 只读视图（每个 Hook 拿到独立深拷贝）。
		scoped := scopedDataToExtensionEntries(req.ScopedData)
		for _, id := range def.BeforeModelHooks {
			impl, err := resolveBeforeModelHook(catalog, def.AgentID, id)
			if err != nil {
				return req, err
			}
			beforeMessagesHash := beforeModelFingerprint(current)
			result, err := impl.BeforeModel(ctx, extension.BeforeModelRequest{
				Ctx: hookCtx, Round: req.Round, Messages: current, Tools: candidate, Options: options,
				ScopedData: cloneExtensionScopedData(scoped),
			})
			if err != nil {
				return req, fmt.Errorf("before model hook %s: %w", id, err)
			}
			if len(result.Messages) == 0 {
				return req, fmt.Errorf("before model hook %s returned empty messages", id)
			}
			visible, dropped := intersectToolDefinitions(candidate, result.Tools)
			if err := validateToolChoiceVisible(result.Options.ToolChoice, visible); err != nil {
				return req, fmt.Errorf("before model hook %s: %w", id, err)
			}
			afterMessagesHash := beforeModelFingerprint(result.Messages)
			emitBeforeModelHookEvent(ctx, id, req.Round, beforeMessagesHash, afterMessagesHash, len(current), len(result.Messages), candidate, visible, dropped, options.ToolChoice, result.Options.ToolChoice)
			current = result.Messages
			candidate = visible
			options = result.Options
		}
		req.Messages = modelCallMessagesFromBeforeModel(current)
		// 从本轮基线全集中回选可见子集（保留完整 schema）。
		req.Tools = modelToolDefinitionsFromCandidate(baseTools, candidate)
		req.AllowTools = len(req.Tools) > 0
		req.Options.ToolChoice = options.ToolChoice
		return req, nil
	}
}

func extensionModelHookContext(ctx context.Context, env kernel.TurnEnvironment) extension.Context {
	trace := observability.MustTraceContext(ctx)
	return extension.Context{
		InvocationKind: extension.InvocationRoot,
		TenantID:       trace.TenantID,
		UserID:         trace.UserID,
		SessionID:      trace.SessionID,
		RunID:          trace.RunID,
		ParentRunID:    trace.ParentRunID,
		RootRunID:      trace.RootRunID,
		AgentID:        trace.AgentID,
		TraceID:        trace.TraceID,
		Environment:    env.Environment,
		SDKVersion:     env.SDKVersion,
		SchemaVersions: env.SchemaVersions,
	}
}

func beforeModelFingerprint(messages []extension.BeforeModelMessage) string {
	data, err := json.Marshal(messages)
	if err != nil {
		return ""
	}
	return hashBytes(data)
}

func hashText(value string) string {
	return hashBytes([]byte(value))
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:16])
}
