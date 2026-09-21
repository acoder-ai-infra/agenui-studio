package observability

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewZapLoggerWritesJSONFileAndHonorsLevel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness.log")
	logger, err := NewZapLogger(LoggerConfig{
		ServiceName: "sdk-host",
		Environment: "testing",
		Level:       "info",
		Output:      "file",
		File: &FileLoggerConfig{
			Filename:   path,
			MaxSizeMB:  10,
			MaxBackups: 3,
			MaxAgeDays: 7,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	logger.Debug(context.Background(), "hidden debug")
	logger.Info(context.Background(), "visible info", String("request_id", "req-1"))
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}

	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	var lines []map[string]any
	for scanner.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", scanner.Text(), err)
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("log line count = %d, want 1: %#v", len(lines), lines)
	}
	if lines[0]["msg"] != "visible info" || lines[0]["level"] != "info" || lines[0]["service"] != "sdk-host" || lines[0]["env"] != "testing" || lines[0]["request_id"] != "req-1" {
		t.Fatalf("unexpected structured log: %#v", lines[0])
	}
}

func TestNewZapLoggerWritesBothOutputsWithExpectedEncoding(t *testing.T) {
	var stdout bytes.Buffer
	path := filepath.Join(t.TempDir(), "harness.log")
	logger, err := NewZapLogger(LoggerConfig{
		Development: true,
		Level:       "warn",
		Output:      "both",
		Stdout:      &stdout,
		File: &FileLoggerConfig{
			Filename:   path,
			MaxSizeMB:  10,
			MaxBackups: 3,
			MaxAgeDays: 7,
			Compress:   true,
			LocalTime:  true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	logger.Info(context.Background(), "hidden info")
	logger.Warn(context.Background(), "visible warning")
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}

	if got := stdout.String(); !strings.Contains(got, "visible warning") || strings.Contains(got, "hidden info") {
		t.Fatalf("stdout = %q", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &line); err != nil {
		t.Fatalf("file output is not JSON: %q: %v", data, err)
	}
	if line["msg"] != "visible warning" || line["level"] != "warn" {
		t.Fatalf("file log = %#v", line)
	}
}

func TestNewRotatingFileSinkMapsAllRotationOptions(t *testing.T) {
	cfg := FileLoggerConfig{
		Filename:   filepath.Join(t.TempDir(), "harness.log"),
		MaxSizeMB:  123,
		MaxBackups: 9,
		MaxAgeDays: 17,
		Compress:   true,
		LocalTime:  true,
	}
	sink := newRotatingFileSink(cfg)
	if sink.Filename != cfg.Filename || sink.MaxSize != cfg.MaxSizeMB || sink.MaxBackups != cfg.MaxBackups || sink.MaxAge != cfg.MaxAgeDays || !sink.Compress || !sink.LocalTime {
		t.Fatalf("rotating sink = %#v", sink)
	}
}

func TestNewZapLoggerRejectsIncompleteOutputConfig(t *testing.T) {
	tests := []LoggerConfig{
		{Level: "trace", Output: "stdout"},
		{Level: "info", Output: "stderr"},
		{Level: "info", Output: "stdout", File: &FileLoggerConfig{Filename: "ignored.log", MaxSizeMB: 1, MaxBackups: 1, MaxAgeDays: 1}},
		{Level: "info", Output: "file"},
		{Level: "info", Output: "both"},
	}
	for _, cfg := range tests {
		if logger, err := NewZapLogger(cfg); err == nil {
			_ = logger.Close()
			t.Fatalf("NewZapLogger(%#v) accepted", cfg)
		}
	}
}

func TestNewZapLoggerFailsWhenFileCannotBeOpened(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "harness.log")
	logger, err := NewZapLogger(LoggerConfig{
		Level: "info", Output: "file",
		File: &FileLoggerConfig{Filename: path, MaxSizeMB: 10, MaxBackups: 3, MaxAgeDays: 7},
	})
	if err == nil {
		_ = logger.Close()
		t.Fatalf("NewZapLogger() accepted inaccessible path %q", path)
	}
}

func TestZapLoggerCloseIsIdempotent(t *testing.T) {
	logger, err := NewZapLogger(LoggerConfig{Level: "info", Output: "stdout", Stdout: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
}
