package harness_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk"
)

// buildQueryTestEngine 使用 scenario mock 模型构建真实 Engine（与
// artifact_host_e2e_test.go 同一基建），保证普通文本输入也能抵达
// run_completed 并落库 assistant 消息。
func buildQueryTestEngine(t *testing.T) (harness.Engine, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "query-fake-token")
	engine := buildHITLEngineWithOptions(t, ctx, t.TempDir(), "")
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = engine.Close(shutdownCtx)
	})
	return engine, ctx
}

// TestHostSessionQueryE2E 验证 SDK 宿主查询透出的验收基线：
// Start 真实 Run 落库后，宿主可用同一 Engine 查询 Session（GetSession /
// ListSessions）与消息历史（ListMessages）；越权（跨 user / 跨 tenant /
// ownerless）一律 fail closed，缺失记录返回 ErrNotFound。
func TestHostSessionQueryE2E(t *testing.T) {
	engine, ctx := buildQueryTestEngine(t)

	owner := harness.Identity{TenantID: "public", UserID: "query-host"}

	// --- 两个会话各跑一个真实 Run，让 Session/Message 都有持久化事实 ---
	execA, err := engine.Start(ctx, harness.StartRequest{
		Identity: harness.Identity{TenantID: owner.TenantID, UserID: owner.UserID, SessionID: "sess-query-a"},
		Input:    harness.TextMessage("first question"),
	})
	if err != nil {
		t.Fatalf("start run A: %v", err)
	}
	terminalA, _ := drainUntilTerminal(t, ctx, execA)
	if terminalA != harness.EventRunCompleted {
		t.Fatalf("run A terminal = %s, want run_completed", terminalA)
	}
	runAID := execA.Handle().Identity.RunID

	execB, err := engine.Start(ctx, harness.StartRequest{
		Identity: harness.Identity{TenantID: owner.TenantID, UserID: owner.UserID, SessionID: "sess-query-b"},
		Input:    harness.TextMessage("second question"),
	})
	if err != nil {
		t.Fatalf("start run B: %v", err)
	}
	terminalB, _ := drainUntilTerminal(t, ctx, execB)
	if terminalB != harness.EventRunCompleted {
		t.Fatalf("run B terminal = %s, want run_completed", terminalB)
	}

	// --- GetSession：正常读取 + 字段投影 ---
	sessA := harness.Identity{TenantID: owner.TenantID, UserID: owner.UserID, SessionID: "sess-query-a"}
	view, err := engine.GetSession(ctx, harness.GetSessionRequest{Identity: sessA})
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if view.Identity.TenantID != owner.TenantID || view.Identity.UserID != owner.UserID || view.Identity.SessionID != "sess-query-a" {
		t.Fatalf("session identity mismatch: %+v", view.Identity)
	}
	if view.Identity.AgentID != "demo" || view.Status != harness.SessionStatusActive || view.Channel != "sdk" {
		t.Fatalf("session projection mismatch: agent=%q status=%q channel=%q", view.Identity.AgentID, view.Status, view.Channel)
	}
	if view.CreatedAt.IsZero() || view.UpdatedAt.IsZero() {
		t.Fatalf("session timestamps must be populated: %+v", view)
	}

	// --- GetSession：缺失 / 越权 / 非法入参 ---
	if _, err := engine.GetSession(ctx, harness.GetSessionRequest{
		Identity: harness.Identity{TenantID: owner.TenantID, UserID: owner.UserID, SessionID: "sess-missing"},
	}); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("missing session must return ErrNotFound, got %v", err)
	}
	if _, err := engine.GetSession(ctx, harness.GetSessionRequest{
		Identity: harness.Identity{TenantID: owner.TenantID, UserID: "intruder", SessionID: "sess-query-a"},
	}); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("cross-user get must return ErrPermissionDenied, got %v", err)
	}
	if _, err := engine.GetSession(ctx, harness.GetSessionRequest{
		Identity: harness.Identity{TenantID: "other-tenant", UserID: owner.UserID, SessionID: "sess-query-a"},
	}); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("cross-tenant get must return ErrPermissionDenied, got %v", err)
	}
	if _, err := engine.GetSession(ctx, harness.GetSessionRequest{
		Identity: harness.Identity{TenantID: owner.TenantID, UserID: owner.UserID},
	}); !errors.Is(err, harness.ErrInvalidRequest) {
		t.Fatalf("incomplete identity must return ErrInvalidRequest, got %v", err)
	}

	// --- ListSessions：归属过滤 + Agent 过滤 + keyset 翻页 ---
	page, err := engine.ListSessions(ctx, harness.ListSessionsRequest{Identity: owner})
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	owned := map[string]bool{}
	for _, item := range page.Items {
		owned[item.Identity.SessionID] = true
		if item.Identity.TenantID != owner.TenantID || item.Identity.UserID != owner.UserID {
			t.Fatalf("list leaked foreign session: %+v", item.Identity)
		}
	}
	if !owned["sess-query-a"] || !owned["sess-query-b"] {
		t.Fatalf("own sessions missing from list: %v", owned)
	}

	agentFiltered, err := engine.ListSessions(ctx, harness.ListSessionsRequest{Identity: owner, AgentID: "demo"})
	if err != nil {
		t.Fatalf("list sessions by agent: %v", err)
	}
	if len(agentFiltered.Items) != len(page.Items) {
		t.Fatalf("agent=demo filter should keep all own sessions: %d != %d", len(agentFiltered.Items), len(page.Items))
	}
	agentMiss, err := engine.ListSessions(ctx, harness.ListSessionsRequest{Identity: owner, AgentID: "no-such-agent"})
	if err != nil {
		t.Fatalf("list sessions by missing agent: %v", err)
	}
	for _, item := range agentMiss.Items {
		if item.Identity.SessionID == "sess-query-a" || item.Identity.SessionID == "sess-query-b" {
			t.Fatalf("agent filter leaked session %s", item.Identity.SessionID)
		}
	}

	// keyset 翻页：Limit=1 逐页收集，不重不漏且能覆盖全部自有会话。
	seen := map[string]int{}
	cursor := harness.ListSessionsRequest{Identity: owner, Limit: 1}
	for pageNum := 0; pageNum < 8; pageNum++ {
		p, listErr := engine.ListSessions(ctx, cursor)
		if listErr != nil {
			t.Fatalf("list sessions page %d: %v", pageNum, listErr)
		}
		if len(p.Items) > 1 {
			t.Fatalf("page %d returned %d items, want <= 1", pageNum, len(p.Items))
		}
		for _, item := range p.Items {
			seen[item.Identity.SessionID]++
		}
		if !p.HasMore {
			break
		}
		cursor.BeforeUpdatedAt = p.NextBeforeUpdatedAt
		cursor.BeforeSessionID = p.NextBeforeSessionID
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("session %s appeared %d times across pages", id, count)
		}
	}
	if seen["sess-query-a"] != 1 || seen["sess-query-b"] != 1 {
		t.Fatalf("paged list did not cover own sessions: %v", seen)
	}

	// --- ListMessages：终态后 user + assistant 两条，role/可见性/投影正确 ---
	msgPage, err := engine.ListMessages(ctx, harness.ListMessagesRequest{Identity: sessA})
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(msgPage.Items) != 2 {
		t.Fatalf("messages = %d, want user + assistant 2 items", len(msgPage.Items))
	}
	userMsg, assistantMsg := msgPage.Items[0], msgPage.Items[1]
	if userMsg.Role != harness.MessageRoleUser || assistantMsg.Role != harness.MessageRoleAssistant {
		t.Fatalf("message roles = %q/%q, want user/assistant", userMsg.Role, assistantMsg.Role)
	}
	if userMsg.Visibility != harness.VisibilityUserVisible || assistantMsg.Visibility != harness.VisibilityUserVisible {
		t.Fatalf("host query must only surface user_visible messages: %q/%q", userMsg.Visibility, assistantMsg.Visibility)
	}
	if !strings.Contains(userMsg.ContentPreview, "first question") {
		t.Fatalf("user message preview drifted: %q", userMsg.ContentPreview)
	}
	if assistantMsg.ContentPreview == "" && assistantMsg.ContentRef == "" {
		t.Fatal("assistant message must carry preview or content ref")
	}
	if assistantMsg.RunID != runAID {
		t.Fatalf("assistant message run = %q, want %q", assistantMsg.RunID, runAID)
	}
	if userMsg.MessageID == "" || assistantMsg.MessageID == "" || userMsg.CreatedAt.IsZero() {
		t.Fatalf("message projection incomplete: %+v / %+v", userMsg, assistantMsg)
	}

	// 翻页：Limit=1 先拿最新一条（assistant），再向更旧方向翻。
	latest, err := engine.ListMessages(ctx, harness.ListMessagesRequest{Identity: sessA, Limit: 1})
	if err != nil {
		t.Fatalf("list messages latest page: %v", err)
	}
	if len(latest.Items) != 1 || latest.Items[0].Role != harness.MessageRoleAssistant || !latest.HasMore {
		t.Fatalf("latest page = %+v, want single assistant item with HasMore", latest)
	}
	older, err := engine.ListMessages(ctx, harness.ListMessagesRequest{Identity: sessA, Limit: 1, BeforeMessageID: latest.NextBeforeMessageID})
	if err != nil {
		t.Fatalf("list messages older page: %v", err)
	}
	if len(older.Items) != 1 || older.Items[0].Role != harness.MessageRoleUser {
		t.Fatalf("older page = %+v, want the user message", older)
	}

	// --- ListMessages：越权 / 缺失 ---
	if _, err := engine.ListMessages(ctx, harness.ListMessagesRequest{
		Identity: harness.Identity{TenantID: owner.TenantID, UserID: "intruder", SessionID: "sess-query-a"},
	}); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("cross-user list messages must return ErrPermissionDenied, got %v", err)
	}
	if _, err := engine.ListMessages(ctx, harness.ListMessagesRequest{
		Identity: harness.Identity{TenantID: "other-tenant", UserID: owner.UserID, SessionID: "sess-query-a"},
	}); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("cross-tenant list messages must return ErrPermissionDenied, got %v", err)
	}
	if _, err := engine.ListMessages(ctx, harness.ListMessagesRequest{
		Identity: harness.Identity{TenantID: owner.TenantID, UserID: owner.UserID, SessionID: "sess-missing"},
	}); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("missing session list messages must return ErrNotFound, got %v", err)
	}
	if _, err := engine.ListMessages(ctx, harness.ListMessagesRequest{
		Identity: harness.Identity{TenantID: owner.TenantID, SessionID: "sess-query-a"},
	}); !errors.Is(err, harness.ErrInvalidRequest) {
		t.Fatalf("incomplete identity list messages must return ErrInvalidRequest, got %v", err)
	}
}

