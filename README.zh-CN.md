# AGenUI Studio

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

[English](README.md) · [文档](docs/README.md) · [更新日志](CHANGELOG.md)

AGenUI Studio 是一个可自托管的 AGenUI 智能界面工作台：在后台配置模型、知识、接口、算子和规则，
生成并实时预览卡片，再通过多轮对话持续修改。

![AGenUI Studio 将意图转为规则驱动、完成绑定并可执行的界面](docs/assets/product-overview.png)

_一条真实生成主链：契约优先、规则驱动设计、数据与算子绑定、受保护多轮编辑，最终交付
可移植 Runtime Package；中间画面来自绑定仓库内置 `demo.offers` 数据源的公开安全运行。_

## 真实生成示例

<table>
  <tr>
    <td><img src="docs/assets/showcase-weekly-picks.jpg" alt="Studio 实际生成的本周精选卡" width="240"></td>
    <td><img src="docs/assets/showcase-ticket-inventory.jpg" alt="Studio 实际生成的票务列表卡" width="240"></td>
    <td><img src="docs/assets/showcase-hotel-results.jpg" alt="Studio 实际生成的酒店结果卡" width="240"></td>
  </tr>
  <tr>
    <td align="center">图片、优惠、价格与主操作</td>
    <td align="center">票务标签、原价与重复列表</td>
    <td align="center">酒店结果与可执行数据绑定</td>
  </tr>
  <tr>
    <td><img src="docs/assets/showcase-coffee-offers.jpg" alt="Studio 实际生成的咖啡优惠卡" width="240"></td>
    <td><img src="docs/assets/showcase-route-weather.jpg" alt="Studio 实际生成的路线天气卡" width="240"></td>
    <td><img src="docs/assets/showcase-offers-card-detail.png" alt="无设备外壳的优惠卡细节预览" width="240"></td>
  </tr>
  <tr>
    <td align="center">图片、分隔线、状态与优惠券动作</td>
    <td align="center">路线、天气、预警与动态状态</td>
    <td align="center">无设备外壳的卡片细节预览</td>
  </tr>
</table>

_前四张来自 Studio 使用已配置模型、规则与 Workspace 工具完成的真实运行，并由 Renderer 实际渲染。为便于
查看版式细节，最后一张采用无设备外壳的展示方式。示例覆盖图片、重复列表、价格、标签、动作和数据绑定；
线上原始素材、业务数据及内部规则不随本仓库分发。_

## 从一句话到可执行界面

![AGenUI Studio 生成与受保护多轮编辑闭环](docs/assets/generation-loop.svg)

模型负责理解语义和设计；带版本的 Workspace 工具约束 DSL 的创建、绑定和修改方式；最终生成
已经展开的 Runtime Package，无需重新连接 Studio 即可执行。

## 新项目如何复刻一张卡

上面的示例并不依赖隐藏模板或业务代码。启动本地项目后，可直接复制[生成提示词、编辑提示词和
小型 Markdown 规则示例](docs/reproduction.zh-CN.md)：第一个提示词会在尚未接入数据源时生成完整预览，
Binder 再报告可下载卡包仍需完成的字段映射；编辑提示词只修改声明的视觉目标集合，保留已有数据和
动作。

允许使用合规参考图作为视觉输入，但不要把产品素材、业务术语或私有规则提交到开源仓库。当视觉
系统需要稳定约束时，上传一份小而通用的 Markdown 规则 revision 即可。

## 能力

- 在页面中配置自己的 OpenAI-compatible 或 Anthropic Messages API 模型地址、
  模型名和 API Key。
- 生成卡片、实时预览，并在同一会话中多轮编辑。
- 将成功卡片下载为自包含 Runtime Package；数据源、绑定、动作和算子不再依赖 Studio ID。
- 注册投递目标后可在工作台直接发布该运行包；内置 Callback，MQ Adapter 共用同一发布端口。
- 用本地 SQLite 知识库直接跑起来；知识、接口、算子和规则都能在后台维护。
- 多轮编辑时冻结目标集合、改动集合和保护集合，避免未指定区域被误改。
- 嵌入独立的[卡片 Runtime](runtime/README.zh-CN.md)，无需连接 Studio 即可执行已打包的
  数据源、绑定和 TypeScript 算子。
- Markdown 规则解析成功后以不可变 revision 发布，并原子切换活动指针。

## 模型兼容性

AGenUI Studio 可通过 OpenAI-compatible Chat Completions 或 Anthropic Messages
API 连接用户自行提供的模型。项目贡献者已测试以下模型：

