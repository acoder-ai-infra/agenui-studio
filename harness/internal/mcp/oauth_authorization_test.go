package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestOAuthAuthorizationManagerCompletesDiscoveryPKCEAndPerUserGrant(t *testing.T) {
	var registeredRedirectURI string
	var tokenForm url.Values
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/source-control":
			writeOAuthTestJSON(t, w, map[string]any{
				"resource":              oauthTestOrigin(r) + "/source-control",
				"authorization_servers": []string{oauthTestOrigin(r)},
			})
		case "/.well-known/oauth-authorization-server":
			writeOAuthTestJSON(t, w, map[string]any{
				"issuer":                           oauthTestOrigin(r),
				"authorization_endpoint":           oauthTestOrigin(r) + "/oauth/authorize",
				"token_endpoint":                   oauthTestOrigin(r) + "/oauth/token",
				"registration_endpoint":            oauthTestOrigin(r) + "/oauth/register",
				"response_types_supported":         []string{"code"},
				"grant_types_supported":            []string{"authorization_code", "refresh_token"},
				"code_challenge_methods_supported": []string{"S256"},
			})
		case "/oauth/register":
			var request struct {
				RedirectURIs []string `json:"redirect_uris"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			registeredRedirectURI = request.RedirectURIs[0]
			writeOAuthTestJSON(t, w, map[string]any{"client_id": "client-1", "client_secret": "secret-1", "token_endpoint_auth_method": "client_secret_post"})
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			tokenForm = r.PostForm
			writeOAuthTestJSON(t, w, map[string]any{"access_token": "access-1", "refresh_token": "refresh-1", "token_type": "Bearer", "expires_in": 3600})
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "oauth-flow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ApplySQLiteOAuthSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := ApplySQLiteOAuthAuthorizationSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := ApplySQLiteOAuthRefreshSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	codec, err := NewOAuthTokenCodec([]byte("unit-test-secret"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	pendingStore := NewSQLOAuthPendingStore(db, codec)
	pendingStore.now = func() time.Time { return now }
	manager, err := NewOAuthAuthorizationManager(OAuthAuthorizationManagerConfig{
		PublicBaseURL: "http://127.0.0.1:18083", ClientName: "Harness Test", PendingTTL: 10 * time.Minute,
		HTTPClient: provider.Client(), Grants: NewSQLOAuthGrantStore(db, codec), Pending: pendingStore,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := ServerDefinition{
		ID: "mcp_1", Version: "v1", TenantID: "tenant-a", Scope: ScopeTenant,
		Transport: "streamable-http", Endpoint: provider.URL + "/source-control",
		Auth: AuthConfig{Type: AuthTypeOAuth2, Provider: "oauth-provider", Resource: provider.URL + "/source-control", Scopes: []string{"source:read"}},
	}
	principal := ManagementDebugPrincipal("tenant-a", "alice")
	started, err := manager.Start(context.Background(), principal, definition)
	if err != nil {
		t.Fatal(err)
	}
	authorizeURL, err := url.Parse(started.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	if authorizeURL.Path != "/oauth/authorize" || authorizeURL.Query().Get("code_challenge_method") != "S256" || authorizeURL.Query().Get("code_challenge") == "" || authorizeURL.Query().Get("state") == "" {
		t.Fatalf("authorization url = %s", started.AuthorizationURL)
	}
	if registeredRedirectURI != "http://127.0.0.1:18083/api/v1/mcp/oauth/callback" {
		t.Fatalf("registered redirect URI = %q", registeredRedirectURI)
	}

	completed, err := manager.Complete(context.Background(), authorizeURL.Query().Get("state"), "code-1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if completed.ServerID != "mcp_1" || completed.TenantID != "tenant-a" || completed.UserID != "alice" {
		t.Fatalf("completion = %#v", completed)
	}
	if tokenForm.Get("code_verifier") == "" || tokenForm.Get("code") != "code-1" || tokenForm.Get("client_id") != "client-1" || tokenForm.Get("client_secret") != "secret-1" || tokenForm.Get("resource") != definition.Auth.Resource {
		t.Fatalf("token form = %#v", tokenForm)
	}
	key, err := OAuthGrantKeyFor(principal, definition)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := manager.grants.GetGrant(context.Background(), key)
	if err != nil || grant.AccessToken != "access-1" || grant.RefreshToken != "refresh-1" || !grant.AccessExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("grant=%#v err=%v", grant, err)
	}
	if _, err := manager.Complete(context.Background(), authorizeURL.Query().Get("state"), "code-2", "", ""); err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("replayed state error = %v", err)
	}
	var pendingVerifier, pendingSecret string
	if err := db.QueryRow(`SELECT code_verifier_ciphertext, client_secret_ciphertext FROM mcp_oauth_pending LIMIT 1`).Scan(&pendingVerifier, &pendingSecret); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(pendingVerifier, tokenForm.Get("code_verifier")) || strings.Contains(pendingSecret, "secret-1") {
		t.Fatal("pending OAuth secrets were stored in plaintext")
	}
}

func TestOAuthPendingStateExpiresBeforeTokenExchange(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "oauth-expired.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ApplySQLiteOAuthAuthorizationSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	codec, _ := NewOAuthTokenCodec([]byte("unit-test-secret"))
	store := NewSQLOAuthPendingStore(db, codec)
	now := time.Unix(200, 0).UTC()
	store.now = func() time.Time { return now }
	if err := store.Save(context.Background(), OAuthPendingAuthorization{State: "state-1", TenantID: "tenant", UserID: "alice", ServerID: "mcp_1", Provider: "oauth-provider", ScopeHash: "scope", CodeVerifier: "verifier", ClientID: "client", TokenEndpoint: "https://issuer.example/token", RedirectURI: "http://127.0.0.1/callback", ExpiresAt: now.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Consume(context.Background(), "state-1"); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired state error = %v", err)
	}
}

func TestOAuthCredentialProviderRefreshesExpiredGrantAndRotatesRefreshToken(t *testing.T) {
	var refreshRequest url.Values
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		refreshRequest = r.PostForm
		writeOAuthTestJSON(t, w, map[string]any{"access_token": "access-2", "refresh_token": "refresh-2", "token_type": "Bearer", "expires_in": 3600})
	}))
	defer tokenServer.Close()

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "oauth-refresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ApplySQLiteOAuthSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := ApplySQLiteOAuthRefreshSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	codec, err := NewOAuthTokenCodec([]byte("refresh-secret"))
	if err != nil {
		t.Fatal(err)
	}
	grants := NewSQLOAuthGrantStore(db, codec)
	now := time.Unix(1_700_000_000, 0).UTC()
	manager, err := NewOAuthAuthorizationManager(OAuthAuthorizationManagerConfig{
		PublicBaseURL: "http://127.0.0.1:18083", HTTPClient: tokenServer.Client(), Grants: grants,
		Pending: noopOAuthPendingStore{}, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := ServerDefinition{ID: "mcp_1", Version: "v1", Scope: ScopeTenant, TenantID: "tenant", Auth: AuthConfig{Type: AuthTypeOAuth2, Provider: "oauth-provider", Resource: "https://mcp.example/source-control"}}
	principal := ManagementDebugPrincipal("tenant", "alice")
	key, err := OAuthGrantKeyFor(principal, definition)
	if err != nil {
		t.Fatal(err)
	}
	if err := grants.SaveGrant(context.Background(), OAuthGrant{Key: key, Resource: definition.Auth.Resource, Issuer: "https://mcp.example", TokenEndpoint: tokenServer.URL + "/oauth/token", ClientID: "client-1", ClientSecret: "secret-1", TokenAuthMethod: "client_secret_post", AccessToken: "access-1", RefreshToken: "refresh-1", AccessExpiresAt: now.Add(-time.Minute), Status: "active"}); err != nil {
		t.Fatal(err)
	}
	provider := OAuthCredentialProvider{Store: grants, Refresher: manager, Now: func() time.Time { return now }}
	header, err := provider.AuthorizationHeader(context.Background(), principal, definition)
	if err != nil || header != "Bearer access-2" {
		t.Fatalf("header=%q err=%v", header, err)
	}
	if refreshRequest.Get("grant_type") != "refresh_token" || refreshRequest.Get("refresh_token") != "refresh-1" || refreshRequest.Get("client_id") != "client-1" || refreshRequest.Get("client_secret") != "secret-1" {
		t.Fatalf("refresh form = %#v", refreshRequest)
	}
	grant, err := grants.GetGrant(context.Background(), key)
	if err != nil || grant.AccessToken != "access-2" || grant.RefreshToken != "refresh-2" || grant.Revision != 2 {
		t.Fatalf("refreshed grant=%#v err=%v", grant, err)
	}
}

func TestOAuthCredentialProviderMapsInvalidRefreshTokenToAuthorizationRequired(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		writeOAuthTestJSON(t, w, map[string]any{"error": "invalid_grant"})
	}))
	defer tokenServer.Close()
	now := time.Unix(1_700_000_000, 0).UTC()
	grants := NewInMemoryOAuthGrantStore()
	manager, err := NewOAuthAuthorizationManager(OAuthAuthorizationManagerConfig{
		PublicBaseURL: "http://127.0.0.1:18083", HTTPClient: tokenServer.Client(), Grants: grants,
		Pending: noopOAuthPendingStore{}, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := ServerDefinition{ID: "mcp_1", Version: "v1", Scope: ScopeTenant, TenantID: "tenant", Auth: AuthConfig{Type: AuthTypeOAuth2, Provider: "oauth-provider", Resource: "https://mcp.example/source-control"}}
	principal := ManagementDebugPrincipal("tenant", "alice")
	key, err := OAuthGrantKeyFor(principal, definition)
	if err != nil {
		t.Fatal(err)
	}
	if err := grants.SaveGrant(context.Background(), OAuthGrant{Key: key, Resource: definition.Auth.Resource, TokenEndpoint: tokenServer.URL + "/oauth/token", ClientID: "client-1", AccessToken: "access-1", RefreshToken: "refresh-1", AccessExpiresAt: now.Add(-time.Minute), Status: "active"}); err != nil {
		t.Fatal(err)
	}
	_, err = (OAuthCredentialProvider{Store: grants, Refresher: manager, Now: func() time.Time { return now }}).AuthorizationHeader(context.Background(), principal, definition)
	var required *AuthorizationRequiredError
	if !errors.As(err, &required) {
		t.Fatalf("error=%v, want AuthorizationRequiredError", err)
	}
}

type noopOAuthPendingStore struct{}

func (noopOAuthPendingStore) Save(context.Context, OAuthPendingAuthorization) error { return nil }
func (noopOAuthPendingStore) Consume(context.Context, string) (OAuthPendingAuthorization, error) {
	return OAuthPendingAuthorization{}, ErrOAuthStateInvalid
}

func writeOAuthTestJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("content-type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatal(err)
	}
}

// HostURLForTest reconstructs the httptest origin without making production
// discovery trust forwarded headers.
func oauthTestOrigin(r *http.Request) string {
	return "http://" + r.Host
}
