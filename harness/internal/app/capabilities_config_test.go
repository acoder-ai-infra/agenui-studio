package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	storagememory "github.com/AGenUI/agenui-studio/harness/internal/storage/memory"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway/runtimeadapter"
	_ "modernc.org/sqlite"
)

// fileSourcedCapabilityComponents 构造 file 来源的 skills/mcp 声明（path 留空），
// 用于验证能力配置预检和后续构造失败的资源清理。
func fileSourcedCapabilityComponents() HarnessComponentPaths {
	return HarnessComponentPaths{
		Skills: HarnessSourcedComponent{Source: ComponentSourceFile},
		MCP:    HarnessSourcedComponent{Source: ComponentSourceFile},
	}
}

func TestConfiguredCapabilitiesDatabaseSourcesRemainSupportedInSharedEnvironments(t *testing.T) {
	// database 模式仍保持原有路径；tools.yaml 恒定加载，故仍需可读目录。
	for _, environment := range []Environment{EnvironmentStaging, EnvironmentProduction} {
		t.Run(string(environment), func(t *testing.T) {
			cfg := HarnessConfig{
				Environment: environment,
				Components: HarnessComponentPaths{
					Tools:  "../../testdata/local/catalogs/tools.yaml",
					Skills: HarnessSourcedComponent{Source: ComponentSourceDatabase},
					MCP:    HarnessSourcedComponent{Source: ComponentSourceDatabase},
				},
			}
			if _, err := installConfiguredCapabilities(context.Background(), cfg, storage.Stores{}, nil, observability.NoopLogger{}, nil); err != nil {
				t.Fatalf("database-sourced capability install error = %v, want gate bypass", err)
			}
		})
	}
}

func TestConfiguredHTTPToolBuildsAndExecutes(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer daily-token" {
			t.Errorf("request method=%s authorization=%q", r.Method, r.Header.Get("Authorization"))
		}
		if r.URL.Query().Get("query") != "hello" || r.Body != http.NoBody {
			t.Errorf("GET query=%q body=%T", r.URL.RawQuery, r.Body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	t.Setenv("DAILY_TOOL_AUTH", "Bearer daily-token")

	catalog := toolCatalogConfig{
		Enabled: []string{"daily.http@1.0.0"},
		Definitions: []configuredTool{{
			Name: "daily.http", Version: "1.0.0", Type: string(toolgateway.ToolTypeHTTP),
			Description: "daily HTTP tool", RiskLevel: string(toolgateway.RiskMedium),
			AllowedAgents: []string{"demo"}, TimeoutMS: 1000,
			InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
			HTTP: &configuredHTTPTool{
				Method: http.MethodGet, URL: upstream.URL, ResponseMode: "json",
				HeadersEnv: map[string]string{"Authorization": "DAILY_TOOL_AUTH"},
			},
		}},
	}
	definitions, _, err := buildConfiguredTools(catalog, nil, nil)
	if err != nil {
		t.Fatalf("buildConfiguredTools() error = %v", err)
	}
	if len(definitions) != 1 || definitions[0].HTTP == nil || definitions[0].RiskLevel != toolgateway.RiskMedium {
		t.Fatalf("configured definition = %#v", definitions)
	}
	stores := storagememory.New().Stores()
	gateway := toolgateway.NewGateway(toolgateway.GatewayConfig{
		Registry:   toolgateway.NewStaticRegistry(definitions),
		EventStore: toolGatewayEventStore{events: stores.Events},
		StepStore:  toolGatewayStepStore{steps: stores.Steps},
		Executors:  []toolgateway.ToolExecutor{toolgateway.NewHTTPExecutor(upstream.Client())},
	})
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{
		TraceID: "trace-http", TenantID: "tenant", UserID: "user", SessionID: "session", RunID: "run", AgentID: "demo",
	})
	result, err := gateway.Invoke(ctx, toolgateway.ToolCallRequest{
		ToolCallID: "call-http", TenantID: "tenant", UserID: "user", SessionID: "session", RunID: "run", StepID: "step-http", AgentID: "demo",
		ToolName: "daily.http", ToolVersion: "1.0.0", Arguments: json.RawMessage(`{"query":"hello"}`),
		Caller: toolgateway.ToolCaller{Type: "agent", AgentID: "demo"},
		Policy: toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskMedium, IdempotencyKey: "run:call-http"},
	})
	if err != nil {
		t.Fatalf("ToolGateway.Invoke() error = %v", err)
	}
	if calls != 1 || result.Status != toolgateway.ToolCallSucceeded || string(result.ModelContextResult) != `{"ok":true}` {
		t.Fatalf("calls=%d result=%#v", calls, result)
	}
}

