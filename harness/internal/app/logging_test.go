package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestNewConfiguredLoggerUsesHarnessLogDirectory(t *testing.T) {
	logDir := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatal(err)
	}
	cfg := HarnessConfig{
		Environment: EnvironmentTesting,
		Service:     HarnessServiceConfig{Name: "embedded-service"},
		Paths:       HarnessPathConfig{LogDir: logDir},
		Logging: HarnessLoggingConfig{
			Level:  "info",
			Output: "file",
			File: &HarnessLogFileConfig{
				Name: "engine.log", MaxSizeMB: 10, MaxBackups: 3, MaxAgeDays: 7,
			},
		},
	}

	logger, err := NewConfiguredLogger(cfg)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info(context.Background(), "engine ready")
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(logDir, "engine.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"msg":"engine ready"`, `"service":"embedded-service"`, `"env":"testing"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("configured log %q missing %q", data, want)
		}
	}
}

func TestKernelCloseClosesAppBeforeManagedLoggerOnce(t *testing.T) {
	var mu sync.Mutex
	var order []string
	appErr := errors.New("app close")
	loggerErr := errors.New("logger close")
	application := &App{closeResources: func() error {
		mu.Lock()
		order = append(order, "app")
		mu.Unlock()
		return appErr
	}}
	k := newKernelFromApp(application, observability.NoopLogger{}, observability.NewNoopTracer("test"), func() error {
		mu.Lock()
		order = append(order, "logger")
		mu.Unlock()
		return loggerErr
	})

	const callers = 8
	results := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- k.Close(context.Background())
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if !errors.Is(err, appErr) || !errors.Is(err, loggerErr) {
			t.Fatalf("Kernel.Close() error = %v, want both close errors", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "app" || order[1] != "logger" {
		t.Fatalf("close order = %v, want [app logger]", order)
	}
}
