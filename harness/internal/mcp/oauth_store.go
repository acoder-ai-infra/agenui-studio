package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

type InMemoryOAuthGrantStore struct {
	mu     sync.RWMutex
	grants map[OAuthGrantKey]OAuthGrant
}

func NewInMemoryOAuthGrantStore() *InMemoryOAuthGrantStore {
	return &InMemoryOAuthGrantStore{grants: make(map[OAuthGrantKey]OAuthGrant)}
}

func (s *InMemoryOAuthGrantStore) SaveGrant(_ context.Context, grant OAuthGrant) error {
	if err := validateOAuthGrant(grant); err != nil {
		return err
	}
	grant.Scopes = append([]string(nil), grant.Scopes...)
	if grant.Status == "" {
		grant.Status = "active"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if grant.Revision <= 0 {
		grant.Revision = 1
	}
	s.grants[grant.Key] = grant
	return nil
}

func (s *InMemoryOAuthGrantStore) GetGrant(_ context.Context, key OAuthGrantKey) (OAuthGrant, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	grant, ok := s.grants[key]
	if !ok {
		return OAuthGrant{}, ErrOAuthGrantMissing
	}
	grant.Scopes = append([]string(nil), grant.Scopes...)
	return grant, nil
}

func (s *InMemoryOAuthGrantStore) SaveGrantCAS(_ context.Context, grant OAuthGrant, expectedRevision int64) (bool, error) {
	if err := validateOAuthGrant(grant); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.grants[grant.Key]
	if !ok || current.Revision != expectedRevision {
		return false, nil
	}
	grant.Scopes = append([]string(nil), grant.Scopes...)
	grant.Revision = expectedRevision + 1
	if grant.Status == "" {
		grant.Status = "active"
	}
	s.grants[grant.Key] = grant
	return true, nil
}

func (s *InMemoryOAuthGrantStore) DeleteGrant(_ context.Context, key OAuthGrantKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.grants, key)
	return nil
}

type SQLOAuthGrantStore struct {
	db    *sql.DB
	codec OAuthTokenCodec
	now   func() time.Time
}

func NewSQLOAuthGrantStore(db *sql.DB, codec OAuthTokenCodec) *SQLOAuthGrantStore {
	return &SQLOAuthGrantStore{db: db, codec: codec, now: time.Now}
}

func (s *SQLOAuthGrantStore) SaveGrant(ctx context.Context, grant OAuthGrant) error {
	if s == nil || s.db == nil || s.codec == nil {
		return errorsConfiguration("mcp oauth sql store requires database and token codec")
	}
	if err := validateOAuthGrant(grant); err != nil {
		return err
	}
	access, err := s.codec.SealToken(grant.AccessToken)
	if err != nil {
		return err
	}
	refresh := ""
	if grant.RefreshToken != "" {
		refresh, err = s.codec.SealToken(grant.RefreshToken)
		if err != nil {
			return err
		}
	}
	secret := ""
	if grant.ClientSecret != "" {
		secret, err = s.codec.SealToken(grant.ClientSecret)
		if err != nil {
			return err
		}
	}
	now := s.now
	if now == nil {
		now = time.Now
	}
	updatedAt := grant.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = now().UTC()
	}
	status := firstNonEmpty(grant.Status, "active")
	digest := oauthGrantIdentityDigest(grant.Key)
	result, err := s.db.ExecContext(ctx, `UPDATE mcp_oauth_grants
SET scopes_json=?, resource=?, issuer=?, token_endpoint=?, client_id=?, client_secret_ciphertext=?, token_auth_method=?, access_token_ciphertext=?, refresh_token_ciphertext=?, access_expires_at_ms=?, refresh_expires_at_ms=?, status=?, revision=revision+1, updated_at_ms=?
WHERE identity_digest=? AND tenant_id=? AND user_id=? AND server_id=? AND provider=? AND scope_hash=?`,
		string(mustMarshalOAuthScopes(grant.Scopes)), grant.Resource, grant.Issuer, grant.TokenEndpoint, grant.ClientID, secret, firstNonEmpty(grant.TokenAuthMethod, "none"), access, refresh, timeToUnixMilli(grant.AccessExpiresAt), timeToUnixMilli(grant.RefreshExpiresAt), status, updatedAt.UnixMilli(),
		digest, grant.Key.TenantID, grant.Key.UserID, grant.Key.ServerID, grant.Key.Provider, grant.Key.ScopeHash)
	if err != nil {
		return fmt.Errorf("update mcp oauth grant: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected > 0 {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO mcp_oauth_grants (
tenant_id, user_id, server_id, provider, scope_hash, scopes_json, resource, issuer, token_endpoint, client_id, client_secret_ciphertext, token_auth_method, access_token_ciphertext, refresh_token_ciphertext, access_expires_at_ms, refresh_expires_at_ms, status, revision, updated_at_ms, identity_digest
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		grant.Key.TenantID, grant.Key.UserID, grant.Key.ServerID, grant.Key.Provider, grant.Key.ScopeHash, string(mustMarshalOAuthScopes(grant.Scopes)), grant.Resource, grant.Issuer, grant.TokenEndpoint, grant.ClientID, secret, firstNonEmpty(grant.TokenAuthMethod, "none"), access, refresh,
		timeToUnixMilli(grant.AccessExpiresAt), timeToUnixMilli(grant.RefreshExpiresAt), status, updatedAt.UnixMilli(), digest)
	if err != nil {
		return fmt.Errorf("insert mcp oauth grant: %w", err)
	}
	return nil
}

