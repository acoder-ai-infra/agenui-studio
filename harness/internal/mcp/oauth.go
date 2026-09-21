package mcp

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var (
	ErrOAuthProviderRequired = errors.New("mcp oauth provider required")
	ErrOAuthTokenInvalid     = errors.New("mcp oauth token invalid")
)

const oauthMaintainerGrantUserID = "__mcp_oauth_maintainer_shared__"

type AuthorizationRequiredError struct {
	ServerID string
	Provider string
	Scopes   []string
	Resource string
}

func (e *AuthorizationRequiredError) Error() string {
	if e == nil {
		return "mcp oauth authorization required"
	}
	return "mcp oauth authorization required for server " + e.ServerID
}

func (e *AuthorizationRequiredError) Is(target error) bool {
	return target == ErrOAuthGrantMissing
}

type OAuthGrantKey struct {
	TenantID  string
	UserID    string
	ServerID  string
	Provider  string
	ScopeHash string
}

type OAuthGrant struct {
	Key              OAuthGrantKey
	Revision         int64
	Scopes           []string
	Resource         string
	Issuer           string
	TokenEndpoint    string
	ClientID         string
	ClientSecret     string
	TokenAuthMethod  string
	AccessToken      string
	RefreshToken     string
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
	Status           string
	UpdatedAt        time.Time
}

type OAuthGrantStore interface {
	SaveGrant(ctx context.Context, grant OAuthGrant) error
	GetGrant(ctx context.Context, key OAuthGrantKey) (OAuthGrant, error)
	DeleteGrant(ctx context.Context, key OAuthGrantKey) error
}

// OAuthGrantCASStore is implemented by durable stores that can fence
// concurrent refreshes. The base OAuthGrantStore remains intentionally small
// for local/test adapters.
type OAuthGrantCASStore interface {
	OAuthGrantStore
	SaveGrantCAS(ctx context.Context, grant OAuthGrant, expectedRevision int64) (bool, error)
}

// OAuthGrantRefresher is the optional provider used by the credential path
// when an access token has expired. Refresh is kept behind a narrow port so
// MCP transport code does not know OAuth protocol details.
type OAuthGrantRefresher interface {
	RefreshGrant(ctx context.Context, principal Principal, definition ServerDefinition) error
}

type CredentialProvider interface {
	AuthorizationHeader(ctx context.Context, principal Principal, definition ServerDefinition) (string, error)
}

type OAuthCredentialProvider struct {
	Store     OAuthGrantStore
	Refresher OAuthGrantRefresher
	Now       func() time.Time
}

