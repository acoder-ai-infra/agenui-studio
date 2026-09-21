package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol/server"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

func TestUsage_TenantScoped(t *testing.T) {
	deps, stores := newDeps()
	ctx := context.Background()
	rec := func(tenant, id string) *storage.ModelUsageRecord {
		return &storage.ModelUsageRecord{ID: id, TenantID: tenant, Provider: "mock", Model: "mock-model", PromptTokens: 12, CompletionTokens: 8}
	}
	if err := stores.Usage.Record(ctx, rec("acme", "u1")); err != nil {
		t.Fatal(err)
	}
	if err := stores.Usage.Record(ctx, rec("other", "u2")); err != nil {
		t.Fatal(err)
	}
	router := server.NewRouter(deps)

	r := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/usage", nil)
	req = req.WithContext(observability.WithTraceContext(req.Context(), observability.TraceContext{TenantID: "acme"}))
	router.ServeHTTP(r, req)
	if r.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	body := r.Body.String()
	if !strings.Contains(body, `"u1"`) {
		t.Fatalf("acme record missing: %s", body)
	}
	if strings.Contains(body, `"u2"`) {
		t.Fatalf("tenant leak: other-tenant record visible: %s", body)
	}
}
