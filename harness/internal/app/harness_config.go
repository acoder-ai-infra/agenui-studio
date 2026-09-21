package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/AGenUI/agenui-studio/harness/internal/identifiercontract"
	"gopkg.in/yaml.v3"
)

const HarnessConfigSchemaVersion = "harness.composition.v1"

type Environment string

const (
	EnvironmentLocal      Environment = "local"
	EnvironmentTesting    Environment = "testing"
	EnvironmentStaging    Environment = "staging"
	EnvironmentProduction Environment = "production"
)

func (e Environment) IsLocal() bool { return e == EnvironmentLocal }

type HarnessConfig struct {
	SchemaVersion string                `yaml:"schema_version"`
	Environment   Environment           `yaml:"environment"`
	Service       HarnessServiceConfig  `yaml:"service"`
	Paths         HarnessPathConfig     `yaml:"paths"`
	Logging       HarnessLoggingConfig  `yaml:"logging"`
	Components    HarnessComponentPaths `yaml:"components"`
	Runtime       HarnessRuntimeConfig  `yaml:"runtime"`
	Features      HarnessFeatureConfig  `yaml:"features"`
	MCPOAuth      HarnessMCPOAuthConfig `yaml:"mcp_oauth"`
}

type HarnessServiceConfig struct {
	Name                   string `yaml:"name"`
	Address                string `yaml:"address"`
	ReadHeaderTimeoutMS    int    `yaml:"read_header_timeout_ms"`
	ShutdownTimeoutSeconds int    `yaml:"shutdown_timeout_seconds"`
}

type HarnessPathConfig struct {
	RuntimeRoot string `yaml:"runtime_root"`
	DataDir     string `yaml:"data_dir"`
	LogDir      string `yaml:"log_dir"`
	RunDir      string `yaml:"run_dir"`
	TempDir     string `yaml:"temp_dir"`
}

type HarnessLoggingConfig struct {
	Development       bool                  `yaml:"development"`
	Level             string                `yaml:"level"`
	Output            string                `yaml:"output"`
	RecentBufferItems int                   `yaml:"recent_buffer_items"`
	File              *HarnessLogFileConfig `yaml:"file,omitempty"`
}

type HarnessLogFileConfig struct {
	Name       string `yaml:"name"`
	MaxSizeMB  int    `yaml:"max_size_mb"`
	MaxBackups int    `yaml:"max_backups"`
	MaxAgeDays int    `yaml:"max_age_days"`
	Compress   bool   `yaml:"compress"`
	LocalTime  bool   `yaml:"local_time"`
}

type HarnessComponentPaths struct {
	Models   HarnessSourcedComponent `yaml:"models"`
	Storage  string                  `yaml:"storage"`
	Artifact string                  `yaml:"artifact"`
	Auth     string                  `yaml:"auth"`
	Redis    string                  `yaml:"redis"`
	Skills   HarnessSourcedComponent `yaml:"skills"`
	// Tools 不参与 file|database 双源：tools.yaml 是平台工具目录（含治理属性），
	// 而 tool_configs / http_tools 两张表分别是它的租户级配置覆盖层与租户自建
	// 工具扩展层——三者分层互补而非同一份数据的两个来源，故保持纯路径字段。
	Tools   string                  `yaml:"tools"`
	MCP     HarnessSourcedComponent `yaml:"mcp"`
	Agents  HarnessSourcedComponent `yaml:"agents"`
	Prompts HarnessSourcedComponent `yaml:"prompts"`
}

// HarnessComponentSource 是组件配置的来源开关：file 只加载本地配置文件且
// 完全不读写数据库；database 只读数据库中的数据并忽略 path（ADR-0001/0002）。
type HarnessComponentSource string

const (
	ComponentSourceFile     HarnessComponentSource = "file"
	ComponentSourceDatabase HarnessComponentSource = "database"
)

// HarnessSourcedComponent 是 models/agents/prompts/mcp/skills 五个组件
// 的双源配置。旧的纯字符串写法不再兼容（ADR-0004），必须写成嵌套结构。
type HarnessSourcedComponent struct {
	Source HarnessComponentSource `yaml:"source"`
	Path   string                 `yaml:"path"`
}

