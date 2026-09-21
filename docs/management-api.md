# Management API / 管理接口

Studio provides local-first CRUD for the bundled console. Runtime execution,
history, reconnect and HITL always use native Harness APIs.

Studio 为内置后台提供本地优先的 CRUD；运行、历史、重连与 HITL 始终使用 Harness 原生接口。

| Resource / 资源 | Routes / 路由 | Semantics / 语义 |
| --- | --- | --- |
| Model connection / 模型连接 | `GET/POST /api/v1/agenui/admin/local/model` | Encrypted local key; never echoed / 本地加密，密钥不回显 |
| API definitions / 接口定义 | `GET/POST /api/v1/agenui/admin/local/apis` | Same facts exposed by local MCP / 与本地 MCP 共用事实 |
| Knowledge search / 知识检索 | `GET /api/v1/agenui/admin/local/knowledge/search` | SQLite BM25 |
| Operators / 算子 | `/api/v1/agenui/admin/local/operators/*` | System operators are idempotently published at startup; user drafts remain mutable and published versions immutable / 系统算子启动时幂等发布；用户草稿可改，发布版不可变 |
| Rules / 规则 | `/api/v1/agenui/admin/local/rules/*` | Markdown upload, worker job, immutable revision / Markdown 上传、Worker 解析、不可变 revision |
| Demo initialization / 示例初始化 | `POST /api/v1/agenui/admin/local/init-demo` | Idempotent public fixtures / 幂等公开示例 |
| Final artifact / 最终产物 | `GET /api/v1/agenui/agent/sessions/{id}/final` | Latest immutable AGenUI artifact |
| Runtime package / 运行包 | `GET /api/v1/agenui/agent/sessions/{id}/package` | Expanded sources, bindings and operators / 展开数据源、绑定与算子 |
| Publication target / 发布目标 | `GET/POST /api/v1/agenui/admin/local/publication-target` | Configure callback or an installed MQ adapter / 配置回调或已安装的 MQ 适配器 |
| Publish package / 发布运行包 | `POST /api/v1/agenui/agent/sessions/{id}/publish` | Export first, then deliver with a stable idempotency key / 先导出自包含包，再以稳定幂等键投递 |

These routes do not expose another Session or Run state machine. External
services should authenticate and proxy them according to their own deployment
requirements; the local profile is intended for development and evaluation.

这些接口不定义第二套 Session/Run 状态机。外部部署应按自身要求增加认证和代理；本地 profile
用于开发与评测。
