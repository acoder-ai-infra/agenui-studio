package harness_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestBuildUsesHarnessFileLoggingWhenLoggerIsNotInjected(t *testing.T) {
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "logging-file-fake-token")
	repoRoot := findRepoRoot(t)
	tempRoot := t.TempDir()
	config := withIntegrationFileLogging(renderIntegrationConfig(t, repoRoot, tempRoot), "engine.log")
	configPath := filepath.Join(tempRoot, "harness.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := harness.RunMigration(ctx, harness.MigrationOptions{ConfigPath: configPath, Environment: "local"}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	engine, _, err := harness.Build(ctx, harness.WithConfigPath(configPath), harness.WithEnvironment("local"))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := engine.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tempRoot, "var", "log", "engine.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"service":"harness"`) || !strings.Contains(string(data), `"env":"local"`) {
		t.Fatalf("managed file log is missing standard fields: %q", data)
	}
}

func TestBuildWithLoggerZapOverridesConfiguredFileAndKeepsHostOwnership(t *testing.T) {
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "logging-injected-fake-token")
	repoRoot := findRepoRoot(t)
	tempRoot := t.TempDir()
	config := withIntegrationFileLogging(renderIntegrationConfig(t, repoRoot, tempRoot), "must-not-exist.log")
	configPath := filepath.Join(tempRoot, "harness.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	observedCore, logs := observer.New(zapcore.DebugLevel)
	var syncs atomic.Int32
	hostLogger := zap.New(countingSyncCore{Core: observedCore, syncs: &syncs})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := harness.RunMigration(ctx, harness.MigrationOptions{ConfigPath: configPath, Environment: "local"}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	engine, _, err := harness.Build(ctx,
		harness.WithConfigPath(configPath),
		harness.WithEnvironment("local"),
		harness.WithLogger(hostLogger),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := engine.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(filepath.Join(tempRoot, "var", "log", "must-not-exist.log")); !os.IsNotExist(err) {
		t.Fatalf("configured file output was used despite WithLogger: %v", err)
	}
	if syncs.Load() != 0 {
		t.Fatalf("Engine synchronized caller-owned logger %d times", syncs.Load())
	}
	entries := logs.All()
	if len(entries) == 0 {
		t.Fatal("caller-owned logger received no Harness logs")
	}
	foundStandardFields := false
	for _, entry := range entries {
		fields := entry.ContextMap()
		if fields["service"] == "harness" && fields["env"] == "local" {
			foundStandardFields = true
			break
		}
	}
	if !foundStandardFields {
		t.Fatalf("caller-owned logger entries are missing service/env: %#v", entries)
	}
	hostLogger.Info("host logger remains usable")
	if logs.FilterMessage("host logger remains usable").Len() != 1 {
		t.Fatal("caller-owned logger is not usable after Engine.Close")
	}
}

func TestBuildWithCustomLoggerUsesPublicFacadeAndStandardFields(t *testing.T) {
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "logging-custom-fake-token")
	repoRoot := findRepoRoot(t)
	tempRoot := t.TempDir()
	config := withIntegrationFileLogging(renderIntegrationConfig(t, repoRoot, tempRoot), "custom-must-not-exist.log")
	configPath := filepath.Join(tempRoot, "harness.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	customLogger := &collectingHarnessLogger{}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := harness.RunMigration(ctx, harness.MigrationOptions{ConfigPath: configPath, Environment: "local"}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	engine, _, err := harness.Build(ctx,
		harness.WithConfigPath(configPath),
		harness.WithEnvironment("local"),
		harness.WithLogger(customLogger),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := engine.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(filepath.Join(tempRoot, "var", "log", "custom-must-not-exist.log")); !os.IsNotExist(err) {
		t.Fatalf("configured file output was used despite custom WithLogger: %v", err)
	}
	records := customLogger.snapshot()
	if len(records) == 0 {
		t.Fatal("custom logger received no Harness logs")
	}
	for _, record := range records {
		if record.Fields["service"] == "harness" && record.Fields["env"] == "local" {
			return
		}
	}
	t.Fatalf("custom logger records are missing service/env: %#v", records)
}

func withIntegrationFileLogging(config, name string) string {
	return strings.Replace(config,
		"logging: {development: true, level: info, output: stdout, recent_buffer_items: 512}\n",
		"logging:\n  development: false\n  level: debug\n  output: file\n  recent_buffer_items: 512\n  file:\n    name: "+name+"\n    max_size_mb: 10\n    max_backups: 3\n    max_age_days: 7\n",
		1,
	)
}

type countingSyncCore struct {
	zapcore.Core
	syncs *atomic.Int32
}

func (c countingSyncCore) With(fields []zapcore.Field) zapcore.Core {
	return countingSyncCore{Core: c.Core.With(fields), syncs: c.syncs}
}

func (c countingSyncCore) Sync() error {
	c.syncs.Add(1)
	return c.Core.Sync()
}

type collectingHarnessLogger struct {
	mu      sync.Mutex
	records []harness.LogRecord
}

func (l *collectingHarnessLogger) Log(_ context.Context, record harness.LogRecord) {
	l.mu.Lock()
	l.records = append(l.records, record)
	l.mu.Unlock()
}

func (l *collectingHarnessLogger) snapshot() []harness.LogRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]harness.LogRecord(nil), l.records...)
}
