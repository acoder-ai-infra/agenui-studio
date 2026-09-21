package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const oauthCallbackPath = "/api/v1/mcp/oauth/callback"

type OAuthAuthorizationManagerConfig struct {
	PublicBaseURL                  string
	ClientName                     string
	PendingTTL                     time.Duration
	HTTPClient                     *http.Client
	Grants                         OAuthGrantStore
	Pending                        OAuthPendingStore
	Now                            func() time.Time
	AllowInsecureHTTPPublicBaseURL bool
}

type OAuthAuthorizationManager struct {
	publicBaseURL *url.URL
	clientName    string
	pendingTTL    time.Duration
	httpClient    *http.Client
	grants        OAuthGrantStore
	pending       OAuthPendingStore
	now           func() time.Time
	random        io.Reader
	refreshMu     sync.Mutex
}

type OAuthAuthorizationStart struct {
	ServerID         string    `json:"server_id"`
	Status           string    `json:"status"`
	AuthorizationURL string    `json:"authorization_url"`
	ExpiresAt        time.Time `json:"expires_at"`
}

type OAuthAuthorizationCompletion struct {
	TenantID string `json:"tenant_id"`
	UserID   string `json:"user_id"`
	ServerID string `json:"server_id"`
	Status   string `json:"status"`
}

