# Architecture / 架构

AGenUI Studio keeps one execution runtime and one mutable UI document. Harness
owns the Agent lifecycle; AGenUI tools own the generated interface.

AGenUI Studio 只有一套执行运行时和一份可变 UI 文档。Harness 负责 Agent 生命周期，
AGenUI 工具负责生成式界面。

![AGenUI Studio architecture](assets/architecture.svg)

Solid boxes are the stable product path. Dashed blue boxes are ports: the
bundled implementation makes local startup simple, while production can replace
the adapter without introducing a second generation protocol or state machine.

实线框是稳定产品主链，蓝色虚线框是可替换端口。内置实现负责降低本地启动成本；生产部署
替换适配器时不需要另建生成协议或第二套状态机。

![AGenUI Studio extension points](assets/extension-points.png)

## The Workspace tool / Workspace 工具

The model does not rewrite one large DSL string. It uses bounded operations on
a versioned document. Every mutation carries the previous `revision`; stale or
concurrent writes fail instead of overwriting newer work.

模型不再重写一整段大型 DSL，而是对带版本的文档调用有界方法。每次修改都携带上一次
`revision`；过期或并发写入会失败，不会覆盖新结果。

- `begin` creates identity and the preview value tree.
- `put_components` adds or replaces at most 32 components by stable ID.
- `set_slots` publishes semantic field/action slots and the rule receipt. The
  Host derives stable IDs from component, Contract and property identities;
  callers do not invent `slotId` values.
- `set_preview_data` supplies readable, non-sensitive examples. `commit`
  reports exact empty paths, rejects slots that reference missing components,
  verifies each field slot is connected to its preview-data path, and checks
  that every required frozen Contract item/action has a semantic slot. It also
  compiles and freezes the Requirement projection in the same atomic commit;
  the Main Agent does not run a second compiler. Every
  mutation invalidates the previous commit, so an unchecked draft can never
  inherit an older committed state.
- `inspect` returns an index or selected components, not the complete document.
- `apply_edit_contract` applies only Host-authorized paths. The model cannot
  enlarge the target set or protected set.
- `inspect_binding` returns requirements, slots and authorized sources without
  exposing the component tree. `commit_binding` accepts one typed Bind Result,
  validates transforms and protected baselines, then persists Binding and Final
  artifacts before reporting success. Ordinary field/action mappings are
  derived from frozen facts.
- A binding-only continuation starts with `agenui_prepare_binding_edit`. This
  turns the Main Agent's semantic decision into a single-use typed route fact;
  the model never writes internal task markers.
- Every result returns the current revision, completion counts, preview state
  and bounded `next_actions`. Components use one canonical flat shape; a nested
  tool-call shape is normalized only at this syntax boundary, then the same
  catalog and structure checks run.

`begin` 创建文档身份和预览数据树；`put_components` 按稳定 ID 分批增量写组件；
`set_slots` 写语义字段/动作槽位和规则回执，模型不填写 `slotId`，Host 根据组件、契约和
属性身份稳定生成；`set_preview_data` 写可读且无敏感信息的
示例值。`commit` 会指出空值的精确路径，校验槽位引用真实组件并已连接预览数据；任意修改
还会检查冻结 Contract 中的必填内容和动作均有语义槽位，并在同一次原子提交中编译、冻结
Requirement，主 Agent 不再二次编译。任意修改都会使旧 commit
失效，保证未校验草稿不会继承旧提交状态。编辑只能通过冻结的
Edit Contract 应用；Binder 只读取 Requirement、槽位和授权数据源投影，不读取整棵组件树。
Binder 通过 `commit_binding` 只提交一份 Bind Result；Workspace 在同一次提交中校验
transforms、保护基线并持久化 Binding/Final Artifact，普通执行映射由冻结事实派生。纯绑定修改先调用
`agenui_prepare_binding_edit`，把主 Agent 的语义判断转成一次性类型化路由事实；
模型不再填写内部任务标记。

## Model decisions and Host facts / 模型判断与 Host 事实

Natural-language intent belongs to the model. The Host exposes facts and
enforces exact identities: component IDs, requirement IDs, property paths,
catalog membership, revisions, hashes and protected paths. It does not contain
Chinese keyword tables, field-name aliases or domain-specific object guesses.

自然语言意图由模型判断。Host 只提供并校验事实：组件 ID、Requirement ID、属性路径、
Catalog 成员、revision、哈希和保护路径；不维护中文关键词表、字段名别名或业务对象猜测。

## Binding admission and materialization / Binding 准入与物化

Workspace is the only admission boundary. It validates the typed Bind Result
against frozen requirements, slots, source identities and paths, plus durable
operator receipts. The deterministic Materializer then expands the admitted
field/action mappings without guessing, repairing or dropping model decisions.
The immutable Final Artifact is written to Harness Artifact Store.

Workspace 是唯一准入边界：按冻结的 Requirement、槽位、数据源身份与路径，以及持久算子回执
校验类型化 Bind Result。确定性 Materializer 只展开已通过准入的字段和动作映射，不猜测、
修复或删除模型决策；最终产物写入 Harness Artifact Store。

