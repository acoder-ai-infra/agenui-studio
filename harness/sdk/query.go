package harness

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// SessionStatus 映射持久化会话的状态枚举（active / archived / deleted）。
type SessionStatus string

const (
	// SessionStatusActive 是可继续产生新 Run 的活跃会话。
	SessionStatusActive SessionStatus = "active"
	// SessionStatusArchived 是已归档会话；记录仍可读。
	SessionStatusArchived SessionStatus = "archived"
	// SessionStatusDeleted 是软删除会话；默认列表查询不浮现。
	SessionStatusDeleted SessionStatus = "deleted"
)

// MessageRole 标识一条会话消息的发言主体。
type MessageRole string

const (
	// MessageRoleUser 是终端用户消息。
	MessageRoleUser MessageRole = "user"
	// MessageRoleAssistant 是 Agent 答复消息。
	MessageRoleAssistant MessageRole = "assistant"
	// MessageRoleSystem 是系统消息。
	MessageRoleSystem MessageRole = "system"
)

// SessionView 是 Session 持久化状态的只读投影，由 GetSession / ListSessions
// 返回。它是 ledger 事实的拷贝；修改它不会写回存储。
type SessionView struct {
	// Identity 承载会话归属：TenantID / UserID / SessionID / AgentID。
	Identity Identity
	// Title 是可选的会话标题（由 hosted 入口或业务侧维护）。
	Title string
	// Channel 是创建会话的入口通道（app / web / sdk 等）。
	Channel string
	// Status 是当前 SessionStatus。
	Status SessionStatus
	// 时间戳。
	CreatedAt time.Time
	UpdatedAt time.Time
	// Metadata 是业务侧随会话保存的小型标签映射。
	Metadata map[string]string
}

// MessageView 是一条会话消息的只读投影。大正文经 ContentRef（Artifact）
// 承载，ContentPreview 只保留短内联预览；需要全文时用 ArtifactClient.Get
// 读取 ContentRef。
type MessageView struct {
	// MessageID 是持久化消息 id，也是 ListMessages 翻页游标的单位。
	MessageID string
	// SessionID / TurnID / RunID 标识消息归属的会话、轮次与 Run（系统
	// 消息可能没有 RunID）。
	SessionID string
	TurnID    string
	RunID     string
	// Role 是发言主体（user / assistant / system）。
	Role MessageRole
	// Visibility 是消息的 canonical 可见性。宿主查询只浮现 user_visible。
	Visibility Visibility
	// ContentPreview 是消息正文的短预览。
	ContentPreview string
	// ContentRef 是大正文对应的 artifact:// 引用；为空表示正文全部在
	// ContentPreview 内。
	ContentRef string
	// CreatedAt 是消息写入时刻。
	CreatedAt time.Time
}

// GetSessionRequest 是对单个会话视图的只读查询。Identity 的 TenantID /
// UserID / SessionID 必填；会话必须归属该 tenant + user，否则返回
// ErrPermissionDenied。
type GetSessionRequest struct {
	Identity Identity
}

// ListSessionsRequest 以 keyset 游标分页列出调用方的会话，排序与 hosted
// /api/v1/sessions 一致（updated_at desc, session_id desc）。
type ListSessionsRequest struct {
	// Identity 的 TenantID / UserID 必填；只浮现该用户的会话。
	Identity Identity
	// AgentID 可选：只列出绑定指定 Agent 的会话。
	AgentID string
	// Limit 是单页上限；零值表示使用存储层默认页大小。
	Limit int
	// BeforeUpdatedAt / BeforeSessionID 是上一页返回的 keyset 游标
	//（SessionPage.NextBeforeUpdatedAt / NextBeforeSessionID），两者同时
	// 为零值表示取第一页。
	BeforeUpdatedAt time.Time
	BeforeSessionID string
	// IncludeArchived 为 true 时 archived 会话一并浮现；deleted 永不浮现。
	IncludeArchived bool
}

// SessionPage 是 ListSessions 的一页结果。
type SessionPage struct {
	Items   []SessionView
	HasMore bool
	// 下一页游标；HasMore 为 false 时为零值。
	NextBeforeUpdatedAt time.Time
	NextBeforeSessionID string
}

