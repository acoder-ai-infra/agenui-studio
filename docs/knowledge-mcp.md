# SQLite KnowRAG MCP / SQLite 知识 MCP

The bundled public MCP service is `agenui-agent/cmd/agenui-knowrag-mcp`; the
all-in-one local service is `agenui-agent/cmd/agenui-local-demo`.

It implements the AGenUI knowledge tool contracts over SQLite with a CJK/Latin
BM25 tokenizer. The management console and the MCP service use the same local
data plane, so users can add and revise knowledge without a separate service.

## 中文说明

公开内置 MCP 命令是 `agenui-agent/cmd/agenui-knowrag-mcp`；一键本地服务使用
`agenui-agent/cmd/agenui-local-demo`。它在 SQLite 上实现 AGenUI 的知识工具合同，并采用
支持中英文的 BM25 分词与排序，不依赖向量数据库。

管理后台与 MCP 共用同一份本地数据，因此用户可直接新增和修改知识、接口与示例数据。
生产环境可以用兼容 MCP 替换本地实现；模型负责从工具返回的候选中做语义选择，Host 只校验
来源、结构、字段路径和不可变 revision，不做关键词业务路由。
