package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/httptooldef"
	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/skill"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	"github.com/AGenUI/agenui-studio/harness/internal/toolconfig"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway/runtimeadapter"
)

type toolCatalogConfig struct {
	SchemaVersion string           `yaml:"schema_version"`
	Enabled       []string         `yaml:"enabled"`
	Definitions   []configuredTool `yaml:"definitions"`
}

type configuredTool struct {
	Name                   string              `yaml:"name"`
	DisplayName            string              `yaml:"display_name"`
	Version                string              `yaml:"version"`
	Type                   string              `yaml:"type"`
	Description            string              `yaml:"description"`
	RiskLevel              string              `yaml:"risk_level"`
	AllowedAgents          []string            `yaml:"allowed_agents"`
	Handler                string              `yaml:"handler"`
	ServerID               string              `yaml:"server_id"`
	MCPToolName            string              `yaml:"mcp_tool_name"`
	InputSchema            map[string]any      `yaml:"input_schema"`
	OutputSchema           map[string]any      `yaml:"output_schema"`
	ConfigSchema           map[string]any      `yaml:"config_schema"`
	TimeoutMS              int                 `yaml:"timeout_ms"`
	MaxInlineBytes         int                 `yaml:"max_inline_bytes"`
	MaxModelContextBytes   int                 `yaml:"max_model_context_bytes"`
	MaxSSEPreviewBytes     int                 `yaml:"max_sse_preview_bytes"`
	ArtifactThresholdBytes int                 `yaml:"artifact_threshold_bytes"`
	HTTP                   *configuredHTTPTool `yaml:"http"`
}

type configuredHTTPTool struct {
	Method       string            `yaml:"method"`
	URL          string            `yaml:"url"`
	ResponseMode string            `yaml:"response_mode"`
	Write        bool              `yaml:"write"`
	Headers      map[string]string `yaml:"headers"`
	HeadersEnv   map[string]string `yaml:"headers_env"`
}

type skillCatalogConfig struct {
	SchemaVersion string            `yaml:"schema_version"`
	Enabled       []string          `yaml:"enabled"`
	Definitions   []configuredSkill `yaml:"definitions"`
}

type configuredSkill struct {
	ID                string   `yaml:"id"`
	Version           string   `yaml:"version"`
	Description       string   `yaml:"description"`
	InjectionStrategy string   `yaml:"injection_strategy"`
	Scope             string   `yaml:"scope"`
	AllowedAgents     []string `yaml:"allowed_agents"`
	MaxInputTokens    int      `yaml:"max_input_tokens"`
	Content           string   `yaml:"content"`
}

type mcpCatalogConfig struct {
	SchemaVersion string          `yaml:"schema_version"`
	Enabled       []string        `yaml:"enabled"`
	Definitions   []configuredMCP `yaml:"definitions"`
}

type configuredMCP struct {
	ID                 string              `yaml:"id"`
	Version            string              `yaml:"version"`
	Scope              string              `yaml:"scope"`
	Type               string              `yaml:"type"`
	Transport          string              `yaml:"transport"`
	URL                string              `yaml:"url"`
	ProtocolVersion    string              `yaml:"protocol_version"`
	Headers            map[string]string   `yaml:"headers"`
	TenantID           string              `yaml:"tenant_id"`
	UserID             string              `yaml:"user_id"`
	AllowedAgents      []string            `yaml:"allowed_agents"`
	BlockedAgents      []string            `yaml:"blocked_agents"`
	HITLTools          []string            `yaml:"hitl_tools"`
	MaxConcurrentCalls int                 `yaml:"max_concurrent_calls"`
	MaxResultBytes     int64               `yaml:"max_result_bytes"`
	Auth               configuredMCPAuth   `yaml:"auth"`
	Tools              []configuredMCPTool `yaml:"tools"`
}

type configuredMCPAuth struct {
	Type     string   `yaml:"type"`
	Provider string   `yaml:"provider"`
	Scopes   []string `yaml:"scopes"`
	Resource string   `yaml:"resource"`
}

type configuredMCPTool struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	InputSchema map[string]any `yaml:"input_schema"`
}

type installedCapabilities struct {
	Snapshots agentruntime.GovernedCapabilitySnapshotProvider
	Tools     agentruntime.ToolInvoker
	Rebuilder agentruntime.ModelContextRebuilder
	Registry  toolgateway.ToolRegistry
	MCP       *mcp.Service
	Skills    *skill.Service
	// AdminSkills 是技能管理面服务：skills=file 时为 DB-only overlay（写接口
	// 落库但不影响运行时，ADR-0005）；database 模式下与 Skills 相同。
	AdminSkills *skill.Service
	ManagedMCP  *mcp.SQLManagedRegistry
	MCPOAuth    *mcp.OAuthAuthorizationManager
	ToolConfig  *toolconfig.SQLManagedRegistry
	HTTPTools   *httptooldef.SQLManagedRegistry
	// BoundToolHandlers 是被至少一个 tools.yaml function 工具引用的 handler
	// 名集合（含内置与扩展实现），供 BuildReport 标注 unbound 实现。
	BoundToolHandlers map[string]bool
}

func installConfiguredCapabilities(ctx context.Context, cfg HarnessConfig, stores storage.Stores, artifacts *artifact.Store, logger observability.StructuredLogger, tracer observability.TraceProvider, managedDB ...*sql.DB) (installedCapabilities, error) {
	return installConfiguredCapabilitiesWithExtensions(ctx, cfg, stores, artifacts, logger, tracer, nil, managedDB...)
}

