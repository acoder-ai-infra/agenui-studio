// Package harness 是面向 Harness 在线 Agent 运行时的公开 Go SDK。
//
// 外部 module（把 Harness 以进程内方式嵌入的业务服务）只能 import 本包及其
// 子包 harness/extension、harness/resource、harness/testkit；harness 的其他所有
// 包都位于 internal/ 之下，属于 Go 语言 internal import 规则的禁区。
//
// 一页说清 SDK 契约：
//
//   - 一个 Kernel，两个入口。cmd/harness（Hosted）和本包（Embedded）都构建同一个
//     internal/kernel.Kernel；SDK Engine 是这个共享 Composition Root 之上的稳定
//     Facade。
//   - 事实归 Harness。Run、AgentEvent、Checkpoint、ControlRequest、AgentBinding
//     和 ArtifactMeta 的 owner 在进程内运行时不会变化。
//   - 业务状态归宿主。页面步骤、领域对象、事务留在宿主，通过 ScopedData /
//     Artifact 引用 / 业务存储承接；SDK 不会伪造 AGenUI 或 App-Factory 形状的字段。
//   - 配置在 Build 时冻结。Run / Resume 只消费不可变的 EffectiveConfig 快照；
//     不存在环境变量或热更新通路。
//   - 扩展由受信 Go 代码注册，由 YAML 按稳定 ID 选中启用。SDK 与配置文件都不会
//     加载任意二进制。扩展的执行机制位于底层挂载点而非 SDK Facade：
//     预回合阶段走 kernel.TurnPipeline（HTTP 与 SDK 入口共享同一治理链），
//     ToolProvider 的实现合并进 Tool Gateway，OutputValidator 桥接 runtime hook，
//     EventObserver 在服务端事件发布点 fan-out
//     （映射表见 the public SDK contract）。
//   - 每一次模型调用、工具调用和 MCP 调用都要经过 Harness gateway；业务代码
//     无法绕过最终输入治理。
//
// 稳定性：
//
// 本 SDK 为 v0 API。公共形状（Engine、Message、Event、Extension DTO、资源注册
// 签名、Build / Readiness DTO）都归属 canonical schema 线 "harness.<entity>.v1"，
// 遵循 the public protocol contract 与 the public SDK contract
// 的兼容规则。破坏性变更需要 schema 升级和新的 import 路径。
//
// Import path:  github.com/AGenUI/agenui-studio/harness/sdk
package harness

// Version 标识 SDK 的公共契约线。它不是 module tag，而是 DTO 遵循的 schema
// 线，会写入 BuildReport 便于排查跨版本事故。
const Version = "harness.sdk.v0"
