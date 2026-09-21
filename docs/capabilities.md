# Capability and acceptance guide / 能力与验收说明

This document describes the supported public behavior of AGenUI Studio. It is
an acceptance guide, not a claim that every model will produce identical UI.

本文说明 AGenUI Studio 对外支持的行为和验收方式。它是验收说明，不承诺不同模型
会生成完全相同的界面结果。

![AGenUI Studio generation and protected editing loop](assets/generation-loop.svg)

## Core generation and editing / 生成与编辑核心

Each request is executed as a durable Harness run. Production applications
consume the native run event stream; AGenUI does not define a second public
streaming protocol. The bundled console has a page-oriented presentation
projection, but it neither owns execution state nor replaces Harness replay.
Sessions, runs, messages, steps, checkpoints, interrupts and usage records are
persisted by Harness so an interrupted run can be inspected and resumed.

每次请求都作为可持久化的 Harness Run 执行。生产应用直接消费原生运行事件流，AGenUI
不定义第二套公开流协议。内置后台有一层面向页面的展示投影，但不拥有执行状态，也不替代
Harness replay。Harness 持久化 session、run、message、step、checkpoint、interrupt 和 usage，
因此中断运行可检查并恢复。

Before Style chooses a layout, the main Agent freezes a content contract from
the user's request. Style performs one broad knowledge/API search and asks the
Host to preserve each required content or action capability as `unknown` when
there are candidates, or `unavailable` when no candidate exists, from evidence
observed in the current Run. It is guidance for design, not early data binding
or a visual gate: a user's explicit component, hierarchy, interaction, or
reference-image intent remains authoritative even when the evidence is
unavailable. When the request leaves the interface open-ended, Style should
prefer expressions supported by the evidence so the card does not invent an
unimplementable data model. Exact API, field and operator selection remains
Binder's responsibility.

Style builds the interface through the versioned Workspace tool rather than
emitting one large DSL blob. It writes components in bounded batches, publishes
semantic slots, fills readable demo values, and commits against the active
renderer catalog and frozen Contract identities. Slot IDs are derived by the
Host; the model supplies component and Contract identities, not global IDs.
The same commit atomically freezes the compiled Requirement projection, so a
committed Design cannot fail a second downstream admission. Binder receives only the
requirement/slot/source projection. The Workspace draft is Run-local; only
successful Design/Binding/Final artifacts become durable.

Every Workspace result includes bounded `next_actions`, so the model can recover
from schema/catalog errors without rereading the whole DSL or repeating successful
mutations. The first Style commit must contain complete non-sensitive demo values;
therefore the renderer shows the intended card before any real API is bound.

Style 通过带版本的 Workspace 工具构建界面，不一次输出完整大型 DSL：组件分批写入，
语义槽位单独发布，示例数据补齐后再按当前 Renderer Catalog 提交。Binder 只接收
Requirement、槽位和授权数据源投影。
槽位 ID 由 Host 根据组件和冻结契约稳定生成；同一次 commit 原子冻结编译后的 Requirement，
不存在 Design 已提交后又被下游二次准入拒绝的双轨。
Workspace 草稿只存在于当前 Run，只有成功提交的 Design、Binding 与 Final Artifact 持久化。
每次工具结果都返回有界 `next_actions`，模型可从 Schema/Catalog 错误恢复，无需重读整份
DSL 或重复成功写入。首次 Style commit 必须补齐无敏感信息的示例值，因此真实 API 尚未绑定时
Renderer 也能完整展示预期卡片。

Style 选择布局前，主 Agent 先从用户原话冻结内容契约。Style 做一次宽松知识/API 检索；Host
仅依据当前 Run 的真实证据，在有候选时保留 `unknown`，无候选时标记 `unavailable`，不冒充模型
判断语义可用性。它用于指导设计，不提前绑定数据、更不是视觉门禁：用户明确指定的组件、层级、交互
或参考图意图始终优先，即使暂无数据源也必须生成可编辑的示例预览；只有用户没有明确元素或布局时，
才优先采用有能力证据支撑的表达，避免无依据地发散。精确 API、字段和算子仍由 Binder 选择。

An unbound Design is a valid draft, not a failed card. It remains editable and
renders only non-sensitive preview data. Binder records a typed issue for each
requirement that cannot be mapped, including its expected shape and the next
action for the user. A downloadable or publishable Runtime Package is the
separate boundary: it is created only after all required runtime mappings are
admitted. Preview data is never silently exported as runtime data.

