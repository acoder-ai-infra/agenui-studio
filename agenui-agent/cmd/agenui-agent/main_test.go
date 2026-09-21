package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/bootstrap"
)

func TestOpenApplicationDBUsesHarnessSQLiteJournalContract(t *testing.T) {
	const dsnEnv = "AGENUI_TEST_APPLICATION_DATABASE_DSN"
	t.Setenv(dsnEnv, "file:"+filepath.Join(t.TempDir(), "studio.db"))

	db, err := openApplicationDB(context.Background(), bootstrap.ProcessConfig{
		DatabaseDriver: "sqlite",
		DatabaseDSNEnv: dsnEnv,
	})
	if err != nil {
		t.Fatalf("openApplicationDB() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("query journal mode: %v", err)
	}
	if journalMode != "delete" {
		t.Fatalf("journal mode = %q, want delete", journalMode)
	}
}

func TestServiceInstanceIDsAreNonEmptyAndUnique(t *testing.T) {
	first, err := newServiceInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := newServiceInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || second == "" || first == second {
		t.Fatalf("unexpected service instance ids: first=%q second=%q", first, second)
	}
}
