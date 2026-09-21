package harness_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// hostLookupTool 模拟"宿主进程内查业务库"的 function 工具实现：闭包持有
// 内存表（等价于 agenui 的 dbGetter 模式），定义与治理属性在 tools.yaml。
type hostLookupTool struct {
	store map[string]string
}

func (t *hostLookupTool) Name() string { return "e2e.host_lookup" }

func (t *hostLookupTool) Invoke(_ context.Context, call extension.FunctionCall) (*extension.FunctionResult, error) {
	var args struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return nil, err
	}
	data, err := json.Marshal(map[string]string{"template": t.store[args.Key]})
	if err != nil {
		return nil, err
	}
	return &extension.FunctionResult{Data: data, MimeType: "application/json"}, nil
}

// badSchemaTool 返回违反 tools.yaml output_schema 的结果（输出校验在场证明）。
type badSchemaTool struct{}

func (badSchemaTool) Name() string { return "e2e.bad_schema" }

func (badSchemaTool) Invoke(context.Context, extension.FunctionCall) (*extension.FunctionResult, error) {
	return &extension.FunctionResult{Data: json.RawMessage(`{"unexpected":"shape"}`), MimeType: "application/json"}, nil
}

type hostFnProvider struct {
	lookup *hostLookupTool
}

func (hostFnProvider) ID() string { return "e2e.host_tools" }

func (p hostFnProvider) FunctionTools() []extension.FunctionTool {
	return []extension.FunctionTool{p.lookup, badSchemaTool{}}
}

// reservedNameProvider 用于 Build 负向：实现名占用 harness. 保留前缀。
type reservedNameTool struct{}

func (reservedNameTool) Name() string { return "harness.evil" }
func (reservedNameTool) Invoke(context.Context, extension.FunctionCall) (*extension.FunctionResult, error) {
	return nil, nil
}

type reservedNameProvider struct{}

func (reservedNameProvider) ID() string { return "e2e.reserved" }

func (reservedNameProvider) FunctionTools() []extension.FunctionTool {
	return []extension.FunctionTool{reservedNameTool{}}
}

const hostFnToolCatalogPatch = `  - name: e2e_host_lookup
    version: 1.0.0
    type: function
    description: rc.6 acceptance host lookup tool backed by an in-process store.
    allowed_agents: [control_agent]
    handler: e2e.host_lookup
    input_schema:
      type: object
      properties: {key: {type: string}}
      required: [key]
      additionalProperties: false
    output_schema:
      type: object
      properties: {template: {type: string}}
      required: [template]
      additionalProperties: false
    max_inline_bytes: 4096
  - name: e2e_bad_schema
    version: 1.0.0
    type: function
    description: rc.6 acceptance tool returning schema-violating output.
    allowed_agents: [control_agent]
    handler: e2e.bad_schema
    input_schema:
      type: object
      properties: {key: {type: string}}
      required: [key]
      additionalProperties: false
    output_schema:
      type: object
      properties: {template: {type: string}}
      required: [template]
      additionalProperties: false
    max_inline_bytes: 4096
`

// prepareHostFnConfig creates an isolated generic fixture with two host tools.
func prepareHostFnConfig(t *testing.T, tempRoot string) string {
	t.Helper()
	repoRoot := findRepoRoot(t)
	fixture := filepath.Join(repoRoot, "testdata", "local")

	// tools.yaml：追加 enabled 与 definitions。
	rawTools, err := os.ReadFile(filepath.Join(fixture, "catalogs", "tools.yaml"))
	if err != nil {
		t.Fatalf("read tools.yaml: %v", err)
	}
	patched := strings.Replace(string(rawTools), "definitions:\n",
		"  - e2e_host_lookup@1.0.0\n  - e2e_bad_schema@1.0.0\ndefinitions:\n"+hostFnToolCatalogPatch, 1)
	if patched == string(rawTools) {
		t.Fatal("tools.yaml patch anchor not found")
	}
	toolsPath := filepath.Join(tempRoot, "tools.yaml")
	if err := os.WriteFile(toolsPath, []byte(patched), 0o600); err != nil {
		t.Fatalf("write tools.yaml: %v", err)
	}

	// Copy the generic agents and add the two host tools to control_agent.
	agentsDir := filepath.Join(tempRoot, "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir agents: %v", err)
	}
	sourceAgents := filepath.Join(fixture, "agents")
	dirEntries, err := os.ReadDir(sourceAgents)
	if err != nil {
		t.Fatalf("read agents dir: %v", err)
	}
	for _, entry := range dirEntries {
		if entry.IsDir() {
			continue
		}
		body, readErr := os.ReadFile(filepath.Join(sourceAgents, entry.Name()))
		if readErr != nil {
			t.Fatalf("read agent %s: %v", entry.Name(), readErr)
		}
		if entry.Name() == "control_agent.yaml" {
			patchedAgent := strings.Replace(string(body), "tools:\n",
				"tools:\n  - name: e2e_host_lookup\n    version: 1.0.0\n  - name: e2e_bad_schema\n    version: 1.0.0\n", 1)
			if patchedAgent == string(body) {
				t.Fatal("control_agent.yaml patch anchor not found")
			}
			body = []byte(patchedAgent)
		}
		if writeErr := os.WriteFile(filepath.Join(agentsDir, entry.Name()), body, 0o600); writeErr != nil {
			t.Fatalf("write agent %s: %v", entry.Name(), writeErr)
		}
	}

	config := renderIntegrationConfig(t, repoRoot, tempRoot)
	config = strings.Replace(config,
		"  tools: "+filepath.Join(fixture, "catalogs", "tools.yaml"),
		"  tools: "+toolsPath, 1)
	config = strings.Replace(config,
		"  agents: {source: file, path: "+filepath.Join(fixture, "agents")+"}",
		"  agents: {source: file, path: "+agentsDir+"}", 1)
	configPath := filepath.Join(tempRoot, "harness.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write harness.yaml: %v", err)
	}
	return configPath
}

