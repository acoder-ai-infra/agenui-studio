package mcp

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestOAuthCredentialProviderRequiresPerUserGrant(t *testing.T) {
	definition := ServerDefinition{
		ID: "source_control", Version: "v1", Scope: ScopeTenant, TenantID: "tenant",
		Auth: AuthConfig{Type: AuthTypeOAuth2, Provider: "oauth_provider", Scopes: []string{"source:read"}, Resource: "https://mcp.example.test/source-control"},
	}
	provider := OAuthCredentialProvider{Store: NewInMemoryOAuthGrantStore(), Now: func() time.Time { return time.Unix(100, 0) }}
	principal := Principal{TenantID: "tenant", UserID: "alice", AgentID: "agent"}
	_, err := provider.AuthorizationHeader(context.Background(), principal, definition)
	var required *AuthorizationRequiredError
	if !errors.As(err, &required) || !errors.Is(err, ErrOAuthGrantMissing) || required.ServerID != "source_control" || required.Provider != "oauth_provider" {
		t.Fatalf("missing grant error = %T %#v", err, err)
	}

	key, err := OAuthGrantKeyFor(principal, definition)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Store.SaveGrant(context.Background(), OAuthGrant{
		Key: key, Scopes: definition.Auth.Scopes, Resource: definition.Auth.Resource,
		AccessToken: "alice-access-token", AccessExpiresAt: time.Unix(200, 0), Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	header, err := provider.AuthorizationHeader(context.Background(), principal, definition)
	if err != nil || header != "Bearer alice-access-token" {
		t.Fatalf("header=%q err=%v", header, err)
	}

	_, err = provider.AuthorizationHeader(context.Background(), Principal{TenantID: "tenant", UserID: "bob", AgentID: "agent"}, definition)
	if !errors.Is(err, ErrOAuthGrantMissing) {
		t.Fatalf("bob reused alice grant: %v", err)
	}
}

func TestOAuthCredentialProviderCanReuseMaintainerGrantAcrossUsers(t *testing.T) {
	definition := ServerDefinition{
		ID: "source_control", Version: "v1", Scope: ScopeTenant, TenantID: "tenant",
		Auth: AuthConfig{
			Type: AuthTypeOAuth2, Provider: "oauth_provider", GrantMode: OAuthGrantModeMaintainer,
			Scopes: []string{"source:read"}, Resource: "https://mcp.example.test/source-control",
		},
	}
	store := NewInMemoryOAuthGrantStore()
	provider := OAuthCredentialProvider{Store: store, Now: func() time.Time { return time.Unix(100, 0) }}
	alice := Principal{TenantID: "tenant", UserID: "alice", AgentID: "agent"}
	key, err := OAuthGrantKeyFor(alice, definition)
	if err != nil {
		t.Fatal(err)
	}
	if key.UserID != oauthMaintainerGrantUserID {
		t.Fatalf("shared grant user id = %q", key.UserID)
	}
	if err := store.SaveGrant(context.Background(), OAuthGrant{
		Key: key, Scopes: definition.Auth.Scopes, Resource: definition.Auth.Resource,
		AccessToken: "maintainer-access-token", AccessExpiresAt: time.Unix(200, 0), Status: "active",
	}); err != nil {
		t.Fatal(err)
	}

	header, err := provider.AuthorizationHeader(context.Background(), Principal{TenantID: "tenant", UserID: "bob", AgentID: "agent"}, definition)
	if err != nil || header != "Bearer maintainer-access-token" {
		t.Fatalf("header=%q err=%v", header, err)
	}

	_, err = provider.AuthorizationHeader(context.Background(), Principal{TenantID: "other", UserID: "bob", AgentID: "agent"}, definition)
	if !errors.Is(err, ErrOAuthGrantMissing) {
		t.Fatalf("cross-tenant shared grant was reused: %v", err)
	}
}

func TestManagedOAuthClientProviderInjectsBearerPerPrincipal(t *testing.T) {
	definition := ServerDefinition{
		ID: "source_control", Version: "v1", Scope: ScopeTenant, TenantID: "tenant",
		Transport: "streamable-http", Endpoint: "https://mcp.example.test/source-control",
		Auth: AuthConfig{Type: AuthTypeOAuth2, Provider: "oauth_provider", Scopes: []string{"source:read"}},
	}
	store := NewInMemoryOAuthGrantStore()
	principal := Principal{TenantID: "tenant", UserID: "alice", AgentID: "agent"}
	key, err := OAuthGrantKeyFor(principal, definition)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveGrant(context.Background(), OAuthGrant{Key: key, AccessToken: "token-1", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	client, err := (ManagedClientProvider{Credentials: OAuthCredentialProvider{Store: store}}).Client(context.Background(), definition, principal)
	if err != nil {
		t.Fatal(err)
	}
	httpClient, ok := client.(*StreamableHTTPClient)
	if !ok {
		t.Fatalf("client type = %T", client)
	}
	if got := httpClient.headers["Authorization"]; got != "Bearer token-1" {
		t.Fatalf("Authorization header = %q", got)
	}
}

func TestSQLOAuthGrantStoreSealsTokensAndReadsCurrentGrant(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mcp-oauth.db"))
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
	codec, err := NewOAuthTokenCodec([]byte("unit-test-oauth-secret"))
	if err != nil {
		t.Fatal(err)
	}
	store := NewSQLOAuthGrantStore(db, codec)
	key := OAuthGrantKey{TenantID: "tenant", UserID: "alice", ServerID: "aone_code", Provider: "aone_mcp", ScopeHash: OAuthScopeHash([]string{"server:code"}, "")}
	if err := store.SaveGrant(context.Background(), OAuthGrant{
		Key: key, Scopes: []string{"server:code"}, AccessToken: "plain-access", RefreshToken: "plain-refresh",
		AccessExpiresAt: time.Unix(200, 0), RefreshExpiresAt: time.Unix(300, 0),
	}); err != nil {
		t.Fatal(err)
	}
	var accessCiphertext, refreshCiphertext string
	if err := db.QueryRow(`SELECT access_token_ciphertext, refresh_token_ciphertext FROM mcp_oauth_grants WHERE tenant_id='tenant'`).Scan(&accessCiphertext, &refreshCiphertext); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(accessCiphertext, "plain-access") || strings.Contains(refreshCiphertext, "plain-refresh") {
		t.Fatalf("oauth token was stored in plaintext: access=%q refresh=%q", accessCiphertext, refreshCiphertext)
	}
	grant, err := store.GetGrant(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if grant.AccessToken != "plain-access" || grant.RefreshToken != "plain-refresh" || len(grant.Scopes) != 1 || grant.Scopes[0] != "server:code" {
		t.Fatalf("grant = %#v", grant)
	}
}