| 模型 | 协议 | 状态 |
| --- | --- | --- |
| 千问（`qwen3.8-max`） | OpenAI-compatible | 已验证 |
| Claude Opus 4.7 | Anthropic Messages API | 已验证 |
| GLM-5.2 | OpenAI-compatible | 已验证（仅文本） |
| Kimi K3 | OpenAI-compatible | 已验证 |
| GPT-5.6 Sol / Terra | OpenAI-compatible | 已验证 |

实际效果可能随模型版本和服务提供方变化。测试使用的 GLM-5.2 配置不支持多模态输入，
具体模型 ID 以服务提供方为准。

## 架构

![AGenUI Studio 架构](docs/assets/architecture.svg)

Harness 负责可持久化运行；AGenUI Agent 通过有界工具使用规则、Catalog、知识、接口和已发布
算子；Workspace 原子提交不可变产物，供实时渲染、下载或通过 Callback/MQ Adapter 投递。
蓝色虚线框表示可替换端口，不是固定供应商或强制实现。

## 本地开箱即用，线上按边界替换

![AGenUI Studio 的本地默认实现与可替换端口](docs/assets/extension-points.png)

SQLite、BM25、Markdown 规则、Web Renderer 和内置 Runtime 让本地首次运行不依赖外部服务。
线上可以在保持合同不变的前提下，将持久化存储替换为 MySQL，将热状态替换为 Redis，将知识或
数据替换为 MCP 服务，将算子替换为远程运行平台，并通过自建 Callback 服务或 MQ Consumer
接收卡包。Runtime Package 是可移植交付物；内置 Runtime 只是核心参考实现，不限制部署方的
编排、托管和在线执行方式。

## 本地启动

环境要求：Go 1.26.2+、Node.js 20+。

```sh
cd web
npm install
cd ..
make dev
```

打开 `http://127.0.0.1:3100/admin/home`，在“设置”中录入模型地址、模型名和 API Key。
本地数据保存在 `agenui-agent/var/`，不会被 Git 跟踪。`make dev` 会幂等初始化内置的
知识、接口和算子示例，并一起启动本地数据服务、Agent 与管理后台。

生成成功后，可在会话页点击“下载 Runtime Package”，并独立验证：

```sh
go run ./runtime/cmd/agenui-runtime /path/to/agenui-session.json
```

本地服务和 Agent 必须共用 `var/agenui/studio.db`。这样在后台保存并发布的 TypeScript 算子，
才能立即被 Operator Detail API 和运行时 Adapter 读取。

## 对接应用

后台使用 AGenUI 管理接口；业务应用可以订阅原生运行事件：

```text
GET /api/v1/sessions/{session_id}/runs/{run_id}/events
Accept: text/event-stream
X-AGenUI-Tenant-ID: <tenant>
X-AGenUI-User-ID: <user>
```

以上身份 Header 仅用于本地/测试 profile；生产部署必须接入经过认证的
`PrincipalResolver`。

更多内容见[快速开始](docs/quickstart.md)、[卡片复刻](docs/reproduction.zh-CN.md)、[配置](docs/configuration.md)、
[架构](docs/architecture.md)、[规则文档](docs/rules.md)、[能力与验收](docs/capabilities.md)、[API 参考](docs/api-reference.md)和 [Knowledge MCP](docs/knowledge-mcp.md)。

## 社区与联系

- **GitHub Issues**：[问题反馈与功能建议](https://github.com/acoder-ai-infra/agenui-studio/issues)
- **邮箱**：[zhangjingcheng.zjc@alibaba-inc.com](mailto:zhangjingcheng.zjc@alibaba-inc.com)
- **钉钉群**：技术交流群
- **微信群**：技术交流群

<div align="center">
<table>
  <tr>
    <th>钉钉群</th>
    <th>微信群</th>
  </tr>
  <tr>
    <td align="center"><img src="docs/assets/dingtalk-group.png" alt="AGenUI Studio 钉钉群二维码" width="220"></td>
    <td align="center"><img src="docs/assets/wechat-group.png" alt="AGenUI Studio 微信群二维码" width="220"></td>
  </tr>
</table>
</div>

---

## 许可证

Apache License 2.0，见 [LICENSE](LICENSE)。
第三方版权说明见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。

安全问题报告与贡献流程见 [SECURITY.md](SECURITY.md) 和
[CONTRIBUTING.md](CONTRIBUTING.md)。
