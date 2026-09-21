package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/authcontext"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// makeToken 用给定 header/claims 与密钥手工拼一个 JWT,便于覆盖签名/算法/时间等分支。
func makeToken(t *testing.T, header map[string]any, claims map[string]any, secret []byte) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signing := enc(header) + "." + enc(claims)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signing))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signing + "." + sig
}

func hs256Header() map[string]any { return map[string]any{"alg": "HS256", "typ": "JWT"} }

func newJWTReq(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestNewAuthenticator_ModeSelection(t *testing.T) {
	if _, secure, err := NewAuthenticator(AuthConfig{Mode: "insecure"}); err != nil || secure {
		t.Fatalf("explicit insecure mode = (secure=%t, err=%v), want insecure success", secure, err)
	}
	if _, secure, err := NewAuthenticator(AuthConfig{Mode: "jwt", JWTSecret: "s"}); err != nil || !secure {
		t.Fatal("jwt mode with secret must be secure")
	}
	for name, config := range map[string]AuthConfig{
		"empty mode":         {},
		"unknown mode":       {Mode: "basic"},
		"jwt without secret": {Mode: "jwt"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := NewAuthenticator(config); err == nil {
				t.Fatalf("invalid auth config %#v was accepted", config)
			}
		})
	}
}

func TestBuildRejectsInvalidAuthBeforeInfrastructureInitialization(t *testing.T) {
	for name, config := range map[string]AuthConfig{
		"empty mode":         {},
		"unknown mode":       {Mode: "basic"},
		"jwt without secret": {Mode: "jwt"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Build(ModelConfig{}, StorageConfig{Backend: "memory"}, config, RedisConfig{}, nil, nil); err == nil || !strings.Contains(err.Error(), "auth:") {
				t.Fatalf("Build() error = %v, want auth configuration rejection", err)
			}
		})
	}
}

func TestInsecureAuthenticator_TrustsHeaders(t *testing.T) {
	auth, _, err := NewAuthenticator(AuthConfig{Mode: "insecure"})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("x-user-id", "u1")
	r.Header.Set("x-tenant-id", "t1")
	r.Header.Set("x-debug", "1")
	p, err := auth.Authenticate(r)
	if err != nil {
		t.Fatalf("insecure authenticate: %v", err)
	}
	if p.UserID != "u1" || p.TenantID != "t1" || !p.Debug || len(p.Scopes) != 1 || p.Scopes[0] != "agent.config.admin" {
		t.Fatalf("unexpected principal: %+v", p)
	}
}

func TestJWTAuthenticator_HappyPath(t *testing.T) {
	secret := []byte("top-secret")
	auth := &JWTAuthenticator{Secret: secret, Issuer: "harness", Audience: "harness", Now: func() time.Time { return time.Unix(1000, 0) }}
	tok := makeToken(t, hs256Header(), map[string]any{
		"sub": "user-42", "iss": "harness", "aud": "harness",
		"exp": 2000, "nbf": 500, "tenant_id": "acme", "debug": true, "scopes": []string{"read"},
	}, secret)
	req := newJWTReq(tok)
	req.Header.Set("x-tenant-id", "spoofed-tenant")
	req.Header.Set("x-user-id", "spoofed-user")
	p, err := auth.Authenticate(req)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if p.UserID != "user-42" || p.TenantID != "acme" || !p.Debug || len(p.Scopes) != 1 {
		t.Fatalf("unexpected principal: %+v", p)
	}
}

func TestJWTAuthenticator_AudienceArray(t *testing.T) {
	secret := []byte("s")
	auth := &JWTAuthenticator{Secret: secret, Audience: "harness", Now: func() time.Time { return time.Unix(1000, 0) }}
	tok := makeToken(t, hs256Header(), map[string]any{"sub": "u", "tenant_id": "t", "aud": []string{"other", "harness"}}, secret)
	if _, err := auth.Authenticate(newJWTReq(tok)); err != nil {
		t.Fatalf("array aud should match: %v", err)
	}
}

func TestJWTAuthenticator_Rejections(t *testing.T) {
	secret := []byte("top-secret")
	now := func() time.Time { return time.Unix(1000, 0) }
	auth := &JWTAuthenticator{Secret: secret, Issuer: "harness", Audience: "harness", Now: now}

	cases := map[string]string{
		"missing bearer":  "",
		"expired":         makeToken(t, hs256Header(), map[string]any{"sub": "u", "iss": "harness", "aud": "harness", "exp": 999}, secret),
		"not yet valid":   makeToken(t, hs256Header(), map[string]any{"sub": "u", "iss": "harness", "aud": "harness", "nbf": 1001}, secret),
		"wrong issuer":    makeToken(t, hs256Header(), map[string]any{"sub": "u", "iss": "evil", "aud": "harness"}, secret),
		"wrong audience":  makeToken(t, hs256Header(), map[string]any{"sub": "u", "iss": "harness", "aud": "nope"}, secret),
		"empty subject":   makeToken(t, hs256Header(), map[string]any{"sub": "", "iss": "harness", "aud": "harness"}, secret),
		"empty tenant":    makeToken(t, hs256Header(), map[string]any{"sub": "u", "iss": "harness", "aud": "harness"}, secret),
		"tampered sig":    makeToken(t, hs256Header(), map[string]any{"sub": "u", "iss": "harness", "aud": "harness"}, []byte("wrong-secret")),
		"alg none":        makeToken(t, map[string]any{"alg": "none", "typ": "JWT"}, map[string]any{"sub": "u", "iss": "harness", "aud": "harness"}, secret),
		"alg rs256 spoof": makeToken(t, map[string]any{"alg": "RS256", "typ": "JWT"}, map[string]any{"sub": "u", "iss": "harness", "aud": "harness"}, secret),
		"malformed":       "not.a.jwt.token",
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := auth.Authenticate(newJWTReq(tok)); err == nil {
				t.Fatalf("expected rejection for %q", name)
			}
		})
	}
}