func (p OAuthCredentialProvider) AuthorizationHeader(ctx context.Context, principal Principal, definition ServerDefinition) (string, error) {
	if definition.Auth.Type != AuthTypeOAuth2 {
		return "", errorsConfiguration("mcp oauth credential provider only supports oauth2 definitions")
	}
	if err := principal.Validate(); err != nil {
		return "", err
	}
	if OAuthGrantMode(definition.Auth) == OAuthGrantModeUser && strings.TrimSpace(principal.UserID) == "" {
		return "", ErrPrincipalRequired
	}
	if p.Store == nil {
		return "", oauthRequired(definition)
	}
	key, err := OAuthGrantKeyFor(principal, definition)
	if err != nil {
		return "", err
	}
	grant, err := p.Store.GetGrant(ctx, key)
	if err != nil {
		if errors.Is(err, ErrOAuthGrantMissing) {
			return "", oauthRequired(definition)
		}
		return "", err
	}
	if strings.TrimSpace(grant.AccessToken) == "" || !strings.EqualFold(firstNonEmpty(grant.Status, "active"), "active") {
		return "", oauthRequired(definition)
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	if !grant.AccessExpiresAt.IsZero() && !now().Before(grant.AccessExpiresAt) {
		if p.Refresher == nil {
			return "", oauthRequired(definition)
		}
		if err := p.Refresher.RefreshGrant(ctx, principal, definition); err != nil {
			return "", err
		}
		grant, err = p.Store.GetGrant(ctx, key)
		if err != nil || strings.TrimSpace(grant.AccessToken) == "" || !strings.EqualFold(firstNonEmpty(grant.Status, "active"), "active") {
			if err != nil {
				return "", err
			}
			return "", oauthRequired(definition)
		}
		if !grant.AccessExpiresAt.IsZero() && !now().Before(grant.AccessExpiresAt) {
			return "", oauthRequired(definition)
		}
	}
	return "Bearer " + grant.AccessToken, nil
}

func OAuthGrantKeyFor(principal Principal, definition ServerDefinition) (OAuthGrantKey, error) {
	provider := strings.TrimSpace(definition.Auth.Provider)
	if provider == "" {
		return OAuthGrantKey{}, ErrOAuthProviderRequired
	}
	userID, err := OAuthGrantUserIDFor(principal, definition.Auth)
	if err != nil {
		return OAuthGrantKey{}, err
	}
	return OAuthGrantKey{
		TenantID:  principal.TenantID,
		UserID:    userID,
		ServerID:  definition.ID,
		Provider:  provider,
		ScopeHash: OAuthScopeHash(definition.Auth.Scopes, definition.Auth.Resource),
	}, nil
}

func OAuthGrantMode(auth AuthConfig) string {
	switch strings.TrimSpace(auth.GrantMode) {
	case "", OAuthGrantModeUser:
		return OAuthGrantModeUser
	case OAuthGrantModeMaintainer:
		return OAuthGrantModeMaintainer
	default:
		return strings.TrimSpace(auth.GrantMode)
	}
}

func OAuthGrantUserIDFor(principal Principal, auth AuthConfig) (string, error) {
	switch OAuthGrantMode(auth) {
	case OAuthGrantModeUser:
		if strings.TrimSpace(principal.UserID) == "" {
			return "", ErrPrincipalRequired
		}
		return principal.UserID, nil
	case OAuthGrantModeMaintainer:
		return oauthMaintainerGrantUserID, nil
	default:
		return "", errorsConfiguration("unsupported mcp oauth grant_mode")
	}
}

func OAuthScopeHash(scopes []string, resource string) string {
	normalized := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope != "" {
			normalized = append(normalized, scope)
		}
	}
	sort.Strings(normalized)
	return hashJSON(struct {
		Scopes   []string `json:"scopes"`
		Resource string   `json:"resource,omitempty"`
	}{Scopes: normalized, Resource: strings.TrimSpace(resource)})
}

func oauthRequired(definition ServerDefinition) error {
	return &AuthorizationRequiredError{
		ServerID: definition.ID,
		Provider: definition.Auth.Provider,
		Scopes:   append([]string(nil), definition.Auth.Scopes...),
		Resource: definition.Auth.Resource,
	}
}

type OAuthTokenCodec interface {
	SealToken(plaintext string) (string, error)
	OpenToken(ciphertext string) (string, error)
}

type OAuthTokenAEADCodec struct {
	aead cipher.AEAD
}

func NewOAuthTokenCodec(secret []byte) (*OAuthTokenAEADCodec, error) {
	if len(secret) == 0 {
		return nil, errors.New("mcp oauth token secret is required")
	}
	key := sha256.Sum256(append([]byte("harness:mcp-oauth-token:key:v1\x00"), secret...))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &OAuthTokenAEADCodec{aead: aead}, nil
}

func (c *OAuthTokenAEADCodec) SealToken(plaintext string) (string, error) {
	if c == nil || strings.TrimSpace(plaintext) == "" {
		return "", ErrOAuthTokenInvalid
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), []byte("harness:mcp-oauth-token:v1"))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (c *OAuthTokenAEADCodec) OpenToken(ciphertext string) (string, error) {
	if c == nil || strings.TrimSpace(ciphertext) == "" {
		return "", ErrOAuthTokenInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", ErrOAuthTokenInvalid
	}
	nonceSize := c.aead.NonceSize()
	if len(raw) <= nonceSize {
		return "", ErrOAuthTokenInvalid
	}
	plaintext, err := c.aead.Open(nil, raw[:nonceSize], raw[nonceSize:], []byte("harness:mcp-oauth-token:v1"))
	if err != nil {
		return "", ErrOAuthTokenInvalid
	}
	return string(plaintext), nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func validateOAuthGrant(grant OAuthGrant) error {
	if grant.Key.TenantID == "" || grant.Key.UserID == "" || grant.Key.ServerID == "" || grant.Key.Provider == "" || grant.Key.ScopeHash == "" {
		return errorsConfiguration("mcp oauth grant key is incomplete")
	}
	if strings.TrimSpace(grant.AccessToken) == "" {
		return ErrOAuthTokenInvalid
	}
	if grant.Status != "" && !strings.EqualFold(grant.Status, "active") && !strings.EqualFold(grant.Status, "revoked") {
		return fmt.Errorf("mcp oauth grant status %q is invalid", grant.Status)
	}
	return nil
}