type OAuthAuthorizationStatus struct {
	ServerID  string    `json:"server_id"`
	Status    string    `json:"status"`
	GrantMode string    `json:"grant_mode,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

type oauthProtectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
}

type oauthAuthorizationServerMetadata struct {
	Issuer                        string   `json:"issuer"`
	AuthorizationEndpoint         string   `json:"authorization_endpoint"`
	TokenEndpoint                 string   `json:"token_endpoint"`
	RegistrationEndpoint          string   `json:"registration_endpoint"`
	ResponseTypesSupported        []string `json:"response_types_supported"`
	GrantTypesSupported           []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
}

type oauthClientRegistration struct {
	ClientID                string `json:"client_id"`
	ClientSecret            string `json:"client_secret"`
	TokenEndpointAuthMethod string `json:"token_endpoint_auth_method"`
}

type oauthHTTPError struct {
	StatusCode int
	Code       string
}

func (e *oauthHTTPError) Error() string {
	if e == nil {
		return "oauth upstream request failed"
	}
	return fmt.Sprintf("oauth http status=%d error=%s", e.StatusCode, firstNonEmpty(e.Code, "upstream_error"))
}

func NewOAuthAuthorizationManager(config OAuthAuthorizationManagerConfig) (*OAuthAuthorizationManager, error) {
	base, err := url.Parse(strings.TrimSpace(config.PublicBaseURL))
	if err != nil || base.Host == "" || !safeOAuthPublicBaseURL(base, config.AllowInsecureHTTPPublicBaseURL) || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errorsConfiguration("mcp oauth public_base_url must be an https URL or an http loopback URL")
	}
	if config.Grants == nil || config.Pending == nil {
		return nil, errorsConfiguration("mcp oauth grant and pending stores are required")
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	ttl := config.PendingTTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &OAuthAuthorizationManager{
		publicBaseURL: base, clientName: firstNonEmpty(config.ClientName, "Harness Harness"), pendingTTL: ttl,
		httpClient: client, grants: config.Grants, pending: config.Pending, now: now, random: rand.Reader,
	}, nil
}

func (m *OAuthAuthorizationManager) Start(ctx context.Context, principal Principal, definition ServerDefinition) (OAuthAuthorizationStart, error) {
	if m == nil || m.pending == nil || m.grants == nil {
		return OAuthAuthorizationStart{}, errorsConfiguration("mcp oauth authorization manager is unavailable")
	}
	if err := principal.Validate(); err != nil {
		return OAuthAuthorizationStart{}, err
	}
	if principal.UserID == "" {
		return OAuthAuthorizationStart{}, ErrPrincipalRequired
	}
	if definition.Auth.Type != AuthTypeOAuth2 {
		return OAuthAuthorizationStart{}, errorsConfiguration("mcp server does not use oauth2")
	}
	resource := firstNonEmpty(definition.Auth.Resource, definition.Endpoint)
	resourceURL, err := parseSafeOAuthURL(resource)
	if err != nil {
		return OAuthAuthorizationStart{}, fmt.Errorf("mcp oauth resource: %w", err)
	}
	protected, err := m.discoverProtectedResource(ctx, resourceURL)
	if err != nil {
		return OAuthAuthorizationStart{}, err
	}
	if len(protected.AuthorizationServers) == 0 {
		return OAuthAuthorizationStart{}, errors.New("mcp oauth protected resource has no authorization server")
	}
	metadataResource, err := parseSafeOAuthURL(protected.Resource)
	if err != nil || metadataResource.String() != resourceURL.String() {
		return OAuthAuthorizationStart{}, errors.New("mcp oauth protected resource metadata does not match requested resource")
	}
	authorizationServer, err := m.discoverAuthorizationServer(ctx, protected.AuthorizationServers[0])
	if err != nil {
		return OAuthAuthorizationStart{}, err
	}
	if !containsString(authorizationServer.ResponseTypesSupported, "code") || !containsString(authorizationServer.CodeChallengeMethodsSupported, "S256") {
		return OAuthAuthorizationStart{}, errors.New("mcp oauth authorization server must support code and PKCE S256")
	}
	redirectURI := m.publicBaseURL.ResolveReference(&url.URL{Path: oauthCallbackPath}).String()
	client, err := m.registerClient(ctx, authorizationServer, redirectURI)
	if err != nil {
		return OAuthAuthorizationStart{}, err
	}
	state, err := randomBase64URL(m.random, 32)
	if err != nil {
		return OAuthAuthorizationStart{}, err
	}
	verifier, err := randomBase64URL(m.random, 32)
	if err != nil {
		return OAuthAuthorizationStart{}, err
	}
	challengeDigest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeDigest[:])
	now := m.now().UTC()
	key, err := OAuthGrantKeyFor(principal, definition)
	if err != nil {
		return OAuthAuthorizationStart{}, err
	}
	if err := m.pending.Save(ctx, OAuthPendingAuthorization{
		State: state, TenantID: key.TenantID, UserID: key.UserID, ServerID: key.ServerID, Provider: key.Provider, ScopeHash: key.ScopeHash,
		Scopes: append([]string(nil), definition.Auth.Scopes...), Resource: resource, Issuer: authorizationServer.Issuer,
		TokenEndpoint: authorizationServer.TokenEndpoint, RedirectURI: redirectURI, ClientID: client.ClientID, ClientSecret: client.ClientSecret,
		TokenAuthMethod: client.TokenEndpointAuthMethod, CodeVerifier: verifier, CreatedAt: now, ExpiresAt: now.Add(m.pendingTTL),
	}); err != nil {
		return OAuthAuthorizationStart{}, err
	}
	authorize, err := parseSafeOAuthURL(authorizationServer.AuthorizationEndpoint)
	if err != nil {
		return OAuthAuthorizationStart{}, err
	}
	query := authorize.Query()
	query.Set("response_type", "code")
	query.Set("client_id", client.ClientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("state", state)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	query.Set("resource", resource)
	if len(definition.Auth.Scopes) > 0 {
		query.Set("scope", strings.Join(definition.Auth.Scopes, " "))
	}
	authorize.RawQuery = query.Encode()
	return OAuthAuthorizationStart{ServerID: definition.ID, Status: "authorization_required", AuthorizationURL: authorize.String(), ExpiresAt: now.Add(m.pendingTTL)}, nil
}

func (m *OAuthAuthorizationManager) Complete(ctx context.Context, state, code, callbackError, errorDescription string) (OAuthAuthorizationCompletion, error) {
	if m == nil || m.pending == nil || m.grants == nil {
		return OAuthAuthorizationCompletion{}, errorsConfiguration("mcp oauth authorization manager is unavailable")
	}
	pending, err := m.pending.Consume(ctx, state)
	if err != nil {
		return OAuthAuthorizationCompletion{}, err
	}
	if callbackError != "" {
		return OAuthAuthorizationCompletion{}, fmt.Errorf("mcp oauth authorization denied: %s", firstNonEmpty(errorDescription, callbackError))
	}
	if strings.TrimSpace(code) == "" {
		return OAuthAuthorizationCompletion{}, errors.New("mcp oauth callback code is required")
	}
	token, err := m.exchangeCode(ctx, pending, code)
	if err != nil {
		return OAuthAuthorizationCompletion{}, err
	}
	now := m.now().UTC()
	expiresAt := time.Time{}
	if token.ExpiresIn > 0 {
		expiresAt = now.Add(time.Duration(token.ExpiresIn) * time.Second)
	}
	grant := OAuthGrant{
		Key:             OAuthGrantKey{TenantID: pending.TenantID, UserID: pending.UserID, ServerID: pending.ServerID, Provider: pending.Provider, ScopeHash: pending.ScopeHash},
		Scopes:          pending.Scopes,
		Resource:        pending.Resource,
		Issuer:          pending.Issuer,
		TokenEndpoint:   pending.TokenEndpoint,
		ClientID:        pending.ClientID,
		ClientSecret:    pending.ClientSecret,
		TokenAuthMethod: pending.TokenAuthMethod,
		AccessToken:     token.AccessToken,
		RefreshToken:    token.RefreshToken,
		AccessExpiresAt: expiresAt, Status: "active", UpdatedAt: now,
	}
	if err := m.grants.SaveGrant(ctx, grant); err != nil {
		return OAuthAuthorizationCompletion{}, err
	}
	return OAuthAuthorizationCompletion{TenantID: pending.TenantID, UserID: pending.UserID, ServerID: pending.ServerID, Status: "authorized"}, nil
}

// RefreshGrant refreshes an expired grant and persists any rotated refresh
// token. A process-local mutex avoids duplicate upstream refreshes while the
// durable CAS store fences concurrent refreshes across instances.
func (m *OAuthAuthorizationManager) RefreshGrant(ctx context.Context, principal Principal, definition ServerDefinition) error {
	if m == nil || m.grants == nil {
		return errorsConfiguration("mcp oauth authorization manager is unavailable")
	}
	key, err := OAuthGrantKeyFor(principal, definition)
	if err != nil {
		return err
	}
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	grant, err := m.grants.GetGrant(ctx, key)
	if err != nil {
		if errors.Is(err, ErrOAuthGrantMissing) {
			return oauthRequired(definition)
		}
		return err
	}
	now := m.now
	if now == nil {
		now = time.Now
	}
	if strings.EqualFold(firstNonEmpty(grant.Status, "active"), "active") &&
		(grant.AccessExpiresAt.IsZero() || now().Before(grant.AccessExpiresAt)) {
		return nil
	}
	if strings.TrimSpace(grant.RefreshToken) == "" || strings.TrimSpace(grant.TokenEndpoint) == "" || strings.TrimSpace(grant.ClientID) == "" {
		return oauthRequired(definition)
	}
	token, err := m.exchangeRefreshToken(ctx, grant)
	if err != nil {
		var upstreamErr *oauthHTTPError
		if errors.As(err, &upstreamErr) && (strings.EqualFold(upstreamErr.Code, "invalid_grant") || strings.EqualFold(upstreamErr.Code, "invalid_token")) {
			return oauthRequired(definition)
		}
		return err
	}
	refreshed := grant
	refreshed.AccessToken = token.AccessToken
	if strings.TrimSpace(token.RefreshToken) != "" {
		refreshed.RefreshToken = token.RefreshToken
	}
	refreshed.AccessExpiresAt = time.Time{}
	if token.ExpiresIn > 0 {
		refreshed.AccessExpiresAt = now().Add(time.Duration(token.ExpiresIn) * time.Second)
	}
	refreshed.Status = "active"
	refreshed.UpdatedAt = now().UTC()
	if cas, ok := m.grants.(OAuthGrantCASStore); ok && grant.Revision > 0 {
		won, err := cas.SaveGrantCAS(ctx, refreshed, grant.Revision)
		if err != nil {
			return err
		}
		if won {
			return nil
		}
		latest, getErr := m.grants.GetGrant(ctx, key)
		if getErr == nil && (latest.AccessExpiresAt.IsZero() || now().Before(latest.AccessExpiresAt)) && latest.Status == "active" {
			return nil
		}
		return oauthRequired(definition)
	}
	return m.grants.SaveGrant(ctx, refreshed)
}

func (m *OAuthAuthorizationManager) Status(ctx context.Context, principal Principal, definition ServerDefinition) (OAuthAuthorizationStatus, error) {
	if m == nil || m.grants == nil {
		return OAuthAuthorizationStatus{}, errorsConfiguration("mcp oauth authorization manager is unavailable")
	}
	key, err := OAuthGrantKeyFor(principal, definition)
	if err != nil {
		return OAuthAuthorizationStatus{}, err
	}
	grant, err := m.grants.GetGrant(ctx, key)
	if errors.Is(err, ErrOAuthGrantMissing) {
		return OAuthAuthorizationStatus{ServerID: definition.ID, Status: "authorization_required", GrantMode: OAuthGrantMode(definition.Auth)}, nil
	}
	if err != nil {
		return OAuthAuthorizationStatus{}, err
	}
	status := "authorized"
	if grant.Status != "active" || (!grant.AccessExpiresAt.IsZero() && !m.now().Before(grant.AccessExpiresAt)) {
		status = "authorization_required"
	}
	return OAuthAuthorizationStatus{ServerID: definition.ID, Status: status, GrantMode: OAuthGrantMode(definition.Auth), ExpiresAt: grant.AccessExpiresAt}, nil
}

func (m *OAuthAuthorizationManager) Revoke(ctx context.Context, principal Principal, definition ServerDefinition) error {
	key, err := OAuthGrantKeyFor(principal, definition)
	if err != nil {
		return err
	}
	return m.grants.DeleteGrant(ctx, key)
}

func (m *OAuthAuthorizationManager) discoverProtectedResource(ctx context.Context, resource *url.URL) (oauthProtectedResourceMetadata, error) {
	metadataURL := &url.URL{Scheme: resource.Scheme, Host: resource.Host, Path: "/.well-known/oauth-protected-resource" + resource.EscapedPath()}
	var metadata oauthProtectedResourceMetadata
	if err := m.getJSON(ctx, metadataURL.String(), &metadata); err != nil {
		return metadata, fmt.Errorf("discover mcp oauth protected resource: %w", err)
	}
	if metadata.Resource == "" {
		return metadata, errors.New("mcp oauth protected resource metadata is missing resource")
	}
	return metadata, nil
}

func (m *OAuthAuthorizationManager) discoverAuthorizationServer(ctx context.Context, issuer string) (oauthAuthorizationServerMetadata, error) {
	issuerURL, err := parseSafeOAuthURL(issuer)
	if err != nil {
		return oauthAuthorizationServerMetadata{}, err
	}
	path := "/.well-known/oauth-authorization-server"
	if issuerURL.EscapedPath() != "" && issuerURL.EscapedPath() != "/" {
		path += issuerURL.EscapedPath()
	}
	metadataURL := &url.URL{Scheme: issuerURL.Scheme, Host: issuerURL.Host, Path: path}
	var metadata oauthAuthorizationServerMetadata
	if err := m.getJSON(ctx, metadataURL.String(), &metadata); err != nil {
		return metadata, fmt.Errorf("discover mcp oauth authorization server: %w", err)
	}
	if metadata.Issuer == "" || metadata.AuthorizationEndpoint == "" || metadata.TokenEndpoint == "" || metadata.RegistrationEndpoint == "" {
		return metadata, errors.New("mcp oauth authorization server metadata is incomplete")
	}
	metadataIssuer, err := parseSafeOAuthURL(metadata.Issuer)
	if err != nil || metadataIssuer.String() != issuerURL.String() {
		return metadata, errors.New("mcp oauth authorization server issuer mismatch")
	}
	for _, endpoint := range []string{metadata.Issuer, metadata.AuthorizationEndpoint, metadata.TokenEndpoint, metadata.RegistrationEndpoint} {
		if _, err := parseSafeOAuthURL(endpoint); err != nil {
			return metadata, err
		}
	}
	return metadata, nil
}

func (m *OAuthAuthorizationManager) registerClient(ctx context.Context, metadata oauthAuthorizationServerMetadata, redirectURI string) (oauthClientRegistration, error) {
	payload, err := json.Marshal(map[string]any{
		"client_name": m.clientName, "redirect_uris": []string{redirectURI}, "grant_types": []string{"authorization_code", "refresh_token"},
		"response_types": []string{"code"}, "token_endpoint_auth_method": "none",
	})
	if err != nil {
		return oauthClientRegistration{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, metadata.RegistrationEndpoint, bytes.NewReader(payload))
	if err != nil {
		return oauthClientRegistration{}, err
	}
	req.Header.Set("content-type", "application/json")
	var registration oauthClientRegistration
	if err := m.doJSON(req, &registration); err != nil {
		return registration, fmt.Errorf("register mcp oauth client: %w", err)
	}
	if registration.ClientID == "" {
		return registration, errors.New("mcp oauth registration response is missing client_id")
	}
	if registration.TokenEndpointAuthMethod == "" {
		if registration.ClientSecret == "" {
			registration.TokenEndpointAuthMethod = "none"
		} else {
			registration.TokenEndpointAuthMethod = "client_secret_post"
		}
	}
	return registration, nil
}

type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
}

func (m *OAuthAuthorizationManager) exchangeCode(ctx context.Context, pending OAuthPendingAuthorization, code string) (oauthTokenResponse, error) {
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {pending.RedirectURI},
		"client_id": {pending.ClientID}, "code_verifier": {pending.CodeVerifier}, "resource": {pending.Resource},
	}
	if pending.ClientSecret != "" && pending.TokenAuthMethod != "none" {
		form.Set("client_secret", pending.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pending.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return oauthTokenResponse{}, err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	var token oauthTokenResponse
	if err := m.doJSON(req, &token); err != nil {
		return token, fmt.Errorf("exchange mcp oauth authorization code: %w", err)
	}
	if token.AccessToken == "" || (token.TokenType != "" && !strings.EqualFold(token.TokenType, "Bearer")) {
		return token, errors.New("mcp oauth token response is missing a bearer access token")
	}
	return token, nil
}

func (m *OAuthAuthorizationManager) exchangeRefreshToken(ctx context.Context, grant OAuthGrant) (oauthTokenResponse, error) {
	form := url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {grant.RefreshToken},
		"client_id": {grant.ClientID}, "resource": {grant.Resource},
	}
	if grant.ClientSecret != "" && grant.TokenAuthMethod != "none" {
		form.Set("client_secret", grant.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, grant.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return oauthTokenResponse{}, err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	var token oauthTokenResponse
	if err := m.doJSON(req, &token); err != nil {
		return token, fmt.Errorf("refresh mcp oauth grant: %w", err)
	}
	if token.AccessToken == "" || (token.TokenType != "" && !strings.EqualFold(token.TokenType, "Bearer")) {
		return token, errors.New("mcp oauth refresh response is missing a bearer access token")
	}
	return token, nil
}

func (m *OAuthAuthorizationManager) getJSON(ctx context.Context, endpoint string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	return m.doJSON(req, target)
}

func (m *OAuthAuthorizationManager) doJSON(req *http.Request, target any) error {
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var oauthError struct {
			Code        string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(data, &oauthError)
		return &oauthHTTPError{StatusCode: resp.StatusCode, Code: firstNonEmpty(oauthError.Code, "upstream_error")}
	}
	if resp.Request == nil || !safeOAuthURL(resp.Request.URL) {
		return errors.New("mcp oauth redirect resolved to an unsafe endpoint")
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode oauth response: %w", err)
	}
	return nil
}

func parseSafeOAuthURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || !safeOAuthURL(parsed) {
		return nil, errors.New("mcp oauth endpoint must use https or http loopback without userinfo")
	}
	return parsed, nil
}

func safeOAuthURL(value *url.URL) bool {
	if value == nil {
		return false
	}
	if value.Scheme == "https" {
		return true
	}
	if value.Scheme != "http" {
		return false
	}
	host := value.Hostname()
	return strings.EqualFold(host, "localhost") || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

func safeOAuthPublicBaseURL(value *url.URL, allowInsecureHTTP bool) bool {
	if safeOAuthURL(value) {
		return true
	}
	return allowInsecureHTTP && value != nil && value.Scheme == "http" && value.Hostname() != ""
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func randomBase64URL(source io.Reader, size int) (string, error) {
	data := make([]byte, size)
	if _, err := io.ReadFull(source, data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}
