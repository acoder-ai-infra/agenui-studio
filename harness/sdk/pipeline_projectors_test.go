package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// autoProjector emits one Frame per event so tests can assert on the
// accumulated frames.
type autoProjector struct{ frames atomic.Int64 }

func (a *autoProjector) Project(_ context.Context, ev extension.ProtocolEvent) ([]extension.Frame, error) {
	a.frames.Add(1)
	return []extension.Frame{{
		Kind:     "auto.frame",
		Sequence: ev.Sequence,
	}}, nil
}

// panicObserver panics inside Observe to prove the stream fan-out survives.
type panicObserver struct {
	events atomic.Int64
}

func (p *panicObserver) Observe(context.Context, extension.ProtocolEvent) error {
	p.events.Add(1)
	panic("scripted observer panic")
}

// TestExecutionProjectedFramesCapturesAutoProjectorOutput proves the
// auto-projector fan-out records frames onto Execution.ProjectedFrames.
func TestExecutionProjectedFramesCapturesAutoProjectorOutput(t *testing.T) {
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "auto-project-fake-token")

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

	projector := &autoProjector{}
	engine, _, err := harness.Build(ctx,
		harness.WithConfigPath(configPath),
		harness.WithEnvironment("local"),
		harness.WithProtocolProjector("auto.projector", projector),
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
		Identity: harness.Identity{TenantID: "public", UserID: "auto-projector-tester"},
		Input:    harness.TextMessage("project this"),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer exec.Events().Close()

	streamCtx, cancelStream := context.WithTimeout(ctx, 3*time.Second)
	defer cancelStream()
	for i := 0; i < 8; i++ {
		if _, err := exec.Events().Next(streamCtx); err != nil {
			break
		}
	}
	frames := exec.ProjectedFrames()
	if len(frames) == 0 {
		t.Fatal("Execution.ProjectedFrames() returned 0 frames; projector was not fired")
	}
	if projector.frames.Load() == 0 {
		t.Fatal("projector.frames == 0; projector never invoked")
	}
	if projector.frames.Load() != int64(len(frames)) {
		t.Fatalf("frame count mismatch: projector=%d execution=%d", projector.frames.Load(), len(frames))
	}
	for _, f := range frames {
		if f.Kind != "auto.frame" {
			t.Fatalf("unexpected frame kind: %+v", f)
		}
	}
}

// TestPanickyObserverIsRecoveredAndStreamStillDrains proves an observer that
// panics does not crash the SDK stream fan-out; the caller keeps draining
// events afterwards.
func TestPanickyObserverIsRecoveredAndStreamStillDrains(t *testing.T) {
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "panic-observer-fake-token")

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

	obs := &panicObserver{}
	engine, _, err := harness.Build(ctx,
		harness.WithConfigPath(configPath),
		harness.WithEnvironment("local"),
		harness.WithEventObserver("panic.observer", obs),
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
		Identity: harness.Identity{TenantID: "public", UserID: "panic-observer-tester"},
		Input:    harness.TextMessage("panic if you dare"),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer exec.Events().Close()

	streamCtx, cancelStream := context.WithTimeout(ctx, 3*time.Second)
	defer cancelStream()
	events := 0
	for i := 0; i < 5; i++ {
		if _, err := exec.Events().Next(streamCtx); err != nil {
			break
		}
		events++
	}
	if events == 0 {
		t.Fatal("stream produced 0 events; observer panic broke fan-out")
	}
	if obs.events.Load() == 0 {
		t.Fatal("panic observer was not invoked")
	}
}

// TestProtocolProjectorErrorIsSwallowed proves projector errors do not crash
// the stream nor short-circuit other projectors.
func TestProtocolProjectorErrorIsSwallowed(t *testing.T) {
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "error-projector-fake-token")

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
		harness.WithProtocolProjector("err.projector", errorProjector{err: errors.New("boom")}),
		harness.WithProtocolProjector("ok.projector", &autoProjector{}),
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
		Identity: harness.Identity{TenantID: "public", UserID: "error-projector-tester"},
		Input:    harness.TextMessage("drop me"),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer exec.Events().Close()

	streamCtx, cancelStream := context.WithTimeout(ctx, 3*time.Second)
	defer cancelStream()
	for i := 0; i < 5; i++ {
		if _, err := exec.Events().Next(streamCtx); err != nil {
			break
		}
	}
	// The ok projector must still have contributed frames despite the sibling error projector.
	frames := exec.ProjectedFrames()
	if len(frames) == 0 {
		t.Fatal("second projector must contribute frames even when first fails")
	}
}

type errorProjector struct{ err error }

func (e errorProjector) Project(context.Context, extension.ProtocolEvent) ([]extension.Frame, error) {
	return nil, e.err
}

// Force imports to be used even if tests are pruned by build tags.
var (
	_ = json.Marshal
)