// TestHostQueryAfterCloseE2E 验证查询 API 与既有入口一致：Engine Close 之后
// 所有查询一律返回 ErrClosed，不触碰已释放的存储资源。
func TestHostQueryAfterCloseE2E(t *testing.T) {
	engine, ctx := buildQueryTestEngine(t)
	identity := harness.Identity{TenantID: "public", UserID: "query-host", SessionID: "sess-closed"}
	if err := engine.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := engine.Close(ctx); err != nil {
		t.Fatalf("second close must be idempotent: %v", err)
	}

	if _, err := engine.GetSession(ctx, harness.GetSessionRequest{Identity: identity}); !errors.Is(err, harness.ErrClosed) {
		t.Fatalf("GetSession after close must return ErrClosed, got %v", err)
	}
	if _, err := engine.ListSessions(ctx, harness.ListSessionsRequest{Identity: identity}); !errors.Is(err, harness.ErrClosed) {
		t.Fatalf("ListSessions after close must return ErrClosed, got %v", err)
	}
	if _, err := engine.ListMessages(ctx, harness.ListMessagesRequest{Identity: identity}); !errors.Is(err, harness.ErrClosed) {
		t.Fatalf("ListMessages after close must return ErrClosed, got %v", err)
	}
	if _, err := engine.Artifacts().List(ctx, harness.ListArtifactsRequest{Identity: identity}); !errors.Is(err, harness.ErrClosed) {
		t.Fatalf("Artifacts.List after close must return ErrClosed, got %v", err)
	}
}

