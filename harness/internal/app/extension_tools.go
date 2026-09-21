package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// extension_tools.go 把 SDK 注册的 ToolProvider（rc.6 重定义：纯实现提供者）
// 接入 Tool Gateway 的 function handler 表。
//
// 工具的定义与治理属性（schema/allowed_agents/risk/timeout/结果策略）只在
// tools.yaml 中维护；ToolProvider 只提供 handler 名 → Go 实现的映射。
// tools.yaml 的 function 工具经配置目录组装路径引用这些 handler（引用未
// 注册实现时 buildConfiguredTools 既有校验 fail-closed），执行时统一走
// Tool Gateway（schema 校验、agent ACL、事件落账、超时、大结果溢出）。

// builtinHandlerPrefix 是内置 handler 的保留命名段。
const builtinHandlerPrefix = "harness."

// collectExtensionFunctionHandlers 遍历 catalog 的 tool_provider 条目，把
// 每个 FunctionTool 以 Name 为键包装成 toolgateway.FunctionTool。约束
// （均 Build fail-closed）：实现名非空、禁用 harness. 前缀、跨 provider
// 不得重名。
func collectExtensionFunctionHandlers(catalog *kernel.ExtensionCatalog, env kernel.TurnEnvironment) (map[string]toolgateway.FunctionTool, error) {
	if catalog == nil {
		return nil, nil
	}
	entries := catalog.ByKind(kernel.ExtToolProvider)
	if len(entries) == 0 {
		return nil, nil
	}
	handlers := make(map[string]toolgateway.FunctionTool)
	owners := make(map[string]string)
	for _, entry := range entries {
		provider, ok := entry.Implementation.(extension.ToolProvider)
		if !ok {
			return nil, fmt.Errorf("app: extension %s does not implement extension.ToolProvider", entry.ID)
		}
		for _, tool := range provider.FunctionTools() {
			if tool == nil {
				return nil, fmt.Errorf("app: tool provider %s returned a nil FunctionTool", entry.ID)
			}
			name := strings.TrimSpace(tool.Name())
			if name == "" {
				return nil, fmt.Errorf("app: tool provider %s declared a FunctionTool without a name", entry.ID)
			}
			if strings.HasPrefix(name, builtinHandlerPrefix) {
				return nil, fmt.Errorf("app: tool provider %s handler %s uses the reserved %q prefix", entry.ID, name, builtinHandlerPrefix)
			}
			if owner, exists := owners[name]; exists {
				return nil, fmt.Errorf("app: handler %s is declared by both tool provider %s and %s", name, owner, entry.ID)
			}
			owners[name] = entry.ID
			handlers[name] = newExtensionFunctionHandler(tool, env)
		}
	}
	return handlers, nil
}

// mergeGlobalFunctionHandlers 把全集（extension.RegisterTools）注册的
// FunctionTool 合并进 handler 表作为兜底层：Options（WithToolProvider）已
// 占用的名字优先；"harness." 前缀保留段 fail-closed（内置 handler 全部
// 位于该前缀下，因此全集与内置不会重名）。
func mergeGlobalFunctionHandlers(handlers map[string]toolgateway.FunctionTool, env kernel.TurnEnvironment) (map[string]toolgateway.FunctionTool, error) {
	globals := extension.GlobalFunctionTools()
	if len(globals) == 0 {
		return handlers, nil
	}
	if handlers == nil {
		handlers = make(map[string]toolgateway.FunctionTool, len(globals))
	}
	for name, tool := range globals {
		if strings.HasPrefix(name, builtinHandlerPrefix) {
			return nil, fmt.Errorf("app: global function tool %s uses the reserved %q prefix", name, builtinHandlerPrefix)
		}
		if _, exists := handlers[name]; exists {
			// Options 注册优先于全集（解析优先级契约）。
			continue
		}
		handlers[name] = newExtensionFunctionHandler(tool, env)
	}
	return handlers, nil
}

// newExtensionFunctionHandler 把一次 gateway function 调用映射为
// extension.FunctionTool.Invoke（toolgateway.FunctionCall → 镜像契约）。
func newExtensionFunctionHandler(tool extension.FunctionTool, env kernel.TurnEnvironment) toolgateway.FunctionTool {
	return func(ctx context.Context, call toolgateway.FunctionCall) (*toolgateway.FunctionResult, error) {
		result, err := tool.Invoke(ctx, extension.FunctionCall{
			Ctx: extension.Context{
				InvocationKind: extension.InvocationRoot,
				TenantID:       call.Trace.TenantID,
				UserID:         call.Trace.UserID,
				SessionID:      call.Trace.SessionID,
				RunID:          call.Trace.RunID,
				ParentRunID:    call.Trace.ParentRunID,
				RootRunID:      call.Trace.RootRunID,
				AgentID:        call.Trace.AgentID,
				TraceID:        call.Trace.TraceID,
				Environment:    env.Environment,
				SDKVersion:     env.SDKVersion,
				SchemaVersions: env.SchemaVersions,
			},
			Name:      call.ToolName,
			Version:   call.ToolVersion,
			Arguments: call.Arguments,
		})
		if err != nil {
			return nil, classifyExtensionFunctionError(err)
		}
		if result == nil {
			return nil, fmt.Errorf("app: handler %s returned a nil FunctionResult", tool.Name())
		}
		return &toolgateway.FunctionResult{
			Data:         result.Data,
			MimeType:     result.MimeType,
			Presentation: extensionResultPresentation(result.Presentation),
		}, nil
	}
}

func extensionResultPresentation(source *extension.ResultPresentation) *toolgateway.ResultPresentation {
	if source == nil {
		return nil
	}
	details := make([]toolgateway.ResultPresentationDetail, 0, len(source.Details))
	for _, detail := range source.Details {
		details = append(details, toolgateway.ResultPresentationDetail{Label: detail.Label, Value: detail.Value})
	}
	return &toolgateway.ResultPresentation{Title: source.Title, Summary: source.Summary, Details: details}
}

func classifyExtensionFunctionError(err error) error {
	var functionErr *extension.FunctionError
	if !errors.As(err, &functionErr) {
		// A local extension handler is not an upstream transport. Untyped errors
		// remain internal until the provider explicitly declares otherwise.
		return toolgateway.NewToolError(toolgateway.ErrorTypeInternal, "extension function handler failed", false, err)
	}
	switch functionErr.Type {
	case extension.FunctionErrorInvalidArgument:
		return toolgateway.NewToolError(toolgateway.ErrorTypeInvalidArgument, functionErr.Message, false, err)
	case extension.FunctionErrorPermissionDenied:
		return toolgateway.NewToolError(toolgateway.ErrorTypePermissionDenied, functionErr.Message, false, err)
	case extension.FunctionErrorUnavailable:
		return toolgateway.NewToolError(toolgateway.ErrorTypeUpstreamError, functionErr.Message, functionErr.Retryable, err)
	case extension.FunctionErrorInternal:
		return toolgateway.NewToolError(toolgateway.ErrorTypeInternal, functionErr.Message, false, err)
	default:
		return toolgateway.NewToolError(toolgateway.ErrorTypeInternal, "extension function handler returned an unknown error type", false, err)
	}
}