// FromFile 报告组件是否声明为本地文件来源。
func (c HarnessSourcedComponent) FromFile() bool { return c.Source == ComponentSourceFile }

// FromDatabase 报告组件是否声明为数据库来源。
func (c HarnessSourcedComponent) FromDatabase() bool { return c.Source == ComponentSourceDatabase }

// validate 执行解析期校验：source 必填且合法；file 模式 path 必填；database
// 模式禁止出现 path（数据库是唯一事实源，残留 path 会误导读者以为文件仍生效）。
func (c HarnessSourcedComponent) validate(name string) error {
	switch c.Source {
	case ComponentSourceFile:
		if strings.TrimSpace(c.Path) == "" {
			return fmt.Errorf("components.%s: source \"file\" requires a non-empty path", name)
		}
		return nil
	case ComponentSourceDatabase:
		if strings.TrimSpace(c.Path) != "" {
			return fmt.Errorf("components.%s: source \"database\" must not declare a path (got %q)", name, c.Path)
		}
		return nil
	case "":
		return fmt.Errorf("components.%s: source is required (file|database)", name)
	default:
		return fmt.Errorf("components.%s: unsupported source %q (expected file or database)", name, c.Source)
	}
}

type HarnessRuntimeConfig struct {
	DefaultAgentID  string `yaml:"default_agent_id"`
	MaxConcurrent   int    `yaml:"max_concurrent"`
	ContextMessages int    `yaml:"context_messages"`
}

type HarnessFeatureConfig struct {
	A2A bool `yaml:"a2a"`
	// AllowInProcessCapabilities 仅保留旧配置严格解析兼容；双源组件的
	// file|database 选择已不受环境限制，Composition Root 忽略该字段。
	AllowInProcessCapabilities bool `yaml:"allow_in_process_capabilities"`
}

type HarnessMCPOAuthConfig struct {
	Enabled           bool   `yaml:"enabled"`
	PublicBaseURL     string `yaml:"public_base_url"`
	PublicBaseURLEnv  string `yaml:"public_base_url_env"`
	TokenKeyEnv       string `yaml:"token_key_env"`
	ClientName        string `yaml:"client_name"`
	PendingTTLSeconds int    `yaml:"pending_ttl_seconds"`
	AllowInsecureHTTP bool   `yaml:"allow_insecure_http"`
}

type ArtifactConfig struct {
	ObjectBackend   string `json:"object_backend"`
	MetadataBackend string `json:"metadata_backend"`
	Root            string `json:"root,omitempty"`
	DownloadBase    string `json:"download_base,omitempty"`
	// DownloadTokenKey is resolved once by the Composition Root and never
	// serialized into config snapshots or debug introspection.
	DownloadTokenKeyEnv string `json:"download_token_key_env,omitempty"`
	DownloadTokenKey    string `json:"-"`
	MaxObjectBytes      int64  `json:"max_object_bytes,omitempty"`
}

type CapabilityCatalog struct {
	SchemaVersion string   `yaml:"schema_version"`
	Enabled       []string `yaml:"enabled"`
	// Definitions are decoded again by the owning capability installer. Keeping
	// the generic envelope here avoids coupling app configuration to domain DTOs.
	Definitions []map[string]any `yaml:"definitions,omitempty"`
}

type ComponentConfigs struct {
	Models   ModelConfig
	Storage  StorageConfig
	Artifact ArtifactConfig
	Auth     AuthConfig
	Redis    RedisConfig
	Skills   CapabilityCatalog
	Tools    CapabilityCatalog
	MCP      CapabilityCatalog
}

func DefaultHarnessConfigPath(environment string) string {
	if strings.TrimSpace(environment) == "" {
		environment = string(EnvironmentLocal)
	}
	return filepath.Join("configs", "environments", environment, "harness.yaml")
}

func ValidateSelectedEnvironment(selected string, cfg HarnessConfig) error {
	if strings.TrimSpace(selected) == "" {
		return fmt.Errorf("selected environment is required")
	}
	if Environment(selected) != cfg.Environment {
		return fmt.Errorf("selected environment %q does not match config environment %q", selected, cfg.Environment)
	}
	return nil
}

