package main

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/bootstrap"
	"github.com/AGenUI/agenui-studio/agenui-agent/pkg/localadmin"
	"github.com/AGenUI/agenui-studio/harness/sdk/testkit"
	_ "github.com/mattn/go-sqlite3"
)

func TestLocalRuleWorkerAssemblesWithPublicModelBoundaryAndSourceSchema(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mux := http.NewServeMux()
	if _, err := localadmin.Register(mux, db); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	assembled := make(chan bool, 1)
	config := bootstrap.ProcessConfig{
		DatabaseDriver:          "sqlite3",
		DesignKnowledgeRoot:     "../../configs/design",
		DesignKnowledgeRevision: "revisions/revision-5",
		RendererCatalogRoot:     "../../configs/renderer-catalogs",
		Bootstrap:               bootstrap.Config{Environment: "local"},
		RuleWorker:              bootstrap.RuleWorkerConfig{Enabled: true, PollInterval: 10 * time.Millisecond, OutputRoot: t.TempDir(), OutputPrefix: "test"},
	}
	stop := startRuleWorker(ctx, config, "../../configs/environments/local/agenui.toml", db, nil, testkit.NewMockEngine(testkit.Scenario{}), mux, func(value bool) { assembled <- value })
	select {
	case got := <-assembled:
		if !got {
			t.Fatal("worker did not assemble")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker assembly timed out")
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := stop(stopCtx); err != nil {
		t.Fatalf("stop worker: %v", err)
	}
}
