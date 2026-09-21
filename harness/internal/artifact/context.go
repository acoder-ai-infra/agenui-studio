package artifact

import "context"

type ActorRole string

const (
	ActorUser          ActorRole = "user"
	ActorRuntime       ActorRole = "runtime"
	ActorContextEngine ActorRole = "context_engine"
	ActorDebug         ActorRole = "debug"
	ActorAudit         ActorRole = "audit"
	// ActorHost 是嵌入式宿主（业务后端）主体：经 SDK ArtifactClient 以会话
	// 作用域读写业务 artifact，允许跨 Run（同一 session 内），但受写入类型
	// 白名单与读取系统类型黑名单约束。
	ActorHost ActorRole = "host"
)

type Actor struct {
	TenantID  string
	UserID    string
	SessionID string
	RunID     string
	AgentID   string
	Role      ActorRole
}

type actorContextKey struct{}

func ContextWithActor(ctx context.Context, actor Actor) context.Context {
	return context.WithValue(ctx, actorContextKey{}, actor)
}

func ActorFromContext(ctx context.Context) (Actor, bool) {
	actor, ok := ctx.Value(actorContextKey{}).(Actor)
	return actor, ok
}
