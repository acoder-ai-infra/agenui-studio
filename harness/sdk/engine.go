package harness

import "context"

// Engine 是 SDK 的公开入口，是共享 internal/kernel Composition Root 之上的
// 稳定 Facade。调用方通过 harness.Build 拿到一个 Engine，并借助 Start /
// Resume / Cancel / Subscribe 驱动 Run。
//
// 不变量（详见 the public SDK contract）：
//
//   - Start / Resume 会在把 Run 交给 runtime 之前先装好 EventStream
//     订阅；调用方不会丢失首个热 delta。
//   - Cancel 与 EventStream.Close 解耦。关闭 stream 只解除订阅，要停止
//     Run 必须显式调 Cancel。
//   - GetResult / GetRun 任何时刻都可安全调用；它们返回 Run 持久化状态
//     的投影。
//   - Close 会先停止接收新 Run，再对进行中的 Run 做流失，最后释放
//     kernel 管理的资源；宿主自持的外部资源永远不会被 Engine 关闭。
type Engine interface {
	// Start 以 req.Identity 开启一个新 Run，并返回一个 Execution；它的
	// Events() 会按 Sequence 顺序推送本 Run 的每一条 canonical AgentEvent，
	// 直至终态。
	Start(ctx context.Context, req StartRequest) (Execution, error)

	// Resume 通过提交一个 ControlResponse 让处于 waiting_control 的 Run
	// 继续执行。返回的 Execution.Events() 从 resume_accepted 事件之后开始
	// 流出直至终态。
	Resume(ctx context.Context, req ResumeRequest) (Execution, error)

	// Cancel 请求 kernel 取消指定 Run。对已进入终态的 Run，Cancel 是幂等的。
	Cancel(ctx context.Context, req CancelRequest) error

	// Subscribe 为已有 Run 附加一个新的 EventStream，用于断线重连或审计，
	// 不会修改 Run 状态。
	Subscribe(ctx context.Context, req SubscribeRequest) (EventStream, error)

	// GetRun 返回 Run 持久化后的 RunView。
	GetRun(ctx context.Context, req GetRunRequest) (RunView, error)

	// GetResult 返回 Run 的最终 ResultView。在 Run 进入终态之前调用是安全的
	//（在 final response 尚未写入时返回 ErrNotReady）。
	GetResult(ctx context.Context, req GetResultRequest) (ResultView, error)

	// GetSession 返回 Session 持久化后的 SessionView。会话必须归属
	// Identity 的 tenant + user，不存在返回 ErrNotFound，越权返回
	// ErrPermissionDenied。
	GetSession(ctx context.Context, req GetSessionRequest) (SessionView, error)

	// ListSessions 以 keyset 游标分页列出 Identity.UserID 名下的会话。
	ListSessions(ctx context.Context, req ListSessionsRequest) (SessionPage, error)

	// ListMessages 分页列出一个会话的 user_visible 消息历史；会话归属
	// 校验与 GetSession 一致。
	ListMessages(ctx context.Context, req ListMessagesRequest) (MessagePage, error)

	// Artifacts 返回宿主的受控 Artifact 读写门面：以 host 主体经 ACL
	// 校验的 Put / Get / Head（会话作用域，允许同 session 跨 Run 读取）。
	Artifacts() ArtifactClient

	// Readiness 返回实时的 ReadinessReport（会现探测各类资源）。
	Readiness(ctx context.Context) (ReadinessReport, error)

	// Report 返回 Build 时冻结的 BuildReport。开销极小；健康检查处理器可
	// 安全调用。
	Report() BuildReport

	// Close 停止接收新 Run，流失进行中的 Run（受 ctx deadline 约束），并
	// 释放 kernel 管理的资源。可并发、可重复调用；每个调用者看到相同结果。
	Close(ctx context.Context) error
}