// ListMessagesRequest 分页列出一个会话的消息历史，排序为
// （created_at asc, message_id asc）。只浮现 user_visible 消息——internal /
// debug / restricted 可见性属于平台内部事实，不对宿主透出。
type ListMessagesRequest struct {
	// Identity 的 TenantID / UserID / SessionID 必填；会话必须归属该
	// tenant + user，否则返回 ErrPermissionDenied。
	Identity Identity
	// BeforeMessageID / AfterMessageID 是翻页游标（上一页的
	// MessagePage.NextBeforeMessageID / NextAfterMessageID）：Before 向更旧
	// 方向翻页，After 向更新方向翻页；同时为空表示取最新一页。
	BeforeMessageID string
	AfterMessageID  string
	// Limit 是单页上限；零值表示使用存储层默认页大小。
	Limit int
}

// MessagePage 是 ListMessages 的一页结果。
type MessagePage struct {
	Items   []MessageView
	HasMore bool
	// 下一页游标；HasMore 为 false 时为空字符串。
	NextBeforeMessageID string
	NextAfterMessageID  string
}

// GetSession 返回 Session 持久化后的 SessionView。
func (e *engineImpl) GetSession(ctx context.Context, req GetSessionRequest) (SessionView, error) {
	if e.closed.Load() {
		return SessionView{}, closedErrorf("GetSession", nil)
	}
	if err := requireSessionQueryIdentity("GetSession", req.Identity); err != nil {
		return SessionView{}, err
	}
	if e.kernel == nil || e.kernel.Stores.Sessions == nil {
		return SessionView{}, fmt.Errorf("%w: session store missing", ErrNotReady)
	}
	sess, err := e.authorizeSessionAccess(ctx, "GetSession", req.Identity)
	if err != nil {
		return SessionView{}, err
	}
	return sessionViewFromRecord(sess), nil
}

// ListSessions 列出调用方（Identity.UserID）名下的会话，keyset 分页。
func (e *engineImpl) ListSessions(ctx context.Context, req ListSessionsRequest) (SessionPage, error) {
	if e.closed.Load() {
		return SessionPage{}, closedErrorf("ListSessions", nil)
	}
	if req.Identity.TenantID == "" || req.Identity.UserID == "" {
		return SessionPage{}, wrapInvalidRequest("ListSessions requires Identity.TenantID and Identity.UserID")
	}
	if e.kernel == nil || e.kernel.Stores.Sessions == nil {
		return SessionPage{}, fmt.Errorf("%w: session store missing", ErrNotReady)
	}
	page, err := e.kernel.Stores.Sessions.List(ctx, storage.SessionListQuery{
		UserID:          req.Identity.UserID,
		AgentID:         strings.TrimSpace(req.AgentID),
		BeforeUpdatedAt: req.BeforeUpdatedAt,
		BeforeSessionID: req.BeforeSessionID,
		Limit:           req.Limit,
		IncludeArchived: req.IncludeArchived,
	})
	if err != nil {
		return SessionPage{}, fmt.Errorf("harness: list sessions: %w", err)
	}
	items := make([]SessionView, 0, len(page.Items))
	for _, sess := range page.Items {
		if sess == nil {
			continue
		}
		// 防御性租户过滤：SessionListQuery 以 UserID 为主键过滤，store 不
		// 保证跨租户隔离；查询结果绝不能放大到调用方租户之外。
		if sess.TenantID != req.Identity.TenantID {
			continue
		}
		items = append(items, sessionViewFromRecord(sess))
	}
	return SessionPage{
		Items:               items,
		HasMore:             page.HasMore,
		NextBeforeUpdatedAt: page.NextBeforeUpdatedAt,
		NextBeforeSessionID: page.NextBeforeSessionID,
	}, nil
}

