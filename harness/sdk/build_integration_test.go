package harness_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk"
)

func TestBuildIntegrationLocalWithSharedSQLite(t *testing.T) {
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "integration-fake-token")

	repoRoot := findRepoRoot(t)
	tempRoot := t.TempDir()
	configPath := filepath.Join(tempRoot, "harness.yaml")
	if err := os.WriteFile(configPath, []byte(renderIntegrationConfig(t, repoRoot, tempRoot)), 0o600); err != nil {
		t.Fatalf("write harness.yaml: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := harness.RunMigration(ctx, harness.MigrationOptions{
		ConfigPath: configPath, Environment: "local",
	}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	dbPath := filepath.Join(tempRoot, "var", "data", "harness.sqlite")
	sharedDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open caller sqlite: %v", err)
	}
	sharedDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sharedDB.Close() })

	engine, _, err := harness.Build(ctx,
		harness.WithConfigPath(configPath),
		harness.WithEnvironment("local"),
		harness.WithSharedSQLDatabase(sharedDB),
	)
	if err != nil {
		t.Fatalf("harness.Build with shared sqlite: %v", err)
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := engine.Close(shutdownCtx); err != nil {
		t.Fatalf("engine.Close: %v", err)
	}
	if err := sharedDB.PingContext(ctx); err != nil {
		t.Fatalf("Engine closed caller-owned sqlite pool: %v", err)
	}
}