未绑定的 Design 是有效草稿，不是失败卡片：它仍可编辑，并只渲染无敏感信息的预览数据。
Binder 会为无法映射的 Requirement 记录类型化 issue，说明期望的数据形态和用户可执行的下一步。
可下载、可发布的 Runtime Package 是独立边界，只有全部必需运行时映射通过准入后才会产生；预览数据
绝不会被悄悄导出为运行时数据。

For a follow-up edit, the host constructs an edit contract from the current
artifact: target set, requested property changes, protected set, impact set,
base revision and preconditions. The model selects within the supplied facts;
the host rejects an invented target, a property absent from that component's
real `editable_paths`, a protected object change, or a stale base revision.
This is intentionally not a keyword-based edit rule engine.

多轮编辑时，宿主从当前产物构造编辑契约：目标集合、请求属性改动、保护集合、影响集合、
基准版本和前置条件。模型只能在给出的事实中选择；宿主会拒绝虚构目标、不在组件真实
`editable_paths` 中的属性、保护对象改动及过期基准版本。这不是按中文关键词匹配的编辑规则引擎。

Acceptance: generate a card, ask for one visible property change, then compare
the final artifact with the prior revision. The requested property must change;
the protected object hashes and the base-revision fence must remain valid.

验收：先生成卡片，再要求只修改一个可见属性，对比前后产物。请求的属性必须变化；
保护对象哈希和基准版本 fencing 必须仍然有效。

## Rule and knowledge lifecycle / 规则与知识生命周期

The bundled profile starts with SQLite and local files. Knowledge retrieval uses
the bundled SQLite BM25 path; rules, operators, API definitions and knowledge
documents are managed from Studio. A rule revision is an immutable directory.
Publishing atomically advances a small local pointer and reloads the same
provider in process. Runtime Package delivery is separate from rule publication:
Studio bundles HTTP Callback delivery, while deployments can install an MQ
adapter after defining a concrete consumer and retry contract.

内置配置使用 SQLite 和本地文件启动。知识检索使用内置 SQLite BM25 路径；规则、算子、
接口定义和知识文档都可在 Studio 中维护。规则 revision 是不可变目录。发布会原子推进本地
pointer，并在进程内热加载同一 provider。只有部署方明确真实消费者和重试语义后，才在
同一 Runtime Package 投递端口安装 MQ Adapter；HTTP Callback 由 Studio 内置提供。

Parsing success creates an immutable revision and atomically advances the
active pointer. Deployments that require approval can wrap publication at
their application boundary; Studio does not publish a speculative governance API.

解析成功后生成不可变 revision，并原子推进活动指针。需要审批的部署可在自身应用边界包装
发布动作；Studio 不预设尚未稳定的治理接口。

## Local-first, extensible by boundary / 本地优先，按边界扩展

Minimum local setup is a model endpoint/key configured from Settings, SQLite,
and the bundled demo assets. No remote configuration service, object storage,
or enterprise database is required. For production, replace model, knowledge,
rule provider, operator/API, durable storage, hot state, identity, Artifact Store,
renderer or Runtime Package delivery adapters at their documented boundaries while
retaining the same run and validation semantics.

最小本地配置只需在“设置”中配置模型地址和密钥，加上 SQLite 与内置示例资产；不依赖远程
配置服务、对象存储或企业数据库。生产环境可在文档化边界替换模型、知识、算子/API、存储、
规则 Provider、热状态、身份、Artifact Store、Renderer 或 Runtime Package 投递适配器，
同时保留相同的运行和校验语义。

## What is deliberately not promised / 不作承诺的内容

- Generated visual quality is model-, prompt-, and knowledge-dependent.
- Studio does not bundle a managed vector database, message broker, remote
  configuration center, object store, or proprietary knowledge/rule content.
- The built-in console is an administration and testing workspace; an
  application should integrate the native run/event API rather than automate
  the console UI.
- The public default contains no application-specific business, style,
  template, analytics, design-token, or visual-source validator. New policy hooks
  should be added only when their contract is stable enough to document and
  test independently.

- 生成视觉质量取决于模型、提示词和知识质量。
- Studio 不内置托管向量库、消息队列、远程配置中心、对象存储或任何专有知识/规则内容。
- 内置后台用于管理和测试；业务应用应接入原生运行/事件 API，而不是自动化后台页面。
- 开源默认不携带业务、样式、模板、埋点、Design Token 或视觉稿门禁。只有当扩展合同能够独立
  文档化并测试后，才新增对应接口。