// ListMessages 分页列出一个会话的 user_visible 消息历史。
func (e *engineImpl) ListMessages(ctx context.Context, req ListMessagesRequest) (MessagePage, error) {
	if e.closed.Load() {
		return MessagePage{}, closedErrorf("ListMessages", nil)
	}
	if err := requireSessionQueryIdentity("ListMessages", req.Identity); err != nil {
		return MessagePage{}, err
	}
	if e.kernel == nil || e.kernel.Stores.Sessions == nil || e.kernel.Stores.Messages == nil {
		return MessagePage{}, fmt.Errorf("%w: session/message store missing", ErrNotReady)
	}
	if _, err := e.authorizeSessionAccess(ctx, "ListMessages", req.Identity); err != nil {
		return MessagePage{}, err
	}
	page, err := e.kernel.Stores.Messages.List(ctx, storage.MessageListQuery{
		SessionID:       req.Identity.SessionID,
		BeforeMessageID: req.BeforeMessageID,
		AfterMessageID:  req.AfterMessageID,
		Limit:           req.Limit,
		// Visibilities 留空：存储层默认只返回 user_visible，与 hosted
		// /sessions/{id}/messages 的默认行为一致。
	})
	if err != nil {
		return MessagePage{}, fmt.Errorf("harness: list messages: %w", err)
	}
	items := make([]MessageView, 0, len(page.Items))
	for _, msg := range page.Items {
		if msg == nil {
			continue
		}
		items = append(items, messageViewFromRecord(msg))
	}
	return MessagePage{
		Items:               items,
		HasMore:             page.HasMore,
		NextBeforeMessageID: page.NextBeforeMessageID,
		NextAfterMessageID:  page.NextAfterMessageID,
	}, nil
}

// requireSessionQueryIdentity 校验会话级查询的身份字段完整性。
func requireSessionQueryIdentity(op string, identity Identity) error {
	if identity.TenantID == "" || identity.UserID == "" || identity.SessionID == "" {
		return wrapInvalidRequest(op + " requires Identity.TenantID, Identity.UserID and Identity.SessionID")
	}
	return nil
}

// authorizeSessionAccess 解析目标会话并校验其归属，与 hosted server 的
// sessionAllowed 语义对齐且 fail closed：
//   - tenant 必须严格相等（SDK 侧 Identity.TenantID 必填，不接受空宽容）；
//   - 会话必须有属主且与调用方 UserID 完全匹配——ownerless 遗留数据必须
//     经显式迁移或特权通道访问，绝不向普通宿主查询透出。
func (e *engineImpl) authorizeSessionAccess(ctx context.Context, op string, identity Identity) (*storage.Session, error) {
	sess, err := e.kernel.Stores.Sessions.Get(ctx, identity.SessionID)
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: session %s", ErrNotFound, identity.SessionID)
		}
		return nil, fmt.Errorf("harness: %s: get session: %w", op, err)
	}
	if sess == nil {
		return nil, fmt.Errorf("%w: session %s", ErrNotFound, identity.SessionID)
	}
	if sess.TenantID != identity.TenantID {
		return nil, fmt.Errorf("%w: session belongs to another tenant", ErrPermissionDenied)
	}
	if sess.UserID == "" || sess.UserID != identity.UserID {
		return nil, fmt.Errorf("%w: session does not belong to caller", ErrPermissionDenied)
	}
	return sess, nil
}

func sessionViewFromRecord(sess *storage.Session) SessionView {
	return SessionView{
		Identity: Identity{
			TenantID:  sess.TenantID,
			UserID:    sess.UserID,
			SessionID: sess.ID,
			AgentID:   sess.AgentID,
		},
		Title:     sess.Title,
		Channel:   sess.Channel,
		Status:    SessionStatus(sess.Status),
		CreatedAt: sess.CreatedAt,
		UpdatedAt: sess.UpdatedAt,
		Metadata:  cloneStringMap(sess.Metadata),
	}
}

func messageViewFromRecord(msg *storage.Message) MessageView {
	return MessageView{
		MessageID:      msg.ID,
		SessionID:      msg.SessionID,
		TurnID:         msg.TurnID,
		RunID:          msg.RunID,
		Role:           MessageRole(msg.Role),
		Visibility:     Visibility(msg.Visibility),
		ContentPreview: msg.ContentPreview,
		ContentRef:     msg.ContentRef,
		CreatedAt:      msg.CreatedAt,
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
