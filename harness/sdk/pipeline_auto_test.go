package harness_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// autoContributor counts invocations to prove Start called it.
type autoContributor struct {
	calls atomic.Int64
	kind  string
	text  string
}

func (a *autoContributor) Contribute(_ context.Context, _ extension.ContribRequest) ([]extension.ContextFragment, error) {
	a.calls.Add(1)
	return []extension.ContextFragment{{Kind: a.kind, Source: extension.FragmentSourceBusiness, Text: a.text}}, nil
}

// autoNormalizer replaces user text so tests can prove the run saw the
// normalized version.
type autoNormalizer struct {
	calls   atomic.Int64
	prefix  string
	failErr error
}

func (a *autoNormalizer) ID() string { return "test.auto.normalizer" }
func (a *autoNormalizer) Normalize(_ context.Context, req extension.NormalizeRequest) (extension.NormalizedInput, error) {
	a.calls.Add(1)
	if a.failErr != nil {
		return extension.NormalizedInput{}, a.failErr
	}
	original := ""
	if len(req.RawInput.Parts) > 0 {
		original = req.RawInput.Parts[0].Text
	}
	return extension.NormalizedInput{
		Message: extension.NormalizedMessage{
			Role: req.RawInput.Role,
			Parts: []extension.NormalizedPart{{
				Kind: "text", Text: a.prefix + original,
			}},
		},
		SourceRefs: []extension.SourceRef{{Kind: "scoped_data", Key: "auto-source"}},
	}, nil
}

// TestStartAutoRunsContextContributorAndNormalizer verifies both auto-execute
// stages run inside Start and their outputs are visible on the RunHandle.
func TestStartAutoRunsContextContributorAndNormalizer(t *testing.T) {
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "auto-pipeline-fake-token")

	repoRoot := findRepoRoot(t)
	tempRoot := t.TempDir()
	configPath := filepath.Join(tempRoot, "harness.yaml")
	if err := os.WriteFile(configPath, []byte(renderIntegrationConfig(t, repoRoot, tempRoot)), 0o600); err != nil {
		t.Fatalf("write harness.yaml: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := harness.RunMigration(ctx, harness.MigrationOptions{
		ConfigPath: configPath, Environment: "local",
	}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	contrib := &autoContributor{kind: "kb.snippet", text: "context body"}
	norm := &autoNormalizer{prefix: "NORM|"}

	engine, _, err := harness.Build(ctx,
		harness.WithConfigPath(configPath),
		harness.WithEnvironment("local"),
		harness.WithContextContributor("auto.contributor", contrib),
		harness.WithInputNormalizer("auto.normalizer", norm),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = engine.Close(shutdownCtx)
	}()

	exec, err := engine.Start(ctx, harness.StartRequest{
		Identity: harness.Identity{TenantID: "public", UserID: "auto-tester"},
		Input:    harness.TextMessage("original"),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer exec.Events().Close()

	if got := contrib.calls.Load(); got != 1 {
		t.Fatalf("ContextContributor.calls = %d; want 1", got)
	}
	if got := norm.calls.Load(); got != 1 {
		t.Fatalf("InputNormalizer.calls = %d; want 1", got)
	}
	// The RunHandle's manifest must reflect the NORMALIZED input, proving the
	// normalizer's output reached the run's frozen input.
	handle := exec.Handle()
	if len(handle.InputManifest.Parts) == 0 {
		t.Fatal("InputManifest.Parts empty")
	}
	// Manifest hashes over the normalized content; we cannot compare text
	// directly against a hash but we can prove the manifest differs from the
	// raw input's manifest.
	rawHandle := harness.BuildInputManifest(harness.TextMessage("original"))
	if handle.InputManifest.Hash == rawHandle.Hash {
		t.Fatalf("manifest hash did not change after normalization: %q", handle.InputManifest.Hash)
	}
}

// TestStartAutoNormalizerFailFailsClosed verifies a normalizer error fails
// Start closed.
func TestStartAutoNormalizerFailFailsClosed(t *testing.T) {
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "auto-normfail-fake-token")

	repoRoot := findRepoRoot(t)
	tempRoot := t.TempDir()
	configPath := filepath.Join(tempRoot, "harness.yaml")
	if err := os.WriteFile(configPath, []byte(renderIntegrationConfig(t, repoRoot, tempRoot)), 0o600); err != nil {
		t.Fatalf("write harness.yaml: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := harness.RunMigration(ctx, harness.MigrationOptions{
		ConfigPath: configPath, Environment: "local",
	}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	sentinel := errors.New("normalizer refused")
	norm := &autoNormalizer{failErr: sentinel}
	engine, _, err := harness.Build(ctx,
		harness.WithConfigPath(configPath),
		harness.WithEnvironment("local"),
		harness.WithInputNormalizer("auto.normalizer", norm),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = engine.Close(shutdownCtx)
	}()

	_, err = engine.Start(ctx, harness.StartRequest{
		Identity: harness.Identity{TenantID: "public", UserID: "auto-norm-fail"},
		Input:    harness.TextMessage("nope"),
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Start should fail-closed on normalizer error; got %v", err)
	}
}
