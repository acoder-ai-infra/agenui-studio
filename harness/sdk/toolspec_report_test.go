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

type reportedFunctionTool struct{ name string }

func (t reportedFunctionTool) Name() string { return t.name }
func (t reportedFunctionTool) Invoke(context.Context, extension.FunctionCall) (*extension.FunctionResult, error) {
	return &extension.FunctionResult{Data: json.RawMessage(`{}`)}, nil
}

type reportedToolProvider struct{}

func (reportedToolProvider) ID() string { return "test.reported_tools" }

func (reportedToolProvider) FunctionTools() []extension.FunctionTool {
	return []extension.FunctionTool{
		reportedFunctionTool{name: "reported.echo"},
		reportedFunctionTool{name: "reported.random"},
	}
}

// TestBuildReportSurfacesToolProviderHandlers 验证 BuildReport.Extensions
// 浮现 ToolProvider 提供的 handler 名（rc.6：定义在 tools.yaml，代码只交
// 实现），供审计方在不触碰 runtime 的情况下盘点业务实现目录。
func TestBuildReportSurfacesToolProviderHandlers(t *testing.T) {
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "toolspec-report-fake")

	repoRoot := findRepoRoot(t)
	tempRoot := t.TempDir()
	configPath := filepath.Join(tempRoot, "harness.yaml")
	if err := os.WriteFile(configPath, []byte(renderIntegrationConfig(t, repoRoot, tempRoot)), 0o600); err != nil {
		t.Fatalf("write harness.yaml: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := harness.RunMigration(ctx, harness.MigrationOptions{ConfigPath: configPath, Environment: "local"}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	engine, report, err := harness.Build(ctx,
		harness.WithConfigPath(configPath),
		harness.WithEnvironment("local"),
		harness.WithToolProvider(reportedToolProvider{}),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = engine.Close(shutdownCtx)
	}()

	var toolExt *harness.ExtensionInfo
	for i, e := range report.Extensions {
		if e.ID == "test.reported_tools" {
			toolExt = &report.Extensions[i]
			break
		}
	}
	if toolExt == nil {
		t.Fatal("ToolProvider extension not surfaced in BuildReport.Extensions")
	}
	if len(toolExt.HandlerNames) != 2 {
		t.Fatalf("HandlerNames len = %d; want 2 (%v)", len(toolExt.HandlerNames), toolExt.HandlerNames)
	}
	seen := map[string]bool{}
	for _, name := range toolExt.HandlerNames {
		seen[name] = true
	}
	// 两个实现都没有被 tools.yaml 引用：必须标注 unbound（审计面浮现绑定状态）。
	if !seen["reported.echo (unbound)"] || !seen["reported.random (unbound)"] {
		t.Fatalf("unbound handlers must be annotated: %v", toolExt.HandlerNames)
	}
}
