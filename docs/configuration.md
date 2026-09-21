# Configuration / 配置

The supported local profile is `agenui-agent/configs/environments/local/agenui.toml`.
It starts the AGenUI Agent with SQLite state and public demo assets. The local
profile is the supported starting point for self-hosted use.

支持的本地 profile 是 `agenui-agent/configs/environments/local/agenui.toml`。它以
SQLite 状态和公开 demo 资产启动 AGenUI Agent；它是自托管场景的支持入口。

## Required local processes / 本地必需进程

| Process | Default address | Responsibility |
| --- | --- | --- |
| AGenUI Agent | `127.0.0.1:18081` | Generation, editing, console APIs, native Harness mounting |
| KnowRAG MCP | `127.0.0.1:18082` | SQLite knowledge and local published-operator detail API |
| Web console | `127.0.0.1:3100` | Model setup and AGenUI administration |

## State and model configuration / 状态与模型配置

| Setting | How to configure | Notes |
| --- | --- | --- |
| Studio database | `database.driver` + `AGENUI_DATABASE_DSN` | Local uses the same `modernc` SQLite driver and DELETE journal contract as Harness; set the driver to `mysql` and provide a MySQL DSN for production. |
| Model endpoint, key, model | Web console → Settings | Stored locally with encryption; the key is not returned to the browser. |
| Knowledge | KnowRAG MCP SQLite or compatible external MCP | The bundled database is a small public fixture. |
| Rules and operators | Web console | Revisions are persisted locally and published through the revision lifecycle. |
| Runtime package source base URL | `[export].data_source_base_url` | Expands relative API paths in downloaded packages; production knowledge may publish absolute endpoints instead. |
| Runtime Package delivery | Web console → Settings | HTTP callback is built in; MQ requires a deployment adapter registered for the package delivery port. |

Generation defaults to disabled deep reasoning and disabled prompt caching. A
deployment can select another compatible model, but quality, cost, and latency
remain that deployment's responsibility.

生成默认关闭深度思考和 prompt cache。部署方可以更换兼容模型，但质量、成本和延迟由
部署方自行验收。

## Public integration contract / 对外集成合同

Use the native Harness HTTP/SSE contract for external integrations. The AGenUI
HTTP routes serve the bundled console and do not
define a replacement public streaming protocol. The local/test profile accepts:

```text
X-AGenUI-Tenant-ID: <tenant>
X-AGenUI-User-ID: <user>
```

Production must install an authenticated `PrincipalResolver`; it does not
trust caller-supplied identity headers.

Rule publication is local and deterministic: the Worker writes an immutable
revision, atomically replaces the active pointer, and the Agent watcher reloads
it. Runtime Package publication uses one delivery port. Studio bundles the HTTP
callback implementation; deployments may register an MQ adapter only after
defining their consumer, acknowledgement, retry and dead-letter behavior.

规则发布采用本地确定性链路：Worker 写入不可变 revision，原子替换 active pointer，
Agent watcher 随后热加载。Runtime Package 发布共用一个投递端口；Studio 内置 HTTP
Callback，部署方应在明确消费、确认、重试和死信语义后注册 MQ Adapter。

## Replaceable deployment adapters / 可替换部署适配器

The local profile is a complete implementation, not an architectural limit:

| Boundary | Local default | Production option |
| --- | --- | --- |
| Client | Studio Web | Your application using native Harness HTTP/SSE |
| Durable state | SQLite | MySQL adapter |
| Hot state and live stream | Process-local memory | Redis for multi-instance deployment |
| Model | UI-configured OpenAI-compatible endpoint | Provider adapter or self-hosted model |
| Knowledge and data | SQLite BM25 MCP and local API catalog | External MCP, vector/search service or API catalog |
| Rules | Editable Markdown, immutable revisions and file watch | Compatible rule registry/provider |
| Operators | Published local TypeScript | Remote `OperatorRuntimePort` implementation |
| Rendering | Published catalog and Web renderer | Trimmed/full catalog and custom/native renderer |
| Identity, artifacts, telemetry | Local identity, files and logs | `PrincipalResolver`, external Artifact Store and OpenTelemetry |
| Runtime delivery | Download, core Runtime and HTTP callback | Your Runtime/orchestrator, callback service or MQ consumer |

本地 profile 是完整实现，不是架构上限。替换适配器时应保持对应端口的类型、版本、幂等和
错误合同；不要在外围重新包装 Harness 的 Run/SSE 协议。

See [API reference](api-reference.md) and
[Management API](management-api.md).
