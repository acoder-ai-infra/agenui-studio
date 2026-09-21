package harness_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// scriptedContributor emits a canned ContextFragment for tests.
type scriptedContributor struct{ kind, text string }

func (s scriptedContributor) Contribute(_ context.Context, _ extension.ContribRequest) ([]extension.ContextFragment, error) {
	return []extension.ContextFragment{{Kind: s.kind, Source: extension.FragmentSourceBusiness, Text: s.text}}, nil
}

type scriptedNormalizer struct{ id string }

func (s scriptedNormalizer) ID() string { return s.id }
func (s scriptedNormalizer) Normalize(_ context.Context, req extension.NormalizeRequest) (extension.NormalizedInput, error) {
	return extension.NormalizedInput{
		Message: extension.NormalizedMessage{
			Role: req.RawInput.Role,
			Parts: []extension.NormalizedPart{{
				Kind: "text", Text: "normalized: " + req.RawInput.Parts[0].Text,
			}},
		},
		SourceRefs: []extension.SourceRef{{Kind: "scoped_data", Key: "seed"}},
	}, nil
}

func harnessBuildFixture(t *testing.T, opts ...harness.Option) (harness.Engine, context.Context, func()) {
	t.Helper()
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "helper-fake-token")
	repoRoot := findRepoRoot(t)
	tempRoot := t.TempDir()
	configPath := filepath.Join(tempRoot, "harness.yaml")
	if err := os.WriteFile(configPath, []byte(renderIntegrationConfig(t, repoRoot, tempRoot)), 0o600); err != nil {
		t.Fatalf("write harness.yaml: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	if _, err := harness.RunMigration(ctx, harness.MigrationOptions{
		ConfigPath: configPath, Environment: "local",
	}); err != nil {
		cancel()
		t.Fatalf("migrate: %v", err)
	}
	opts = append(opts, harness.WithConfigPath(configPath), harness.WithEnvironment("local"))
	engine, _, err := harness.Build(ctx, opts...)
	if err != nil {
		cancel()
		t.Fatalf("build: %v", err)
	}
	teardown := func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = engine.Close(shutdownCtx)
		cancel()
	}
	return engine, ctx, teardown
}

// TestCollectContextFragmentsRunsAllContributors verifies the helper invokes
// every registered ContextContributor in order and concatenates their output.
func TestCollectContextFragmentsRunsAllContributors(t *testing.T) {
	engine, ctx, teardown := harnessBuildFixture(t,
		harness.WithContextContributor("c.a", scriptedContributor{kind: "a", text: "alpha"}),
		harness.WithContextContributor("c.b", scriptedContributor{kind: "b", text: "beta"}),
	)
	defer teardown()

	frags, err := harness.CollectContextFragments(engine, ctx, extension.ContribRequest{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(frags) != 2 {
		t.Fatalf("frags len = %d; want 2", len(frags))
	}
	if frags[0].Kind != "a" || frags[1].Kind != "b" {
		t.Fatalf("order: got %q,%q; want a,b", frags[0].Kind, frags[1].Kind)
	}
}

// TestNormalizeInputUsesRegisteredNormalizer verifies NormalizeInput calls
// the single registered normalizer and returns its NormalizedInput.
func TestNormalizeInputUsesRegisteredNormalizer(t *testing.T) {
	engine, ctx, teardown := harnessBuildFixture(t,
		harness.WithInputNormalizer("normalizer.only", scriptedNormalizer{id: "normalizer.only"}),
	)
	defer teardown()

	res, err := harness.NormalizeInput(engine, ctx, extension.NormalizeRequest{
		RawInput: extension.NormalizedMessage{
			Role:  "user",
			Parts: []extension.NormalizedPart{{Kind: "text", Text: "hello"}},
		},
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(res.Message.Parts) != 1 || res.Message.Parts[0].Text != "normalized: hello" {
		t.Fatalf("unexpected normalized parts: %+v", res.Message.Parts)
	}
	if len(res.SourceRefs) == 0 {
		t.Fatal("normalizer must record SourceRefs")
	}
}

// TestNormalizeInputPassthroughWhenNoNormalizer verifies caller input is
// unchanged when no InputNormalizer is registered.
func TestNormalizeInputPassthroughWhenNoNormalizer(t *testing.T) {
	engine, ctx, teardown := harnessBuildFixture(t)
	defer teardown()

	raw := extension.NormalizedMessage{
		Role:  "user",
		Parts: []extension.NormalizedPart{{Kind: "text", Text: "passthrough"}},
	}
	res, err := harness.NormalizeInput(engine, ctx, extension.NormalizeRequest{RawInput: raw})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(res.Message.Parts) != 1 || res.Message.Parts[0].Text != "passthrough" {
		t.Fatalf("passthrough failed: %+v", res)
	}
}
