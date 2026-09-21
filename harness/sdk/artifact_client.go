package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

// ArtifactKind 是宿主经 ArtifactClient 可写入的业务 artifact 类型白名单。
// 系统内部类型（tool_result / checkpoint_state 等）不对宿主开放。
type ArtifactKind string

const (
	// ArtifactKindFile 是通用文件（文档、数据集）。
	ArtifactKindFile ArtifactKind = "file"
	// ArtifactKindImage 是图片资产。
	ArtifactKindImage ArtifactKind = "image"
	// ArtifactKindSchema 是结构化 schema 定义。
	ArtifactKindSchema ArtifactKind = "schema"
	// ArtifactKindHostData 是宿主自定义的结构化业务数据（如 VisualSpec）。
	ArtifactKindHostData ArtifactKind = "host_data"
)

// hostUploadRunScope 是宿主在 Run 之外上传 artifact 时占用的 run 命名空间
// 段：canonical ref 结构要求 RunID 非空，该常量如实标记"非 Run 产物"。
const hostUploadRunScope = "host-upload"

// PutArtifactRequest 是宿主上传业务 artifact 的入参。
type PutArtifactRequest struct {
	// Identity 提供 ACL 作用域：TenantID / UserID / SessionID 必填；
	// RunID 可选（为空时归入 host-upload 命名空间，同一 session 内的
	// Run 仍可读取）。
	Identity Identity
	// Name 是业务可读名（会做路径安全清洗）。
	Name string
	// MIME 是内容类型（必填，如 application/json）。
	MIME string
	// Kind 是业务类型，必须落在 ArtifactKind 白名单内。
	Kind ArtifactKind
	// Debug 为 true 时以 debug 可见性写入；默认 user_visible。
	Debug bool
	// Content 是待上传内容。
	Content io.Reader
	// IdempotencyKey 可选：同 key 重试收敛到同一对象。
	IdempotencyKey string
}

// GetArtifactRequest 是宿主读取 artifact 的入参。
type GetArtifactRequest struct {
	// Identity 提供 ACL 作用域：TenantID / UserID / SessionID 必填。
	// 读取允许跨 Run（同一 session 内）。
	Identity Identity
	// Ref 是 canonical artifact:// 引用（Put 返回值或事件 PayloadRef）。
	Ref string
	// Offset / Length 可选：只读取内容的一个区间（Length 为 0 表示读到
	// 末尾）。首版为流式跳读实现。
	Offset int64
	Length int64
}

// ArtifactInfo 是 artifact 的公开元信息视图。
type ArtifactInfo struct {
	// Ref 是 canonical artifact:// 引用，可直接用于 Get 或跨轮传递。
	Ref string
	// ArtifactRef 可直接作为 MessagePart.Ref 用于 Start 输入。
	ArtifactRef ArtifactRef
	Name        string
	MIME        string
	Kind        string
	SizeBytes   int64
	Hash        string
	CreatedAt   time.Time
}

// ArtifactContent 是 Get 的返回体；调用方负责 Close Content。
type ArtifactContent struct {
	Info    ArtifactInfo
	Content io.ReadCloser
}

// ListArtifactsRequest 是宿主列出会话内 artifact 的入参。
type ListArtifactsRequest struct {
	// Identity 提供 ACL 作用域：TenantID / UserID / SessionID 必填；
	// RunID 可选（非空时只列出该 Run 产出的 artifact）。
	Identity Identity
	// Kind 可选：只列出指定业务类型的 artifact；为空时列出全部宿主可读
	// 类型（系统内部类型始终由 ACL 过滤，不会浮现）。
	Kind ArtifactKind
	// Limit 是返回条数上限；零值使用默认上限 defaultArtifactListLimit，
	// 超过 maxArtifactListLimit 会被收敛到 maxArtifactListLimit。
	Limit int
}

// ArtifactPage 是 List 的返回体。HasMore 为 true 表示结果因 Limit 截断；
// 调用方可调大 Limit 重新查询（首版不提供翻页游标）。
type ArtifactPage struct {
	Items   []ArtifactInfo
	HasMore bool
}

const (
	// defaultArtifactListLimit 是 List 未显式给出 Limit 时的安全上限。
	defaultArtifactListLimit = 500
	// maxArtifactListLimit 是 List 允许的最大返回条数，防止宿主一次把
	// 大会话的全部 artifact 元数据拉进内存。
	maxArtifactListLimit = 2000
)