func LoadHarnessConfig(path string) (HarnessConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return HarnessConfig{}, fmt.Errorf("read harness config %s: %w", path, err)
	}
	var cfg HarnessConfig
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return HarnessConfig{}, fmt.Errorf("decode harness config %s: %w", path, err)
	}
	if cfg.SchemaVersion != HarnessConfigSchemaVersion {
		return HarnessConfig{}, fmt.Errorf("unsupported harness config schema %q", cfg.SchemaVersion)
	}
	switch cfg.Environment {
	case EnvironmentLocal, EnvironmentTesting, EnvironmentStaging, EnvironmentProduction:
	default:
		return HarnessConfig{}, fmt.Errorf("unsupported environment %q", cfg.Environment)
	}
	if cfg.Runtime.DefaultAgentID == "" {
		return HarnessConfig{}, fmt.Errorf("harness config requires runtime.default_agent_id")
	}
	if err := identifiercontract.Validate(identifiercontract.AgentID(cfg.Runtime.DefaultAgentID)); err != nil {
		return HarnessConfig{}, fmt.Errorf("harness config runtime.default_agent_id: %w", err)
	}
	// 五个双源组件的 source/path 解析期校验（ADR-0008）。tools 不在其中。
	for _, component := range []struct {
		name  string
		value HarnessSourcedComponent
	}{
		{"models", cfg.Components.Models}, {"agents", cfg.Components.Agents}, {"prompts", cfg.Components.Prompts},
		{"mcp", cfg.Components.MCP}, {"skills", cfg.Components.Skills},
	} {
		if err := component.value.validate(component.name); err != nil {
			return HarnessConfig{}, err
		}
	}
	if cfg.Service.Name == "" {
		cfg.Service.Name = "harness"
	}
	if cfg.Service.Address == "" {
		cfg.Service.Address = ":8080"
	}
	if cfg.Service.ReadHeaderTimeoutMS <= 0 {
		cfg.Service.ReadHeaderTimeoutMS = 5000
	}
	if cfg.Service.ShutdownTimeoutSeconds <= 0 {
		cfg.Service.ShutdownTimeoutSeconds = 10
	}
	if cfg.Logging.RecentBufferItems <= 0 {
		cfg.Logging.RecentBufferItems = 2000
	}
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "info"
	}
	if cfg.Logging.Output == "" {
		cfg.Logging.Output = "stdout"
	}
	if err := validateHarnessLoggingConfig(cfg.Logging); err != nil {
		return HarnessConfig{}, err
	}
	if cfg.Runtime.MaxConcurrent <= 0 {
		cfg.Runtime.MaxConcurrent = 256
	}
	if cfg.Runtime.ContextMessages <= 0 {
		cfg.Runtime.ContextMessages = 200
	}
	if cfg.MCPOAuth.Enabled {
		if strings.TrimSpace(cfg.MCPOAuth.PublicBaseURL) == "" && strings.TrimSpace(cfg.MCPOAuth.PublicBaseURLEnv) != "" {
			value, ok := os.LookupEnv(strings.TrimSpace(cfg.MCPOAuth.PublicBaseURLEnv))
			if !ok || strings.TrimSpace(value) == "" {
				return HarnessConfig{}, fmt.Errorf("mcp_oauth public_base_url_env %s is not set", cfg.MCPOAuth.PublicBaseURLEnv)
			}
			cfg.MCPOAuth.PublicBaseURL = strings.TrimSpace(value)
		}
		if strings.TrimSpace(cfg.MCPOAuth.PublicBaseURL) == "" || strings.TrimSpace(cfg.MCPOAuth.TokenKeyEnv) == "" {
			return HarnessConfig{}, fmt.Errorf("mcp_oauth requires public_base_url or public_base_url_env, and token_key_env when enabled")
		}
		if cfg.MCPOAuth.PendingTTLSeconds <= 0 {
			cfg.MCPOAuth.PendingTTLSeconds = 600
		}
		if cfg.MCPOAuth.ClientName == "" {
			cfg.MCPOAuth.ClientName = "Harness Harness"
		}
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return HarnessConfig{}, err
	}
	resolve := func(value string) string {
		if value == "" || filepath.IsAbs(value) {
			return value
		}
		return filepath.Clean(filepath.Join(base, value))
	}
	cfg.Paths.RuntimeRoot = resolve(cfg.Paths.RuntimeRoot)
	if cfg.Paths.RuntimeRoot == "" {
		return HarnessConfig{}, fmt.Errorf("paths.runtime_root is required")
	}
	resolveRuntime := func(value, fallback string) string {
		if value == "" {
			value = fallback
		}
		if filepath.IsAbs(value) {
			return filepath.Clean(value)
		}
		return filepath.Clean(filepath.Join(cfg.Paths.RuntimeRoot, value))
	}
	cfg.Paths.DataDir = resolveRuntime(cfg.Paths.DataDir, "data")
	cfg.Paths.LogDir = resolveRuntime(cfg.Paths.LogDir, "log")
	cfg.Paths.RunDir = resolveRuntime(cfg.Paths.RunDir, "run")
	cfg.Paths.TempDir = resolveRuntime(cfg.Paths.TempDir, "tmp")
	cfg.Components.Storage, cfg.Components.Artifact = resolve(cfg.Components.Storage), resolve(cfg.Components.Artifact)
	cfg.Components.Auth, cfg.Components.Redis = resolve(cfg.Components.Auth), resolve(cfg.Components.Redis)
	cfg.Components.Tools = resolve(cfg.Components.Tools)
	// 双源组件仅在 file 模式下解析 path 并要求文件/目录存在；database 模式
	// 已在 validate 阶段禁止携带 path。
	for _, component := range []struct {
		name  string
		value *HarnessSourcedComponent
	}{
		{"models", &cfg.Components.Models}, {"agents", &cfg.Components.Agents}, {"prompts", &cfg.Components.Prompts},
		{"mcp", &cfg.Components.MCP}, {"skills", &cfg.Components.Skills},
	} {
		if !component.value.FromFile() {
			continue
		}
		component.value.Path = resolve(component.value.Path)
		if _, err := os.Stat(component.value.Path); err != nil {
			return HarnessConfig{}, fmt.Errorf("components.%s: path %s is not accessible: %w", component.name, component.value.Path, err)
		}
	}
	return cfg, nil
}

