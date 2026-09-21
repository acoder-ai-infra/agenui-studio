// Package extension declares the supported Harness SDK extension contracts.
//
// 八类扩展的注册路径唯一（harness.WithXxx → kernel.ExtensionCatalog →
// Composition Root），执行机制按生命周期语义落位到底层原生挂载点
// （详见 the public SDK contract 映射表）：
//
//   - IdentityResolver：   业务身份 -> Harness 身份 的映射（kernel 预回合管线）。
//   - RunInitializer：     每轮 ScopedData / Artifact 引用的初始化（kernel 预回合管线）。
//   - ContextContributor： 业务 context fragment，透传进 ModelContext 装配与最终输入治理。
//   - InputNormalizer：    每个 Agent 版本的确定性输入规范化（kernel 预回合管线）。
//   - ToolProvider：       宿主注册的函数工具实现（定义在 tools.yaml），经 Tool Gateway 执行。
//   - OutputValidator：    输出校验，桥接为 runtime.before_response hook（落盘前拦截）。
//   - ProtocolProjector：  把 canonical event 投影为业务协议帧（SDK 客户端）。
//   - EventObserver：      只读旁路（服务端事件发布点 fan-out）。
package extension
