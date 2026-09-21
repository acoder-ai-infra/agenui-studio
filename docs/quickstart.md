# Quick start / 快速开始

AGenUI Studio runs the AGenUI Agent on the native Harness runtime. The
local profile uses SQLite and a bundled BM25 knowledge MCP; no external
database, broker, or vector service is needed for the first run.

AGenUI Studio 通过原生 Harness 运行 AGenUI Agent。本地 profile 使用 SQLite 和
内置 BM25 知识 MCP；首次运行不需要外部数据库、消息队列或向量服务。

## Start everything / 一键启动

```sh
cd web
npm install
cd ..
make dev
```

The local demo exposes the bundled knowledge MCP and the published Operator
Detail API. It includes only small public fixtures. Add real knowledge through
the management console or replace it with compatible services.

本地服务同时提供内置 Knowledge MCP 和已发布算子的 Detail API，只包含少量公开示例数据。
可通过管理后台维护真实知识，或替换为兼容服务。

The Agent listens at `http://127.0.0.1:18081`; the local data service listens at
`http://127.0.0.1:18082`. Both processes use `var/agenui/studio.db`. This shared local data plane is required
for a console-published TypeScript operator to be resolved by the runtime
adapter. 两个进程必须共用 `var/agenui/studio.db`，否则后台发布的算子无法被运行时查询。

Open `http://127.0.0.1:3100/admin/home`, then open Settings to configure your
model endpoint, API key, and model name. The key is stored locally in encrypted
form and is never returned by the API.

打开 `http://127.0.0.1:3100/admin/home`，进入设置页配置模型地址、API Key 和模型名。
密钥仅以本机加密形式存储，API 不会回显。

## Verify a run / 验证生成

Use the workspace to create a card. For external integrations, use the native
Harness event stream:

```text
GET /api/v1/sessions/{session_id}/runs/{run_id}/events
Accept: text/event-stream
```

The stream schema is `harness.sse.v1`; AGenUI Studio does not add a second AGenUI event
envelope.

## Next steps / 下一步

- Maintain APIs, operators, rules, and knowledge in the management console.
- Replace the local SQLite MCP with your own compatible MCP service when needed.
- Observe rule publication from the immutable revision directory and atomic
  active-pointer file. Add external delivery in your application only when it
  has a real consumer and defined retry semantics.
- See [Configuration](configuration.md) for runtime configuration and
  [Knowledge MCP](knowledge-mcp.md) for the knowledge contract.
