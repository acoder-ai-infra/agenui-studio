package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestManagedRegistryIsolatesTenantsAndUsesCAS(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mcp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := ApplySQLiteSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	registry := NewSQLManagedRegistry(db)
	definition := ServerDefinition{
		ID: "knowledge", DisplayName: "Knowledge service", Version: "v1", Transport: "streamable-http", Endpoint: "https://mcp.example.test/rpc",
		AllowedAgents: []string{"agent-a"}, BlockedAgents: []string{"agent-b"},
	}
	a, err := registry.Save(context.Background(), "tenant-a", "alice", definition, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Definition.AllowedAgents) != 0 || len(a.Definition.BlockedAgents) != 0 {
		t.Fatalf("managed tenant MCP must not retain Agent ownership: %#v", a.Definition)
	}
	if a.Definition.DisplayName != definition.DisplayName {
		t.Fatalf("display name = %q, want %q", a.Definition.DisplayName, definition.DisplayName)
	}
	definition.Endpoint = "https://tenant-b.example.test/rpc"
	if _, err := registry.Save(context.Background(), "tenant-b", "bob", definition, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Save(context.Background(), "tenant-a", "alice", definition, 0); !errors.Is(err, ErrManagedConflict) {
		t.Fatalf("duplicate create error = %v", err)
	}
	definition.Endpoint = "https://updated.example.test/rpc"
	updated, err := registry.Save(context.Background(), "tenant-a", "alice", definition, a.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 2 || updated.Definition.Endpoint != definition.Endpoint {
		t.Fatalf("updated server = %#v", updated)
	}
	resolved, err := registry.GetForPrincipal(context.Background(), Principal{TenantID: "tenant-b", AgentID: "agent"}, "knowledge")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Endpoint != "https://tenant-b.example.test/rpc" {
		t.Fatalf("tenant definition leaked: %#v", resolved)
	}
}

func TestManagedRegistryIgnoresDeprecatedAgentOwnership(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mcp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := ApplySQLiteSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	definition := ServerDefinition{
		ID: "knowledge", Version: "v1", Scope: ScopeTenant, TenantID: "tenant-a",
		Transport: "streamable-http", Endpoint: "https://mcp.example.test/rpc",
		AllowedAgents: []string{"agent-a"}, BlockedAgents: []string{"agent-b"},
	}
	encoded, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().UnixMilli()
	if _, err := db.Exec(`INSERT INTO mcp_servers (
tenant_id, server_id, definition_json, revision, created_at_ms, updated_at_ms, updated_by, identity_digest
) VALUES (?, ?, ?, 1, ?, ?, ?, ?)`, "tenant-a", definition.ID, encoded, now, now, "fixture", mcpIdentityDigest("tenant-a", definition.ID)); err != nil {
		t.Fatal(err)
	}

	server, err := NewSQLManagedRegistry(db).GetManaged(context.Background(), "tenant-a", definition.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(server.Definition.AllowedAgents) != 0 || len(server.Definition.BlockedAgents) != 0 {
		t.Fatalf("deprecated managed Agent ownership must be ignored: %#v", server.Definition)
	}
}

func TestManagedRegistryRejectsLiteralCredentials(t *testing.T) {
	definition := ServerDefinition{ID: "knowledge", Version: "v1", Scope: ScopeTenant, TenantID: "tenant-a", Transport: "streamable-http", Endpoint: "https://user:secret@example.test/rpc"}
	if err := validateManagedDefinition(definition); err == nil {
		t.Fatal("endpoint userinfo must be rejected")
	}
	definition.Endpoint = "https://example.test/rpc"
	definition.HeaderEnv = map[string]string{"Authorization": "Bearer secret"}
	if err := validateManagedDefinition(definition); err == nil {
		t.Fatal("literal header credentials must be rejected")
	}
}

func TestManagedRegistryPersistsOAuthDefinitionAndRejectsMixedAuthorization(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mcp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := ApplySQLiteSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	registry := NewSQLManagedRegistry(db)
	definition := ServerDefinition{
		ID: "source_control", Version: "v1", Scope: ScopeTenant, TenantID: "tenant-a",
		Transport: "streamable-http", Endpoint: "https://mcp.example.test/source-control", ProtocolVersion: "2025-06-18",
		Auth: AuthConfig{Type: AuthTypeOAuth2, Provider: "oauth_provider", GrantMode: OAuthGrantModeMaintainer, Scopes: []string{"source:read"}, Resource: "https://mcp.example.test/source-control"},
	}
	saved, err := registry.Save(context.Background(), "tenant-a", "alice", definition, 0)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Definition.Auth.Type != AuthTypeOAuth2 || saved.Definition.Auth.Provider != "oauth_provider" || saved.Definition.Auth.GrantMode != OAuthGrantModeMaintainer || saved.Definition.ProtocolVersion != "2025-06-18" {
		t.Fatalf("saved oauth definition = %#v", saved.Definition)
	}
	definition.HeaderEnv = map[string]string{"authorization": "MCP_TOKEN"}
	if _, err := registry.Save(context.Background(), "tenant-a", "alice", definition, saved.Revision); err == nil {
		t.Fatal("oauth definition with Authorization header_env was accepted")
	}
	definition.HeaderEnv = nil
	definition.Auth.GrantMode = "team"
	if _, err := registry.Save(context.Background(), "tenant-a", "alice", definition, saved.Revision); err == nil {
		t.Fatal("oauth definition with invalid grant_mode was accepted")
	}
}

func TestManagedRegistryGeneratesServerIDsAndVersions(t *testing.T) {
	nextID := NextGeneratedServerID([]ManagedServer{
		{Definition: ServerDefinition{ID: "mcp_1"}},
		{Definition: ServerDefinition{ID: "custom"}},
		{Definition: ServerDefinition{ID: "mcp_9"}},
	})
	if nextID != "mcp_10" {
		t.Fatalf("next id = %q, want mcp_10", nextID)
	}
	for _, tc := range []struct {
		current string
		want    string
	}{
		{"", "v1"},
		{"v1", "v2"},
		{"release-9", "release-10"},
		{"draft", "draft2"},
	} {
		if got := NextManagedVersion(tc.current); got != tc.want {
			t.Fatalf("next version for %q = %q, want %q", tc.current, got, tc.want)
		}
	}
}

func TestManagedRegistryPersistsDebugHistoryByTenantAndServer(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mcp-debug.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := ApplySQLiteSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := ApplySQLiteOAuthSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := ApplySQLiteDebugHistorySchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := ApplySQLiteOAuthAuthorizationSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := ApplySQLiteOAuthRefreshSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	registry := NewSQLManagedRegistry(db)
	record, err := registry.AppendDebugRecord(context.Background(), DebugRecord{
		ID: "rec-1", TenantID: "tenant-a", ServerID: "knowledge", OperatorID: "alice",
		Operation: DebugOperationCallTool, ToolName: "read", SnapshotID: "snapshot-1",
		RequestJSON: json.RawMessage(`{"arguments":{"token":"[REDACTED]","query":"hello"}}`), ResponseJSON: json.RawMessage(`{"content":"ok"}`),
		LatencyMS: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.CreatedAt.IsZero() {
		t.Fatal("created_at was not assigned")
	}
	if _, err := registry.AppendDebugRecord(context.Background(), DebugRecord{
		ID: "rec-2", TenantID: "tenant-b", ServerID: "knowledge", OperatorID: "bob",
		Operation: DebugOperationListTools, RequestJSON: json.RawMessage(`{}`), ResponseJSON: json.RawMessage(`{"tools":[]}`),
	}); err != nil {
		t.Fatal(err)
	}
	records, err := registry.ListDebugRecords(context.Background(), "tenant-a", "knowledge", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != "rec-1" || string(records[0].RequestJSON) == "" || records[0].LatencyMS != 12 {
		t.Fatalf("tenant-scoped records = %#v", records)
	}
}
