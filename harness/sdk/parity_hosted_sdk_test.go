package harness_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/app"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/sdk"
)

// TestHostedSDKKernelParity is the plan-required Hosted vs SDK 同构 gate. It
// boots the same harness config through both entry points (app.Build for the
// hosted server, harness.Build for the SDK) and asserts they produce
// equivalent Composition Root artifacts:
//
//   - DefaultAgentID (kernel default vs App exposure).
//   - Registered model providers.
//   - Tenant->provider fan-out.
//   - Environment tier.
//
// Both paths go through the same buildComposer registered by internal/app.init,
// so parity is a strong invariant; drift here means someone bypassed the
// shared composer.
func TestHostedSDKKernelParity(t *testing.T) {
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "parity-fake-token")

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

	// Hosted path: internal/app.Build directly.
	hostedCfg, err := app.LoadHarnessConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err := app.EnsureRuntimeDirectories(hostedCfg); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	hostedComponents, err := app.LoadComponentConfigs(hostedCfg)
	if err != nil {
		t.Fatalf("load components: %v", err)
	}
	logger := observability.NoopLogger{}
	tracer := observability.NewNoopTracer("parity.hosted")
	hostedApp, err := app.Build(
		hostedComponents.Models,
		hostedComponents.Storage,
		hostedComponents.Auth,
		hostedComponents.Redis,
		logger,
		tracer,
		app.WithHarnessConfig(hostedCfg),
		app.WithArtifactConfig(hostedComponents.Artifact),
		app.WithCapabilityCatalogs(hostedComponents.Skills, hostedComponents.Tools, hostedComponents.MCP),
	)
	if err != nil {
		t.Fatalf("hosted app.Build: %v", err)
	}
	defer hostedApp.Close()

	// SDK path: harness.Build.
	sdkEngine, sdkReport, err := harness.Build(ctx,
		harness.WithConfigPath(configPath),
		harness.WithEnvironment("local"),
	)
	if err != nil {
		t.Fatalf("harness.Build: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = sdkEngine.Close(shutdownCtx)
	}()

	// Parity assertions.
	if hostedApp.DefaultAgentID != "demo" {
		t.Fatalf("hosted DefaultAgentID = %q; want demo", hostedApp.DefaultAgentID)
	}
	if len(sdkReport.Agents) == 0 || sdkReport.Agents[0].AgentID != hostedApp.DefaultAgentID {
		t.Fatalf("SDK Report.Agents[0] = %+v; hosted DefaultAgentID = %q", sdkReport.Agents, hostedApp.DefaultAgentID)
	}
	if !equalStringSets(hostedApp.Providers, sdkReport.Providers) {
		t.Fatalf("providers drift: hosted=%v sdk=%v", hostedApp.Providers, sdkReport.Providers)
	}
	if !equalTenantSets(hostedApp.Tenants, sdkReport.Tenants) {
		t.Fatalf("tenants drift: hosted=%v sdk=%v", hostedApp.Tenants, sdkReport.Tenants)
	}
	if sdkReport.Environment != "local" {
		t.Fatalf("SDK Report.Environment = %q; want local", sdkReport.Environment)
	}
	// Hosted App exposes Environment via the internal Environment type; string
	// comparison keeps the assertion independent of type identity.
	if string(hostedApp.Environment) != sdkReport.Environment {
		t.Fatalf("environment drift: hosted=%q sdk=%q", hostedApp.Environment, sdkReport.Environment)
	}
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int, len(a))
	for _, v := range a {
		m[v]++
	}
	for _, v := range b {
		m[v]--
	}
	for _, count := range m {
		if count != 0 {
			return false
		}
	}
	return true
}

func equalTenantSets(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for tenant, providers := range a {
		other, ok := b[tenant]
		if !ok {
			return false
		}
		if !equalStringSets(providers, other) {
			return false
		}
	}
	return true
}