// installConfiguredCapabilitiesWithExtensions 在配置目录之外额外接受 SDK
// 扩展目录：tool_provider 条目会被投影为 Tool Gateway 的 function 工具
// （R2b），与配置工具共用同一个 registry / executor / 事件链路。
func installConfiguredCapabilitiesWithExtensions(ctx context.Context, cfg HarnessConfig, stores storage.Stores, artifacts *artifact.Store, logger observability.StructuredLogger, tracer observability.TraceProvider, extensions *kernel.ExtensionCatalog, managedDB ...*sql.DB) (installedCapabilities, error) {
	skillsFromFile := cfg.Components.Skills.FromFile()
	mcpFromFile := cfg.Components.MCP.FromFile()
	// 双源组件的 file|database 选择不受 environment 限制。tools.yaml 恒定加载；
	// skills/mcp 仅在 file 模式下读本地 YAML，database 模式使用空目录，运行时
	// 数据只来自数据库（ADR-0001/0002/0006）。环境级持久化和安全约束仍由各
	// 基础设施装配点独立校验，source 选择不会放宽这些约束。
	tools := toolCatalogConfig{SchemaVersion: "harness.capability-catalog.v1"}
	if err := decodeStrictYAML(cfg.Components.Tools, &tools); err != nil {
		return installedCapabilities{}, err
	}
	skills := skillCatalogConfig{SchemaVersion: "harness.capability-catalog.v1"}
	if skillsFromFile {
		if err := decodeStrictYAML(cfg.Components.Skills.Path, &skills); err != nil {
			return installedCapabilities{}, err
		}
	}
	mcps := mcpCatalogConfig{SchemaVersion: "harness.capability-catalog.v1"}
	if mcpFromFile {
		if err := decodeStrictYAML(cfg.Components.MCP.Path, &mcps); err != nil {
			return installedCapabilities{}, err
		}
	}
	for name, version := range map[string]string{"tools": tools.SchemaVersion, "skills": skills.SchemaVersion, "mcp": mcps.SchemaVersion} {
		if version != "harness.capability-catalog.v1" {
			return installedCapabilities{}, fmt.Errorf("%s catalog has unsupported schema %q", name, version)
		}
	}

	// tool_configs（租户配置覆盖）与 http_tools（租户自建工具）与 tools.yaml
	// 分层互补，只要有 SQL 连接就全部接入运行时。
	var toolConfigStore *toolconfig.SQLManagedRegistry
	var httpToolStore *httptooldef.SQLManagedRegistry
	if len(managedDB) > 0 && managedDB[0] != nil {
		toolConfigStore = toolconfig.NewSQLManagedRegistry(managedDB[0])
		httpToolStore = httptooldef.NewSQLManagedRegistry(managedDB[0])
	}

	// SDK 注册的 ToolProvider（rc.6：纯实现提供者）先收集为 handler 表；
	// tools.yaml 的 function 工具经配置目录路径引用这些 handler（治理属性
	// 全部由 yaml 承载）。解析优先级：内置 → Options → 全集
	//（extension.RegisterTools）；三处都没有时 buildConfiguredTools fail-closed。
	capabilityTurnEnvironment := kernel.TurnEnvironment{
		Environment:    string(cfg.Environment),
		SDKVersion:     kernel.SDKContractVersion,
		SchemaVersions: kernel.CanonicalSchemaVersions(),
	}
	extensionHandlers, err := collectExtensionFunctionHandlers(extensions, capabilityTurnEnvironment)
	if err != nil {
		return installedCapabilities{}, err
	}
	extensionHandlers, err = mergeGlobalFunctionHandlers(extensionHandlers, capabilityTurnEnvironment)
	if err != nil {
		return installedCapabilities{}, err
	}
	definitions, handlers, err := buildConfiguredTools(tools, artifacts, extensionHandlers, toolConfigStore)
	if err != nil {
		return installedCapabilities{}, err
	}
	staticRegistry := toolgateway.NewStaticRegistry(definitions)
	registry := governedCapabilityToolRegistry{base: staticRegistry}

	mcpService, err := buildConfiguredMCP(mcps)
	if err != nil {
		return installedCapabilities{}, err
	}
	skillService, err := buildConfiguredSkills(ctx, skills)
	if err != nil {
		return installedCapabilities{}, err
	}
	// adminSkillService 是管理面视图：file 模式下仍指向 SQL 仓库（ADR-0005
	// 允许提前灌备数据），但不参与运行时（严格排他，ADR-0006）。
	adminSkillService := skillService
	var managedMCP *mcp.SQLManagedRegistry
	var mcpOAuth *mcp.OAuthAuthorizationManager
	if len(managedDB) > 0 && managedDB[0] != nil {
		managedMCP = mcp.NewSQLManagedRegistry(managedDB[0])
		var credentials mcp.CredentialProvider
		if cfg.MCPOAuth.Enabled {
			secret, ok := os.LookupEnv(cfg.MCPOAuth.TokenKeyEnv)
			if !ok || strings.TrimSpace(secret) == "" {
				return installedCapabilities{}, fmt.Errorf("mcp oauth token key environment variable %s is not set", cfg.MCPOAuth.TokenKeyEnv)
			}
			codec, codecErr := mcp.NewOAuthTokenCodec([]byte(secret))
			if codecErr != nil {
				return installedCapabilities{}, codecErr
			}
			grantStore := mcp.NewSQLOAuthGrantStore(managedDB[0], codec)
			pendingStore := mcp.NewSQLOAuthPendingStore(managedDB[0], codec)
			mcpOAuth, err = mcp.NewOAuthAuthorizationManager(mcp.OAuthAuthorizationManagerConfig{
				PublicBaseURL: cfg.MCPOAuth.PublicBaseURL, ClientName: cfg.MCPOAuth.ClientName,
				PendingTTL:                     time.Duration(cfg.MCPOAuth.PendingTTLSeconds) * time.Second,
				AllowInsecureHTTPPublicBaseURL: cfg.MCPOAuth.AllowInsecureHTTP,
				Grants:                         grantStore, Pending: pendingStore,
			})
			if err != nil {
				return installedCapabilities{}, err
			}
			credentials = mcp.OAuthCredentialProvider{Store: grantStore, Refresher: mcpOAuth}
		}
		// 托管 MCP overlay 只在 mcp=database 时叠加进运行时；file 模式运行时
		// 只见本地目录定义（ADR-0006 严格排他）。
		if !mcpFromFile {
			mcpService, err = mcp.NewManagedOverlayWithCredentials(mcpService, managedMCP, credentials)
			if err != nil {
				return installedCapabilities{}, err
			}
		}
		skillRepository := skill.NewSQLRepository(managedDB[0], newSkillPackageArtifactStore(artifacts))
		if skillsFromFile {
			// file 模式：运行时只用 YAML 技能；管理面单独构建一个 DB-only
			// overlay（空 base + SQL 仓库）承接写接口。
			emptyBase, emptyErr := buildConfiguredSkills(ctx, skillCatalogConfig{SchemaVersion: "harness.capability-catalog.v1"})
			if emptyErr != nil {
				return installedCapabilities{}, emptyErr
			}
			adminSkillService, err = skill.NewManagedOverlay(emptyBase, skillRepository)
			if err != nil {
				return installedCapabilities{}, err
			}
		} else {
			skillService, err = skill.NewManagedOverlay(skillService, skillRepository)
			if err != nil {
				return installedCapabilities{}, err
			}
			adminSkillService = skillService
		}
	}
	mcpBridge := configuredMCPGatewayBridge{service: mcpService}
	toolEvents := toolgateway.NewDefaultToolEventSink(toolgateway.ToolEventSinkConfig{
		EventStore: toolGatewayEventStore{events: stores.Events}, ArtifactStore: artifacts, Logger: logger,
	})
	gateway := toolgateway.NewGateway(toolgateway.GatewayConfig{
		Registry: registry, EventStore: toolGatewayEventStore{events: stores.Events}, StepStore: toolGatewayStepStore{steps: stores.Steps}, ArtifactStore: artifacts,
		Idempotency: toolgateway.NewMemoryIdempotencyStore(), Logger: logger, TraceProvider: tracer, ToolEventSink: toolEvents,
		RequireFrozenSourceBinding: true,
		Executors: []toolgateway.ToolExecutor{
			toolgateway.NewFunctionExecutor(handlers),
			toolgateway.NewHTTPExecutor(nil).WithConfigResolver(httpToolConfigResolver(toolConfigStore)),
			toolgateway.NewMCPExecutor(mcpBridge),
		},
	})
	invoker, err := runtimeadapter.New(gateway, runtimeadapter.RegistryInvocationResolver{
		Registry: registry,
		Policy: runtimeadapter.InvocationPolicyResolverFunc(func(_ context.Context, req agentruntime.ToolInvocationRequest, def toolgateway.ToolDefinition) (toolgateway.ToolCallPolicy, error) {
			return configuredInvocationPolicy(req, def), nil
		}),
		MCP: configuredMCPInvocationResolver(definitions),
	})
	if err != nil {
		return installedCapabilities{}, err
	}
	// 供 BuildReport 标注 unbound：收集被启用工具定义引用的 handler 名。
	boundHandlers := make(map[string]bool)
	for _, def := range definitions {
		if def.Function != nil && def.Function.HandlerName != "" {
			boundHandlers[def.Function.HandlerName] = true
		}
	}
	return installedCapabilities{
		Snapshots: agentruntime.GovernedCapabilitySnapshotProvider{
			MCP: mcpService, Skills: skillService, ToolSchemas: configuredToolSchemaProvider{registry: registry},
			HTTPTools: httpToolSnapshotProvider{store: httpToolStore},
		},
		Tools: invoker, Rebuilder: configuredContextRebuilder{}, Registry: registry,
		MCP: mcpService, Skills: skillService, AdminSkills: adminSkillService,
		ManagedMCP: managedMCP, MCPOAuth: mcpOAuth, ToolConfig: toolConfigStore, HTTPTools: httpToolStore,
		BoundToolHandlers: boundHandlers,
	}, nil
}

