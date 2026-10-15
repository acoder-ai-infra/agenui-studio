# Card Execution Package

[English](card-package.md)

Card Execution Package 是 Runtime 的输入契约。它是**自包含**的：数据源和算子引用
都展开为完整定义，执行时不需要访问 Studio。Studio 会从会话最后一次成功生成中组装并
下载 Package；不可变发布、活跃版本选择和回滚仍由宿主控制面负责。

版本为 `2.0`。完整示例见
[`runtime/testdata/sample-package.json`](../runtime/testdata/sample-package.json)，机器可读
Schema 见 [`runtime/package.schema.json`](../runtime/package.schema.json)。

## 顶层结构

```jsonc
{
  "version": "2.0",
  "cardId": "demo-product-list",
  "name": "Product list",
  "contract": { /* 冻结的 Content Contract，仅用于追溯，不参与执行 */ },
  "protocol": [
    {"version": "v0.9", "createSurface": {"surfaceId": "card", "catalogId": "agenui"}},
    {"version": "v0.9", "updateComponents": {"surfaceId": "card", "components": [ /* ... */ ]}},
    {"version": "v0.9", "updateDataModel": {"surfaceId": "card", "path": "/", "value": { /* 预览数据 */ }}}
  ],
  "dataSources": [ /* 展开的 API 定义 */ ],
  "bindings":    [ /* 槽位到字段的投影及转换链 */ ],
  "operators":   [ /* 包含源码和版本的完整算子定义 */ ],
  "actions":     [ /* 声明的交互意图 */ ],
  "meta":        { /* 哈希、生成器、时间戳 */ }
}
```

## 数据源

必须有且只有一个 `primary`，其余数据源为 `supplement`。

| 字段 | 含义 |
| --- | --- |
| `id` | Package 内的数据源 ID（如 `ds-1`、`ds-2`），不使用 Studio 内部 ID。 |
| `endpoint`、`method`、`headers` | API 调用方式。 |
| `role` | `primary` 为实体列表主源，`supplement` 补全同一实体。 |
| `itemsPath` | 响应中的实体数组路径，如 `$.items`，不允许通配符。 |
| `entityKey` | 将补充源记录匹配到主源实体的字段。 |
| `params[]` | 请求参数；`template` 支持用 `{{name}}` 引用执行参数。 |

主源和补充源并行执行，补充源只能补全同一实体，不支持数据源串联、循环或通用 DAG。

## Binding

| 字段 | 含义 |
| --- | --- |
| `slotId` / `requirementId` | 语义来源：设计槽位和编译后的 Requirement。 |
| `dataSourceId` | 必须引用 `dataSources[].id`。 |
| `fieldPath` | 响应中的完整源路径，如 `$.product.price` 或 `$.items[*].price_cents`。 |
| `refKey` | DataModel 中的完整绝对目标路径，如 `/product/price` 或 `/items[*]/price`。 |
| `missingPolicy` | `block` 中止执行，`hide` 跳过该槽位，`fallback` 使用 `fallbackValue`。 |
| `fallbackValue` | 源值缺失且策略为 `fallback` 时使用的值，在转换链执行前填入。 |
| `transforms[]` | 按顺序执行的算子调用；每个 `operatorVersionId` 必须引用 `operators[].operatorVersionId`。 |

路径支持对象键、固定数组索引和最多两层通配符。`refKey` 含通配符时，`fieldPath` 必须有
相同数量的通配符；转换链对每个匹配值分别执行，并保留其坐标。`refKey` 不含通配符时，
完整源值或投影数组进入转换链，结果一次性写入目标。Runtime 不会隐式聚合列表或取第一个元素。

## 算子

每个算子直接使用后台已发布 Operator Detail 的字段，不增加字段转换层：
`operatorVersionId`、`operatorKey`、数字类型的 `version`、`inputSchema`、
`paramsSchema`、`outputSchema`、`sourceHash`、`language`、`sourceCode` 和 `entry`，
以及可选的 `languageVersion`。`sourceHash` 必须是原始 `sourceCode` 的 SHA-256 哈希，
带 `sha256:` 前缀。Runtime 按包内 Schema 校验实际输入、参数和输出。`language` 支持
`javascript`/`js` 和 `typescript`/`ts`。
TypeScript 先转为 JavaScript，再由原 Goja 路径在新 VM 中执行
`entry(value, params)`，并施加超时和大小限制。

Goja 仍在宿主进程内运行；超时和大小限制不是恶意代码的内存/OOM 隔离边界。算子失败
会直接中止执行，错误中带 Binding 与算子 ID；修正算子或输入后可人工重试。

## 动作

每个 Action 使用 Package 本地 `dataSourceId`、响应 `path` 和所属 `componentId`。
Runtime 从与字段 Binding 相同的数据快照解析动作值，写入 `resolvedActions`，并同步投影到
DataModel 的 `/__actions/{componentId}/{type}`。未展开的数据源、缺失路径或组件会在加载或
执行阶段直接报错。

## 追溯元数据

`meta` 中的 `contractHash`、`designHash`、`requirementsHash` 用于回放和追溯；
`generator` 标识生成该包的 Studio 版本。

## 校验规则

`runtime.Load` 执行以下校验：

- `version` 必须为 `2.0`；必须提供 `cardId` 和非空 `protocol`。
- 必须有且只有一个主数据源。
- 每条 Binding 引用已声明的数据源；每次转换调用的 `operatorVersionId` 引用已声明的已发布算子版本。
- 每条 Binding 必须有合法的源路径 `fieldPath`、绝对目标路径 `refKey` 和缺失策略；通配符坐标遵循上述规则。
- 补充源 Binding 的目标必须包含通配符，且主源和补充源必须使用相同的非空 `entityKey`。
- 每个算子必须包含已发布的标识、版本、可执行源码、匹配的源码哈希和三个合法 Schema；调用参数必须符合 `paramsSchema`。
- `meta` 必须包含 Contract、Design 和 Requirements 的来源哈希。
- 每个 Action 引用已声明的数据源，并提供路径和所属组件。

## 交付边界

可在生成页面下载，也可调用
`GET /api/v1/agenui/agent/sessions/{session_id}/package`。Studio 返回前会把数据源和算子
ID 展开为 Package 内的本地定义。可执行
`go run ./runtime/cmd/agenui-runtime package.json`，也可嵌入 `runtime.Load` 与
`runtime.Engine`。工作台“发布”按钮会导出同一份自包含 Package，并使用稳定幂等键投递到
后台注册的目标。项目内置 HTTP Callback；MQ 通过同一 Package Delivery Port 安装适配器。
活跃版本选择和回滚仍由宿主控制面负责。