// ArtifactClient 是嵌入式宿主的受控 artifact 读写门面。所有调用都以
// host 主体经 ACL 校验：写入受业务类型白名单约束，读取拒绝系统内部
// 类型（checkpoint / 上下文快照 / 控制响应）与 restricted 可见性。
type ArtifactClient interface {
	// Put 上传业务 artifact 并返回含 canonical ref 的元信息。
	Put(ctx context.Context, req PutArtifactRequest) (ArtifactInfo, error)
	// Get 读取 artifact 内容；支持可选区间读取。
	Get(ctx context.Context, req GetArtifactRequest) (*ArtifactContent, error)
	// Head 仅读取元信息（大小 / hash / 类型），供大对象决策。
	Head(ctx context.Context, req GetArtifactRequest) (ArtifactInfo, error)
	// List 按会话（可选 Run / Kind 过滤）列出宿主可读的 artifact 元信息。
	// 结果经与 Get 相同的 ACL 逐行过滤：checkpoint / 上下文快照 / 控制
	// 响应等系统内部类型永不浮现。
	List(ctx context.Context, req ListArtifactsRequest) (ArtifactPage, error)
}

// Artifacts 返回 Engine 绑定的 ArtifactClient。
func (e *engineImpl) Artifacts() ArtifactClient {
	return &artifactClient{engine: e}
}

type artifactClient struct {
	engine *engineImpl
}

var hostWritableKinds = map[ArtifactKind]artifact.ArtifactType{
	ArtifactKindFile:     artifact.ArtifactTypeFile,
	ArtifactKindImage:    artifact.ArtifactTypeImage,
	ArtifactKindSchema:   artifact.ArtifactTypeSchema,
	ArtifactKindHostData: artifact.ArtifactTypeHostData,
}

func (c *artifactClient) store() (*artifact.Store, error) {
	if c.engine == nil || c.engine.closed.Load() {
		return nil, closedErrorf("Artifacts", nil)
	}
	if c.engine.kernel == nil || c.engine.kernel.Artifacts == nil {
		return nil, fmt.Errorf("%w: artifact store is not configured", ErrUnsupportedCapability)
	}
	return c.engine.kernel.Artifacts, nil
}

func hostActorContext(ctx context.Context, identity Identity) (context.Context, error) {
	if identity.TenantID == "" || identity.UserID == "" || identity.SessionID == "" {
		return nil, wrapInvalidRequest("artifact access requires Identity.TenantID, UserID and SessionID")
	}
	return artifact.ContextWithActor(ctx, artifact.Actor{
		Role:      artifact.ActorHost,
		TenantID:  identity.TenantID,
		UserID:    identity.UserID,
		SessionID: identity.SessionID,
		RunID:     identity.RunID,
	}), nil
}

func (c *artifactClient) Put(ctx context.Context, req PutArtifactRequest) (ArtifactInfo, error) {
	store, err := c.store()
	if err != nil {
		return ArtifactInfo{}, err
	}
	artifactType, ok := hostWritableKinds[req.Kind]
	if !ok {
		return ArtifactInfo{}, wrapInvalidRequest("artifact kind must be one of file/image/schema/host_data")
	}
	if req.Content == nil {
		return ArtifactInfo{}, wrapInvalidRequest("artifact content is required")
	}
	actorCtx, err := hostActorContext(ctx, req.Identity)
	if err != nil {
		return ArtifactInfo{}, err
	}
	runID := strings.TrimSpace(req.Identity.RunID)
	if runID == "" {
		runID = hostUploadRunScope
	}
	visibility := artifact.VisibilityUserVisible
	retention := artifact.RetentionSessionTTL
	if req.Debug {
		visibility = artifact.VisibilityDebug
		retention = artifact.RetentionDebugShortTTL
	}
	meta, err := store.Put(actorCtx, artifact.PutArtifactRequest{
		TenantID:        req.Identity.TenantID,
		UserID:          req.Identity.UserID,
		SessionID:       req.Identity.SessionID,
		RunID:           runID,
		OwnerModule:     artifact.OwnerModuleHost,
		OwnerID:         "host:" + req.Identity.TenantID,
		ArtifactType:    artifactType,
		MimeType:        req.MIME,
		Name:            req.Name,
		Visibility:      visibility,
		Content:         req.Content,
		RetentionPolicy: retention,
		CreatedBy:       "host",
		IdempotencyKey:  req.IdempotencyKey,
	})
	if err != nil {
		return ArtifactInfo{}, mapArtifactError("Put", err)
	}
	return artifactInfoFromMeta(*meta), nil
}