func validateHarnessLoggingConfig(cfg HarnessLoggingConfig) error {
	switch cfg.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("unsupported logging.level %q; expected debug, info, warn, or error", cfg.Level)
	}

	switch cfg.Output {
	case "stdout":
		if cfg.File != nil {
			return fmt.Errorf("logging.file must not be declared when logging.output is stdout")
		}
		return nil
	case "file", "both":
		if cfg.File == nil {
			return fmt.Errorf("logging.file is required when logging.output is %s", cfg.Output)
		}
	default:
		return fmt.Errorf("unsupported logging.output %q; expected stdout, file, or both", cfg.Output)
	}

	name := cfg.File.Name
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("logging.file.name is required")
	}
	if name != strings.TrimSpace(name) || filepath.IsAbs(name) || filepath.Base(name) != name || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("logging.file.name %q must be a base name without path separators", name)
	}
	if cfg.File.MaxSizeMB <= 0 {
		return fmt.Errorf("logging.file.max_size_mb must be greater than zero")
	}
	if cfg.File.MaxBackups <= 0 {
		return fmt.Errorf("logging.file.max_backups must be greater than zero")
	}
	if cfg.File.MaxAgeDays <= 0 {
		return fmt.Errorf("logging.file.max_age_days must be greater than zero")
	}
	return nil
}

func EnsureRuntimeDirectories(cfg HarnessConfig) error {
	for _, dir := range []string{cfg.Paths.DataDir, cfg.Paths.LogDir, cfg.Paths.RunDir, cfg.Paths.TempDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create runtime directory %s: %w", dir, err)
		}
	}
	return nil
}