func (s *SQLOAuthGrantStore) SaveGrantCAS(ctx context.Context, grant OAuthGrant, expectedRevision int64) (bool, error) {
	if s == nil || s.db == nil || s.codec == nil {
		return false, errorsConfiguration("mcp oauth sql store requires database and token codec")
	}
	if expectedRevision <= 0 {
		return false, errorsConfiguration("mcp oauth grant revision is required")
	}
	if err := validateOAuthGrant(grant); err != nil {
		return false, err
	}
	access, err := s.codec.SealToken(grant.AccessToken)
	if err != nil {
		return false, err
	}
	refresh := ""
	if grant.RefreshToken != "" {
		refresh, err = s.codec.SealToken(grant.RefreshToken)
		if err != nil {
			return false, err
		}
	}
	secret := ""
	if grant.ClientSecret != "" {
		secret, err = s.codec.SealToken(grant.ClientSecret)
		if err != nil {
			return false, err
		}
	}
	now := s.now
	if now == nil {
		now = time.Now
	}
	updatedAt := grant.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = now().UTC()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE mcp_oauth_grants
SET scopes_json=?, resource=?, issuer=?, token_endpoint=?, client_id=?, client_secret_ciphertext=?, token_auth_method=?, access_token_ciphertext=?, refresh_token_ciphertext=?, access_expires_at_ms=?, refresh_expires_at_ms=?, status=?, revision=revision+1, updated_at_ms=?
WHERE identity_digest=? AND tenant_id=? AND user_id=? AND server_id=? AND provider=? AND scope_hash=? AND revision=?`,
		string(mustMarshalOAuthScopes(grant.Scopes)), grant.Resource, grant.Issuer, grant.TokenEndpoint, grant.ClientID, secret, firstNonEmpty(grant.TokenAuthMethod, "none"), access, refresh, timeToUnixMilli(grant.AccessExpiresAt), timeToUnixMilli(grant.RefreshExpiresAt), firstNonEmpty(grant.Status, "active"), updatedAt.UnixMilli(), oauthGrantIdentityDigest(grant.Key), grant.Key.TenantID, grant.Key.UserID, grant.Key.ServerID, grant.Key.Provider, grant.Key.ScopeHash, expectedRevision)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

func (s *SQLOAuthGrantStore) GetGrant(ctx context.Context, key OAuthGrantKey) (OAuthGrant, error) {
	if s == nil || s.db == nil || s.codec == nil {
		return OAuthGrant{}, errorsConfiguration("mcp oauth sql store requires database and token codec")
	}
	var scopesJSON, resource, issuer, tokenEndpoint, clientID, clientSecretCiphertext, tokenAuthMethod, accessCiphertext, refreshCiphertext, status string
	var accessExpiresAtMS, refreshExpiresAtMS, updatedAtMS int64
	var revision int64
	err := s.db.QueryRowContext(ctx, `SELECT scopes_json, resource, issuer, token_endpoint, client_id, client_secret_ciphertext, token_auth_method, access_token_ciphertext, refresh_token_ciphertext, access_expires_at_ms, refresh_expires_at_ms, status, revision, updated_at_ms
	FROM mcp_oauth_grants WHERE identity_digest=? AND tenant_id=? AND user_id=? AND server_id=? AND provider=? AND scope_hash=?`,
		oauthGrantIdentityDigest(key), key.TenantID, key.UserID, key.ServerID, key.Provider, key.ScopeHash).Scan(
		&scopesJSON, &resource, &issuer, &tokenEndpoint, &clientID, &clientSecretCiphertext, &tokenAuthMethod, &accessCiphertext, &refreshCiphertext, &accessExpiresAtMS, &refreshExpiresAtMS, &status, &revision, &updatedAtMS,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return OAuthGrant{}, ErrOAuthGrantMissing
		}
		return OAuthGrant{}, err
	}
	accessToken, err := s.codec.OpenToken(accessCiphertext)
	if err != nil {
		return OAuthGrant{}, err
	}
	refreshToken := ""
	if refreshCiphertext != "" {
		refreshToken, err = s.codec.OpenToken(refreshCiphertext)
		if err != nil {
			return OAuthGrant{}, err
		}
	}
	clientSecret := ""
	if clientSecretCiphertext != "" {
		clientSecret, err = s.codec.OpenToken(clientSecretCiphertext)
		if err != nil {
			return OAuthGrant{}, err
		}
	}
	scopes, err := unmarshalOAuthScopes([]byte(scopesJSON))
	if err != nil {
		return OAuthGrant{}, err
	}
	return OAuthGrant{
		Key: key, Revision: revision, Scopes: scopes, Resource: resource, Issuer: issuer, TokenEndpoint: tokenEndpoint, ClientID: clientID, ClientSecret: clientSecret, TokenAuthMethod: tokenAuthMethod, AccessToken: accessToken, RefreshToken: refreshToken,
		AccessExpiresAt: unixMilliToTime(accessExpiresAtMS), RefreshExpiresAt: unixMilliToTime(refreshExpiresAtMS),
		Status: status, UpdatedAt: unixMilliToTime(updatedAtMS),
	}, nil
}

func (s *SQLOAuthGrantStore) DeleteGrant(ctx context.Context, key OAuthGrantKey) error {
	if s == nil || s.db == nil {
		return errorsConfiguration("mcp oauth sql store requires database")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE mcp_oauth_grants SET status='revoked', revision=revision+1, updated_at_ms=?
WHERE identity_digest=? AND tenant_id=? AND user_id=? AND server_id=? AND provider=? AND scope_hash=?`,
		time.Now().UTC().UnixMilli(), oauthGrantIdentityDigest(key), key.TenantID, key.UserID, key.ServerID, key.Provider, key.ScopeHash)
	return err
}

func oauthGrantIdentityDigest(key OAuthGrantKey) []byte {
	return mcpIdentityDigest(key.TenantID, key.UserID, key.ServerID, key.Provider, key.ScopeHash)
}

func mustMarshalOAuthScopes(scopes []string) []byte {
	data, _ := json.Marshal(append([]string(nil), scopes...))
	return data
}

func unmarshalOAuthScopes(data []byte) ([]string, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var scopes []string
	if err := json.Unmarshal(data, &scopes); err != nil {
		return nil, err
	}
	return scopes, nil
}

func timeToUnixMilli(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UTC().UnixMilli()
}

func unixMilliToTime(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(value).UTC()
}
