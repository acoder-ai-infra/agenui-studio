package mcp

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

var (
	ErrManagedConflict = errors.New("mcp managed server revision conflict")
	safeServerID       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	safeEnvName        = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type ManagedServer struct {
	Definition ServerDefinition `json:"definition"`
	Revision   int64            `json:"revision"`
	CreatedAt  time.Time        `json:"created_at"`
	UpdatedAt  time.Time        `json:"updated_at"`
	UpdatedBy  string           `json:"updated_by"`
}

type SQLManagedRegistry struct {
	db  *sql.DB
	now func() time.Time
}

func NewSQLManagedRegistry(db *sql.DB) *SQLManagedRegistry {
	return &SQLManagedRegistry{db: db, now: time.Now}
}

func NextGeneratedServerID(existing []ManagedServer) string {
	maxID := int64(0)
	for _, server := range existing {
		id := strings.TrimSpace(server.Definition.ID)
		if !strings.HasPrefix(id, "mcp_") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimPrefix(id, "mcp_"), 10, 64)
		if err == nil && n > maxID {
			maxID = n
		}
	}
	return fmt.Sprintf("mcp_%d", maxID+1)
}

func NextManagedVersion(current string) string {
	value := strings.TrimSpace(current)
	if value == "" {
		return "v1"
	}
	lastDigit := len(value)
	for lastDigit > 0 && value[lastDigit-1] >= '0' && value[lastDigit-1] <= '9' {
		lastDigit--
	}
	if lastDigit == len(value) {
		return value + "2"
	}
	prefix := value[:lastDigit]
	n, err := strconv.ParseInt(value[lastDigit:], 10, 64)
	if err != nil || n < 0 {
		return value + "2"
	}
	return fmt.Sprintf("%s%d", prefix, n+1)
}

func (r *SQLManagedRegistry) Save(ctx context.Context, tenantID, actor string, definition ServerDefinition, expectedRevision int64) (ManagedServer, error) {
	if r == nil || r.db == nil || tenantID == "" || actor == "" || expectedRevision < 0 {
		return ManagedServer{}, errorsConfiguration("database, tenant, actor and non-negative revision are required")
	}
	definition.TenantID = tenantID
	definition.Scope = ScopeTenant
	// Tenant-managed MCP servers are shared capabilities. Agent ownership lives
	// in each Agent's tool_policy.mcp_servers binding, not in the server record.
	definition.AllowedAgents = nil
	definition.BlockedAgents = nil
	if err := validateManagedDefinition(definition); err != nil {
		return ManagedServer{}, err
	}
	encoded, err := json.Marshal(definition)
	if err != nil {
		return ManagedServer{}, err
	}
	now := r.now().UTC()
	if expectedRevision == 0 {
		_, err = r.db.ExecContext(ctx, `INSERT INTO mcp_servers (
tenant_id, server_id, definition_json, revision, created_at_ms, updated_at_ms, updated_by, identity_digest
) VALUES (?, ?, ?, 1, ?, ?, ?, ?)`, tenantID, definition.ID, encoded, now.UnixMilli(), now.UnixMilli(), actor, mcpIdentityDigest(tenantID, definition.ID))
	} else {
		result, updateErr := r.db.ExecContext(ctx, `UPDATE mcp_servers SET definition_json=?, revision=revision+1, updated_at_ms=?, updated_by=?
WHERE identity_digest=? AND tenant_id=? AND server_id=? AND revision=?`, encoded, now.UnixMilli(), actor,
			mcpIdentityDigest(tenantID, definition.ID), tenantID, definition.ID, expectedRevision)
		if updateErr != nil {
			return ManagedServer{}, updateErr
		}
		affected, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return ManagedServer{}, rowsErr
		}
		if affected != 1 {
			return ManagedServer{}, ErrManagedConflict
		}
	}
	if err != nil {
		if _, getErr := r.GetManaged(ctx, tenantID, definition.ID); getErr == nil {
			return ManagedServer{}, ErrManagedConflict
		}
		return ManagedServer{}, fmt.Errorf("save managed mcp server: %w", err)
	}
	return r.GetManaged(ctx, tenantID, definition.ID)
}

func (r *SQLManagedRegistry) GetManaged(ctx context.Context, tenantID, serverID string) (ManagedServer, error) {
	row := r.db.QueryRowContext(ctx, `SELECT definition_json, revision, created_at_ms, updated_at_ms, updated_by
FROM mcp_servers WHERE identity_digest=? AND tenant_id=? AND server_id=?`, mcpIdentityDigest(tenantID, serverID), tenantID, serverID)
	return scanManagedServer(row)
}

func (r *SQLManagedRegistry) List(ctx context.Context, tenantID string) ([]ManagedServer, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT definition_json, revision, created_at_ms, updated_at_ms, updated_by
FROM mcp_servers WHERE tenant_id=? ORDER BY server_id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ManagedServer
	for rows.Next() {
		server, err := scanManagedServer(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, server)
	}
	return result, rows.Err()
}

func (r *SQLManagedRegistry) Get(ctx context.Context, serverID string) (ServerDefinition, error) {
	return ServerDefinition{}, ErrServerNotFound
}

