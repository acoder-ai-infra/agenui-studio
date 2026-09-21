package extension

import (
	"context"
	"encoding/json"
)

// 本文件定义 function 工具的宿主实现契约。设计原则（rc.6 收敛）：
//
//   - 工具的定义与治理属性（input/output schema、allowed_agents、risk_level、
//     timeout、结果策略）只在 tools.yaml 配置目录中维护，不在代码中声明；
//   - ToolProvider 只提供实现（handler 名 → Go 函数体），执行统一经
//     Tool Gateway（schema 校验、agent ACL、事件落账、超时、大结果溢出）；
//   - 契约形状镜像 Tool Gateway 的 FunctionCall/FunctionResult（extension 是
//     公开包，不 import internal，Composition Root 负责逐字段映射）。

// FunctionCall 镜像 Tool Gateway 的函数工具调用形状。
type FunctionCall struct {
	// Ctx 携带身份与环境（tenant/user/session/run/agent/trace）。
	Ctx Context
	// Name / Version 是 tools.yaml 中该工具的名称与版本。
	Name    string
	Version string
	// Arguments 是模型给出的 JSON 入参，已由 Tool Gateway 依据 tools.yaml
	// 的 input_schema 完成校验。
	Arguments json.RawMessage
}

// FunctionResult 镜像 Tool Gateway 的函数工具结果形状。大结果无需特殊
// 处理：超过 tools.yaml artifact_threshold_bytes 的结果由 gateway 自动
// 溢出为 PayloadRef artifact。
type FunctionResult struct {
	Data         json.RawMessage
	MimeType     string
	Presentation *ResultPresentation
}

// ResultPresentation is optional user-facing metadata for one tool result.
// It is stored on the existing tool_call_completed event; it does not create a
// second progress event or change the model-facing Data contract.
type ResultPresentation struct {
	Title   string
	Summary string
	Details []ResultPresentationDetail
}

type ResultPresentationDetail struct {
	Label string
	Value string
}

// FunctionTool 是一款 function 工具的宿主实现体。实现可以捕获宿主进程内
// 资源（业务库连接池等）；必须响应 ctx 的取消（gateway 按 tools.yaml 的
// timeout_ms 设截止）。
type FunctionTool interface {
	// Name 必须等于 tools.yaml 中该工具 handler 字段的值。禁用 "harness."
	// 前缀（内置 handler 保留段，Build 时 fail-closed）。
	Name() string
	// Invoke 执行一次工具调用。
	Invoke(ctx context.Context, call FunctionCall) (*FunctionResult, error)
}

// ToolProvider 打包一组 function 工具实现；不在代码中声明任何工具定义。
// 可以有多个 ToolProvider 共存（不同业务模块各自打包）；实现名在 Engine
// 级全局唯一，重名在 Build 时 fail-closed。tools.yaml 中 handler 引用的实现
// 先查 Options 注册（WithToolProvider），未命中再查全集（RegisterTools），
// 两处都没有时 Build 失败；注册了实现但无任何工具引用时在
// BuildReport.Extensions 中标注为 unbound。
type ToolProvider interface {
	// ID 返回稳定的提供者 ID（ExtensionCatalog 去重与 BuildReport 展示用）。
	ID() string
	FunctionTools() []FunctionTool
}