func buildConfiguredTools(catalog toolCatalogConfig, artifacts *artifact.Store, extensionHandlers map[string]toolgateway.FunctionTool, toolConfigs ...*toolconfig.SQLManagedRegistry) ([]toolgateway.ToolDefinition, map[string]toolgateway.FunctionTool, error) {
	enabled := stringSet(catalog.Enabled)
	definitions := make([]toolgateway.ToolDefinition, 0, len(enabled))
	handlers := configuredFunctionHandlers(artifacts, toolConfigs...)
	// 合并 SDK ToolProvider 提供的实现（rc.6）：与内置 handler 重名 fail-closed
	//（harness. 前缀禁令之外的兜底）。
	for name, handler := range extensionHandlers {
		if handlers[name] != nil {
			return nil, nil, fmt.Errorf("extension handler %s conflicts with a builtin handler", name)
		}
		handlers[name] = handler
	}
	seen := make(map[string]struct{}, len(catalog.Definitions))
	for _, item := range catalog.Definitions {
		ref := item.Name + "@" + item.Version
		if _, exists := seen[ref]; exists {
			return nil, nil, fmt.Errorf("duplicate configured tool definition %s", ref)
		}
		seen[ref] = struct{}{}
		if _, ok := enabled[ref]; !ok {
			continue
		}
		input, err := json.Marshal(item.InputSchema)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal tool %s input schema: %w", ref, err)
		}
		output, err := json.Marshal(item.OutputSchema)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal tool %s output schema: %w", ref, err)
		}
		var configSchema json.RawMessage
		if len(item.ConfigSchema) > 0 {
			configSchema, err = json.Marshal(item.ConfigSchema)
			if err != nil {
				return nil, nil, fmt.Errorf("marshal tool %s config schema: %w", ref, err)
			}
		}
		policy := toolgateway.DefaultToolOutputPolicy()
		if item.MaxInlineBytes > 0 {
			policy.MaxInlineBytes = item.MaxInlineBytes
		}
		if item.MaxModelContextBytes > 0 {
			policy.MaxModelContextBytes = item.MaxModelContextBytes
		}
		if item.MaxSSEPreviewBytes > 0 {
			policy.MaxSSEPreviewBytes = item.MaxSSEPreviewBytes
		}
		if item.ArtifactThresholdBytes > 0 {
			policy.ArtifactThresholdBytes = item.ArtifactThresholdBytes
		}
		risk, err := configuredRiskLevel(item.RiskLevel)
		if err != nil {
			return nil, nil, fmt.Errorf("configured tool %s: %w", ref, err)
		}
		def := toolgateway.ToolDefinition{
			Name: item.Name, Version: item.Version, Type: toolgateway.ToolType(item.Type),
			DisplayName: item.DisplayName, Description: item.Description, InputSchema: input, OutputSchema: output,
			ConfigSchema: configSchema,
			RiskLevel:    risk, Timeout: time.Duration(item.TimeoutMS) * time.Millisecond,
			Retry: toolgateway.RetryPolicy{MaxAttempts: 1, Idempotent: true}, ResultPolicy: policy,
			Permissions: toolgateway.ToolPermissions{AllowedAgents: append([]string(nil), item.AllowedAgents...), TenantScope: "tenant"},
			Visibility:  observability.VisibilityUserVisible,
		}
		switch def.Type {
		case toolgateway.ToolTypeFunction:
			if handlers[item.Handler] == nil {
				return nil, nil, fmt.Errorf("configured tool %s references unknown function handler %q", ref, item.Handler)
			}
			def.Function = &toolgateway.FunctionToolSpec{HandlerName: item.Handler}
		case toolgateway.ToolTypeHTTP:
			spec, err := configuredHTTPToolSpec(item, risk)
			if err != nil {
				return nil, nil, fmt.Errorf("configured tool %s: %w", ref, err)
			}
			def.HTTP = spec
		case toolgateway.ToolTypeMCP:
			// SnapshotID is frozen per Run and is supplied by the runtime adapter.
			def.MCP = &toolgateway.MCPToolSpec{ServerID: item.ServerID, SnapshotID: "runtime-bound", MCPToolName: item.MCPToolName}
		default:
			return nil, nil, fmt.Errorf("configured tool %s has unsupported type %q", ref, item.Type)
		}
		definitions = append(definitions, def)
		delete(enabled, ref)
	}
	if len(enabled) > 0 {
		return nil, nil, fmt.Errorf("enabled tool definitions missing: %v", sortedKeys(enabled))
	}
	registry := toolgateway.NewStaticRegistry(definitions)
	for _, definition := range definitions {
		if _, err := registry.Get(context.Background(), definition.Name, definition.Version); err != nil {
			return nil, nil, fmt.Errorf("validate configured tool %s@%s: %w", definition.Name, definition.Version, err)
		}
	}
	return definitions, handlers, nil
}

func configuredRiskLevel(value string) (toolgateway.RiskLevel, error) {
	if value == "" {
		return toolgateway.RiskLow, nil
	}
	risk := toolgateway.RiskLevel(value)
	switch risk {
	case toolgateway.RiskLow, toolgateway.RiskMedium, toolgateway.RiskHigh:
		return risk, nil
	default:
		return "", fmt.Errorf("unsupported risk_level %q", value)
	}
}