func LoadComponentConfigs(cfg HarnessConfig) (ComponentConfigs, error) {
	var out ComponentConfigs
	componentItems := []struct {
		path     string
		target   any
		required bool
	}{
		{cfg.Components.Storage, &out.Storage, true}, {cfg.Components.Artifact, &out.Artifact, true},
		{cfg.Components.Auth, &out.Auth, true}, {cfg.Components.Redis, &out.Redis, true},
	}
	// models 仅在 file 模式下读取本地文件；database 模式下运行时只读库，
	// 启动配置保持零值（ADR-0001/0002）。
	if cfg.Components.Models.FromFile() {
		componentItems = append(componentItems, struct {
			path     string
			target   any
			required bool
		}{cfg.Components.Models.Path, &out.Models, true})
	}
	for _, item := range componentItems {
		if err := decodeStrictComponentYAML(item.path, item.target, item.required); err != nil {
			return out, err
		}
	}
	for _, item := range []struct {
		name   string
		source HarnessSourcedComponent
		target *CapabilityCatalog
	}{
		{"skills", cfg.Components.Skills, &out.Skills},
		{"mcp", cfg.Components.MCP, &out.MCP},
		// tools 恒定从本地文件加载（tools 不参与双源，见
		// HarnessComponentPaths.Tools 注释）。
		{"tools", HarnessSourcedComponent{Source: ComponentSourceFile, Path: cfg.Components.Tools}, &out.Tools},
	} {
		if !item.source.FromFile() {
			// database 模式的能力目录不读文件；置合法空目录占位。
			*item.target = CapabilityCatalog{SchemaVersion: "harness.capability-catalog.v1"}
			continue
		}
		if err := decodeStrictYAML(item.source.Path, item.target); err != nil {
			return out, err
		}
		if item.target.SchemaVersion != "harness.capability-catalog.v1" {
			return out, fmt.Errorf("%s catalog has unsupported schema %q", item.name, item.target.SchemaVersion)
		}
	}
	if err := validateComponentSourceBackends(cfg, out.Storage); err != nil {
		return out, err
	}
	if err := resolveComponentSecrets(&out); err != nil {
		return out, err
	}
	if err := normalizeComponentPaths(cfg, &out); err != nil {
		return out, err
	}
	if err := ValidateEnvironmentBackends(cfg, out); err != nil {
		return out, err
	}
	return out, nil
}

// validateComponentSourceBackends 是 ADR-0003 的 fail-fast 校验：任一组件
// 声明 source=database 但存储后端不具备 SQL 能力时，启动直接失败，不静默降级。
func validateComponentSourceBackends(cfg HarnessConfig, storageConfig StorageConfig) error {
	if databaseStorageBackend(storageConfig.Backend) {
		return nil
	}
	for _, component := range []struct {
		name  string
		value HarnessSourcedComponent
	}{
		{"models", cfg.Components.Models}, {"agents", cfg.Components.Agents}, {"prompts", cfg.Components.Prompts},
		{"mcp", cfg.Components.MCP}, {"skills", cfg.Components.Skills},
	} {
		if component.value.FromDatabase() {
			return fmt.Errorf("components.%s: source \"database\" requires a sqlite/mysql storage backend, got %q", component.name, storageConfig.Backend)
		}
	}
	return nil
}

// LoadStorageConfig loads only the database component required by migration
// jobs. It deliberately does not resolve model, auth, or Redis secrets.
func LoadStorageConfig(cfg HarnessConfig) (StorageConfig, error) {
	var storageConfig StorageConfig
	if err := decodeStrictComponentYAML(cfg.Components.Storage, &storageConfig, true); err != nil {
		return StorageConfig{}, err
	}
	if err := normalizeStorageConfig(cfg, &storageConfig); err != nil {
		return StorageConfig{}, err
	}
	switch storageConfig.Backend {
	case "memory", "":
		if !cfg.Environment.IsLocal() {
			return StorageConfig{}, fmt.Errorf("environment %s requires shared durable storage, got %q", cfg.Environment, storageConfig.Backend)
		}
	case "sqlite":
		// SQLite is an explicit deployment topology choice in every environment.
		// Path normalization and dependency initialization still fail closed.
	case "mysql":
		var value MySQLConfig
		if err := decodeStrictRawConfig(storageConfig.Config, &value); err != nil {
			return StorageConfig{}, fmt.Errorf("environment %s storage config is invalid: %w", cfg.Environment, err)
		}
		if err := validateMySQLBackendConfig(storageConfig.Backend, value, !cfg.Environment.IsLocal()); err != nil {
			return StorageConfig{}, fmt.Errorf("environment %s storage config is invalid: %w", cfg.Environment, err)
		}
	default:
		return StorageConfig{}, fmt.Errorf("unknown storage backend %q", storageConfig.Backend)
	}
	return storageConfig, nil
}

