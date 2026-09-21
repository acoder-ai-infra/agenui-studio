// Package modelgateway 是所有 LLM 调用的统一治理入口(收口 + 观测 + 多下游适配)。
//
// 精简后的职责只保留四件事:
//   - 多下游适配:ProviderAdapter / ChatProvider 把各家协议归一为 NormalizedChunk;
//   - 统一入口 + 观测:Facade.Chat 流式产出 canonical model_* AgentEvent;
//   - 按 provider 选路 + 简单降级:ModelRouter 给出 primary + fallback 列表,
//     Facade 顺序尝试,失败(可重试)则换下一个 target(可跨 provider);
//   - SSE 内嵌 throttling 识别(在 adapter 内)+ usage/cost 归一。
//
// 依赖方向:modelgateway 只依赖 observability(不依赖 storage / agentruntime /
// artifact),被 agentruntime 的 Runtime 适配器调用。它只 emit model_* 事件
// (Sequence=0),不碰 EventStore —— sequence/落库由 Phase 1 storage 负责。
package modelgateway