// TestHostArtifactListE2E 验证 ArtifactClient.List：按会话列出宿主可读
// artifact，支持 Kind / RunID 过滤与 Limit 截断；跨会话隔离、非法入参
// fail closed。
func TestHostArtifactListE2E(t *testing.T) {
	engine, ctx := buildQueryTestEngine(t)

	sessA := harness.Identity{TenantID: "public", UserID: "query-host", SessionID: "sess-art-a"}
	sessB := harness.Identity{TenantID: "public", UserID: "query-host", SessionID: "sess-art-b"}

	put := func(identity harness.Identity, name, mime string, kind harness.ArtifactKind) harness.ArtifactInfo {
		t.Helper()
		info, err := engine.Artifacts().Put(ctx, harness.PutArtifactRequest{
			Identity: identity, Name: name, MIME: mime, Kind: kind,
			Content: strings.NewReader("body of " + name),
		})
		if err != nil {
			t.Fatalf("put %s: %v", name, err)
		}
		return info
	}
	put(sessA, "notes.txt", "text/plain", harness.ArtifactKindFile)
	put(sessA, "diagram.png", "image/png", harness.ArtifactKindImage)
	runScoped := harness.Identity{TenantID: sessA.TenantID, UserID: sessA.UserID, SessionID: sessA.SessionID, RunID: "run-x"}
	put(runScoped, "run-output.json", "application/json", harness.ArtifactKindFile)
	put(sessB, "other.txt", "text/plain", harness.ArtifactKindFile)

	// --- 会话级全量：sess-a 三件，sess-b 一件，互不泄漏 ---
	all, err := engine.Artifacts().List(ctx, harness.ListArtifactsRequest{Identity: sessA})
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(all.Items) != 3 || all.HasMore {
		t.Fatalf("list all = %d items hasMore=%v, want 3 items", len(all.Items), all.HasMore)
	}
	for _, item := range all.Items {
		if item.Ref == "" || item.Name == "" || item.SizeBytes <= 0 || item.Hash == "" {
			t.Fatalf("artifact info incomplete: %+v", item)
		}
		switch harness.ArtifactKind(item.Kind) {
		case harness.ArtifactKindFile, harness.ArtifactKindImage:
		default:
			t.Fatalf("host list surfaced unexpected kind %q", item.Kind)
		}
	}
	other, err := engine.Artifacts().List(ctx, harness.ListArtifactsRequest{Identity: sessB})
	if err != nil {
		t.Fatalf("list artifacts sess-b: %v", err)
	}
	if len(other.Items) != 1 || other.Items[0].Name != "other.txt" {
		t.Fatalf("sess-b list = %+v, want only other.txt", other.Items)
	}

	// --- Kind / RunID 过滤 ---
	images, err := engine.Artifacts().List(ctx, harness.ListArtifactsRequest{Identity: sessA, Kind: harness.ArtifactKindImage})
	if err != nil {
		t.Fatalf("list images: %v", err)
	}
	if len(images.Items) != 1 || images.Items[0].Name != "diagram.png" {
		t.Fatalf("image filter = %+v, want diagram.png only", images.Items)
	}
	runOnly, err := engine.Artifacts().List(ctx, harness.ListArtifactsRequest{Identity: runScoped})
	if err != nil {
		t.Fatalf("list run-scoped: %v", err)
	}
	if len(runOnly.Items) != 1 || runOnly.Items[0].Name != "run-output.json" {
		t.Fatalf("run filter = %+v, want run-output.json only", runOnly.Items)
	}

	// --- Limit 截断 ---
	limited, err := engine.Artifacts().List(ctx, harness.ListArtifactsRequest{Identity: sessA, Limit: 2})
	if err != nil {
		t.Fatalf("list limited: %v", err)
	}
	if len(limited.Items) != 2 || !limited.HasMore {
		t.Fatalf("limited list = %d items hasMore=%v, want 2 items with HasMore", len(limited.Items), limited.HasMore)
	}

	// --- 非法入参 ---
	if _, err := engine.Artifacts().List(ctx, harness.ListArtifactsRequest{
		Identity: harness.Identity{TenantID: "public", SessionID: "sess-art-a"},
	}); !errors.Is(err, harness.ErrInvalidRequest) {
		t.Fatalf("incomplete identity list must return ErrInvalidRequest, got %v", err)
	}
	if _, err := engine.Artifacts().List(ctx, harness.ListArtifactsRequest{
		Identity: sessA, Kind: harness.ArtifactKind("tool_result"),
	}); !errors.Is(err, harness.ErrInvalidRequest) {
		t.Fatalf("system kind list must return ErrInvalidRequest, got %v", err)
	}
}
