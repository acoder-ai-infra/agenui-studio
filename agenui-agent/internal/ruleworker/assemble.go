package ruleworker

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	harness "github.com/AGenUI/agenui-studio/harness/sdk"
)

// assemble.go centralises the worker wiring (store + model client + local artifact sink +
// local pointer baseline/pointer + redis lock) so the in-service runner and the
// test-only standalone cmd build an identical worker.

// AssembleConfig is everything needed to build a Worker.
type AssembleConfig struct {
	DB        *sql.DB
	ConfigDir string // directory holding artifact.yaml

	// Optional explicit path; when empty it defaults to ConfigDir/artifact.yaml.
	ArtifactConfigPath string

	// Rule parsing is an ordinary Harness Agent Run and therefore shares the
	// configured model gateway and credentials.
	Engine        harness.Engine
	ParserAgentID string

	// Output + scheduling.
	OutputRoot          string
	OutputPrefix        string
	PollInterval        time.Duration
	WorkerID            string
	RendererCatalogRoot string

	// BaselineDir, when set, overrides local pointer: read the baseline from this local
	// directory. LocalRevisionRoot/PointerPath preserve source pointer semantics
	// with a durable local file and activate the service's existing provider.
	BaselineDir            string
	LocalRevisionRoot      string
	LocalPointerPath       string
	LocalRevisionActivator func(context.Context, string) error
}

// Assemble builds the worker and verifies the design tables are reachable.
func Assemble(ctx context.Context, cfg AssembleConfig) (*Worker, string, error) {
	store, err := NewStore(cfg.DB)
	if err != nil {
		return nil, "", err
	}
	if err := store.Check(ctx); err != nil {
		return nil, "", fmt.Errorf("design tables not reachable (seed db/design/agenui_design_tables.sqlite.sql for local sqlite): %w", err)
	}

	revisionChatModel, err := NewHarnessCompleter(cfg.Engine, cfg.ParserAgentID)
	if err != nil {
		return nil, "", err
	}
	artifactPath := cfg.ArtifactConfigPath
	if artifactPath == "" {
		artifactPath = filepath.Join(cfg.ConfigDir, "artifact.yaml")
	}
	artifactCfg, err := LoadArtifactConfig(artifactPath)
	if err != nil {
		return nil, "", err
	}
	sink, err := NewSink(artifactCfg)
	if err != nil {
		return nil, "", err
	}

	baseline, pointer, baseDesc, err := buildBaselineWiring(cfg)
	if err != nil {
		return nil, "", err
	}

	locker, lockDesc := Locker(&InProcessLocker{}), "in-process"

	worker := NewWorker(store, revisionChatModel, sink, baseline, pointer, locker, Config{
		OutputRoot:          cfg.OutputRoot,
		OutputPrefix:        cfg.OutputPrefix,
		PollInterval:        cfg.PollInterval,
		WorkerID:            cfg.WorkerID,
		RendererCatalogRoot: cfg.RendererCatalogRoot,
	})
	desc := fmt.Sprintf("baseline=%s lock=%s sink=%s", baseDesc, lockDesc, sink.Root())
	return worker, desc, nil
}

// buildBaselineWiring is intentionally local-only. The pointer is a small
// durable file, so generated revisions can be inspected, backed up and moved
// with ordinary filesystem tooling.
func buildBaselineWiring(cfg AssembleConfig) (BaselineResolver, PointerPublisher, string, error) {
	if strings.TrimSpace(cfg.BaselineDir) == "" {
		return nil, nil, "", fmt.Errorf("ruleworker: local baseline directory is required")
	}
	if strings.TrimSpace(cfg.LocalRevisionRoot) != "" && strings.TrimSpace(cfg.LocalPointerPath) != "" {
		return LocalRevisionBaselineResolver{FallbackDir: cfg.BaselineDir, RevisionRoot: cfg.LocalRevisionRoot, PointerPath: cfg.LocalPointerPath},
			LocalRevisionPointerPublisher{RevisionRoot: cfg.LocalRevisionRoot, PointerPath: cfg.LocalPointerPath, Activate: cfg.LocalRevisionActivator},
			fmt.Sprintf("local-pointer:%s", cfg.LocalPointerPath), nil
	}
	return nil, nil, "", fmt.Errorf("ruleworker: local revision root and pointer path are required")
}
