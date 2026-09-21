package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseMigrateAction(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: nil, want: "up"},
		{args: []string{"up"}, want: "up"},
		{args: []string{"status"}, want: "status"},
	} {
		got, err := parseMigrateAction(test.args)
		if err != nil || got != test.want {
			t.Fatalf("parseMigrateAction(%v) = %q, %v", test.args, got, err)
		}
	}
}

func TestParseMigrateActionRejectsUnknownOrExtraArguments(t *testing.T) {
	for _, args := range [][]string{{"down"}, {"up", "extra"}} {
		if _, err := parseMigrateAction(args); err == nil {
			t.Fatalf("parseMigrateAction(%v) unexpectedly succeeded", args)
		}
	}
}

func TestRunMigrateReportsConfigurationErrorsWithoutPanic(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exitCode := runMigrate([]string{"up"}, func(string) string { return "/missing/harness.yaml" }, &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf("exit code = %d", exitCode)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "migration failed") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunMigrateLoadsOnlyStorageComponent(t *testing.T) {
	root := t.TempDir()
	storagePath := filepath.Join(root, "storage.yaml")
	if err := os.WriteFile(storagePath, []byte("backend: sqlite\nconfig:\n  path: data/harness.db\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	harnessPath := filepath.Join(root, "harness.yaml")
	harness := fmt.Sprintf(`schema_version: harness.composition.v1
environment: local
paths:
  runtime_root: %s
components:
  storage: %s
  models: {source: database}
  agents: {source: database}
  prompts: {source: database}
  mcp: {source: database}
  tools: tools.yaml
  skills: {source: database}
runtime:
  default_agent_id: test
`, root, storagePath)
	if err := os.WriteFile(harnessPath, []byte(harness), 0o600); err != nil {
		t.Fatal(err)
	}
	values := func(key string) string {
		switch key {
		case "HARNESS_ENV":
			return "local"
		case "HARNESS_CONFIG":
			return harnessPath
		default:
			return ""
		}
	}

	for _, action := range []string{"up", "status"} {
		var stdout, stderr bytes.Buffer
		if exitCode := runMigrate([]string{action}, values, &stdout, &stderr); exitCode != 0 {
			t.Fatalf("%s exit=%d stdout=%q stderr=%q", action, exitCode, stdout.String(), stderr.String())
		}
		if !strings.Contains(stdout.String(), "backend=sqlite ready=true") || stderr.Len() != 0 {
			t.Fatalf("%s stdout=%q stderr=%q", action, stdout.String(), stderr.String())
		}
	}
	if _, err := os.Stat(filepath.Join(root, "data", "data", "harness.db")); err != nil {
		t.Fatalf("migrated sqlite database: %v", err)
	}
}
