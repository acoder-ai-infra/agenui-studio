# Renderer catalog / 渲染 catalog

The renderer catalog is the one published protocol contract for AGenUI
components. It supplies the component/property context for rule parsing,
constrains generation, and is read by the final component-availability check.
That check only rejects component types absent from the active catalog. Normal
protocol schema validation remains responsible for message shape and component
structure; the open-source default has no application-specific style, business,
or template validator.

渲染 catalog 是 AGenUI 组件唯一的已发布协议合同。它向规则解析提供组件/属性上下文，约束
生成，并被最终组件可用性检查读取。该检查只拒绝当前 catalog 中不存在的组件类型。消息结构和
组件结构仍由 AGenUI 协议 schema 检查；开源默认不包含业务、样式或模板门禁。

## Catalog choices / 如何选择 catalog

The base protocol catalog is at
`agenui-agent/pkg/agenui/schema/assets/0.9/basic_catalog.json`. A client that
fully renders every base component can publish it unchanged. A constrained
client should copy it and remove unsupported component schemas before release.
Catalogs are protocol JSON; they are not Markdown rule documents.

基础协议 catalog 位于
`agenui-agent/pkg/agenui/schema/assets/0.9/basic_catalog.json`。完整渲染所有基础组件的客户端可
原样发布它；能力受限的客户端应复制它，并在发布前移除不支持的组件 schema。catalog 是协议 JSON，
不是 Markdown 规则文档。

Studio's bundled renderer renders every component in the base catalog, and the
default `1.1.0` release publishes that full catalog. Publishing a later full or
trimmed catalog is a versioned catalog update, not a prompt change.

Studio 内置 renderer 已渲染基础 catalog 中的全部组件，默认 `1.1.0` release 已发布完整基础
catalog。后续发布完整或裁剪 catalog 都应作为带版本的 catalog 更新，而不是修改提示词。

## Publish an update / 发布更新

1. Create `agenui-agent/configs/renderer-catalogs/<version>/catalog.json`.
2. Add its exact SHA-256, catalog ID, version, and `"published"` status to
   `index.json`; point `current.json` at the same release.
3. Add renderer acceptance coverage for every newly allowed component, then
   restart the Agent process. The new process freezes the selected catalog into
   its Harness prompts; Worker passes and final component-availability checks read `current.json`
   on their next operation.

1. 创建 `agenui-agent/configs/renderer-catalogs/<版本>/catalog.json`。
2. 在 `index.json` 中写入文件精确 SHA-256、catalog ID、版本和 `"published"` 状态；再将
   `current.json` 指向同一 release。
3. 为每个新允许组件补充 renderer 验收覆盖，然后重启 Agent 进程。新进程会将选中的 catalog
   固化进 Harness 提示；Worker 批次和最终组件可用性检查会在下一次操作时重新读取 `current.json`。

The pointer and index use `catalog_version`, `catalog_id`, and
`catalog_sha256`. `catalog_version` is only a release identity; it does not
enable a policy validator.

pointer 与 index 使用 `catalog_version`、`catalog_id` 和 `catalog_sha256`。
`catalog_version` 只是发布版本，不代表启用业务门禁。

## Prompt size / 提示大小

Rule parsing receives a deterministic authoring view derived from the selected
catalog: `catalogId`, all component schemas, and the transitive `$defs` actually
referenced by those components. Runtime functions and theme metadata are
omitted. It is not a second configuration and never a
validation source; final validation always reads the full checksum-verified
catalog. Generation retains the full protocol catalog because it also needs
the runtime function contract.

规则解析接收从选中 catalog 确定性派生的 authoring view：`catalogId`、全部组件 schema 和这些
组件传递引用到的 `$defs`。其中移除了运行时 functions 和主题元数据。它不是第二份配置，也绝不作为验证来源；最终
校验始终读取完整且通过 checksum 验证的 catalog。生成链路仍保留完整协议 catalog，因为它还需要
运行时函数合同。
