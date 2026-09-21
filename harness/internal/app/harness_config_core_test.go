package app

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateMySQLBackendConfig(t *testing.T) {
	valid := MySQLConfig{Name: "harness", DBName: "harness", Host: "127.0.0.1", Port: 3306, User: "harness", PasswordEnv: "MYSQL_PASSWORD"}
	if err := validateMySQLBackendConfig("mysql", valid, true); err != nil {
		t.Fatal(err)
	}
	invalid := valid
	invalid.PasswordEnv = ""
	invalid.Password = "literal-secret"
	if err := validateMySQLBackendConfig("mysql", invalid, true); err == nil || !strings.Contains(err.Error(), "password_env") {
		t.Fatalf("error = %v, want password_env guidance", err)
	}
}

func TestNormalizeStorageConfigDefaultsSQLitePath(t *testing.T) {
	raw, err := json.Marshal(SQLiteConfig{})
	if err != nil {
		t.Fatal(err)
	}
	storage := StorageConfig{Backend: "sqlite", Config: raw}
	cfg := HarnessConfig{Paths: HarnessPathConfig{DataDir: t.TempDir()}}
	if err := normalizeStorageConfig(cfg, &storage); err != nil {
		t.Fatal(err)
	}
	var got SQLiteConfig
	if err := json.Unmarshal(storage.Config, &got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got.Path, "sqlite/harness.db") {
		t.Fatalf("sqlite path = %q", got.Path)
	}
}
