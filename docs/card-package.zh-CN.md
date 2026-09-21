# Card Execution Package

[English](card-package.md)

Card Execution Package 是 Runtime 的输入契约。它是**自包含**的：数据源和算子引用
都展开为完整定义，执行时不需要访问 Studio。Studio 会从会话最后一次成功生成中组装并
下载 Package；不可变发布、活跃版本选择和回滚仍由宿主控制面负责。

版本为 `1.0`。完整示例见
[`demo/packages/sample-package.json`](../demo/packages/sample-package.json)，机器可读
Schema 见 [`docs/schemas/card-package.schema.json`](schemas/card-package.schema.json)。

## 顶层结构

Package 包含冻结的 Content Contract、AGenUI `protocol`、完整 `dataSources`、
`bindings`、`operators`、`actions` 和用于追溯的 `meta`。

## 数据源

必须有且只有一个 `primary`，其余数据源为 `supplement`。主源和补充源并行执行，
补充源只能按 `entityKey` 补全同一实体，不支持数据源串联、循环或通用 DAG。

`itemsPath` 指向响应中的实体数组；`params[].template` 可用 `{{name}}` 引用执行参数。

## Binding

每条 Binding 把 `dataSourceId` 的 `fieldPath` 投影到卡片或列表实体的 `target`。
`scope` 为 `card` 或 `list_item`；`missingPolicy` 为 `block`、`hide` 或
`fallback`。`transform[]` 按顺序通过 `operatorVersionId` 引用
`operators[].operatorVersionId`。

## 算子

每个算子直接使用后台已发布 Operator Detail 的字段，不增加字段转换层：
`operatorVersionId`、数字类型的 `version`、`sourceHash`、`language`、
`languageVersion`、`sourceCode` 和 `entry`。`language` 支持
`javascript`/`js` 和 `typescript`/`ts`。
TypeScript 先转为 JavaScript，再由原 Goja 路径在新 VM 中执行
`entry(value, params)`。

Goja 仍在宿主进程内运行；超时和大小限制不是恶意代码的内存/OOM 隔离边界。算子失败
会直接中止执行，错误中带 Binding 与算子 ID；修正算子或输入后可人工重试。

## 动作

每个 Action 使用 Package 本地 `dataSourceId`、响应 `path` 和所属 `componentId`。
Runtime 从与字段 Binding 相同的数据快照解析动作值，写入 `resolvedActions`，并同步投影到
DataModel 的 `/__actions/{componentId}/{type}`。未展开的数据源、缺失路径或组件会在加载或
执行阶段直接报错。

## 校验与交付边界

`runtime.Load` 校验必要字段、唯一主数据源、Binding 的数据源/算子引用和 Scope，以及 Action
的数据源、路径和组件引用。
可在生成页面下载，也可调用
`GET /api/v1/agenui/agent/sessions/{session_id}/package`。Studio 返回前会把数据源和算子
ID 展开为 Package 内的本地定义。可执行
`go run ./runtime/cmd/agenui-runtime package.json`，也可嵌入 `runtime.Load` 与
`runtime.Engine`。工作台“发布”按钮会导出同一份自包含 Package，并使用稳定幂等键投递到
后台注册的目标。项目内置 HTTP Callback；MQ 通过同一 Package Delivery Port 安装适配器。
活跃版本选择和回滚仍由宿主控制面负责。
