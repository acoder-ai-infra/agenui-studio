package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/downloadtoken"
	metamem "github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	objmem "github.com/AGenUI/agenui-studio/harness/internal/artifact/objectstore"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol/server"
)

const downloadURLPath = "/api/v1/artifacts/download-url"

// newArtifactStoreWithCodec builds a memory-backed artifact store whose object
// store and redemption path share one download-token codec, so a minted URL can
// be redeemed in the same test.
func newArtifactStoreWithCodec(t *testing.T) *artifact.Store {
	t.Helper()
	codec, err := downloadtoken.New([]byte("download-url-handler-test-key"))
	if err != nil {
		t.Fatalf("new codec: %v", err)
	}
	objects := objmem.NewMemory()
	objects.SetDownloadCodec(codec)
	return artifact.NewStore(artifact.StoreConfig{
		ObjectStore:   objects,
		MetadataStore: metamem.NewMemory(),
		DownloadCodec: codec,
	})
}

func mustPutUserVisibleArtifact(t *testing.T, art *artifact.Store, tenant, user, sid, rid, payload string) string {
	t.Helper()
	putCtx := artifact.ContextWithActor(context.Background(), artifact.Actor{
		TenantID: tenant, SessionID: sid, RunID: rid, Role: artifact.ActorRuntime,
	})
	meta, err := art.Put(putCtx, artifact.PutArtifactRequest{
		TenantID: tenant, UserID: user, SessionID: sid, RunID: rid,
		OwnerModule: artifact.OwnerModuleModelGateway, OwnerID: rid,
		ArtifactType: artifact.ArtifactTypeFinalResult, MimeType: "text/plain",
		Name: "report.txt", Visibility: artifact.VisibilityUserVisible, Content: strings.NewReader(payload),
	})
	if err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	return meta.ArtifactRef
}

func TestCreateDownloadURLOwnerAndRoundTrip(t *testing.T) {
	deps, stores := newDeps()
	ring := observability.NewRingLogger(observability.NoopLogger{}, 10)
	deps.Logger = ring
	art := newArtifactStoreWithCodec(t)
	deps.Artifacts = art
	mustCreateOwnedSessionRun(t, stores, "s1", "r1", "acme", "u1")
	ref := mustPutUserVisibleArtifact(t, art, "acme", "u1", "s1", "r1", "secret output")
	router := server.NewRouter(deps)

	// Owner mints a download URL.
	body := `{"ref":"` + ref + `"}`
	req := httptest.NewRequest(http.MethodPost, downloadURLPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, withIdentity(req, "acme", "u1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner mint: want 200 got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("mint Cache-Control = %q", got)
	}
	var grant struct {
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &grant); err != nil {
		t.Fatalf("decode grant: %v", err)
	}
	if !strings.HasPrefix(grant.URL, "/api/v1/artifacts/download/") {
		t.Fatalf("unexpected download URL: %q", grant.URL)
	}
	// The bearer URL must not leak the ref or storage layout.
	if strings.Contains(grant.URL, ref) || strings.Contains(grant.URL, "tenants/") {
		t.Fatalf("download URL leaks ref/layout: %q", grant.URL)
	}
	if until := time.Until(grant.ExpiresAt); until <= 0 || until > 5*time.Minute+time.Second {
		t.Fatalf("expires_at outside policy window: %v", grant.ExpiresAt)
	}

	// Redeem the minted token at the download endpoint (bearer, no identity).
	token := strings.TrimPrefix(grant.URL, "/api/v1/artifacts/download/")
	redeem := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/download/"+token, nil)
	rrec := httptest.NewRecorder()
	router.ServeHTTP(rrec, redeem)
	if rrec.Code != http.StatusOK {
		t.Fatalf("redeem: want 200 got %d body=%s", rrec.Code, rrec.Body.String())
	}
	if got := rrec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("redeem Cache-Control = %q", got)
	}
	if got := rrec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("redeem X-Content-Type-Options = %q", got)
	}
	if got := rrec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("redeem Referrer-Policy = %q", got)
	}
	if got := rrec.Header().Get("Content-Disposition"); !strings.Contains(got, "attachment") || !strings.Contains(got, "report.txt") {
		t.Fatalf("redeem Content-Disposition = %q", got)
	}
	if got := rrec.Body.String(); got != "secret output" {
		t.Fatalf("redeemed bytes = %q, want %q", got, "secret output")
	}
	logs := ring.QueryLogs(observability.LogQuery{})
	encodedLogs, err := json.Marshal(logs)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 || !strings.Contains(string(encodedLogs), "artifact download audit") || strings.Contains(string(encodedLogs), token) || strings.Contains(string(encodedLogs), "storage_key") {
		t.Fatalf("unsafe or incomplete artifact audit logs: %s", encodedLogs)
	}
	tenantLogs := ring.QueryLogs(observability.LogQuery{TenantID: "acme"})
	if len(tenantLogs) != 2 {
		t.Fatalf("tenant-scoped artifact audit logs = %d, want issue and redeem", len(tenantLogs))
	}

	deleteCtx := artifact.ContextWithActor(context.Background(), artifact.Actor{
		TenantID: "acme", UserID: "u1", SessionID: "s1", RunID: "r1", Role: artifact.ActorRuntime,
	})
	if err := art.Delete(deleteCtx, ref, artifact.DeleteReasonUser); err != nil {
		t.Fatalf("delete artifact: %v", err)
	}
	deleted := httptest.NewRecorder()
	router.ServeHTTP(deleted, httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/download/"+token, nil))
	if deleted.Code == http.StatusOK {
		t.Fatal("deleted artifact token remained redeemable")
	}
	for _, secret := range []string{ref, "tenants/", "sessions/", "storage_key"} {
		if strings.Contains(deleted.Body.String(), secret) {
			t.Fatalf("deleted artifact response leaked %q: %s", secret, deleted.Body.String())
		}
	}
}