// TestHostFunctionToolThroughGatewayE2E 验证 rc.6 工具治理收敛的三类门禁：
// 定义在 tools.yaml、实现由 ToolProvider 提供、执行统一走 Tool Gateway。
func TestHostFunctionToolThroughGatewayE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	tempRoot := t.TempDir()
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "hostfn-fake-token")
	configPath := prepareHostFnConfig(t, tempRoot)
	if _, err := harness.RunMigration(ctx, harness.MigrationOptions{ConfigPath: configPath, Environment: "local"}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	lookup := &hostLookupTool{store: map[string]string{"welcome_card": "<layout name=\"welcome\"/>"}}
	engine, report, err := harness.Build(ctx,
		harness.WithConfigPath(configPath),
		harness.WithEnvironment("local"),
		harness.WithToolProvider(hostFnProvider{lookup: lookup}),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = engine.Close(shutdownCtx)
	}()
	// BuildReport 浮现实现目录；两个实现均被 tools.yaml 引用（bound，无标注）。
	var handlerNames []string
	for _, ext := range report.Extensions {
		if ext.ID == "e2e.host_tools" {
			handlerNames = ext.HandlerNames
		}
	}
	if len(handlerNames) != 2 {
		t.Fatalf("HandlerNames = %v; want 2 entries", handlerNames)
	}
	for _, name := range handlerNames {
		if strings.Contains(name, "unbound") {
			t.Fatalf("bound handler must not carry the unbound annotation: %v", handlerNames)
		}
	}

	// --- 正向：模型调用宿主工具，读取进程内状态并回到模型循环 ---
	execution, err := engine.Start(ctx, harness.StartRequest{
		Identity: harness.Identity{TenantID: "public", UserID: "hostfn-e2e", AgentID: "control_agent"},
		Input:    harness.TextMessage("[[harness:tool:host_fn]] 请查询欢迎卡模板"),
	})
	if err != nil {
		t.Fatalf("start host_fn run: %v", err)
	}
	terminal, payloads := drainUntilTerminal(t, ctx, execution)
	if terminal != harness.EventRunCompleted {
		t.Fatalf("host_fn run terminal = %s, want run_completed", terminal)
	}
	var sawTemplate bool
	for _, payload := range payloads {
		if strings.Contains(payload, "welcome") {
			sawTemplate = true
			break
		}
	}
	if !sawTemplate {
		t.Fatalf("host tool result must reach the model loop: %v", payloads)
	}

	// --- 输出 schema 在场证明：违反 output_schema 的结果被 gateway 拦截 ---
	badRun, err := engine.Start(ctx, harness.StartRequest{
		Identity: harness.Identity{TenantID: "public", UserID: "hostfn-e2e", AgentID: "control_agent"},
		Input:    harness.TextMessage("[[harness:tool:host_bad_schema]] 触发违规输出"),
	})
	if err != nil {
		t.Fatalf("start bad_schema run: %v", err)
	}
	var sawToolFailed bool
	for {
		ev, streamErr := badRun.Events().Next(ctx)
		if streamErr != nil {
			t.Fatalf("bad_schema stream: %v", streamErr)
		}
		if ev.EventType == harness.EventToolCallFailed {
			sawToolFailed = true
		}
		if isTerminalE2E(ev.EventType) {
			break
		}
	}
	if !sawToolFailed {
		t.Fatal("schema-violating output must surface tool_call_failed (output schema validation)")
	}
}

// TestHostFunctionToolBuildFailClosed 验证 Build 期两条负向门禁：
// harness. 保留前缀与 tools.yaml 引用未注册实现。
func TestHostFunctionToolBuildFailClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	tempRoot := t.TempDir()
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "hostfn-fake-token")
	configPath := prepareHostFnConfig(t, tempRoot)
	if _, err := harness.RunMigration(ctx, harness.MigrationOptions{ConfigPath: configPath, Environment: "local"}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 保留前缀 fail-closed。
	if _, _, err := harness.Build(ctx,
		harness.WithConfigPath(configPath),
		harness.WithEnvironment("local"),
		harness.WithToolProvider(hostFnProvider{lookup: &hostLookupTool{}}, reservedNameProvider{}),
	); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved prefix must fail closed, got %v", err)
	}

	// tools.yaml 引用了 e2e.host_lookup / e2e.bad_schema，但宿主未注册实现。
	if _, _, err := harness.Build(ctx,
		harness.WithConfigPath(configPath),
		harness.WithEnvironment("local"),
	); err == nil || !strings.Contains(err.Error(), "unknown function handler") {
		t.Fatalf("unregistered handler reference must fail closed, got %v", err)
	}
}