func TestConfiguredHTTPWriteRequiresControlBeforeNetwork(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer upstream.Close()

	definitions, _, err := buildConfiguredTools(toolCatalogConfig{
		Enabled: []string{"daily.write@1.0.0"},
		Definitions: []configuredTool{{
			Name: "daily.write", Version: "1.0.0", Type: string(toolgateway.ToolTypeHTTP),
			RiskLevel: string(toolgateway.RiskHigh), AllowedAgents: []string{"demo"}, TimeoutMS: 1000,
			InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
			HTTP: &configuredHTTPTool{Method: http.MethodPost, URL: upstream.URL, ResponseMode: "json", Write: true},
		}},
	}, nil, nil)
	if err != nil {
		t.Fatalf("buildConfiguredTools() error = %v", err)
	}
	stores := storagememory.New().Stores()
	gateway := toolgateway.NewGateway(toolgateway.GatewayConfig{
		Registry:   toolgateway.NewStaticRegistry(definitions),
		EventStore: toolGatewayEventStore{events: stores.Events},
		StepStore:  toolGatewayStepStore{steps: stores.Steps},
		Executors:  []toolgateway.ToolExecutor{toolgateway.NewHTTPExecutor(upstream.Client())},
	})
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{
		TraceID: "trace-write", TenantID: "tenant", UserID: "user", SessionID: "session", RunID: "run", AgentID: "demo",
	})
	result, err := gateway.Invoke(ctx, toolgateway.ToolCallRequest{
		ToolCallID: "call-write", TenantID: "tenant", UserID: "user", SessionID: "session", RunID: "run", StepID: "step-write", AgentID: "demo",
		ToolName: "daily.write", ToolVersion: "1.0.0", Arguments: json.RawMessage(`{"value":"hello"}`),
		Caller: toolgateway.ToolCaller{Type: "agent", AgentID: "demo"},
		Policy: toolgateway.ToolCallPolicy{
			RiskLevel: toolgateway.RiskHigh, RequireApproval: true, IdempotencyKey: "run:call-write",
		},
	})
	if err != nil {
		t.Fatalf("ToolGateway.Invoke() error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("high-risk HTTP write reached upstream %d times without control approval", calls)
	}
	if result == nil || result.Status != toolgateway.ToolCallFailed || result.Failure == nil ||
		result.Failure.ErrorType != string(toolgateway.ErrorTypeControlRequired) || result.Failure.Executed {
		t.Fatalf("high-risk write result = %#v, want unexecuted control_required", result)
	}
}