func (c *artifactClient) Get(ctx context.Context, req GetArtifactRequest) (*ArtifactContent, error) {
	store, err := c.store()
	if err != nil {
		return nil, err
	}
	actorCtx, err := hostActorContext(ctx, req.Identity)
	if err != nil {
		return nil, err
	}
	if req.Offset < 0 || req.Length < 0 {
		return nil, wrapInvalidRequest("artifact read range must be non-negative")
	}
	object, err := store.Get(actorCtx, req.Ref, artifact.GetOptions{Purpose: artifact.PurposeHostAccess})
	if err != nil {
		return nil, mapArtifactError("Get", err)
	}
	content := object.Content
	if req.Offset > 0 {
		if _, err := io.CopyN(io.Discard, content, req.Offset); err != nil {
			_ = content.Close()
			if errors.Is(err, io.EOF) {
				return nil, wrapInvalidRequest("artifact read offset exceeds content size")
			}
			return nil, fmt.Errorf("harness: Get artifact range: %w", err)
		}
	}
	if req.Length > 0 {
		content = struct {
			io.Reader
			io.Closer
		}{io.LimitReader(content, req.Length), object.Content}
	}
	return &ArtifactContent{Info: artifactInfoFromMeta(object.Meta), Content: content}, nil
}

func (c *artifactClient) List(ctx context.Context, req ListArtifactsRequest) (ArtifactPage, error) {
	store, err := c.store()
	if err != nil {
		return ArtifactPage{}, err
	}
	actorCtx, err := hostActorContext(ctx, req.Identity)
	if err != nil {
		return ArtifactPage{}, err
	}
	query := artifact.ListQuery{
		TenantID:  req.Identity.TenantID,
		SessionID: req.Identity.SessionID,
		RunID:     strings.TrimSpace(req.Identity.RunID),
	}
	if req.Kind != "" {
		artifactType, ok := hostWritableKinds[req.Kind]
		if !ok {
			return ArtifactPage{}, wrapInvalidRequest("artifact kind must be one of file/image/schema/host_data")
		}
		query.Type = artifactType
	}
	metas, err := store.List(actorCtx, query)
	if err != nil {
		return ArtifactPage{}, mapArtifactError("List", err)
	}
	limit := req.Limit
	if limit <= 0 {
		limit = defaultArtifactListLimit
	}
	if limit > maxArtifactListLimit {
		limit = maxArtifactListLimit
	}
	page := ArtifactPage{Items: make([]ArtifactInfo, 0, min(len(metas), limit))}
	for index, meta := range metas {
		if index >= limit {
			page.HasMore = true
			break
		}
		page.Items = append(page.Items, artifactInfoFromMeta(meta))
	}
	return page, nil
}

func (c *artifactClient) Head(ctx context.Context, req GetArtifactRequest) (ArtifactInfo, error) {
	store, err := c.store()
	if err != nil {
		return ArtifactInfo{}, err
	}
	actorCtx, err := hostActorContext(ctx, req.Identity)
	if err != nil {
		return ArtifactInfo{}, err
	}
	// Store.Head 不执行完整 authorize（无 purpose），为保持读取面一致，
	// 宿主 Head 复用 Get 的授权路径后立即关闭内容流。
	object, err := store.Get(actorCtx, req.Ref, artifact.GetOptions{Purpose: artifact.PurposeHostAccess})
	if err != nil {
		return ArtifactInfo{}, mapArtifactError("Head", err)
	}
	_ = object.Content.Close()
	return artifactInfoFromMeta(object.Meta), nil
}

func artifactInfoFromMeta(meta artifact.ArtifactMeta) ArtifactInfo {
	return ArtifactInfo{
		Ref: meta.ArtifactRef,
		ArtifactRef: ArtifactRef{
			ID:        meta.ArtifactID,
			TenantID:  meta.TenantID,
			SessionID: meta.SessionID,
			RunID:     meta.RunID,
			MIME:      meta.MimeType,
			Filename:  meta.Name,
			Hash:      meta.Hash,
			Size:      meta.SizeBytes,
		},
		Name:      meta.Name,
		MIME:      meta.MimeType,
		Kind:      string(meta.ArtifactType),
		SizeBytes: meta.SizeBytes,
		Hash:      meta.Hash,
		CreatedAt: meta.CreatedAt,
	}
}

// mapArtifactError 把 internal/artifact 错误码映射为 SDK 公共错误族。
func mapArtifactError(op string, err error) error {
	switch {
	case artifact.IsErrorCode(err, artifact.ErrNotFound):
		return fmt.Errorf("harness: %s: %w: %v", op, ErrInvalidRequest, err)
	case artifact.IsErrorCode(err, artifact.ErrPermissionDenied):
		return fmt.Errorf("harness: %s: artifact permission denied: %w", op, err)
	case artifact.IsErrorCode(err, artifact.ErrInvalidArgument):
		return fmt.Errorf("harness: %s: %w: %v", op, ErrInvalidRequest, err)
	case artifact.IsErrorCode(err, artifact.ErrConflict):
		return fmt.Errorf("harness: %s: %w: %v", op, ErrConflict, err)
	default:
		return fmt.Errorf("harness: %s: %w", op, err)
	}
}
