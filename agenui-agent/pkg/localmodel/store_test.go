package localmodel

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func testStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db, keyFile: filepath.Join(t.TempDir(), "secrets", "model.key")}
	if err := s.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, db
}

func TestStoreEncryptsCredentialAndPreservesBlankUpdate(t *testing.T) {
	s, db := testStore(t)
	defer db.Close()
	ctx := context.Background()
	if err := s.Save(ctx, Config{BaseURL: "https://models.example.test/v1", Model: "demo-model", APIKey: "sk-plain-secret", Protocol: ProtocolAnthropic}); err != nil {
		t.Fatal(err)
	}
	var ciphertext string
	if err := db.QueryRow(`SELECT api_key_ciphertext FROM local_model_config WHERE id=1`).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if ciphertext == "" || strings.Contains(ciphertext, "plain-secret") {
		t.Fatalf("credential was not encrypted: %q", ciphertext)
	}
	if err := s.Save(ctx, Config{BaseURL: "https://models.example.test/v1", Model: "next-model"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Load(ctx)
	if err != nil || !ok {
		t.Fatalf("Load = %#v, %v, %v", got, ok, err)
	}
	if got.Model != "next-model" || got.APIKey != "sk-plain-secret" || got.Protocol != ProtocolAnthropic {
		t.Fatalf("unexpected saved config: %#v", got)
	}
	info, err := os.Stat(s.keyFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("local key permission = %v, err=%v", info.Mode().Perm(), err)
	}
}

func TestEnsureSchemaInfersAnthropicProtocolForLegacyGateway(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE local_model_config (id INTEGER PRIMARY KEY, base_url TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', api_key_ciphertext TEXT NOT NULL DEFAULT ''); INSERT INTO local_model_config(id,base_url,model) VALUES(1,'https://gateway.example/open_api/anthropic','claude-opus-4-7')`); err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db, keyFile: filepath.Join(t.TempDir(), "model.key")}
	if err := s.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	var protocol string
	if err := db.QueryRow(`SELECT protocol FROM local_model_config WHERE id=1`).Scan(&protocol); err != nil {
		t.Fatal(err)
	}
	if protocol != ProtocolAnthropic {
		t.Fatalf("migrated protocol = %q, want %q", protocol, ProtocolAnthropic)
	}
}

func TestPreparePublishesManagedProvidersWithoutGeneratedConfig(t *testing.T) {
	work := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(context.Background(), Config{BaseURL: "https://models.example.test/v1", Model: "user-model", APIKey: "sk-user-secret", Protocol: ProtocolAnthropic}); err != nil {
		t.Fatal(err)
	}
	harnessPath := filepath.Join(work, "source-harness.yaml")
	if err := os.WriteFile(harnessPath, []byte("schema_version: harness.composition.v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, ok, err := Prepare(context.Background(), db, harnessPath)
	if err != nil || !ok {
		t.Fatalf("Prepare = %#v, %v, %v", prepared, ok, err)
	}
	if prepared.HarnessConfigPath != harnessPath {
		t.Fatalf("HarnessConfigPath = %q, want unchanged %q", prepared.HarnessConfigPath, harnessPath)
	}
	if os.Getenv(modelKeyEnv) != "sk-user-secret" {
		t.Fatal("model key environment was not prepared")
	}
	rows, err := db.Query(`SELECT tenant_id,definition_json,revision FROM model_providers ORDER BY tenant_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var count int
	for rows.Next() {
		var tenant, definitionJSON string
		var revision int64
		if err := rows.Scan(&tenant, &definitionJSON, &revision); err != nil {
			t.Fatal(err)
		}
		count++
		if revision < 1 || strings.Contains(definitionJSON, "sk-user-secret") {
			t.Fatalf("invalid managed provider for %s: revision=%d definition=%s", tenant, revision, definitionJSON)
		}
		var definition map[string]any
		if err := json.Unmarshal([]byte(definitionJSON), &definition); err != nil {
			t.Fatal(err)
		}
		if definition["tenant_id"] != tenant || definition["default_model"] != "user-model" || definition["protocol"] != ProtocolAnthropic || definition["api_key_env"] != modelKeyEnv {
			t.Fatalf("managed provider mismatch for %s: %#v", tenant, definition)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("managed provider rows = %d, want 2", count)
	}
	if _, err := os.Stat(filepath.Join(work, "var", "local-runtime")); !os.IsNotExist(err) {
		t.Fatalf("Prepare must not generate runtime configuration: %v", err)
	}
}
