package harness_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

type dummyToolProvider struct{}

func (dummyToolProvider) ID() string { return "myapp.echo_tool" }

type dummyEchoTool struct{}

func (dummyEchoTool) Name() string { return "wiring.echo" }
func (dummyEchoTool) Invoke(context.Context, extension.FunctionCall) (*extension.FunctionResult, error) {
	return &extension.FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
}

func (dummyToolProvider) FunctionTools() []extension.FunctionTool {
	return []extension.FunctionTool{dummyEchoTool{}}
}

type dummyIdentityResolver struct{}

func (dummyIdentityResolver) Resolve(context.Context, extension.IdentityRequest) (extension.ResolvedIdentity, error) {
	return extension.ResolvedIdentity{TenantID: "resolved-tenant"}, nil
}

// TestBuildRegistersExtensionsInBuildReport is the plan-required proof that
// WithExtension flows all the way through harness.Build -> kernel.Build ->
// the composer -> Kernel.Extensions -> BuildReport.Extensions.
func TestBuildRegistersExtensionsInBuildReport(t *testing.T) {
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "ext-fake-token")

	repoRoot := findRepoRoot(t)
	tempRoot := t.TempDir()
	configPath := filepath.Join(tempRoot, "harness.yaml")
	if err := os.WriteFile(configPath, []byte(renderIntegrationConfig(t, repoRoot, tempRoot)), 0o600); err != nil {
		t.Fatalf("write harness.yaml: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if _, err := harness.RunMigration(ctx, harness.MigrationOptions{
		ConfigPath:  configPath,
		Environment: "local",
	}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	engine, report, err := harness.Build(ctx,
		harness.WithConfigPath(configPath),
		harness.WithEnvironment("local"),
		harness.WithIdentityResolver("myapp.identity", dummyIdentityResolver{}),
		harness.WithToolProvider(dummyToolProvider{}),
	)
	if err != nil {
		t.Fatalf("harness.Build: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = engine.Close(shutdownCtx)
	}()

	if len(report.Extensions) != 2 {
		t.Fatalf("BuildReport.Extensions len = %d; want 2", len(report.Extensions))
	}
	seen := map[string]bool{}
	for _, e := range report.Extensions {
		if e.ID == "" {
			t.Fatal("extension entry missing ID")
		}
		if e.Fingerprint == "" {
			t.Fatalf("extension %q missing Fingerprint", e.ID)
		}
		if e.FailurePolicy != "fail_closed" {
			t.Fatalf("extension %q FailurePolicy = %q; want fail_closed", e.ID, e.FailurePolicy)
		}
		seen[e.ID] = true
	}
	if !seen["myapp.identity"] || !seen["myapp.echo_tool"] {
		t.Fatalf("BuildReport.Extensions missing entries: %+v", report.Extensions)
	}
}
