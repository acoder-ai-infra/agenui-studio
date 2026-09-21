package harness

import (
	"encoding/json"
	"time"
)

// StartRequest 在指定 Identity 下开启一个新 Run，并把一条 Message 交给绑定的
// Agent。kernel 会在把 Run 分发出去之前先装好 EventStream 订阅，因此调用方
// 不会丢失最早的热 delta。
type StartRequest struct {
	// Identity 承载 tenant / session / agent 作用域；必需字段见 Identity 本身。
	Identity Identity
	// Input 是本轮用户消息；Parts 必须非空并满足 MessagePart 中的 PartKind 约束。
	Input Message
	// ScopedData 是一份小型的 per-Run 状态包（业务偏好、设备能力等）。
	// 保持轻量；大型 payload 必须以 Artifact ref 传递。
	ScopedData ScopedData
	// IdempotencyKey 让 Start 具备安全可重试性：同一 key + 同一 Identity +
	// 同一 Input hash 会返回相同的 Run；同一 key 下的不同 body 会以
	// ErrIdempotencyMismatch 失败。非空时最多 128 个 UTF-8 字符。
	IdempotencyKey string
	// Metadata 是一份小的调用方标签映射，会通过 trace 浮现。
	Metadata map[string]string
	// Deadline 是本轮可选的墙钟截止时间。零值表示“使用 kernel 默认的
	// per-Run 超时”。
	Deadline time.Time
}

// ScopedData 与 agentruntime.ScopedData 保持一致但不 import 内部包。Key 跨
// Turn 稳定；kernel 在分发时对 ScopedData 拍一份快照，令扩展看到一致视图。
type ScopedData struct {
	// Run 是 Run 级作用域，被 Run 内每一步共享。
	Run map[string]ScopedDataItem
	// Agents 是 per-agent-id 作用域；key 是 AgentRegistry 中的 agent id。
	Agents map[string]map[string]ScopedDataItem
}

// ScopedDataItem 是 ScopedData 中的一项。小 blob 直接内联 Value；已在
// Artifact Store 中的大 blob 请使用 Ref + Hash。
type ScopedDataItem struct {
	Source     string          `json:"source"`
	Visibility Visibility      `json:"visibility,omitempty"`
	Value      json.RawMessage `json:"value,omitempty"`
	Ref        string          `json:"ref,omitempty"`
	Hash       string          `json:"hash,omitempty"`
}

// ResumeRequest 通过提交一个 ControlResponse 让抵达 waiting_control 的 Run
// 继续执行。kernel 会先验证 resume ticket、加载 checkpoint，再重新进入
// runtime；同样遵循“先装订阅、后分发”的顺序约束。
type ResumeRequest struct {
	// Identity 至少需要 RunID；kernel 会从持久化的 Run 中解析出 session /
	// tenant。其他字段若填写，必须与持久化 Run 完全一致。
	Identity Identity
	// ControlRequestID 是正在响应的 pending ControlRequest。
	ControlRequestID string
	// CheckpointID 是 Run 挂起时所在的 checkpoint。
	CheckpointID string
	// ControlTicket 是 kernel 在创建 ControlRequest 时下发的、加密且按租户
	// 作用域绑定的 resume 凭据。调用方绝不可伪造或持久化原始 resume token。
	ControlTicket string
	// Response 是调用方提交的答复 payload（JSON）；其 schema 由产生它的
	// ControlRequest 决定。
	Response json.RawMessage
	// ResponseArtifactRef 可选，指向一个已上传的响应体（大媒体、多文件
	// 答复）。当它非空时 kernel 读取它作为答复 payload；两者都填时 Response
	// 会被忽略。
	ResponseArtifactRef string
	// Metadata 语义与 StartRequest.Metadata 一致。
	Metadata map[string]string
}

// CancelRequest 请求 kernel 取消一个进行中的 Run。Cancel 是幂等的：对已在
// 终态的 Run 返回 nil。
type CancelRequest struct {
	Identity Identity
	// Reason 是与 run_cancelled 事件一起持久化的短字符串。
	Reason string
}

// SubscribeRequest 为已有 Run 附加一个新的 EventStream，并可选择从一个持久化
// Sequence 之后开始。用于断线重连或审计路径。
type SubscribeRequest struct {
	Identity Identity
	// AfterSequence 若非零，则只浮现 Sequence > AfterSequence 的事件；为零
	// 时返回从头开始的每一条已持久化事件。
	AfterSequence int64
	// Cursor 是 AfterSequence 的另一种表达（附带 RunID 校验）。两者同时
	// 提供时 Cursor.AfterSequence 优先。
	Cursor Cursor
	// IncludeChildren 为 true 且 broker 支持 session fan-in 时，会一并浮现
	// 共享同一 session 的 platform_child_run 事件。
	IncludeChildren bool
}

// GetRunRequest 是对 Run 当前状态视图的只读查询。
type GetRunRequest struct {
	Identity Identity
}

// GetResultRequest 是对 Run 最终结果视图的只读查询。调用方必须在 Run 进入
// 终态之后再调用。
type GetResultRequest struct {
	Identity Identity
}
