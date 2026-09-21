package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/authcontext"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/tenantadmin"
)

// TenantSessionCookie holds the tenant login secret key. The agent-console logs
// in by POSTing a key to /api/v1/tenant-session, which sets this HttpOnly
// cookie; subsequent requests are resolved to that tenant by
// TenantCookieAuthenticator. Switching tenants just replaces the cookie.
const TenantSessionCookie = "harness_tenant"

// Principal 是鉴权层从可信凭证中解析出的调用方身份。下游一切租户隔离 / session
// 归属 / debug 可见性都只读它,绝不信任客户端请求头。
type Principal struct {
	UserID   string
	TenantID string
	Scopes   []string
	Debug    bool // 是否允许查看 debug/internal 可见性(由凭证决定,而非 x-debug 头)
}

// Authenticator 校验一个 HTTP 请求并返回可信身份。返回 error 表示拒绝(401)。
type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}

// ErrUnauthenticated 是鉴权失败的哨兵错误。
var ErrUnauthenticated = errors.New("unauthenticated")

// --- 配置 ---------------------------------------------------------------------

// AuthConfig 选择鉴权模式。mode=insecure 直接读取请求头；mode=jwt 使用无状态
// Bearer JWT(HS256)。运行环境只负责选择配置文件，不隐式改写鉴权模式。
type AuthConfig struct {
	Mode         string `json:"mode"`       // "jwt" | "insecure"
	JWTSecret    string `json:"jwt_secret"` // HS256 共享密钥(mode=jwt 必填)
	JWTSecretEnv string `json:"jwt_secret_env,omitempty"`
	Issuer       string `json:"issuer"`   // 可选:校验 iss
	Audience     string `json:"audience"` // 可选:校验 aud
}

// NewAuthenticator 严格按显式配置构建 Authenticator。非法模式或缺少 JWT 密钥
// 直接返回错误，不能静默降级为 insecure。
func NewAuthenticator(cfg AuthConfig) (Authenticator, bool, error) {
	if err := validateAuthConfig(cfg); err != nil {
		return nil, false, err
	}
	switch strings.TrimSpace(cfg.Mode) {
	case "insecure":
		return InsecureHeaderAuthenticator{}, false, nil
	case "jwt":
		return &JWTAuthenticator{
			Secret:   []byte(cfg.JWTSecret),
			Issuer:   cfg.Issuer,
			Audience: cfg.Audience,
			Now:      time.Now,
		}, true, nil
	default:
		return nil, false, fmt.Errorf("auth: unsupported mode %q", cfg.Mode)
	}
}

func validateAuthConfig(cfg AuthConfig) error {
	switch strings.TrimSpace(cfg.Mode) {
	case "insecure":
		return nil
	case "jwt":
		if strings.TrimSpace(cfg.JWTSecret) == "" {
			return errors.New("auth: jwt mode requires a non-empty jwt_secret")
		}
		return nil
	default:
		return fmt.Errorf("auth: unsupported mode %q; expected insecure or jwt", cfg.Mode)
	}
}

// --- insecure:从请求头取身份 -------------------------------------------------

// InsecureHeaderAuthenticator directly trusts the public AGenUI Studio identity headers
// X-AGenUI-Tenant-ID / X-AGenUI-User-ID. The original x-tenant-id / x-user-id
// aliases remain as a local-development compatibility fallback.
// 它不校验调用方凭证，配置 owner 必须确保上游网络和请求头边界可信；任何能直接
// 访问服务的客户端都可以伪造身份。
type InsecureHeaderAuthenticator struct{}

func (InsecureHeaderAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	return Principal{
		TenantID: headerAnyOr(r, tenantadmin.DefaultTenantID, "X-AGenUI-Tenant-ID", "x-tenant-id"),
		UserID:   headerAnyOr(r, "anon", "X-AGenUI-User-ID", "x-user-id"),
		Scopes:   []string{"agent.config.admin"},
		Debug:    r.Header.Get("x-debug") == "1",
	}, nil
}

func headerAnyOr(r *http.Request, fallback string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(r.Header.Get(key)); value != "" {
			return value
		}
	}
	return fallback
}

// --- tenant cookie:密钥登录态解析(包装任意底层 Authenticator) ----------------

// TenantCookieAuthenticator resolves the tenant from the TenantSessionCookie
// (a tenant secret key) via the tenant directory, and returns an admin-scoped
// Principal for that tenant. When there is no cookie, it falls through to Base
// and then verifies the resolved tenant exists and is active in the directory.
// Unknown / paused cookies are rejected instead of falling back to another
// tenant, so every authenticated Principal is backed by the tenants table.
type TenantCookieAuthenticator struct {
	Base     Authenticator
	Resolver *tenantadmin.SQLManagedRegistry
}