func TestAuthTraceMiddleware_401OnBadCreds(t *testing.T) {
	auth := &JWTAuthenticator{Secret: []byte("s"), Now: time.Now}
	var reached bool
	h := authTraceMiddleware(auth, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newJWTReq(""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if reached {
		t.Fatal("handler must not run on auth failure")
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("missing WWW-Authenticate header")
	}
}

func TestAuthTraceMiddlewareTestingUIBypassSeedsAdminPrincipal(t *testing.T) {
	auth := &JWTAuthenticator{Secret: []byte("s"), Now: time.Now}
	var got authcontext.Principal
	h := authTraceMiddlewareWithOptions(auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ok bool
		got, ok = authcontext.FromContext(r.Context())
		if !ok {
			t.Fatal("principal missing")
		}
		w.WriteHeader(http.StatusNoContent)
	}), authTraceOptions{TestingUIBypass: true})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/owner/tenants", nil)
	req.Header.Set("x-user-id", "owner-console")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got.UserID != "owner-console" || got.TenantID != "platform" || !got.HasScope("platform.tenant.admin") || !got.Debug {
		t.Fatalf("testing UI principal = %#v", got)
	}
}

func TestAuthTraceMiddlewareTestingUIBypassIsOptIn(t *testing.T) {
	auth := &JWTAuthenticator{Secret: []byte("s"), Now: time.Now}
	var reached bool
	h := authTraceMiddlewareWithOptions(auth, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}), authTraceOptions{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/owner/tenants", nil))
	if rec.Code != http.StatusUnauthorized || reached {
		t.Fatalf("status=%d reached=%v body=%s", rec.Code, reached, rec.Body.String())
	}
}

func TestAuthTraceMiddlewareAllowsArtifactBearerRedemption(t *testing.T) {
	auth := &JWTAuthenticator{Secret: []byte("jwt-secret"), Now: time.Now}
	var reached bool
	h := authTraceMiddleware(auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		tc := observability.MustTraceContext(r.Context())
		if tc.TenantID != "" || tc.UserID != "" || tc.DebugEnabled {
			t.Fatalf("anonymous redemption retained untrusted principal fields: %#v", tc)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/download/opaque-capability", nil)
	req = req.WithContext(observability.WithTraceContext(req.Context(), observability.TraceContext{
		TraceID: "trace-redemption", TenantID: "spoofed", UserID: "spoofed", DebugEnabled: true,
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || !reached {
		t.Fatalf("bearer redemption was gated by JWT: status=%d reached=%v", rec.Code, reached)
	}
}

func TestAuthTraceMiddlewareAllowsOAuthCallbackWithoutHarnessJWT(t *testing.T) {
	auth := &JWTAuthenticator{Secret: []byte("jwt-secret"), Now: time.Now}
	var reached bool
	handler := authTraceMiddleware(auth, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/mcp/oauth/callback?state=opaque&code=code", nil))
	if recorder.Code != http.StatusNoContent || !reached {
		t.Fatalf("oauth callback was gated by JWT: status=%d reached=%v", recorder.Code, reached)
	}
}

func TestAuthTraceMiddleware_SeedsPrincipalIntoTrace(t *testing.T) {
	secret := []byte("s")
	auth := &JWTAuthenticator{Secret: secret, Now: func() time.Time { return time.Unix(1000, 0) }}
	tok := makeToken(t, hs256Header(), map[string]any{"sub": "user-9", "tenant_id": "acme", "debug": true}, secret)

	var gotUser, gotTenant string
	var gotDebug bool
	h := authTraceMiddleware(auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tc := observability.MustTraceContext(r.Context())
		gotUser, gotTenant, gotDebug = tc.UserID, tc.TenantID, tc.DebugEnabled
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newJWTReq(tok))
	if gotUser != "user-9" || gotTenant != "acme" || !gotDebug {
		t.Fatalf("trace not seeded from principal: user=%q tenant=%q debug=%v", gotUser, gotTenant, gotDebug)
	}
}

func TestAuthTraceMiddlewarePreservesTransportTraceAndSpan(t *testing.T) {
	auth := InsecureHeaderAuthenticator{}
	want := observability.TraceContext{TraceID: "trace-http", RootSpanID: "root-http", SpanID: "span-http", ParentSpanID: "parent-http"}
	h := authTraceMiddleware(auth, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got := observability.MustTraceContext(r.Context())
		if got.TraceID != want.TraceID || got.RootSpanID != want.RootSpanID || got.SpanID != want.SpanID || got.ParentSpanID != want.ParentSpanID {
			t.Fatalf("authentication replaced transport trace: got=%#v want=%#v", got, want)
		}
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req.Header.Set("x-tenant-id", "t1")
	req.Header.Set("x-user-id", "u1")
	req = req.WithContext(observability.WithTraceContext(req.Context(), want))
	h.ServeHTTP(httptest.NewRecorder(), req)
}