The Workspace is a Run-local transaction view, not another durable state
store. Harness already records every model/tool call. A successful Style or
Binder commit is materialized once as an immutable Design or Binding artifact;
the deterministic Materializer reads those artifacts and writes the Final
Artifact. For a
style-only continuation, the frozen Edit Contract protects and reuses the
previous Binding/source artifacts instead of asking Binder to rewrite them.

Capability preflight is advisory evidence, not a design admission gate. A
user's explicit interface intent is frozen in the Content Contract and is not
removed because a matching API has not yet been registered. Style commits a
complete, non-sensitive preview so the card can be reviewed first. Binder then
maps what is available and records typed issues for the rest. Only
`finalize_delivery` requires every runtime-required mapping: preview data never
crosses that delivery boundary as disguised production data.

能力预检提供建议性证据，不是设计准入门禁。用户明确的界面意图会冻结在 Content Contract 中，
不会因为尚未注册匹配 API 而被删除。Style 必须先提交完整、无敏感信息的预览，用户可以先看卡片；
Binder 再映射已有来源，并为其余项记录类型化 issue。只有 `finalize_delivery` 要求全部运行时必需映射
通过；预览数据不会伪装成生产数据跨过交付边界。

Workspace 是 Run 内的事务视图，不是另一套持久状态库。Harness 已记录所有模型与工具调用；
Style/Binder 成功提交后才分别物化一次不可变 Design/Binding Artifact；Materializer
读取已提交事实并写入 Final Artifact。设计属性续写由冻结 Edit Contract 保护并复用上一轮
Binding/数据源，不会让 Binder 重新改写。

## Ownership and projections / 数据归属与投影

| Fact | Owner | Studio behavior |
| --- | --- | --- |
| Session, Run, Message, Event | Harness | Read through the public SDK; never copy into a second conversation store |
| Checkpoint, Control, Resume, Usage | Harness | Use native Harness behavior and event stream |
| Model/tool execution | Harness | Configure Agents and tools; do not build a second workflow engine |
| Content/Edit Contract | AGenUI | Store as Run-scoped artifacts because they are generation-domain facts |
| Workspace draft | AGenUI | Run-local transaction view; not persisted as a second mutable DSL or event log |
| Design, requirements, binding and Final Artifact | AGenUI | Store immutable bodies in Harness Artifact Store; derive the manifest from artifact references |
| Console timeline | Studio | Ephemeral/read-optimized projection rebuilt from Harness messages and AGenUI artifacts; never authoritative |

Session、Run、Message、Event、Checkpoint、Control、Resume、Usage 均以 Harness 为
唯一事实源。AGenUI 的 Workspace 草稿只存在于当前 Run；内容/编辑契约及已提交的 Design、
Requirement、Binding 与最终产物属于生成域事实，大对象写入 Harness Artifact Store；
AGenUI Manifest 只从 Artifact 引用派生，不复制 Run 生命周期状态。
后台时间线只是可重建投影，不参与执行判定。

## Extension boundaries / 扩展边界

- Use SQLite and process-local hot state for a one-command local run; switch
  durable SQL to MySQL and multi-instance hot state/live streams to Redis.
- Replace the local BM25 MCP, API catalog, rule provider, or operator runtime
  through their documented interfaces.
- Publish a full or trimmed renderer catalog by version and checksum.
- Use the bundled Web renderer and core Runtime as references, or install a
  custom/native renderer and your own Runtime/orchestrator.
- Deliver a portable Runtime Package by download, the built-in HTTP callback,
  or an MQ consumer installed on the same delivery port.
- Replace local identity, file artifacts and logs with an authenticated
  `PrincipalResolver`, external Artifact Store and OpenTelemetry backend.
- Add an application output validator only in deployment configuration. The
  open-source profile registers none.
- Rule authors upload Markdown. The Worker produces typed documents and
  atomically publishes an immutable revision.

- 本地一键启动使用 SQLite 和进程内热状态；线上可把持久化 SQL 切到 MySQL，并以 Redis
  支撑多实例热状态与直播事件。
- 可通过既有接口替换本地 BM25 MCP、API Catalog、规则 Provider 或算子运行时。
- 可按版本和 checksum 发布完整或裁剪的 Renderer Catalog。
- 可使用内置 Web Renderer 与核心 Runtime，也可安装自定义/原生 Renderer 和自建
  Runtime/编排服务。
- Runtime Package 可下载、通过内置 HTTP Callback 投递，或由安装在同一投递端口上的
  MQ Consumer 接收。
- 可将本地身份、文件 Artifact 和日志替换为认证 `PrincipalResolver`、外部 Artifact Store
  与 OpenTelemetry 后端。
- 业务门禁只能由部署方显式注册，开源默认配置不注册任何业务输出 Validator。
- 用户上传 Markdown 规则；Worker 生成结构化文档并原子发布不可变 revision。