func TestManagedMCPRuntimeInvocationUsesDynamicGatewayMirror(t *testing.T) {
	mcpRegistry, err := mcp.NewInMemoryRegistry(mcp.ServerDefinition{
		ID: "managed", Version: "v1", Scope: mcp.ScopeTenant, TenantID: "tenant",
	})
	if err != nil {
		t.Fatal(err)
	}
	mcpService, err := mcp.NewService(
		mcpRegistry,
		mcp.StaticClientProvider{Value: configuredMCPClient{tools: []mcp.Tool{{
			Name:        "harness.mcp_echo",
			Description: "echo through a managed MCP server",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`),
		}}}},
		mcp.NewInMemorySnapshotStore(),
		func() string { return "mcp_snapshot_1" },
	)
	if err != nil {
		t.Fatal(err)
	}
	principal := mcp.Principal{TenantID: "tenant", UserID: "user", AgentID: "agent"}
	snapshot, err := mcpService.ResolveSnapshot(context.Background(), principal, "managed")
	if err != nil {
		t.Fatal(err)
	}
	registry := governedCapabilityToolRegistry{base: toolgateway.NewStaticRegistry(nil)}
	stores := storagememory.New().Stores()
	gateway := toolgateway.NewGateway(toolgateway.GatewayConfig{
		Registry:                   registry,
		EventStore:                 toolGatewayEventStore{events: stores.Events},
		StepStore:                  toolGatewayStepStore{steps: stores.Steps},
		RequireFrozenSourceBinding: true,
		Executors:                  []toolgateway.ToolExecutor{toolgateway.NewMCPExecutor(configuredMCPGatewayBridge{service: mcpService})},
	})
	invoker, err := runtimeadapter.New(gateway, runtimeadapter.RegistryInvocationResolver{
		Registry: registry,
		MCP:      configuredMCPInvocationResolver(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	pkg := agentruntime.ModelContextPackage{
		PackageID:   "package_1",
		ContextHash: "sha256:placeholder",
		Run: agentruntime.ModelContextRun{
			SessionID: "session", RunID: "run", AgentBindingID: "binding",
			AgentID: "agent", ConfigSnapshotRef: "agent-config://agent/v1/hash", ConfigHash: "sha256:config",
		},
		Security: agentruntime.ModelContextSecurity{TenantID: "tenant", UserID: "user"},
		Capabilities: agentruntime.ModelContextCapabilities{
			MCPServers:   []string{"managed"},
			MCPSnapshots: []mcp.CapabilitySnapshot{snapshot},
		},
	}
	pkg.ContextHash = agentruntime.ComputeModelContextHash(pkg)
	ctx := agentruntime.WithModelContextPackage(context.Background(), pkg)
	ctx = agentruntime.WithRuntimeParentStepID(ctx, "runtime_step")
	ctx = observability.WithTraceContext(ctx, observability.TraceContext{
		TraceID: "trace-mcp", TenantID: "tenant", UserID: "user", SessionID: "session", RunID: "run", AgentID: "agent",
	})

	result, err := invoker.Invoke(ctx, agentruntime.ToolInvocationRequest{
		SessionID: "session", RunID: "run", AgentID: "agent", ToolCallID: "call_mcp",
		ToolName: "harness.mcp_echo", Source: agentruntime.ToolSourceMCP,
		SourceRef: "managed", SnapshotRef: snapshot.ID, Arguments: json.RawMessage(`{"value":"ok"}`),
	}, appToolEventSink{})
	if err != nil {
		t.Fatalf("invoke managed MCP: %v", err)
	}
	if result.IsError || result.Content != `{"value":"ok"}` {
		t.Fatalf("managed MCP result = %#v", result)
	}
}

func TestConfiguredMCPOAuthPolicyValidation(t *testing.T) {
	_, err := buildConfiguredMCP(mcpCatalogConfig{
		Enabled: []string{"source_control"},
		Definitions: []configuredMCP{{
			ID: "source_control", Version: "v1", Scope: string(mcp.ScopeTenant), TenantID: "tenant",
			Transport: "streamable-http", URL: "https://mcp.example.test/source-control", ProtocolVersion: "2025-06-18",
			Auth: configuredMCPAuth{Type: mcp.AuthTypeOAuth2, Provider: "oauth_provider", Scopes: []string{"source:read"}, Resource: "https://mcp.example.test/source-control"},
			Tools: []configuredMCPTool{{
				Name: "code_search", Description: "search code", InputSchema: map[string]any{"type": "object"},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("valid oauth mcp config rejected: %v", err)
	}
	_, err = buildConfiguredMCP(mcpCatalogConfig{
		Enabled: []string{"source_control"},
		Definitions: []configuredMCP{{
			ID: "source_control", Version: "v1", Scope: string(mcp.ScopeTenant), TenantID: "tenant",
			Transport: "streamable-http", URL: "https://mcp.example.test/source-control", ProtocolVersion: "2025-06-18",
			Auth: configuredMCPAuth{Type: mcp.AuthTypeOAuth2},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "oauth2 mcp auth requires provider") {
		t.Fatalf("invalid oauth mcp config error = %v", err)
	}
}

func TestConfiguredMCPOAuthRequiresGrantBeforeSnapshot(t *testing.T) {
	service, err := buildConfiguredMCP(mcpCatalogConfig{
		Enabled: []string{"source_control"},
		Definitions: []configuredMCP{{
			ID: "source_control", Version: "v1", Scope: string(mcp.ScopeTenant), TenantID: "tenant",
			Transport: "streamable-http", URL: "https://mcp.example.test/source-control", ProtocolVersion: "2025-06-18",
			Auth: configuredMCPAuth{Type: mcp.AuthTypeOAuth2, Provider: "oauth_provider", Scopes: []string{"source:read"}},
			Tools: []configuredMCPTool{{
				Name: "code_search", Description: "search code", InputSchema: map[string]any{"type": "object"},
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.ResolveSnapshot(context.Background(), mcp.Principal{TenantID: "tenant", UserID: "alice", AgentID: "agent"}, "source_control")
	if !errors.Is(err, mcp.ErrOAuthGrantMissing) {
		t.Fatalf("ResolveSnapshot error = %v, want oauth grant missing", err)
	}
}

func TestConfiguredMCPOAuthCompositionWiresDurableAuthorizationManager(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mcp-oauth-composition.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, apply := range []func(context.Context, *sql.DB) error{mcp.ApplySQLiteSchema, mcp.ApplySQLiteOAuthSchema, mcp.ApplySQLiteOAuthAuthorizationSchema, mcp.ApplySQLiteOAuthRefreshSchema} {
		if err := apply(context.Background(), db); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HARNESS_MCP_OAUTH_TEST_KEY", "composition-test-secret")
	cfg := HarnessConfig{
		Environment: EnvironmentLocal,
		Components: HarnessComponentPaths{
			Tools:  "../../testdata/local/catalogs/tools.yaml",
			Skills: HarnessSourcedComponent{Source: ComponentSourceFile, Path: "../../testdata/local/catalogs/skills.yaml"},
			MCP:    HarnessSourcedComponent{Source: ComponentSourceFile, Path: "../../testdata/local/catalogs/mcp.yaml"},
		},
		MCPOAuth: HarnessMCPOAuthConfig{
			Enabled: true, PublicBaseURL: "http://127.0.0.1:18083", TokenKeyEnv: "HARNESS_MCP_OAUTH_TEST_KEY", ClientName: "Harness Test", PendingTTLSeconds: 600,
		},
	}
	installed, err := installConfiguredCapabilities(context.Background(), cfg, storagememory.New().Stores(), nil, observability.NoopLogger{}, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	if installed.MCPOAuth == nil || installed.ManagedMCP == nil || installed.MCP == nil {
		t.Fatalf("oauth composition is incomplete: %#v", installed)
	}

	cfg.MCPOAuth.TokenKeyEnv = "HARNESS_MCP_OAUTH_MISSING_KEY"
	if _, err := installConfiguredCapabilities(context.Background(), cfg, storagememory.New().Stores(), nil, observability.NoopLogger{}, nil, db); err == nil || !strings.Contains(err.Error(), "HARNESS_MCP_OAUTH_MISSING_KEY") {
		t.Fatalf("missing OAuth key error = %v", err)
	}
}

func TestConfiguredToolValidationFailsClosed(t *testing.T) {
	base := configuredTool{
		Name: "tool", Version: "v1", Type: string(toolgateway.ToolTypeHTTP), TimeoutMS: 1000,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		HTTP: &configuredHTTPTool{Method: http.MethodGet, URL: "https://tools.example.test/invoke", ResponseMode: "json"},
	}
	tests := map[string]func(*configuredTool){
		"missing timeout":       func(item *configuredTool) { item.TimeoutMS = 0 },
		"invalid scheme":        func(item *configuredTool) { item.HTTP.URL = "file:///tmp/tool" },
		"invalid response mode": func(item *configuredTool) { item.HTTP.ResponseMode = "yaml" },
		"invalid risk":          func(item *configuredTool) { item.RiskLevel = "critical" },
		"write below high risk": func(item *configuredTool) {
			item.HTTP.Method = http.MethodPost
			item.HTTP.Write = true
			item.RiskLevel = string(toolgateway.RiskMedium)
		},
		"literal secret": func(item *configuredTool) {
			item.HTTP.Headers = map[string]string{"Authorization": "secret"}
		},
		"duplicate header": func(item *configuredTool) {
			item.HTTP.Headers = map[string]string{"X-Mode": "a", "x-mode": "b"}
		},
		"missing secret env": func(item *configuredTool) {
			item.HTTP.HeadersEnv = map[string]string{"Authorization": "MISSING_DAILY_TOOL_SECRET"}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			item := base
			httpSpec := *base.HTTP
			item.HTTP = &httpSpec
			mutate(&item)
			_, _, err := buildConfiguredTools(toolCatalogConfig{Enabled: []string{"tool@v1"}, Definitions: []configuredTool{item}}, nil, nil)
			if err == nil {
				t.Fatal("invalid configured tool was accepted")
			}
		})
	}

	query := base
	query.HTTP = &configuredHTTPTool{
		Method: http.MethodPost, URL: "https://tools.example.test/search", ResponseMode: "json",
	}
	if _, _, err := buildConfiguredTools(toolCatalogConfig{
		Enabled: []string{"tool@v1"}, Definitions: []configuredTool{query},
	}, nil, nil); err != nil {
		t.Fatalf("low-risk POST query tool was rejected: %v", err)
	}

	function := base
	function.Type, function.Handler, function.HTTP = string(toolgateway.ToolTypeFunction), "missing.handler", nil
	if _, _, err := buildConfiguredTools(toolCatalogConfig{Enabled: []string{"tool@v1"}, Definitions: []configuredTool{function}}, nil, nil); err == nil {
		t.Fatal("unknown function handler was accepted")
	}
	if _, _, err := buildConfiguredTools(toolCatalogConfig{
		Enabled: []string{"tool@v1"}, Definitions: []configuredTool{base, base},
	}, nil, nil); err == nil {
		t.Fatal("duplicate configured tool definition was accepted")
	}
}

type appToolEventSink struct{}

func (appToolEventSink) Emit(context.Context, observability.AgentEvent) error {
	return nil
}
