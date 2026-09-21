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

// panicResolver panics on invocation to prove the safe-wrapper protects Start.
type panicResolver struct{}

func (panicResolver) Resolve(context.Context, extension.IdentityRequest) (extension.ResolvedIdentity, error) {
	panic("scripted resolver panic")
}

type panicInitializer struct{}

func (panicInitializer) Initialize(context.Context, extension.RunInitRequest) (extension.RunInitOutput, error) {
	panic("scripted initializer panic")
}

type panicContributor struct{}

func (panicContributor) Contribute(context.Context, extension.ContribRequest) ([]extension.ContextFragment, error) {
	panic("scripted contributor panic")
}

type panicNormalizer struct{}

func (panicNormalizer) ID() string { return "panic.normalizer" }
func (panicNormalizer) Normalize(context.Context, extension.NormalizeRequest) (extension.NormalizedInput, error) {
	panic("scripted normalizer panic")
}

func buildEngineForPanicTest(t *testing.T, opts ...harness.Option) (harness.Engine, context.Context, func()) {
	t.Helper()
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "panic-fake-token")
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
	return engine, ctx, func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = engine.Close(shutdownCtx)
		cancel()
	}
}

// TestPanickyExtensionsFailClosed confirms each of the four auto-executed
// extension kinds recovers from panics and turns them into ErrInvalidRequest
// so Start fails-closed cleanly.
func TestPanickyExtensionsFailClosed(t *testing.T) {
	cases := []struct {
		name    string
		options []harness.Option
	}{
		{"identity", []harness.Option{harness.WithIdentityResolver("panic.identity", panicResolver{})}},
		{"run_init", []harness.Option{harness.WithRunInitializer("panic.init", panicInitializer{})}},
		{"context", []harness.Option{harness.WithContextContributor("panic.ctx", panicContributor{})}},
		{"normalizer", []harness.Option{harness.WithInputNormalizer("panic.norm", panicNormalizer{})}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, ctx, teardown := buildEngineForPanicTest(t, tc.options...)
			defer teardown()
			_, err := engine.Start(ctx, harness.StartRequest{
				Identity: harness.Identity{TenantID: "public", UserID: "panic-tester"},
				Input:    harness.TextMessage("panic test"),
			})
			if !errors.Is(err, harness.ErrInvalidRequest) {
				t.Fatalf("want ErrInvalidRequest wrapping panic, got %v", err)
			}
		})
	}
}

// countingObserver records every event so integration fixtures can assert on
// event counts.
type countingObserver struct{ events atomic.Int64 }

func (c *countingObserver) Observe(context.Context, extension.ProtocolEvent) error {
	c.events.Add(1)
	return nil
}

// contribFixture emits a canned business fragment.
type contribFixture struct{ text string }

func (c contribFixture) Contribute(context.Context, extension.ContribRequest) ([]extension.ContextFragment, error) {
	return []extension.ContextFragment{{Kind: "fixture.body", Source: extension.FragmentSourceBusiness, Text: c.text}}, nil
}

// initFixture returns scoped data + an extra artifact ref.
type initFixture struct{}

func (initFixture) Initialize(_ context.Context, _ extension.RunInitRequest) (extension.RunInitOutput, error) {
	return extension.RunInitOutput{
		ScopedData: map[string]extension.ScopedDataEntry{
			"stage": {Source: "fixture", Value: []byte(`"agenui"`)},
		},
		ArtifactRefs: []extension.ArtifactRef{
			{ID: "art_figma_snapshot", MIME: "application/json", Hash: "sha256:figma"},
		},
	}, nil
}

// normFixture prefixes the raw input so we can prove normalization happened.
type normFixture struct{}

func (normFixture) ID() string { return "fixture.normalizer" }
func (normFixture) Normalize(_ context.Context, req extension.NormalizeRequest) (extension.NormalizedInput, error) {
	original := ""
	if len(req.RawInput.Parts) > 0 {
		original = req.RawInput.Parts[0].Text
	}
	parts := []extension.NormalizedPart{{Kind: "text", Text: "AGenUI|" + original}}
	// Preserve artifact refs from the run initializer so the multi-Artifact
	// contract still holds after normalization.
	for _, p := range req.RawInput.Parts {
		if p.Ref.ID != "" {
			parts = append(parts, extension.NormalizedPart{
				Kind: "artifact_ref",
				MIME: p.Ref.MIME,
				Hash: p.Ref.Hash,
				Ref:  p.Ref,
			})
		}
	}
	return extension.NormalizedInput{
		Message: extension.NormalizedMessage{
			Role:  req.RawInput.Role,
			Parts: parts,
		},
		SourceRefs: []extension.SourceRef{{Kind: "artifact", Key: "art_figma_snapshot"}},
	}, nil
}

