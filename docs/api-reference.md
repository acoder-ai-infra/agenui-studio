# HTTP API Reference / HTTP API 参考

AGenUI Studio intentionally exposes a small integration surface. The API is
split between native Harness runtime endpoints and AGenUI-owned local
management/package endpoints; there is no second event protocol.

- Native Harness Run event replay: `GET /api/v1/sessions/{session_id}/runs/{run_id}/events`
  with `Accept: text/event-stream` returns `harness.sse.v1` without an AGenUI wrapper.
- Generation and continuation: `POST /api/v1/agents/{agent_id}/chat`.
- Native chat stream and reconnect:
  `GET /api/v1/sessions/{session_id}/runs/{run_id}/chat-stream`.
- Transcript recovery: `GET /api/v1/sessions/{session_id}/chat-transcript`.
- HITL response: `POST /api/v1/control-requests/{control_id}/responses`.
- Runtime-ready package download for the latest completed generation:
  `GET /api/v1/agenui/agent/sessions/{id}/package`. The response is a
  self-contained JSON attachment with expanded data sources, bindings,
  actions and published operators.
- Runtime Package publication: `POST /api/v1/agenui/agent/sessions/{id}/publish`
  exports the same self-contained package and sends it to the target registered
  through the local management API. HTTP callback delivery is built in; an
  embedding application can install an MQ adapter for the same delivery port.
- Console management APIs and local demo CRUD are documented in
  [Management API](management-api.md).

The public core does not expose an API for overwriting generated template,
binding, or API artifacts. Follow-up changes start another governed Run and use
the edit contract (target, requested changes, protected set, impact set and
base-revision preconditions).

`X-AGenUI-Tenant-ID` and `X-AGenUI-User-ID` are accepted only by the local/test
profile. Production deployments must install an authenticated
`PrincipalResolver`; the process refuses to start in production without one.
The Web console proxy supplies its local development identity server-side.

## 中文说明

AGenUI Studio 只公开两类接口：Harness 原生运行接口，以及 AGenUI 自己拥有的本地管理和
运行包下载接口。生成与续写使用 `POST /api/v1/agents/{agent_id}/chat`；事件回放、聊天流、
会话 transcript 和 HITL 恢复均直接使用上面的 Harness 路由，不增加第二套 SSE 包装。

`GET /api/v1/agenui/agent/sessions/{id}/package` 下载最新完成运行的自包含 JSON 包，其中
数据源、绑定、动作和已发布算子均已展开。生成中间产物不能通过公开 API 直接覆盖；多轮修改
必须新建受 Harness 管理的 Run，并经过目标集合、改动集合、保护集合和基准版本校验。

`POST /api/v1/agenui/agent/sessions/{id}/publish` 会先生成同一份自包含包，再投递到后台注册的
目标。内置支持 HTTP Callback；部署方可为同一发布端口安装 MQ Adapter，不需要改变制卡链路。

`X-AGenUI-Tenant-ID` 与 `X-AGenUI-User-ID` 仅供 local/test profile 使用；生产环境必须安装
经过认证的 `PrincipalResolver`。
