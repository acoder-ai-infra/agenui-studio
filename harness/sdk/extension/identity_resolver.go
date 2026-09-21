package extension

import "context"

// IdentityRequest 是 IdentityResolver.Resolve 的入参。
type IdentityRequest struct {
	Ctx Context
	// BusinessTenantID 是宿主从线上取到的任意租户标记（JWT 声明、header
	// 等）。resolver 负责把它映射到 TenantID。
	BusinessTenantID string
	// BusinessUserID 与 BusinessTenantID 对称，用于用户身份。
	BusinessUserID string
	// BusinessSessionID 与 BusinessTenantID 对称，用于会话身份。
	BusinessSessionID string
	// Metadata 承载 resolver 可能会参考的其他信号（channel、protocol、
	// 自定义 claim 等）。
	Metadata map[string]string
}

// ResolvedIdentity 是 resolver 的返回值。kernel 会把每一个非空字段视作
// 对入参 StartRequest.Identity 的覆盖。
type ResolvedIdentity struct {
	TenantID  string
	UserID    string
	SessionID string
	// Metadata 是合并到 trace context 的少量额外数据。
	Metadata map[string]string
}

// IdentityResolver 把业务身份映射为冻结后的 Harness 身份（详见
// the public SDK contract）。kernel 会在每个 Run 启动、入口
// guardrail 之前恰好调用一次。Resolver 不得触碰下游状态，只能检查请求
// payload 并返回映射。
type IdentityResolver interface {
	Resolve(ctx context.Context, req IdentityRequest) (ResolvedIdentity, error)
}
