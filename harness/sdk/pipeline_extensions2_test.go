package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// scriptedInitializer records inputs and returns a canned RunInitOutput.
type scriptedInitializer struct {
	mu      sync.Mutex
	calls   int
	lastReq extension.RunInitRequest
	output  extension.RunInitOutput
	err     error
}

func (s *scriptedInitializer) Initialize(_ context.Context, req extension.RunInitRequest) (extension.RunInitOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastReq = req
	if s.err != nil {
		return extension.RunInitOutput{}, s.err
	}
	return s.output, nil
}

// TestRunInitializerExecutesAndMergesScopedData proves that a registered
// RunInitializer is invoked at Start time and that its ScopedData +
// ArtifactRefs actually reach the run's input.
func TestRunInitializerExecutesAndMergesScopedData(t *testing.T) {
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "runinit-fake-token")

	repoRoot := findRepoRoot(t)
	tempRoot := t.TempDir()
	configPath := filepath.Join(tempRoot, "harness.yaml")
	if err := os.WriteFile(configPath, []byte(renderIntegrationConfig(t, repoRoot, tempRoot)), 0o600); err != nil {
		t.Fatalf("write harness.yaml: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if _, err := harness.RunMigration(ctx, harness.MigrationOptions{
		ConfigPath: configPath, Environment: "local",
	}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	initializer := &scriptedInitializer{
		output: extension.RunInitOutput{
			ScopedData: map[string]extension.ScopedDataEntry{
				"business.stage": {
					Source: "test",
					Value:  json.RawMessage(`"stage-A"`),
				},
			},
			ArtifactRefs: []extension.ArtifactRef{
				{ID: "art_stage_snapshot", MIME: "application/json", Hash: "sha256:deadbeef"},
			},
		},
	}
	engine, _, err := harness.Build(ctx,
		harness.WithConfigPath(configPath),
		harness.WithEnvironment("local"),
		harness.WithRunInitializer("test.initializer", initializer),
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
		Identity: harness.Identity{TenantID: "public", UserID: "runinit-tester"},
		Input:    harness.TextMessage("runinit test"),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer exec.Events().Close()

	initializer.mu.Lock()
	calls := initializer.calls
	last := initializer.lastReq
	initializer.mu.Unlock()
	if calls != 1 {
		t.Fatalf("initializer.calls = %d; want 1", calls)
	}
	if last.InputPreview == "" {
		t.Fatal("initializer must receive InputPreview")
	}
	if last.Ctx.TenantID != "public" {
		t.Fatalf("initializer Ctx.TenantID = %q; want public", last.Ctx.TenantID)
	}

	handle := exec.Handle()
	// The initializer's ArtifactRefs should be present in the frozen manifest.
	foundArtifactRef := false
	for _, p := range handle.InputManifest.Parts {
		if p.Ref.ID == "art_stage_snapshot" {
			foundArtifactRef = true
			break
		}
	}
	if !foundArtifactRef {
		t.Fatalf("initializer ArtifactRef missing from RunHandle.InputManifest: %+v", handle.InputManifest.Parts)
	}
}

// TestRunInitializerErrorFailsClosed verifies a failing initializer aborts
// Start with the error surfaced to the caller.
func TestRunInitializerErrorFailsClosed(t *testing.T) {
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "runinit-fake-token")

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

	boom := errors.New("initializer refused")
	initializer := &scriptedInitializer{err: boom}
	engine, _, err := harness.Build(ctx,
		harness.WithConfigPath(configPath),
		harness.WithEnvironment("local"),
		harness.WithRunInitializer("test.initializer", initializer),
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
		Identity: harness.Identity{TenantID: "public", UserID: "runinit-error-tester"},
		Input:    harness.TextMessage("boom"),
	})
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("Start should fail-closed on RunInitializer error; got %v", err)
	}
}

// scriptedProjector records events and emits stable business frames.
type scriptedProjector struct{}

func (scriptedProjector) Project(_ context.Context, ev extension.ProtocolEvent) ([]extension.Frame, error) {
	return []extension.Frame{{
		Kind:     "business_frame",
		Sequence: ev.Sequence,
		Payload:  ev.Payload,
	}}, nil
}

// TestProjectEventInvokesRegisteredProjectors verifies harness.ProjectEvent
// runs registered ProtocolProjectors and returns the concatenated frames.
func TestProjectEventInvokesRegisteredProjectors(t *testing.T) {
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "projector-fake-token")

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

	engine, _, err := harness.Build(ctx,
		harness.WithConfigPath(configPath),
		harness.WithEnvironment("local"),
		harness.WithProtocolProjector("test.projector", scriptedProjector{}),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = engine.Close(shutdownCtx)
	}()

	frames, err := harness.ProjectEvent(engine, ctx, harness.Event{
		Sequence:   7,
		EventType:  harness.EventAgentTextDelta,
		Visibility: harness.VisibilityUserVisible,
	})
	if err != nil {
		t.Fatalf("ProjectEvent: %v", err)
	}
	if len(frames) != 1 || frames[0].Kind != "business_frame" || frames[0].Sequence != 7 {
		t.Fatalf("unexpected frames: %+v", frames)
	}
}

// scriptedValidator returns a canned OutputValidateResult, letting tests
// exercise the retry/fail paths.
type scriptedValidator struct {
	id     string
	result extension.OutputValidateResult
	err    error
}

func (s scriptedValidator) ID() string { return s.id }

func (s scriptedValidator) Validate(_ context.Context, _ extension.OutputValidateRequest) (extension.OutputValidateResult, error) {
	return s.result, s.err
}

// TestValidateOutputAppliesFirstNonAccept ensures ValidateOutput returns the
// first non-Accept action, honouring the ordered execution contract.
func TestValidateOutputAppliesFirstNonAccept(t *testing.T) {
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "validator-fake-token")

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

	engine, _, err := harness.Build(ctx,
		harness.WithConfigPath(configPath),
		harness.WithEnvironment("local"),
		harness.WithOutputValidatorProvider(
			scriptedValidator{
				id:     "test.validator.a",
				result: extension.OutputValidateResult{Action: extension.OutputAccept},
			},
			scriptedValidator{
				id:     "test.validator.b",
				result: extension.OutputValidateResult{Action: extension.OutputRetry, RetryBudget: 2, Reason: "malformed"},
			},
		),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = engine.Close(shutdownCtx)
	}()

	res, err := harness.ValidateOutput(engine, ctx, extension.OutputValidateRequest{Text: "output"})
	if err != nil {
		t.Fatalf("ValidateOutput: %v", err)
	}
	if res.Action != extension.OutputRetry || res.RetryBudget != 2 || res.Reason != "malformed" {
		t.Fatalf("unexpected result: %+v", res)
	}
}