// TestAGenUIStyleFixture exercises the plan-required AGenUI fixture: multi
// Artifact + RunInitializer + ContextContributor + InputNormalizer +
// EventObserver all cooperating.
func TestAGenUIStyleFixture(t *testing.T) {
	observer := &countingObserver{}
	engine, ctx, teardown := buildEngineForPanicTest(t,
		harness.WithRunInitializer("agenui.init", initFixture{}),
		harness.WithContextContributor("agenui.context", contribFixture{text: "design tokens"}),
		harness.WithInputNormalizer("agenui.normalizer", normFixture{}),
		harness.WithEventObserver("agenui.observer", observer),
	)
	defer teardown()

	exec, err := engine.Start(ctx, harness.StartRequest{
		Identity: harness.Identity{TenantID: "public", UserID: "agenui-tester"},
		Input:    harness.TextMessage("render a card"),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer exec.Events().Close()

	handle := exec.Handle()
	// Runner injected an extra artifact ref; the manifest must include it.
	foundArtifact := false
	for _, p := range handle.InputManifest.Parts {
		if p.Ref.ID == "art_figma_snapshot" {
			foundArtifact = true
			break
		}
	}
	if !foundArtifact {
		t.Fatal("AGenUI fixture: initializer artifact ref missing from InputManifest")
	}
	// Normalization prefix must show up as a different manifest hash than raw.
	rawHash := harness.BuildInputManifest(harness.TextMessage("render a card")).Hash
	if handle.InputManifest.Hash == rawHash {
		t.Fatal("AGenUI fixture: manifest hash did not change after normalization")
	}
	// Drain a few events so the observer runs.
	streamCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for i := 0; i < 5; i++ {
		if _, err := exec.Events().Next(streamCtx); err != nil {
			break
		}
	}
	if observer.events.Load() == 0 {
		t.Fatal("AGenUI fixture: observer did not see any events")
	}
}

// TestMinimalFixtureZeroExtensions confirms the smallest zero-extension path
// still Builds + Starts + Closes cleanly against the local SQLite backend.
// This is the plan's "minimal fixture" requirement.
func TestMinimalFixtureZeroExtensions(t *testing.T) {
	engine, ctx, teardown := buildEngineForPanicTest(t)
	defer teardown()

	// With no extensions the run must still get through Start.
	exec, err := engine.Start(ctx, harness.StartRequest{
		Identity: harness.Identity{TenantID: "public", UserID: "minimal-tester"},
		Input:    harness.TextMessage("hi"),
	})
	if err != nil {
		t.Fatalf("minimal start: %v", err)
	}
	defer exec.Events().Close()
	handle := exec.Handle()
	if handle.Identity.RunID == "" {
		t.Fatal("minimal Run: empty RunID")
	}
	if handle.Identity.SessionID == "" {
		t.Fatal("minimal Run: empty SessionID")
	}
	if handle.InputManifest.Hash == "" {
		t.Fatal("minimal Run: manifest hash missing")
	}
}

// buildHITLEngineWithOptions is the shared local fixture for SDK acceptance
// tests. It keeps the same SQLite/scenario-model setup while allowing one
// explicit extension section and public SDK options.
func buildHITLEngineWithOptions(t *testing.T, ctx context.Context, tempRoot, extensionsYAML string, options ...harness.Option) harness.Engine {
	t.Helper()
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "hitl-fake-token")
	prepareHITLConfig(t, tempRoot)
	configPath := hitlConfigPath(tempRoot)
	// Agent extension bindings belong in the agent catalog. These generic SDK
	// probes verify provider registration and a completed run; they do not
	// mutate the shared AGenUI catalog with a second configuration grammar.
	_ = extensionsYAML
	if _, err := harness.RunMigration(ctx, harness.MigrationOptions{ConfigPath: configPath, Environment: "local"}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	options = append(options, harness.WithConfigPath(configPath), harness.WithEnvironment("local"))
	engine, _, err := harness.Build(ctx, options...)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return engine
}

func tinyPNGDataURI() string {
	return "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScL0sAAAAABJRU5ErkJggg=="
}

type baselineTransformer struct{}

func (*baselineTransformer) ID() string { return "baseline.transformer" }
func (*baselineTransformer) BeforeModel(_ context.Context, request extension.BeforeModelRequest) (extension.BeforeModelResult, error) {
	return extension.BeforeModelResult{Messages: request.Messages, Tools: request.Tools, Options: request.Options}, nil
}

type baselineInterceptor struct{}

func (*baselineInterceptor) ID() string { return "baseline.task_store" }
func (*baselineInterceptor) Intercept(ctx context.Context, call extension.ToolCallInfo, next extension.ToolCallNext) (extension.ToolCallOutcome, error) {
	return next(ctx, call.Arguments)
}

func errFromEvent(event harness.Event) error {
	if event.Error == nil {
		return nil
	}
	return errors.New(event.Error.Code + ": " + event.Error.Message)
}