func (a *TenantCookieAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	if a.Resolver != nil {
		if cookie, err := r.Cookie(TenantSessionCookie); err == nil && cookie.Value != "" {
			if tenant, resolveErr := a.Resolver.ResolveSecretKey(r.Context(), cookie.Value); resolveErr == nil {
				return Principal{
					TenantID: tenant.Definition.ID,
					UserID:   headerOr(r, "x-user-id", "agent-console"),
					Scopes:   []string{"agent.config.admin"},
					Debug:    r.Header.Get("x-debug") == "1",
				}, nil
			}
			return Principal{}, ErrUnauthenticated
		}
	}
	if a.Base == nil {
		return Principal{}, ErrUnauthenticated
	}
	principal, err := a.Base.Authenticate(r)
	if err != nil {
		return Principal{}, err
	}
	if a.Resolver == nil {
		return principal, nil
	}
	return a.validateTenant(r, principal)
}

func (a *TenantCookieAuthenticator) validateTenant(r *http.Request, principal Principal) (Principal, error) {
	tenant, err := a.Resolver.Get(r.Context(), principal.TenantID)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	if tenant.Definition.Status == tenantadmin.StatusPaused {
		return Principal{}, ErrUnauthenticated
	}
	return principal, nil
}

// --- jwt(生产):HS256 Bearer Token 校验(标准库,无三方依赖) ------------------

// JWTAuthenticator 校验 Authorization: Bearer <jwt>(HS256)。身份完全来自被签名
// 保护的 claims,忽略一切明文身份头。
type JWTAuthenticator struct {
	Secret   []byte
	Issuer   string
	Audience string
	Now      func() time.Time
}

func (a *JWTAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	raw := bearerToken(r)
	if raw == "" {
		return Principal{}, ErrUnauthenticated
	}
	claims, err := verifyHS256(raw, a.Secret, a.now())
	if err != nil {
		return Principal{}, err
	}
	if a.Issuer != "" && claims.Issuer != a.Issuer {
		return Principal{}, ErrUnauthenticated
	}
	if a.Audience != "" && !claims.Audience.contains(a.Audience) {
		return Principal{}, ErrUnauthenticated
	}
	if claims.Subject == "" || claims.TenantID == "" {
		return Principal{}, ErrUnauthenticated
	}
	return Principal{
		UserID:   claims.Subject,
		TenantID: claims.TenantID,
		Scopes:   claims.Scopes,
		Debug:    claims.Debug,
	}, nil
}

func (a *JWTAuthenticator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// jwtClaims 是本服务识别的 claims 子集。tenant_id/debug/scopes 为自定义私有声明。
type jwtClaims struct {
	Subject   string        `json:"sub"`
	Issuer    string        `json:"iss"`
	Audience  audienceClaim `json:"aud"`
	ExpiresAt int64         `json:"exp"`
	NotBefore int64         `json:"nbf"`
	TenantID  string        `json:"tenant_id"`
	Debug     bool          `json:"debug"`
	Scopes    []string      `json:"scopes"`
}

// audienceClaim 兼容 RFC 7519:aud 可为字符串或字符串数组。
type audienceClaim []string

func (a *audienceClaim) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*a = audienceClaim{single}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

func (a audienceClaim) contains(want string) bool {
	for _, v := range a {
		if v == want {
			return true
		}
	}
	return false
}

// verifyHS256 校验一个 HS256 JWT 的签名与时间声明,返回解析后的 claims。
func verifyHS256(token string, secret []byte, now time.Time) (jwtClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return jwtClaims{}, ErrUnauthenticated
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return jwtClaims{}, ErrUnauthenticated
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil || header.Alg != "HS256" {
		// 显式拒绝 alg=none 与非 HS256,防止算法混淆攻击。
		return jwtClaims{}, ErrUnauthenticated
	}
	signing := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signing))
	expected := mac.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return jwtClaims{}, ErrUnauthenticated
	}
	if subtle.ConstantTimeCompare(expected, got) != 1 {
		return jwtClaims{}, ErrUnauthenticated
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return jwtClaims{}, ErrUnauthenticated
	}
	var claims jwtClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return jwtClaims{}, ErrUnauthenticated
	}
	nowUnix := now.Unix()
	if claims.ExpiresAt != 0 && nowUnix >= claims.ExpiresAt {
		return jwtClaims{}, ErrUnauthenticated
	}
	if claims.NotBefore != 0 && nowUnix < claims.NotBefore {
		return jwtClaims{}, ErrUnauthenticated
	}
	return claims, nil
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// --- 中间件 -------------------------------------------------------------------

// authTraceMiddleware 是 Harness 的鉴权+trace 边界:先校验凭证得到可信身份,再据此
// 种入 TraceContext(trace_id/tenant/user/debug)。鉴权失败直接 401,请求不进入下游。
func authTraceMiddleware(auth Authenticator, next http.Handler) http.Handler {
	return authTraceMiddlewareWithOptions(auth, next, authTraceOptions{})
}

type authTraceOptions struct {
	TestingUIBypass bool
	Tenants         *tenantadmin.SQLManagedRegistry
}

