package server_test

// turn_prepare_integration_test.go 验证 HTTP 入口与 SDK 入口共享同一条预回合
// 扩展治理链（R2a Test Plan：HTTP handler 带 TurnPreparer 的集成测试）：
// Deps.TurnPreparer 注入 kernel.TurnPipeline 后，POST /api/v1/sessions/{sid}/runs
// 在 OpenTurn 之前依次执行 IdentityResolver / ContextContributor /
// InputNormalizer，产出反映在被 dispatch 的 OpenTurnResult 上。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol/server"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// capturingDispatcher 捕获被 dispatch 的 OpenTurnResult 供断言。
type capturingDispatcher struct {
	mu   sync.Mutex
	turn *storage.OpenTurnResult
}

func (d *capturingDispatcher) Dispatch(_ context.Context, turn *storage.OpenTurnResult) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.turn = turn
	return nil
}

func (d *capturingDispatcher) captured() *storage.OpenTurnResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.turn
}

// httpIdentityResolver 把调用方身份映射到固定租户。
type httpIdentityResolver struct{}

func (httpIdentityResolver) Resolve(_ context.Context, _ extension.IdentityRequest) (extension.ResolvedIdentity, error) {
	return extension.ResolvedIdentity{TenantID: "resolved-tenant"}, nil
}

// httpContributor 产出一个业务 context fragment。
type httpContributor struct{}

func (httpContributor) Contribute(_ context.Context, _ extension.ContribRequest) ([]extension.ContextFragment, error) {
	return []extension.ContextFragment{{
		Kind: "kb.snippet", Source: extension.FragmentSourceBusiness, Text: "kb body", Priority: 7,
	}}, nil
}

// httpNormalizer 给用户输入加前缀；failErr 非空时拒绝。
type httpNormalizer struct{ failErr error }

func (n httpNormalizer) ID() string { return "http.normalizer" }

func (n httpNormalizer) Normalize(_ context.Context, req extension.NormalizeRequest) (extension.NormalizedInput, error) {
	if n.failErr != nil {
		return extension.NormalizedInput{}, n.failErr
	}
	original := ""
	if len(req.RawInput.Parts) > 0 {
		original = req.RawInput.Parts[0].Text
	}
	return extension.NormalizedInput{Message: extension.NormalizedMessage{
		Role:  "user",
		Parts: []extension.NormalizedPart{{Kind: "text", Text: "NORM|" + original}},
	}}, nil
}

func turnPipelineForHTTP(t *testing.T, entries ...kernel.ExtensionEntry) *kernel.TurnPipeline {
	t.Helper()
	catalog, err := kernel.NewExtensionCatalog(entries)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	return kernel.NewTurnPipeline(catalog, kernel.TurnEnvironment{
		Environment: "local", SDKVersion: kernel.SDKContractVersion, SchemaVersions: kernel.CanonicalSchemaVersions(),
	})
}

// TestCreateRunExecutesTurnPreparerBeforeOpenTurn 验证 HTTP 开 Run 路径穿过
// 预回合管线：身份被 resolver 覆盖、输入被 normalizer 改写、fragment 随
// OpenTurnResult 透传给 dispatcher。
func TestCreateRunExecutesTurnPreparerBeforeOpenTurn(t *testing.T) {
	deps, _ := newDeps()
	dispatcher := &capturingDispatcher{}
	deps.Dispatcher = dispatcher
	deps.TurnPreparer = turnPipelineForHTTP(t,
		kernel.ExtensionEntry{ID: "http.identity", Kind: kernel.ExtIdentityResolver, Implementation: httpIdentityResolver{}},
		kernel.ExtensionEntry{ID: "http.contrib", Kind: kernel.ExtContextContributor, Implementation: httpContributor{}},
		kernel.ExtensionEntry{ID: "http.normalizer", Kind: kernel.ExtInputNormalizer, Implementation: httpNormalizer{}},
	)
	router := server.NewRouter(deps)

	req := withUser(httptest.NewRequest(http.MethodPost, "/api/v1/sessions/s-turnprep/runs",
		strings.NewReader(`{"agent_id":"agent-x","user_content_preview":"original"}`)), "owner")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code >= 300 {
		t.Fatalf("create run status=%d body=%s", rec.Code, rec.Body.String())
	}

	turn := dispatcher.captured()
	if turn == nil {
		t.Fatal("dispatcher did not receive the turn")
	}
	// InputNormalizer 的产出成为冻结的用户输入预览。
	if turn.UserMessage == nil || turn.UserMessage.ContentPreview != "NORM|original" {
		t.Fatalf("user message not normalized: %+v", turn.UserMessage)
	}
	// IdentityResolver 的租户覆盖反映在 Run 事实上。
	if turn.Run.TenantID != "resolved-tenant" {
		t.Fatalf("run tenant = %q; want resolved-tenant", turn.Run.TenantID)
	}
	// ContextContributor 的 fragment 随 OpenTurnResult 透传（进 ModelContext）。
	if len(turn.ContextFragments) != 1 || turn.ContextFragments[0].Content != "kb body" {
		t.Fatalf("context fragments not propagated: %+v", turn.ContextFragments)
	}
	if !strings.Contains(turn.ContextFragments[0].Source, "extension:context_contributor:kb.snippet") {
		t.Fatalf("fragment source lost derived id: %q", turn.ContextFragments[0].Source)
	}
}

// TestCreateRunTurnPreparerFailureFailsClosed 验证扩展拒绝时 fail-closed：
// 不开 Turn、不 dispatch，返回 422。
func TestCreateRunTurnPreparerFailureFailsClosed(t *testing.T) {
	deps, stores := newDeps()
	dispatcher := &capturingDispatcher{}
	deps.Dispatcher = dispatcher
	deps.TurnPreparer = turnPipelineForHTTP(t,
		kernel.ExtensionEntry{ID: "http.normalizer", Kind: kernel.ExtInputNormalizer, Implementation: httpNormalizer{failErr: context.DeadlineExceeded}},
	)
	router := server.NewRouter(deps)

	req := withUser(httptest.NewRequest(http.MethodPost, "/api/v1/sessions/s-turnfail/runs",
		strings.NewReader(`{"user_content_preview":"nope"}`)), "owner")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d; want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "TURN_PIPELINE_REJECTED") {
		t.Fatalf("error code missing: %s", rec.Body.String())
	}
	if dispatcher.captured() != nil {
		t.Fatal("failed pipeline must not dispatch")
	}
	// fail-closed：不得留下任何 Run 事实。
	if _, err := stores.Sessions.Get(context.Background(), "s-turnfail"); err == nil {
		t.Fatal("session must not be created when pipeline fails closed")
	}
}