func normalizeComponentPaths(cfg HarnessConfig, out *ComponentConfigs) error {
	if err := normalizeStorageConfig(cfg, &out.Storage); err != nil {
		return err
	}
	if out.Artifact.ObjectBackend == "file" && !filepath.IsAbs(out.Artifact.Root) {
		out.Artifact.Root = filepath.Join(cfg.Paths.DataDir, out.Artifact.Root)
	}
	return nil
}

func normalizeStorageConfig(cfg HarnessConfig, storageConfig *StorageConfig) error {
	if storageConfig.Backend == "sqlite" {
		var value SQLiteConfig
		if err := decodeStrictRawConfig(storageConfig.Config, &value); err != nil {
			return fmt.Errorf("decode sqlite config: %w", err)
		}
		if value.Path == "" {
			value.Path = "sqlite/harness.db"
		}
		if !filepath.IsAbs(value.Path) {
			value.Path = filepath.Join(cfg.Paths.DataDir, value.Path)
		}
		storageConfig.Config, _ = json.Marshal(value)
	}
	if storageConfig.Backend == "mysql" {
		var value MySQLConfig
		if err := decodeStrictRawConfig(storageConfig.Config, &value); err != nil {
			return fmt.Errorf("decode %s config: %w", storageConfig.Backend, err)
		}
	}
	return nil
}

func decodeStrictRawConfig(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple documents are not allowed")
		}
		return err
	}
	return nil
}

func ValidateEnvironmentBackends(cfg HarnessConfig, components ComponentConfigs) error {
	local := cfg.Environment.IsLocal()
	if err := validateAuthConfig(components.Auth); err != nil {
		return fmt.Errorf("environment %s auth config is invalid: %w", cfg.Environment, err)
	}
	// models=database 时启动文件没有模型配置，模型合法性由管理面写入时校验
	// （validateProviderBuildable），此处跳过文件配置校验（ADR-0002）。
	if err := validateModelFileConfig(cfg, components, local); err != nil {
		return err
	}
	if components.Storage.Backend == "mysql" {
		var value MySQLConfig
		if err := decodeStrictRawConfig(components.Storage.Config, &value); err != nil {
			return fmt.Errorf("environment %s storage config is invalid: %w", cfg.Environment, err)
		}
		if err := validateMySQLBackendConfig(components.Storage.Backend, value, !local); err != nil {
			return fmt.Errorf("environment %s storage config is invalid: %w", cfg.Environment, err)
		}
	}
	if !local {
		if components.Storage.Backend == "memory" {
			return fmt.Errorf("environment %s requires shared durable storage, got %q", cfg.Environment, components.Storage.Backend)
		}
		if components.Artifact.ObjectBackend != "file" {
			return fmt.Errorf("environment %s requires file artifact object backend, got %q", cfg.Environment, components.Artifact.ObjectBackend)
		}
		if components.Storage.Backend == "sqlite" {
			if components.Artifact.MetadataBackend != "sqlite" {
				return fmt.Errorf("environment %s with sqlite storage requires sqlite artifact metadata backend, got %q", cfg.Environment, components.Artifact.MetadataBackend)
			}
		} else if components.Artifact.MetadataBackend != "mysql" {
			return fmt.Errorf("environment %s requires mysql artifact metadata backend, got %q", cfg.Environment, components.Artifact.MetadataBackend)
		}
		if !components.Redis.Enabled || len(components.Redis.Addrs) == 0 {
			return fmt.Errorf("environment %s requires redis coordination", cfg.Environment)
		}
		if components.Redis.SnapshotTTLs <= 0 {
			return fmt.Errorf("environment %s requires redis snapshot_ttl_seconds > 0", cfg.Environment)
		}
		if components.Redis.QuotaCommitTTLs < 30 || components.Redis.QuotaCommitTTLs > 3600 {
			return fmt.Errorf("environment %s requires redis quota_commit_ttl_seconds between 30 and 3600", cfg.Environment)
		}
		if strings.TrimSpace(components.Artifact.Root) == "" || components.Artifact.DownloadBase == "" {
			return fmt.Errorf("environment %s artifact root and download base are required", cfg.Environment)
		}
		if components.Artifact.DownloadTokenKeyEnv == "" || components.Artifact.DownloadTokenKey == "" {
			return fmt.Errorf("environment %s artifact download_token_key_env and resolved key are required", cfg.Environment)
		}
		if len([]byte(strings.TrimSpace(components.Artifact.DownloadTokenKey))) < artifactTokenSecretBytes {
			return fmt.Errorf("environment %s artifact download token key must contain at least %d bytes", cfg.Environment, artifactTokenSecretBytes)
		}
	}
	return nil
}