func authTraceMiddlewareWithOptions(auth Authenticator, next http.Handler, options authTraceOptions) http.Handler {
	ids := observability.NewULIDGenerator("")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Static debug-console assets carry no data and must load before the user
		// can supply a token. Artifact 下载兑换使用自身短期 bearer token，不再
		// 要求 JWT；其余 API 仍由此处校验调用方身份。
		if r.Method == http.MethodGet && isArtifactDownloadPath(r.URL.Path) {
			// 外层传输中间件只负责生成 trace，会暂存原始身份头。兑换端没有
			// JWT principal，必须先清空这些不可信字段；令牌验真并回查元数据后，
			// handler 才会用 Artifact 的真实 tenant 补全成功审计。
			tc := observability.MustTraceContext(r.Context())
			tc.TenantID = ""
			tc.UserID = ""
			tc.DebugEnabled = false
			next.ServeHTTP(w, r.WithContext(observability.WithTraceContext(r.Context(), tc)))
			return
		}
		// OAuth callbacks are authenticated by a high-entropy, one-time state
		// bound to tenant/user/server in durable storage. Browser redirects from
		// the authorization server do not carry the Harness JWT.
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/mcp/oauth/callback" {
			next.ServeHTTP(w, r)
			return
		}
		// Tenant login gate: reachable before any tenant is chosen (and in JWT
		// mode without a Bearer token). The handler validates the secret key
		// itself against the tenant directory; it does not trust a principal.
		if r.URL.Path == "/api/v1/tenant-session" || r.URL.Path == "/api/v1/tenant-options" {
			next.ServeHTTP(w, r)
			return
		}
		if options.TestingUIBypass && isTestingUIAPIPath(r.URL.Path) {
			principal := testingUIPrincipal(r, options.Tenants)
			injectAuthenticatedRequest(ids, principal, next, w, r)
			return
		}
		principal, err := auth.Authenticate(r)
		if err != nil {
			w.Header().Set("content-type", "application/json")
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error_code":"UNAUTHENTICATED","message":"invalid or missing credentials"}`))
			return
		}
		injectAuthenticatedRequest(ids, principal, next, w, r)
	})
}

func injectAuthenticatedRequest(ids observability.IDGenerator, principal Principal, next http.Handler, w http.ResponseWriter, r *http.Request) {
	// Authentication enriches the trace created by the transport boundary; it
	// must not replace trace/span IDs or response headers stop correlating with
	// persisted events. Direct handler tests may not install that boundary, so
	// mint only the missing trace ID here.
	tc := observability.MustTraceContext(r.Context())
	if tc.TraceID == "" {
		tc.TraceID = ids.NewTraceID()
	}
	tc.TenantID = principal.TenantID
	tc.UserID = principal.UserID
	tc.Channel = r.Header.Get("x-channel")
	tc.DebugEnabled = principal.Debug
	ctx := observability.WithTraceContext(r.Context(), tc)
	ctx = authcontext.WithPrincipal(ctx, authcontext.Principal{
		UserID: principal.UserID, TenantID: principal.TenantID,
		Scopes: append([]string(nil), principal.Scopes...), Debug: principal.Debug,
	})
	next.ServeHTTP(w, r.WithContext(ctx))
}

func isTestingUIAPIPath(path string) bool {
	switch {
	case strings.HasPrefix(path, "/api/v1/admin/"):
		return true
	case strings.HasPrefix(path, "/api/v1/owner/tenants"):
		return true
	case strings.HasPrefix(path, "/api/v1/agents/"):
		return true
	case path == "/api/v1/sessions" || strings.HasPrefix(path, "/api/v1/sessions/"):
		return true
	case strings.HasPrefix(path, "/api/v1/runs/"):
		return true
	case strings.HasPrefix(path, "/api/v1/control-requests/"):
		return true
	case path == "/api/v1/artifacts/download-url" || path == "/api/v1/artifacts/ref" || strings.HasPrefix(path, "/api/v1/artifacts/ref/"):
		return true
	case strings.HasPrefix(path, "/api/v1/debug/") || path == "/api/v1/usage" || path == "/api/v1/ai/chat":
		return true
	default:
		return false
	}
}

func testingUIPrincipal(r *http.Request, tenants *tenantadmin.SQLManagedRegistry) Principal {
	tenantID := headerOr(r, "x-tenant-id", tenantadmin.DefaultTenantID)
	if strings.HasPrefix(r.URL.Path, "/api/v1/owner/") {
		tenantID = "platform"
	} else if tenants != nil {
		if cookie, err := r.Cookie(TenantSessionCookie); err == nil && cookie.Value != "" {
			if tenant, resolveErr := tenants.ResolveSecretKey(r.Context(), cookie.Value); resolveErr == nil {
				tenantID = tenant.Definition.ID
			}
		}
	}
	return Principal{
		TenantID: tenantID,
		UserID:   headerOr(r, "x-user-id", "agent-console"),
		Scopes:   []string{"agent.config.admin"},
		Debug:    true,
	}
}

func isArtifactDownloadPath(path string) bool {
	const prefix = "/api/v1/artifacts/download/"
	token := strings.TrimPrefix(path, prefix)
	return token != path && token != "" && !strings.Contains(token, "/")
}