func configuredHTTPToolSpec(item configuredTool, risk toolgateway.RiskLevel) (*toolgateway.HTTPToolSpec, error) {
	if item.HTTP == nil {
		return nil, fmt.Errorf("http spec is required")
	}
	method := strings.ToUpper(strings.TrimSpace(item.HTTP.Method))
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return nil, fmt.Errorf("unsupported http method %q", item.HTTP.Method)
	}
	if item.HTTP.Write && risk != toolgateway.RiskHigh {
		return nil, fmt.Errorf("http write operation requires risk_level high and control approval")
	}
	if item.HTTP.Write && method == http.MethodGet {
		return nil, fmt.Errorf("http GET cannot be declared as a write operation")
	}
	if (method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete) && !item.HTTP.Write {
		return nil, fmt.Errorf("http method %s must be declared as a write operation", method)
	}
	endpoint, err := url.Parse(item.HTTP.URL)
	if err != nil || strings.TrimSpace(item.HTTP.URL) != item.HTTP.URL || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" {
		return nil, fmt.Errorf("http url must be an absolute http/https endpoint without credentials or fragment")
	}
	if item.TimeoutMS <= 0 {
		return nil, fmt.Errorf("timeout_ms must be greater than zero for http tools")
	}
	switch item.HTTP.ResponseMode {
	case "json", "text", "bytes":
	default:
		return nil, fmt.Errorf("http response_mode must be json, text, or bytes")
	}
	headers := make(map[string]string, len(item.HTTP.Headers)+len(item.HTTP.HeadersEnv))
	for name, value := range item.HTTP.Headers {
		if err := validateConfiguredHTTPHeader(name, value); err != nil {
			return nil, err
		}
		if sensitiveHTTPHeader(name) {
			return nil, fmt.Errorf("sensitive http header %q must use headers_env", name)
		}
		canonicalName := http.CanonicalHeaderKey(name)
		if _, duplicated := headers[canonicalName]; duplicated {
			return nil, fmt.Errorf("http header %q is declared more than once", name)
		}
		headers[canonicalName] = value
	}
	for name, envName := range item.HTTP.HeadersEnv {
		if err := validateConfiguredHTTPHeader(name, "placeholder"); err != nil {
			return nil, err
		}
		canonicalName := http.CanonicalHeaderKey(name)
		if _, duplicated := headers[canonicalName]; duplicated {
			return nil, fmt.Errorf("http header %q is declared in both headers and headers_env", name)
		}
		if strings.TrimSpace(envName) == "" {
			return nil, fmt.Errorf("http header %q environment variable name is required", name)
		}
		value, ok := os.LookupEnv(envName)
		if !ok {
			return nil, fmt.Errorf("http header %q environment variable %s is not set", name, envName)
		}
		if err := validateConfiguredHTTPHeader(name, value); err != nil {
			return nil, err
		}
		headers[canonicalName] = value
	}
	return &toolgateway.HTTPToolSpec{
		Method: method, URL: endpoint.String(), Headers: headers,
		Timeout: time.Duration(item.TimeoutMS) * time.Millisecond, ResponseMode: item.HTTP.ResponseMode,
		Write: item.HTTP.Write,
	}, nil
}

func validateConfiguredHTTPHeader(name, value string) error {
	if name == "" || strings.TrimSpace(name) != name {
		return fmt.Errorf("http header name is invalid")
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(character))) {
			return fmt.Errorf("http header name %q is invalid", name)
		}
	}
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("http header %q contains a newline", name)
	}
	return nil
}

func sensitiveHTTPHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "api-key":
		return true
	default:
		return false
	}
}

const maxWriteFileContentBytes = 1 << 20

func configuredFunctionHandlers(artifacts *artifact.Store, toolConfigs ...*toolconfig.SQLManagedRegistry) map[string]toolgateway.FunctionTool {
	_ = toolConfigs
	handlers := map[string]toolgateway.FunctionTool{
		"harness.ask_user":         askUserFunctionHandler(),
		"harness.write_file":       writeFileFunctionHandler(artifacts),
		"harness.read_file":        readFileFunctionHandler(artifacts),
		"harness.list_files":       listFilesFunctionHandler(artifacts),
		"harness.edit_file":        editFileFunctionHandler(artifacts),
		"harness.calculator":       calculatorFunctionHandler(),
		"harness.get_current_time": getCurrentTimeFunctionHandler(nil),
		"harness.json_extract":     jsonExtractFunctionHandler(),
		"harness.request_approval": requestApprovalFunctionHandler(),
	}
	if artifacts != nil {
		handlers["harness.read_artifact"] = newReadArtifactTool(artifacts)
	}
	return handlers
}

func askUserFunctionHandler() toolgateway.FunctionTool {
	return func(_ context.Context, req toolgateway.FunctionCall) (*toolgateway.FunctionResult, error) {
		var arguments struct {
			Questions []map[string]any `json:"questions"`
		}
		if err := json.Unmarshal(req.Arguments, &arguments); err != nil || len(arguments.Questions) == 0 {
			return nil, toolgateway.NewToolError(
				toolgateway.ErrorTypeSchemaValidationFailed,
				"ask_user arguments must match schema",
				false,
				err,
			)
		}
		if req.Resume == nil {
			return nil, &toolgateway.ToolInterruptedError{
				Info: map[string]any{
					"type":      "ask_user",
					"questions": arguments.Questions,
				},
				State: json.RawMessage(`{"schema_version":"harness.ask_user.state.v1"}`),
			}
		}
		if !req.Resume.WasInterrupted || !req.Resume.IsResumeTarget || len(req.Resume.Payload) == 0 || !json.Valid(req.Resume.Payload) {
			return nil, toolgateway.NewToolError(
				toolgateway.ErrorTypeSchemaValidationFailed,
				"ask_user resume payload is invalid",
				false,
				nil,
			)
		}
		response, err := normalizeAskUserResponse(arguments.Questions, req.Resume.Payload)
		if err != nil {
			return nil, toolgateway.NewToolError(
				toolgateway.ErrorTypeSchemaValidationFailed,
				"ask_user response does not match the requested questions",
				false,
				err,
			)
		}
		result, err := json.Marshal(map[string]any{"response": response})
		if err != nil {
			return nil, toolgateway.NewToolError(toolgateway.ErrorTypeInternal, "marshal ask_user result", false, err)
		}
		return &toolgateway.FunctionResult{Data: result, MimeType: "application/json"}, nil
	}
}

type askUserNormalizedAnswer struct {
	QuestionID string `json:"question_id"`
	Question   string `json:"question"`
	OptionID   string `json:"option_id,omitempty"`
	Answer     string `json:"answer"`
	Text       string `json:"text,omitempty"`
}