// validateModelFileConfig 校验启动文件里的模型路由。models=database 时启动
// 文件不承载模型配置，跳过校验（ADR-0002）。
func validateModelFileConfig(cfg HarnessConfig, components ComponentConfigs, local bool) error {
	if cfg.Components.Models.FromDatabase() {
		return nil
	}
	if err := validateModelProviderOptions(components.Models); err != nil {
		return err
	}
	for tenantID, tenant := range components.Models.TenantConfigs() {
		providers := make(map[string]ProviderConfig, len(tenant.Providers))
		for _, provider := range tenant.Providers {
			if provider.Name == "" || provider.Protocol == "" {
				return fmt.Errorf("model tenant %s has provider without name or protocol", tenantID)
			}
			if _, exists := providers[provider.Name]; exists {
				return fmt.Errorf("model tenant %s has duplicate provider %s", tenantID, provider.Name)
			}
			providers[provider.Name] = provider
		}
		provider, exists := providers[tenant.DefaultProvider]
		if !exists || tenant.DefaultModel == "" {
			return fmt.Errorf("model tenant %s has invalid default provider/model", tenantID)
		}
		if !local && provider.Protocol == "mock" {
			return fmt.Errorf("environment %s forbids mock model providers", cfg.Environment)
		}
		if !local {
			for _, value := range providers {
				if value.Protocol != "mock" && value.APIKeyEnv == "" {
					return fmt.Errorf("environment %s model provider %s must use api_key_env", cfg.Environment, value.Name)
				}
			}
		}
	}
	return nil
}

func decodeStrictYAML(path string, target any) error {
	if path == "" {
		return fmt.Errorf("required component config path is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read component config %s: %w", path, err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode component config %s: %w", path, err)
	}
	return nil
}

// decodeStrictComponentYAML keeps YAML as the sole human-authored format while
// reusing the canonical json tags and DisallowUnknownFields contracts on the
// existing strongly typed component configurations.
func decodeStrictComponentYAML(path string, target any, required bool) error {
	if path == "" {
		if required {
			return fmt.Errorf("required component config path is empty")
		}
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !required && os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read component config %s: %w", path, err)
	}
	var document any
	yamlDecoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := yamlDecoder.Decode(&document); err != nil {
		return fmt.Errorf("decode component config %s: %w", path, err)
	}
	var trailing any
	if err := yamlDecoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode component config %s: multiple YAML documents are not allowed", path)
		}
		return fmt.Errorf("decode component config %s: %w", path, err)
	}
	normalized, err := json.Marshal(document)
	if err != nil {
		return fmt.Errorf("normalize component config %s: %w", path, err)
	}
	jsonDecoder := json.NewDecoder(bytes.NewReader(normalized))
	jsonDecoder.DisallowUnknownFields()
	if err := jsonDecoder.Decode(target); err != nil {
		return fmt.Errorf("decode component config %s: %w", path, err)
	}
	return nil
}