func TestArtifactDownloadRejectsOversizedTokenBeforeDecode(t *testing.T) {
	deps := server.Deps{Artifacts: newArtifactStoreWithCodec(t)}
	router := server.NewRouter(deps)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/download/"+strings.Repeat("A", downloadtoken.MaxEncodedTokenBytes+1), nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized token status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateDownloadURLCrossUserDenied(t *testing.T) {
	deps, stores := newDeps()
	art := newArtifactStoreWithCodec(t)
	deps.Artifacts = art
	mustCreateOwnedSessionRun(t, stores, "s1", "r1", "acme", "u1")
	ref := mustPutUserVisibleArtifact(t, art, "acme", "u1", "s1", "r1", "secret output")
	router := server.NewRouter(deps)

	body := `{"ref":"` + ref + `"}`
	req := httptest.NewRequest(http.MethodPost, downloadURLPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, withIdentity(req, "acme", "u2"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-user mint: want 403 got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateDownloadURLMissingRef(t *testing.T) {
	deps := server.Deps{}
	deps.Artifacts = newArtifactStoreWithCodec(t)
	router := server.NewRouter(deps)

	req := httptest.NewRequest(http.MethodPost, downloadURLPath, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing ref: want 400 got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateDownloadURLTTLClampedToPolicy(t *testing.T) {
	deps, stores := newDeps()
	art := newArtifactStoreWithCodec(t)
	deps.Artifacts = art
	mustCreateOwnedSessionRun(t, stores, "s1", "r1", "acme", "u1")
	ref := mustPutUserVisibleArtifact(t, art, "acme", "u1", "s1", "r1", "secret output")
	router := server.NewRouter(deps)

	// Request far beyond the 5-minute cap; the handler must clamp, not 400.
	body := `{"ref":"` + ref + `","ttl_seconds":100000}`
	req := httptest.NewRequest(http.MethodPost, downloadURLPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, withIdentity(req, "acme", "u1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("oversized ttl mint: want 200 got %d body=%s", rec.Code, rec.Body.String())
	}
	var grant struct {
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &grant); err != nil {
		t.Fatalf("decode grant: %v", err)
	}
	if until := time.Until(grant.ExpiresAt); until <= 0 || until > 5*time.Minute+time.Second {
		t.Fatalf("ttl not clamped to policy window: %v", until)
	}
}