func normalizeAskUserResponse(questions []map[string]any, payload json.RawMessage) (map[string]any, error) {
	var raw struct {
		Answers []struct {
			QuestionIndex int    `json:"question_index"`
			QuestionID    string `json:"question_id"`
			Text          string `json:"text"`
			Selected      *struct {
				ID    string `json:"id"`
				Label string `json:"label"`
				Value any    `json:"value"`
			} `json:"selected_option"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil || len(raw.Answers) == 0 {
		if err == nil {
			err = errors.New("answers are required")
		}
		return nil, err
	}
	answers := make([]askUserNormalizedAnswer, 0, len(raw.Answers))
	for _, item := range raw.Answers {
		if item.QuestionIndex < 0 || item.QuestionIndex >= len(questions) {
			return nil, errors.New("question_index is out of range")
		}
		question := strings.TrimSpace(fmt.Sprint(questions[item.QuestionIndex]["question"]))
		questionID := strings.TrimSpace(item.QuestionID)
		if questionID == "" {
			questionID = fmt.Sprintf("q%d", item.QuestionIndex)
		}
		answer := strings.TrimSpace(item.Text)
		optionID := ""
		if item.Selected != nil {
			optionID = strings.TrimSpace(item.Selected.ID)
			answer = strings.TrimSpace(item.Selected.Label)
			if answer == "" && item.Selected.Value != nil {
				answer = strings.TrimSpace(fmt.Sprint(item.Selected.Value))
			}
		}
		if question == "" || answer == "" {
			return nil, errors.New("question and answer are required")
		}
		answers = append(answers, askUserNormalizedAnswer{
			QuestionID: questionID,
			Question:   question,
			OptionID:   optionID,
			Answer:     answer,
			Text:       strings.TrimSpace(item.Text),
		})
	}
	return map[string]any{"answers": answers}, nil
}

func writeFileFunctionHandler(artifacts *artifact.Store) toolgateway.FunctionTool {
	return func(ctx context.Context, req toolgateway.FunctionCall) (*toolgateway.FunctionResult, error) {
		if artifacts == nil {
			return nil, toolgateway.NewToolError(toolgateway.ErrorTypeArtifactError, "artifact store is required for write_file", false, nil)
		}
		var args struct {
			Path     string `json:"path"`
			Content  string `json:"content"`
			MimeType string `json:"mime_type"`
		}
		if err := json.Unmarshal(req.Arguments, &args); err != nil {
			return nil, toolgateway.NewToolError(toolgateway.ErrorTypeSchemaValidationFailed, "write_file arguments must match schema", false, err)
		}
		cleanPath, err := validateWriteFilePath(args.Path)
		if err != nil {
			return nil, err
		}
		if len(args.Content) > maxWriteFileContentBytes {
			return nil, toolgateway.NewToolError(toolgateway.ErrorTypeSchemaValidationFailed, "write_file content exceeds 1MiB", false, nil)
		}
		mimeType := strings.TrimSpace(args.MimeType)
		if mimeType == "" {
			mimeType = "text/markdown; charset=utf-8"
		}
		actor, ok := artifact.ActorFromContext(ctx)
		if !ok {
			return nil, toolgateway.NewToolError(toolgateway.ErrorTypePermissionDenied, "artifact actor scope is required for write_file", false, nil)
		}
		meta, err := artifacts.Put(ctx, artifact.PutArtifactRequest{
			TenantID:        actor.TenantID,
			UserID:          actor.UserID,
			SessionID:       actor.SessionID,
			RunID:           actor.RunID,
			StepID:          req.ToolCallID,
			OwnerModule:     artifact.OwnerModuleToolGateway,
			OwnerID:         req.ToolCallID,
			ArtifactType:    artifact.ArtifactTypeFile,
			MimeType:        mimeType,
			Name:            cleanPath,
			Visibility:      artifact.VisibilityUserVisible,
			Content:         bytes.NewReader([]byte(args.Content)),
			PreviewHint:     artifact.PreviewHint{MaxBytes: 2048},
			RetentionPolicy: artifact.RetentionSessionTTL,
			IdempotencyKey:  "write_file:" + req.ToolCallID + ":" + cleanPath,
			Metadata: map[string]string{
				"requested_path": cleanPath,
				"tool":           "write_file",
			},
		})
		if err != nil {
			return nil, toolgateway.NewToolError(toolgateway.ErrorTypeArtifactError, "failed to persist write_file artifact", false, err)
		}
		if req.ToolContext != nil {
			payload, _ := json.Marshal(map[string]any{"path": cleanPath})
			if err := req.ToolContext.EmitArtifact(ctx, toolgateway.ToolArtifactEvent{
				ArtifactRef: meta.ArtifactRef,
				MimeType:    meta.MimeType,
				SizeBytes:   meta.SizeBytes,
				Hash:        meta.Hash,
				Preview:     payload,
			}); err != nil {
				return nil, err
			}
		}
		data, err := json.Marshal(map[string]any{
			"path":         cleanPath,
			"artifact_ref": meta.ArtifactRef,
			"mime_type":    meta.MimeType,
			"size_bytes":   meta.SizeBytes,
		})
		if err != nil {
			return nil, toolgateway.NewToolError(toolgateway.ErrorTypeInternal, "failed to encode write_file result", false, err)
		}
		return &toolgateway.FunctionResult{Data: data, MimeType: "application/json"}, nil
	}
}

func validateWriteFilePath(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", toolgateway.NewToolError(toolgateway.ErrorTypeSchemaValidationFailed, "write_file path is required", false, nil)
	}
	if len(trimmed) > 240 {
		return "", toolgateway.NewToolError(toolgateway.ErrorTypeSchemaValidationFailed, "write_file path exceeds 240 bytes", false, nil)
	}
	if strings.HasPrefix(trimmed, "/") || strings.Contains(trimmed, "\\") || strings.Contains(trimmed, ":") {
		return "", toolgateway.NewToolError(toolgateway.ErrorTypePermissionDenied, "write_file path must be a relative artifact path", false, nil)
	}
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == ".." {
			return "", toolgateway.NewToolError(toolgateway.ErrorTypePermissionDenied, "write_file path must not contain parent traversal", false, nil)
		}
	}
	cleaned := path.Clean(trimmed)
	if cleaned == "." || strings.HasPrefix(cleaned, "../") {
		return "", toolgateway.NewToolError(toolgateway.ErrorTypePermissionDenied, "write_file path must stay within the run artifact scope", false, nil)
	}
	return cleaned, nil
}

func buildConfiguredSkills(ctx context.Context, catalog skillCatalogConfig) (*skill.Service, error) {
	service, err := skill.NewService(skill.NewInMemoryRepository(), nil, observability.NewULIDGenerator("skill").NewRequestID)
	if err != nil {
		return nil, err
	}
	enabled := stringSet(catalog.Enabled)
	for _, item := range catalog.Definitions {
		ref := item.ID + "@" + item.Version
		if _, ok := enabled[ref]; !ok {
			continue
		}
		definition := skill.Definition{
			ID: item.ID, Version: item.Version, Description: item.Description,
			InjectionStrategy: skill.InjectionStrategy(item.InjectionStrategy),
			Policy:            skill.Policy{Scope: skill.Scope(item.Scope), AllowedAgents: append([]string(nil), item.AllowedAgents...), MaxInputTokens: item.MaxInputTokens},
		}
		if _, _, err := service.Publish(ctx, skill.Principal{System: true}, definition, []byte(item.Content)); err != nil {
			return nil, fmt.Errorf("publish configured skill %s: %w", ref, err)
		}
		delete(enabled, ref)
	}
	if len(enabled) > 0 {
		return nil, fmt.Errorf("enabled skill definitions missing: %v", sortedKeys(enabled))
	}
	return service, nil
}

func buildConfiguredMCP(catalog mcpCatalogConfig) (*mcp.Service, error) {
	enabled := stringSet(catalog.Enabled)
	definitions := make([]mcp.ServerDefinition, 0, len(enabled))
	clients := make(map[string]mcp.Client)
	for _, item := range catalog.Definitions {
		if _, ok := enabled[item.ID]; !ok {
			continue
		}
		transport := firstNonEmptyString(item.Transport, item.Type)
		definitions = append(definitions, mcp.ServerDefinition{
			ID: item.ID, Version: item.Version, Scope: mcp.Scope(item.Scope), TenantID: item.TenantID, UserID: item.UserID,
			AllowedAgents: append([]string(nil), item.AllowedAgents...), BlockedAgents: append([]string(nil), item.BlockedAgents...),
			HITLTools: append([]string(nil), item.HITLTools...), MaxConcurrentCalls: item.MaxConcurrentCalls, MaxResultBytes: item.MaxResultBytes,
			Transport: transport, Endpoint: item.URL, ProtocolVersion: item.ProtocolVersion,
			Auth: mcp.AuthConfig{
				Type: item.Auth.Type, Provider: item.Auth.Provider, Scopes: append([]string(nil), item.Auth.Scopes...), Resource: item.Auth.Resource,
			},
		})
		tools := make([]mcp.Tool, 0, len(item.Tools))
		for _, tool := range item.Tools {
			schema, err := json.Marshal(tool.InputSchema)
			if err != nil {
				return nil, err
			}
			tools = append(tools, mcp.Tool{Name: tool.Name, Description: tool.Description, InputSchema: schema})
		}
		switch transport {
		case "in_process_fixture":
			clients[item.ID] = configuredMCPClient{tools: tools}
		case "streamable-http":
			client, err := mcp.NewStreamableHTTPClient(item.URL,
				mcp.WithStreamableHTTPHeaders(item.Headers),
				mcp.WithStreamableHTTPDeclaredTools(tools),
				mcp.WithStreamableHTTPProtocolVersion(item.ProtocolVersion),
			)
			if err != nil {
				return nil, fmt.Errorf("build streamable-http mcp server %s: %w", item.ID, err)
			}
			clients[item.ID] = client
		default:
			return nil, fmt.Errorf("mcp server %s has unsupported local transport %q", item.ID, transport)
		}
		delete(enabled, item.ID)
	}
	if len(enabled) > 0 {
		return nil, fmt.Errorf("enabled mcp definitions missing: %v", sortedKeys(enabled))
	}
	registry, err := mcp.NewInMemoryRegistry(definitions...)
	if err != nil {
		return nil, err
	}
	return mcp.NewService(registry, configuredMCPClientProvider{clients: clients}, mcp.NewInMemorySnapshotStore(), observability.NewULIDGenerator("mcp").NewRequestID)
}

type configuredMCPClientProvider struct {
	clients     map[string]mcp.Client
	credentials mcp.CredentialProvider
}

func (p configuredMCPClientProvider) Client(ctx context.Context, definition mcp.ServerDefinition, principal mcp.Principal) (mcp.Client, error) {
	if definition.Auth.Type == mcp.AuthTypeOAuth2 {
		return (mcp.ManagedClientProvider{Credentials: p.credentials}).Client(ctx, definition, principal)
	}
	client := p.clients[definition.ID]
	if client == nil {
		return nil, fmt.Errorf("mcp client missing for server %s", definition.ID)
	}
	return client, nil
}

type configuredMCPClient struct{ tools []mcp.Tool }

func (c configuredMCPClient) ListTools(context.Context) ([]mcp.Tool, error) {
	return append([]mcp.Tool(nil), c.tools...), nil
}
func (configuredMCPClient) CallTool(_ context.Context, name string, arguments json.RawMessage, _ mcp.CallOptions) (mcp.ToolResult, error) {
	if name != "harness.mcp_echo" {
		return mcp.ToolResult{}, mcp.ErrToolNotAllowed
	}
	return mcp.ToolResult{Content: append([]byte(nil), arguments...)}, nil
}

type configuredMCPGatewayBridge struct{ service *mcp.Service }

func (b configuredMCPGatewayBridge) ResolveSnapshot(ctx context.Context, req toolgateway.ResolveMCPSnapshotRequest) (*toolgateway.MCPCapabilitySnapshot, error) {
	if len(req.ServerIDs) != 1 {
		return nil, fmt.Errorf("one MCP server is required")
	}
	snapshot, err := b.service.ResolveSnapshot(ctx, mcp.Principal{TenantID: req.TenantID, AgentID: req.AgentID}, req.ServerIDs[0])
	if err != nil {
		return nil, err
	}
	return &toolgateway.MCPCapabilitySnapshot{SnapshotID: snapshot.ID, ServerID: snapshot.ServerID, CapabilityHash: snapshot.CapabilityHash, PolicyHash: snapshot.PolicyHash, CreatedAt: snapshot.CreatedAt}, nil
}
func (b configuredMCPGatewayBridge) CallTool(ctx context.Context, req toolgateway.MCPToolCallRequest) (*toolgateway.MCPToolResult, error) {
	result, err := b.service.CallTool(ctx, mcp.ToolCallRequest{
		Principal: mcp.Principal{TenantID: req.TenantID, UserID: req.UserID, AgentID: req.AgentID},
		ServerID:  req.ServerID, SnapshotID: req.SnapshotID, ToolName: req.ToolName, ToolCallID: req.ToolCallID, Arguments: req.Arguments,
	})
	if err != nil {
		return nil, err
	}
	return &toolgateway.MCPToolResult{Data: append(json.RawMessage(nil), result.Content...), MimeType: "application/json", ResourceRefs: nonEmptyStrings(result.ArtifactRef)}, nil
}

type governedCapabilityToolRegistry struct {
	base toolgateway.ToolRegistry
}

func (r governedCapabilityToolRegistry) Get(ctx context.Context, name string, version string) (*toolgateway.ToolDefinition, error) {
	definition, err := r.base.Get(ctx, name, version)
	if err == nil {
		return definition, nil
	}
	if dynamic, ok, dynamicErr := dynamicMCPToolDefinitionFromContext(ctx, name, version); dynamicErr != nil {
		return nil, dynamicErr
	} else if ok {
		return &dynamic, nil
	}
	if dynamic, ok, dynamicErr := dynamicHTTPToolDefinitionFromContext(ctx, name, version); dynamicErr != nil {
		return nil, dynamicErr
	} else if ok {
		return &dynamic, nil
	}
	return nil, err
}

// dynamicHTTPToolDefinitionFromContext synthesizes a gateway ToolDefinition for a
// tenant-authored HTTP tool frozen into the run's ModelContextPackage. Credential
// headers are resolved from environment variables here, at execution time — the
// resolved secret lives only in this in-memory definition for the duration of the
// call and is never persisted or shown to the model.
func dynamicHTTPToolDefinitionFromContext(ctx context.Context, name, version string) (toolgateway.ToolDefinition, bool, error) {
	pkg, ok := agentruntime.ModelContextPackageFrom(ctx)
	if !ok {
		return toolgateway.ToolDefinition{}, false, nil
	}
	var matched *toolgateway.ToolDefinition
	for _, snapshot := range pkg.Capabilities.HTTPToolSnapshots {
		if snapshot.Name != name || snapshot.DefinitionHash == "" || agentruntime.HTTPToolMirrorVersion(snapshot.DefinitionHash) != version {
			continue
		}
		if matched != nil {
			return toolgateway.ToolDefinition{}, false, toolgateway.NewToolError(toolgateway.ErrorTypeInternal, "ambiguous HTTP tool mirror", false, nil)
		}
		headers := make(map[string]string, len(snapshot.HeaderEnv))
		for header, envName := range snapshot.HeaderEnv {
			value, present := os.LookupEnv(envName)
			if !present || value == "" {
				return toolgateway.ToolDefinition{}, false, toolgateway.NewToolError(toolgateway.ErrorTypeInternal, "http tool credential environment variable is not set", false, nil)
			}
			headers[http.CanonicalHeaderKey(header)] = value
		}
		risk := toolgateway.RiskLevel(snapshot.RiskLevel)
		if risk == "" {
			risk = toolgateway.RiskLow
		}
		policy := toolgateway.DefaultToolOutputPolicy()
		policy.RequireOutputSchema = false
		definition := toolgateway.ToolDefinition{
			Name:         snapshot.Name,
			Version:      version,
			Type:         toolgateway.ToolTypeHTTP,
			Description:  snapshot.Description,
			InputSchema:  append(json.RawMessage(nil), snapshot.InputSchema...),
			RiskLevel:    risk,
			Timeout:      time.Duration(snapshot.TimeoutMS) * time.Millisecond,
			Retry:        toolgateway.RetryPolicy{MaxAttempts: 1, Idempotent: !snapshot.Write},
			Permissions:  toolgateway.ToolPermissions{AllowedAgents: nonEmptyStrings(pkg.Run.AgentID), TenantScope: "tenant"},
			ResultPolicy: policy,
			Visibility:   observability.VisibilityUserVisible,
			HTTP: &toolgateway.HTTPToolSpec{
				Method:       snapshot.Method,
				URL:          snapshot.BaseURL,
				Headers:      headers,
				ResponseMode: snapshot.ResponseMode,
				Write:        snapshot.Write,
			},
			Metadata: map[string]string{
				toolgateway.MetadataHarnessSnapshotRef: snapshot.DefinitionHash,
			},
		}
		matched = &definition
	}
	if matched == nil {
		return toolgateway.ToolDefinition{}, false, nil
	}
	return *matched, true, nil
}

func (r governedCapabilityToolRegistry) ResolveSnapshot(ctx context.Context, req toolgateway.ResolveToolSnapshotRequest) (*toolgateway.ToolSnapshot, error) {
	return r.base.ResolveSnapshot(ctx, req)
}

func (r governedCapabilityToolRegistry) List(ctx context.Context) []toolgateway.ToolDefinition {
	if lister, ok := r.base.(toolgateway.ToolLister); ok {
		return lister.List(ctx)
	}
	return nil
}

func dynamicMCPToolDefinitionFromContext(ctx context.Context, name, version string) (toolgateway.ToolDefinition, bool, error) {
	pkg, ok := agentruntime.ModelContextPackageFrom(ctx)
	if !ok {
		return toolgateway.ToolDefinition{}, false, nil
	}
	var matched *toolgateway.ToolDefinition
	for _, snapshot := range pkg.Capabilities.MCPSnapshots {
		if snapshot.ID == "" || snapshot.ServerID == "" || snapshot.CapabilityHash == "" || snapshot.PolicyHash == "" {
			continue
		}
		for _, item := range snapshot.Tools {
			if item.Name != name || mcpGatewayMirrorVersion(snapshot, item) != version {
				continue
			}
			if matched != nil {
				return toolgateway.ToolDefinition{}, false, toolgateway.NewToolError(toolgateway.ErrorTypeInternal, "ambiguous MCP gateway mirror", false, nil)
			}
			policy := toolgateway.DefaultToolOutputPolicy()
			policy.RequireOutputSchema = false
			definition := toolgateway.ToolDefinition{
				Name:         item.Name,
				Version:      version,
				Type:         toolgateway.ToolTypeMCP,
				Description:  item.Description,
				InputSchema:  append(json.RawMessage(nil), item.InputSchema...),
				RiskLevel:    toolgateway.RiskLow,
				Retry:        toolgateway.RetryPolicy{MaxAttempts: 1, Idempotent: true},
				Permissions:  toolgateway.ToolPermissions{AllowedAgents: nonEmptyStrings(pkg.Run.AgentID), TenantScope: "tenant"},
				ResultPolicy: policy,
				Visibility:   observability.VisibilityUserVisible,
				MCP: &toolgateway.MCPToolSpec{
					ServerID:    snapshot.ServerID,
					SnapshotID:  "runtime-bound",
					MCPToolName: item.Name,
				},
				Metadata: map[string]string{
					"harness.mcp_snapshot_id":     snapshot.ID,
					"harness.mcp_capability_hash": snapshot.CapabilityHash,
					"harness.mcp_policy_hash":     snapshot.PolicyHash,
				},
			}
			matched = &definition
		}
	}
	if matched == nil {
		return toolgateway.ToolDefinition{}, false, nil
	}
	return *matched, true, nil
}

func mcpGatewayMirrorVersion(snapshot mcp.CapabilitySnapshot, tool mcp.Tool) string {
	sum := sha256.Sum256([]byte(snapshot.ServerID + "\x00" + snapshot.ID + "\x00" + snapshot.CapabilityHash + "\x00" + snapshot.PolicyHash + "\x00" + tool.Name + "\x00" + string(tool.InputSchema)))
	return "mcp-" + hex.EncodeToString(sum[:])[:16]
}

type configuredToolSchemaProvider struct{ registry toolgateway.ToolRegistry }

func (p configuredToolSchemaProvider) ResolveToolDefinitions(ctx context.Context, _ agentruntime.RunRequest, refs []string) ([]agentruntime.ModelToolDefinition, error) {
	result := make([]agentruntime.ModelToolDefinition, 0, len(refs))
	for _, ref := range refs {
		name, version, _ := strings.Cut(ref, "@")
		definition, err := p.registry.Get(ctx, name, version)
		if err != nil {
			return nil, err
		}
		result = append(result, agentruntime.ModelToolDefinition{Name: definition.Name, Description: definition.Description, Schema: append(json.RawMessage(nil), definition.InputSchema...)})
	}
	return result, nil
}

func (p configuredToolSchemaProvider) ResolveGovernedToolDefinitions(ctx context.Context, run agentruntime.RunRequest, refs []string) (agentruntime.GovernedToolDefinitionSnapshot, error) {
	definitions, err := p.ResolveToolDefinitions(ctx, run, refs)
	if err != nil {
		return agentruntime.GovernedToolDefinitionSnapshot{}, err
	}
	toolRefs := make([]toolgateway.ToolRef, 0, len(refs))
	for _, ref := range refs {
		name, version, ok := strings.Cut(ref, "@")
		if !ok || name == "" || version == "" {
			return agentruntime.GovernedToolDefinitionSnapshot{}, fmt.Errorf("invalid configured tool ref %q", ref)
		}
		toolRefs = append(toolRefs, toolgateway.ToolRef{Name: name, Version: version})
	}
	snapshot, err := p.registry.ResolveSnapshot(ctx, toolgateway.ResolveToolSnapshotRequest{
		TenantID: run.TenantID, AgentID: run.Definition.AgentID, ToolRefs: toolRefs, Trace: run.Trace,
	})
	if err != nil {
		return agentruntime.GovernedToolDefinitionSnapshot{}, err
	}
	frozenRefs := make([]string, 0, len(snapshot.ToolRefs))
	for _, ref := range snapshot.ToolRefs {
		frozenRefs = append(frozenRefs, ref.Name+"@"+ref.Version)
	}
	return agentruntime.GovernedToolDefinitionSnapshot{
		Snapshot: agentruntime.ToolSchemaSnapshot{
			SnapshotID: snapshot.SnapshotID, ToolRefs: frozenRefs, SchemaArtifactRef: snapshot.SchemaArtifactRef,
			CapabilityHash: snapshot.CapabilityHash, PolicyHash: snapshot.PolicyHash,
		},
		Definitions: definitions,
	}, nil
}

func configuredInvocationPolicy(req agentruntime.ToolInvocationRequest, def toolgateway.ToolDefinition) toolgateway.ToolCallPolicy {
	maxRetries := 0
	if def.Retry.Idempotent && def.Retry.MaxAttempts > 1 {
		maxRetries = def.Retry.MaxAttempts - 1
	}
	return toolgateway.ToolCallPolicy{
		RiskLevel: def.RiskLevel, RequireApproval: def.RiskLevel == toolgateway.RiskHigh,
		Timeout: def.Timeout, MaxRetries: maxRetries, IdempotencyKey: req.RunID + ":" + req.ToolCallID,
	}
}

func configuredMCPInvocationResolver(definitions []toolgateway.ToolDefinition) runtimeadapter.MCPInvocationBindingResolver {
	return runtimeadapter.MCPInvocationBindingResolverFunc(func(_ context.Context, req agentruntime.ToolInvocationRequest, snapshot mcp.CapabilitySnapshot, tool mcp.Tool) (runtimeadapter.MCPInvocationBinding, error) {
		for _, def := range definitions {
			if def.Type != toolgateway.ToolTypeMCP || def.MCP == nil || def.MCP.ServerID != snapshot.ServerID || def.MCP.MCPToolName != tool.Name {
				continue
			}
			return runtimeadapter.MCPInvocationBinding{
				GatewayToolName: def.Name, GatewayToolVersion: def.Version,
				Policy: configuredInvocationPolicy(req, def),
			}, nil
		}
		return runtimeadapter.MCPInvocationBinding{
			GatewayToolName:    tool.Name,
			GatewayToolVersion: mcpGatewayMirrorVersion(snapshot, tool),
			Policy: toolgateway.ToolCallPolicy{
				RiskLevel:      toolgateway.RiskLow,
				IdempotencyKey: req.RunID + ":" + req.ToolCallID,
			},
		}, nil
	})
}

type configuredContextRebuilder struct{}

func (configuredContextRebuilder) Rebuild(_ context.Context, req agentruntime.ModelContextRebuildRequest) (agentruntime.ModelContextPackage, error) {
	pkg := req.InitialPackage
	var arguments map[string]any
	if err := json.Unmarshal(req.ToolCall.Arguments, &arguments); err != nil {
		return agentruntime.ModelContextPackage{}, err
	}
	toolContent := req.Result.Content
	if toolContent == "" && req.Result.ContentRef != "" {
		toolContent = "artifact_ref:" + req.Result.ContentRef
	}
	if req.Result.IsError {
		toolContent = normalizeToolErrorContent(req.Result, toolContent)
	}
	pkg.PackageID = req.InitialPackage.PackageID + ".tool." + req.ToolCall.ToolCallID
	pkg.CreatedAt = time.Now()
	pkg.Messages.ConversationWindow = append(append([]agentruntime.ModelContextMessage(nil), pkg.Messages.ConversationWindow...),
		agentruntime.ModelContextMessage{Role: "assistant", ToolCalls: []contextpkg.ToolCall{{ID: req.ToolCall.ToolCallID, Name: req.ToolCall.Name, Arguments: arguments}}},
		agentruntime.ModelContextMessage{Role: "tool", Content: toolContent, ToolCallID: req.ToolCall.ToolCallID, ToolName: req.ToolCall.Name, ToolResult: &contextpkg.ToolResult{CallID: req.ToolCall.ToolCallID, Name: req.ToolCall.Name, Content: toolContent, IsError: req.Result.IsError}},
	)
	pkg.ContextHash = agentruntime.ComputeModelContextHash(pkg)
	return pkg, nil
}

func normalizeToolErrorContent(result agentruntime.ToolInvocationResult, content string) string {
	content = strings.TrimSpace(content)
	if content != "" {
		return content
	}
	if result.ContentRef != "" {
		return "tool_error: artifact_ref:" + result.ContentRef
	}
	return `{"error":"tool_call_failed","message":"工具调用失败。"}`
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result[value] = struct{}{}
		}
	}
	return result
}

func sortedKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	for i := 1; i < len(result); i++ {
		for j := i; j > 0 && result[j] < result[j-1]; j-- {
			result[j], result[j-1] = result[j-1], result[j]
		}
	}
	return result
}

func nonEmptyStrings(value string) []string {
	if value == "" {
		return nil
	}
	return []string{value}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
