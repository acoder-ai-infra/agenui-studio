package mcp

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrOAuthStateInvalid = errors.New("mcp oauth state is invalid or already consumed")
	ErrOAuthStateExpired = errors.New("mcp oauth state expired")
)

type OAuthPendingAuthorization struct {
	State           string
	TenantID        string
	UserID          string
	ServerID        string
	Provider        string
	ScopeHash       string
	Scopes          []string
	Resource        string
	Issuer          string
	TokenEndpoint   string
	RedirectURI     string
	ClientID        string
	ClientSecret    string
	TokenAuthMethod string
	CodeVerifier    string
	ExpiresAt       time.Time
	CreatedAt       time.Time
}

type OAuthPendingStore interface {
	Save(context.Context, OAuthPendingAuthorization) error
	Consume(context.Context, string) (OAuthPendingAuthorization, error)
}

type SQLOAuthPendingStore struct {
	db    *sql.DB
	codec OAuthTokenCodec
	now   func() time.Time
}

func NewSQLOAuthPendingStore(db *sql.DB, codec OAuthTokenCodec) *SQLOAuthPendingStore {
	return &SQLOAuthPendingStore{db: db, codec: codec, now: time.Now}
}

func (s *SQLOAuthPendingStore) Save(ctx context.Context, pending OAuthPendingAuthorization) error {
	if s == nil || s.db == nil || s.codec == nil {
		return errorsConfiguration("mcp oauth pending store requires database and token codec")
	}
	if strings.TrimSpace(pending.State) == "" || pending.TenantID == "" || pending.UserID == "" || pending.ServerID == "" || pending.Provider == "" || pending.ScopeHash == "" || pending.TokenEndpoint == "" || pending.RedirectURI == "" || pending.ClientID == "" || pending.CodeVerifier == "" || pending.ExpiresAt.IsZero() {
		return errorsConfiguration("mcp oauth pending authorization is incomplete")
	}
	verifier, err := s.codec.SealToken(pending.CodeVerifier)
	if err != nil {
		return err
	}
	secret := ""
	if pending.ClientSecret != "" {
		secret, err = s.codec.SealToken(pending.ClientSecret)
		if err != nil {
			return err
		}
	}
	now := s.now
	if now == nil {
		now = time.Now
	}
	createdAt := pending.CreatedAt
	if createdAt.IsZero() {
		createdAt = now().UTC()
	}
	scopes, err := json.Marshal(append([]string(nil), pending.Scopes...))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO mcp_oauth_pending (
state_digest, tenant_id, user_id, server_id, provider, scope_hash, scopes_json, resource, issuer, token_endpoint, redirect_uri, client_id, client_secret_ciphertext, token_auth_method, code_verifier_ciphertext, expires_at_ms, consumed_at_ms, created_at_ms
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		oauthStateDigest(pending.State), pending.TenantID, pending.UserID, pending.ServerID, pending.Provider, pending.ScopeHash, string(scopes), pending.Resource, pending.Issuer,
		pending.TokenEndpoint, pending.RedirectURI, pending.ClientID, secret, firstNonEmpty(pending.TokenAuthMethod, "none"), verifier, pending.ExpiresAt.UTC().UnixMilli(), createdAt.UTC().UnixMilli())
	if err != nil {
		return fmt.Errorf("save mcp oauth pending authorization: %w", err)
	}
	return nil
}

func (s *SQLOAuthPendingStore) Consume(ctx context.Context, state string) (OAuthPendingAuthorization, error) {
	if s == nil || s.db == nil || s.codec == nil || strings.TrimSpace(state) == "" {
		return OAuthPendingAuthorization{}, ErrOAuthStateInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OAuthPendingAuthorization{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var pending OAuthPendingAuthorization
	var scopesJSON, secretCiphertext, verifierCiphertext string
	var expiresAtMS, consumedAtMS, createdAtMS int64
	err = tx.QueryRowContext(ctx, `SELECT tenant_id, user_id, server_id, provider, scope_hash, scopes_json, resource, issuer, token_endpoint, redirect_uri, client_id, client_secret_ciphertext, token_auth_method, code_verifier_ciphertext, expires_at_ms, consumed_at_ms, created_at_ms
FROM mcp_oauth_pending WHERE state_digest=?`, oauthStateDigest(state)).Scan(
		&pending.TenantID, &pending.UserID, &pending.ServerID, &pending.Provider, &pending.ScopeHash, &scopesJSON, &pending.Resource, &pending.Issuer,
		&pending.TokenEndpoint, &pending.RedirectURI, &pending.ClientID, &secretCiphertext, &pending.TokenAuthMethod, &verifierCiphertext, &expiresAtMS, &consumedAtMS, &createdAtMS,
	)
	if errors.Is(err, sql.ErrNoRows) || consumedAtMS != 0 {
		return OAuthPendingAuthorization{}, ErrOAuthStateInvalid
	}
	if err != nil {
		return OAuthPendingAuthorization{}, err
	}
	now := s.now
	if now == nil {
		now = time.Now
	}
	current := now().UTC()
	if !current.Before(time.UnixMilli(expiresAtMS)) {
		return OAuthPendingAuthorization{}, ErrOAuthStateExpired
	}
	result, err := tx.ExecContext(ctx, `UPDATE mcp_oauth_pending SET consumed_at_ms=? WHERE state_digest=? AND consumed_at_ms=0`, current.UnixMilli(), oauthStateDigest(state))
	if err != nil {
		return OAuthPendingAuthorization{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return OAuthPendingAuthorization{}, ErrOAuthStateInvalid
	}
	if err := json.Unmarshal([]byte(scopesJSON), &pending.Scopes); err != nil {
		return OAuthPendingAuthorization{}, err
	}
	pending.CodeVerifier, err = s.codec.OpenToken(verifierCiphertext)
	if err != nil {
		return OAuthPendingAuthorization{}, err
	}
	if secretCiphertext != "" {
		pending.ClientSecret, err = s.codec.OpenToken(secretCiphertext)
		if err != nil {
			return OAuthPendingAuthorization{}, err
		}
	}
	pending.ExpiresAt = time.UnixMilli(expiresAtMS).UTC()
	pending.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	if err := tx.Commit(); err != nil {
		return OAuthPendingAuthorization{}, err
	}
	return pending, nil
}

func oauthStateDigest(state string) []byte {
	digest := sha256.Sum256([]byte(state))
	return digest[:]
}