func (r *SQLManagedRegistry) GetForPrincipal(ctx context.Context, principal Principal, serverID string) (ServerDefinition, error) {
	if principal.TenantID == "" {
		return ServerDefinition{}, ErrServerNotFound
	}
	server, err := r.GetManaged(ctx, principal.TenantID, serverID)
	if err != nil {
		return ServerDefinition{}, err
	}
	return server.Definition, nil
}

type managedScanner interface{ Scan(...any) error }

func scanManagedServer(row managedScanner) (ManagedServer, error) {
	var server ManagedServer
	var encoded []byte
	var createdAtMS, updatedAtMS int64
	if err := row.Scan(&encoded, &server.Revision, &createdAtMS, &updatedAtMS, &server.UpdatedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ManagedServer{}, ErrServerNotFound
		}
		return ManagedServer{}, err
	}
	if err := json.Unmarshal(encoded, &server.Definition); err != nil {
		return ManagedServer{}, err
	}
	// Ignore deprecated per-Agent ownership fields on managed tenant records. This
	// keeps old rows compatible while making the Agent configuration authoritative.
	server.Definition.AllowedAgents = nil
	server.Definition.BlockedAgents = nil
	server.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	server.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
	return server, nil
}

func validateManagedDefinition(definition ServerDefinition) error {
	if !safeServerID.MatchString(definition.ID) || definition.Version == "" || definition.Scope != ScopeTenant || definition.TenantID == "" {
		return errorsConfiguration("managed server requires safe id, version and tenant scope")
	}
	if definition.Transport != "streamable-http" {
		return errorsConfiguration("managed server transport must be streamable-http")
	}
	endpoint, err := url.ParseRequestURI(definition.Endpoint)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil {
		return errorsConfiguration("managed server endpoint must be an http(s) URL without userinfo")
	}
	for header, envName := range definition.HeaderEnv {
		if strings.TrimSpace(header) == "" || !safeEnvName.MatchString(envName) {
			return errorsConfiguration("header_env must map header names to environment variable names")
		}
	}
	if err := validateAuthConfig(definition.Auth, definition.HeaderEnv); err != nil {
		return err
	}
	return nil
}

func validateAuthConfig(auth AuthConfig, headerEnv map[string]string) error {
	switch auth.Type {
	case AuthTypeNone:
	case AuthTypeOAuth2:
		if strings.TrimSpace(auth.Provider) == "" {
			return errorsConfiguration("oauth2 mcp auth requires provider")
		}
		switch OAuthGrantMode(auth) {
		case OAuthGrantModeUser, OAuthGrantModeMaintainer:
		default:
			return errorsConfiguration("oauth2 mcp auth grant_mode must be user or maintainer")
		}
		for header := range headerEnv {
			if strings.EqualFold(header, "Authorization") {
				return errorsConfiguration("oauth2 mcp auth must not also declare Authorization header_env")
			}
		}
		for _, scope := range auth.Scopes {
			if strings.TrimSpace(scope) == "" || strings.ContainsAny(scope, "\r\n") {
				return errorsConfiguration("oauth2 mcp auth scopes must be non-empty single-line values")
			}
		}
	default:
		return errorsConfiguration("unsupported mcp auth type")
	}
	return nil
}

type ManagedClientProvider struct {
	Credentials CredentialProvider
}

func (p ManagedClientProvider) Client(ctx context.Context, definition ServerDefinition, principal Principal) (Client, error) {
	if err := validateAuthConfig(definition.Auth, definition.HeaderEnv); err != nil {
		return nil, err
	}
	headers := make(map[string]string, len(definition.HeaderEnv))
	if definition.Auth.Type == AuthTypeOAuth2 {
		if p.Credentials == nil {
			return nil, oauthRequired(definition)
		}
		authorization, err := p.Credentials.AuthorizationHeader(ctx, principal, definition)
		if err != nil {
			return nil, err
		}
		headers["Authorization"] = authorization
	}
	keys := make([]string, 0, len(definition.HeaderEnv))
	for key := range definition.HeaderEnv {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, header := range keys {
		value, ok := os.LookupEnv(definition.HeaderEnv[header])
		if !ok || value == "" {
			return nil, fmt.Errorf("mcp credential environment %s is not configured", definition.HeaderEnv[header])
		}
		headers[header] = value
	}
	return NewStreamableHTTPClient(definition.Endpoint,
		WithStreamableHTTPHeaders(headers),
		WithStreamableHTTPProtocolVersion(definition.ProtocolVersion),
	)
}

func mcpIdentityDigest(values ...string) []byte {
	h := sha256.New()
	for _, value := range values {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(value))
	}
	return h.Sum(nil)
}

func NewManagedService(registry *SQLManagedRegistry) (*Service, error) {
	return NewService(registry, ManagedClientProvider{}, NewInMemorySnapshotStore(), observability.NewULIDGenerator("mcp").NewRequestID)
}

var _ Registry = (*SQLManagedRegistry)(nil)
var _ PrincipalRegistry = (*SQLManagedRegistry)(nil)