// TestBuildIntegrationLocal exercises the full harness.Build entry point
// against a real harness.yaml configuration. It writes an isolated
// harness.yaml into t.TempDir(), points every component at the checked-in
// local config files via absolute paths, runs MigrationUp against a fresh
// SQLite database, then calls harness.Build and verifies the returned
// BuildReport / Readiness.
//
// This is the plan-required end-to-end proof that kernel.Build (invoked
// transparently by harness.Build) wires an entire Composition Root against
// real config files. It never talks to a remote provider because it stops
// short of Engine.Start.
func TestBuildIntegrationLocal(t *testing.T) {
	// Provider config needs a token; use a fake one because we never fire a
	// real Start (no model call happens during Build).
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "integration-fake-token")

	repoRoot := findRepoRoot(t)
	tempRoot := t.TempDir()

	configPath := filepath.Join(tempRoot, "harness.yaml")
	if err := os.WriteFile(configPath, []byte(renderIntegrationConfig(t, repoRoot, tempRoot)), 0o600); err != nil {
		t.Fatalf("write harness.yaml: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Apply schema migrations against the isolated tempdir sqlite path first.
	migReport, err := harness.RunMigration(ctx, harness.MigrationOptions{
		ConfigPath:  configPath,
		Environment: "local",
	})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if migReport.Backend != "sqlite" {
		t.Fatalf("unexpected backend %q; want sqlite", migReport.Backend)
	}
	if !migReport.Ready {
		t.Fatalf("migration should report Ready; got %+v", migReport)
	}

	engine, report, err := harness.Build(ctx,
		harness.WithConfigPath(configPath),
		harness.WithEnvironment("local"),
	)
	if err != nil {
		t.Fatalf("harness.Build: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := engine.Close(shutdownCtx); err != nil {
			t.Errorf("engine.Close: %v", err)
		}
	}()

	if report.SDKVersion != harness.Version {
		t.Fatalf("BuildReport.SDKVersion = %q; want %q", report.SDKVersion, harness.Version)
	}
	if report.Environment != "local" {
		t.Fatalf("BuildReport.Environment = %q; want local", report.Environment)
	}
	foundSchema := false
	for _, s := range report.SchemaVersions {
		if s == "harness.agent_event.v1" {
			foundSchema = true
			break
		}
	}
	if !foundSchema {
		t.Fatalf("SchemaVersions missing canonical event schema: %v", report.SchemaVersions)
	}
	if len(report.Agents) == 0 {
		t.Fatalf("BuildReport.Agents empty; kernel should surface the default agent")
	}
	if len(report.Providers) == 0 {
		t.Fatalf("BuildReport.Providers empty; provider registration failed")
	}

	readiness, err := engine.Readiness(ctx)
	if err != nil {
		t.Fatalf("Readiness: %v", err)
	}
	if !readiness.Ready {
		t.Fatalf("Readiness reports not ready: %+v", readiness.Reasons)
	}
	if readiness.CheckedAt.IsZero() {
		t.Fatal("Readiness.CheckedAt must be populated")
	}
}

// renderIntegrationConfig writes a harness.yaml body that points every
// component path at the checked-in local config, overrides runtime paths to
// tempdir, and swaps the storage backend to a tempdir sqlite file.
func renderIntegrationConfig(t *testing.T, repoRoot, tempRoot string) string {
	t.Helper()
	// SDK integration tests use a deliberately small, generic fixture. It proves
	// Harness composition without importing the Studio agent's business catalog.
	fixture := filepath.Join(repoRoot, "testdata", "local")
	catalogs := filepath.Join(fixture, "catalogs")
	agents := filepath.Join(fixture, "agents")
	prompts := filepath.Join(fixture, "prompts.yaml")
	varRoot := filepath.Join(tempRoot, "var")

	// Emit a per-test storage.yaml so we can override the SQLite path.
	storagePath := filepath.Join(tempRoot, "storage.yaml")
	storageBody := "backend: sqlite\nconfig:\n  path: " + filepath.Join(varRoot, "data", "harness.sqlite") + "\n"
	if err := os.WriteFile(storagePath, []byte(storageBody), 0o600); err != nil {
		t.Fatalf("write storage.yaml: %v", err)
	}
	// Tests use a scenario model and never require a user API key.
	modelsPath := filepath.Join(tempRoot, "models.yaml")
	modelsBody := "default:\n  providers:\n    - name: mock\n      protocol: scenario_mock\n      models: [mock-model, qwen3.7-max]\n      capability: {chat: true, streaming: true, tool_calling: true, structured_output: true, json_schema: true, limits: {max_context_tokens: 128000, max_output_tokens: 32768}}\n  default_provider: mock\n  default_model: mock-model\n"
	if err := os.WriteFile(modelsPath, []byte(modelsBody), 0o600); err != nil {
		t.Fatalf("write models.yaml: %v", err)
	}

	return "schema_version: harness.composition.v1\n" +
		"environment: local\n" +
		"service: {name: harness, address: \":19080\", read_header_timeout_ms: 5000, shutdown_timeout_seconds: 10}\n" +
		"paths: {runtime_root: " + varRoot + ", data_dir: data, log_dir: log, run_dir: run, temp_dir: tmp}\n" +
		"logging: {development: true, level: info, output: stdout, recent_buffer_items: 512}\n" +
		"components:\n" +
		"  models: {source: file, path: " + modelsPath + "}\n" +
		"  storage: " + storagePath + "\n" +
		"  artifact: " + filepath.Join(fixture, "artifact.yaml") + "\n" +
		"  auth: " + filepath.Join(fixture, "auth.yaml") + "\n" +
		"  redis: " + filepath.Join(fixture, "redis.yaml") + "\n" +
		"  skills: {source: file, path: " + filepath.Join(catalogs, "skills.yaml") + "}\n" +
		"  tools: " + filepath.Join(catalogs, "tools.yaml") + "\n" +
		"  mcp: {source: file, path: " + filepath.Join(catalogs, "mcp.yaml") + "}\n" +
		"  agents: {source: file, path: " + agents + "}\n" +
		"  prompts: {source: file, path: " + prompts + "}\n" +
		"runtime: {default_agent_id: demo, max_concurrent: 8, context_messages: 32}\n" +
		"features: {a2a: false}\n"
}

// findRepoRoot walks up from the test binary's working directory until it
// finds go.mod. Tests run with pwd = package dir (harness/), so the walk is
// one hop.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir || strings.TrimSpace(parent) == "" {
			t.Fatalf("could not locate repo root (go.mod) from %s", dir)
		}
		dir = parent
	}
}
