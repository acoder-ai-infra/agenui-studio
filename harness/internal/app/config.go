// Package app is the composition root: it wires the Phase 1 storage ledger, the
// Phase 2 model gateway (multi-provider) and the Phase 3 protocol layer into one
// runnable HTTP server, and provides the glue types (gateway-backed runtime, run
// dispatcher) that connect them into an end-to-end "HTTP -> model stream -> SSE"
// path. Nothing imports app except cmd/harness.
package app

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	frameworkmysql "github.com/AGenUI/agenui-studio/harness/framework/mysql"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

// --- 模型网关配置 (按租户隔离) ---

// ProviderConfig declares one downstream model provider endpoint.
type ProviderConfig struct {
	Name                       string                    `json:"name"`
	Protocol                   string                    `json:"protocol"` // openai_compatible | anthropic
	ProviderKind               modelgateway.ProviderKind `json:"provider_kind,omitempty"`
	BaseURL                    string                    `json:"base_url"`
	APIKey                     string                    `json:"api_key"`
	APIKeyEnv                  string                    `json:"api_key_env,omitempty"`
	Models                     []string                  `json:"models"`
	PromptCacheSessionAffinity bool                      `json:"prompt_cache_session_affinity,omitempty"`
	// Costs is keyed by model ID because one provider commonly serves models
	// with different prices. Provider-level pricing would misattribute fallback.
	Costs        map[string]modelgateway.ModelCostTable  `json:"costs,omitempty"`
	Capability   modelgateway.ModelCapability            `json:"capability,omitempty"`
	Capabilities map[string]modelgateway.ModelCapability `json:"capabilities,omitempty"`
}

func validateModelProviderOptions(c ModelConfig) error {
	for tenantID, tenant := range c.TenantConfigs() {
		for _, provider := range tenant.Providers {
			if !modelgateway.IsKnownProviderKind(provider.ProviderKind) {
				return fmt.Errorf("model tenant %s provider %s has unsupported provider_kind %q", tenantID, provider.Name, provider.ProviderKind)
			}
			if provider.ProviderKind == modelgateway.ProviderKindSessionAffinity &&
				!modelgateway.PromptCacheSessionAffinityAllowed(provider.ProviderKind, provider.Protocol, provider.BaseURL) {
				return fmt.Errorf("model tenant %s provider %s session_affinity requires the session-affinity provider endpoint", tenantID, provider.Name)
			}
			if provider.PromptCacheSessionAffinity &&
				!modelgateway.PromptCacheSessionAffinityAllowed(provider.ProviderKind, provider.Protocol, provider.BaseURL) {
				return fmt.Errorf("model tenant %s provider %s prompt_cache_session_affinity requires the session-affinity provider", tenantID, provider.Name)
			}
		}
	}
	return nil
}

// TenantConfig 是单个租户的 provider 配置。每个租户独立配置自己的下游模型 provider、
// 默认 provider 和默认模型。
type TenantConfig struct {
	Providers       []ProviderConfig         `json:"providers"`
	DefaultProvider string                   `json:"default_provider"`
	DefaultModel    string                   `json:"default_model"`
	Quota           modelgateway.TenantQuota `json:"quota,omitempty"`
}

// ModelConfig 是模型网关配置,按租户隔离。Tenants 按 tenant_id 索引,
// 每个租户有独立的 provider 列表和默认路由。Default 是未命中租户时的兜底配置。
type ModelConfig struct {
	Tenants map[string]TenantConfig `json:"tenants,omitempty"`
	Default TenantConfig            `json:"default,omitempty"`
}

// TenantConfigs 返回完整的租户→配置映射(含 Default 兜底)。
// 如果 Tenants 为空,则返回 {"default": Default} 单条目。
func (c ModelConfig) TenantConfigs() map[string]TenantConfig {
	if len(c.Tenants) > 0 {
		return c.Tenants
	}
	return map[string]TenantConfig{"default": c.Default}
}

func validateModelCostAttribution(c ModelConfig) error {
	for tenantID, tenant := range c.TenantConfigs() {
		if tenant.Quota.CostBudget <= 0 && tenant.Quota.DailyCostBudget <= 0 {
			continue
		}
		providers := make(map[string]ProviderConfig, len(tenant.Providers))
		for _, provider := range tenant.Providers {
			providers[provider.Name] = provider
		}
		provider, ok := providers[tenant.DefaultProvider]
		if !ok {
			continue // ordinary provider validation owns this error
		}
		primaryCost := provider.Costs[tenant.DefaultModel]
		if !hasBillableCost(primaryCost) {
			return fmt.Errorf("model tenant %s enables cost budget but %s/%s has no billable cost table", tenantID, tenant.DefaultProvider, tenant.DefaultModel)
		}
		currency := primaryCost.Currency
		// Every configured provider/model pair is routable in buildFacade: the
		// default is primary and the rest form the fallback chain or ModelHint
		// targets. Budgets therefore require prices for every routable target.
		for _, fallback := range tenant.Providers {
			if fallback.Name == "" {
				continue
			}
			for _, model := range configuredProviderModels(fallback, tenant.DefaultModel) {
				fallbackCost := fallback.Costs[model]
				if !hasBillableCost(fallbackCost) {
					return fmt.Errorf("model tenant %s enables cost budget but %s/%s has no billable cost table", tenantID, fallback.Name, model)
				}
				if fallbackCost.Currency != currency {
					return fmt.Errorf("model tenant %s cost budget cannot aggregate currencies %s and %s", tenantID, currency, fallbackCost.Currency)
				}
			}
		}
	}
	return nil
}

func configuredProviderModels(provider ProviderConfig, defaultModel string) []string {
	if len(provider.Models) > 0 {
		return provider.Models
	}
	if defaultModel == "" {
		return nil
	}
	return []string{defaultModel}
}

func hasBillableCost(cost modelgateway.ModelCostTable) bool {
	return cost.Currency != "" && (cost.InputPer1K > 0 || cost.OutputPer1K > 0 || cost.ReasoningPer1K > 0 || cost.CacheReadPer1K > 0 || cost.CacheWritePer1K > 0)
}

// --- 存储配置 (与租户无关) ---

// StorageConfig 是存储后端的抽象配置。Backend 指定使用哪种存储实现,
// Config 是 YAML Loader 归一后的后端专属配置(由 buildStores 解析)。
//
// 示例:
//
//	{"backend": "sqlite", "config": {"path": "var/data/sqlite/harness.db"}}
//	{"backend": "memory"}
//	{"backend": "mysql", "config": {"name": "harness", "dbname": "harness", "user": "app", "password_env": "MYSQL_PASSWORD", "host": "mysql", "port": 3306}}
type StorageConfig struct {
	Backend string          `json:"backend"`          // "sqlite" | "memory" | "mysql"(P1)
	Config  json.RawMessage `json:"config,omitempty"` // backend 专属配置
}

// SQLiteConfig 是 SQLite 后端的专属配置。
type SQLiteConfig struct {
	Path string `json:"path"` // 数据库文件路径，必填
}

// MySQLConfig is the structured connection contract for a direct MySQL pool.
type MySQLConfig struct {
	Name              string `json:"name"`
	DBName            string `json:"dbname,omitempty"`
	DBNameEnv         string `json:"dbname_env,omitempty"`
	User              string `json:"user,omitempty"`
	Password          string `json:"password,omitempty"`
	PasswordEnv       string `json:"password_env,omitempty"`
	Host              string `json:"host,omitempty"`
	Port              int    `json:"port,omitempty"`
	Charset           string `json:"charset,omitempty"`
	ConnectTimeout    string `json:"connect_timeout,omitempty"`
	ReadTimeout       string `json:"read_timeout,omitempty"`
	WriteTimeout      string `json:"write_timeout,omitempty"`
	MaxOpenConns      int    `json:"max_open_conns,omitempty"`
	MaxIdleConns      int    `json:"max_idle_conns,omitempty"`
	ConnMaxLifetime   string `json:"conn_max_lifetime,omitempty"`
	ConnMaxIdleTime   string `json:"conn_max_idle_time,omitempty"`
	InterpolateParams bool   `json:"interpolate_params,omitempty"`
	UnitName          string `json:"unit_name,omitempty"`
	// SkipMigrationLedgerCheck 关闭运行时就绪的「账本校验」层(migration.RequireApplied)。
	// 默认 false:照常校验 harness_schema_migrations 里已登记各迁移标记。
	// 置 true:仅保留「结构校验」层(各 Validate*Schema,读真实库确认表/字段),跳过账本标记
	// 校验。用于表由平台 out-of-band 预建、从不经 SDK 迁移器、账本恒为空的库(如生产 publish
	// 库),避免账本标记缺失导致启动 fail-closed。
	SkipMigrationLedgerCheck bool `json:"skip_migration_ledger_check,omitempty"`
}

func (c MySQLConfig) resolveEnvironment() (MySQLConfig, error) {
	if c.Password != "" && c.PasswordEnv != "" {
		return MySQLConfig{}, fmt.Errorf("storage.config.password and storage.config.password_env must not both be set")
	}
	resolved := c
	if c.DBNameEnv != "" {
		value, ok := os.LookupEnv(c.DBNameEnv)
		if !ok {
			return MySQLConfig{}, fmt.Errorf("storage dbname environment variable %s is not set", c.DBNameEnv)
		}
		if strings.TrimSpace(value) == "" {
			return MySQLConfig{}, fmt.Errorf("storage dbname environment variable %s is empty", c.DBNameEnv)
		}
		resolved.DBName = value
	}
	if c.PasswordEnv != "" {
		value, ok := os.LookupEnv(c.PasswordEnv)
		if !ok {
			return MySQLConfig{}, fmt.Errorf("storage password environment variable %s is not set", c.PasswordEnv)
		}
		resolved.Password = value
	}
	return resolved, nil
}

func (c MySQLConfig) toFrameworkConfig() (frameworkmysql.Config, error) {
	resolved := c
	config := frameworkmysql.Config{
		Name:              resolved.Name,
		DBName:            resolved.DBName,
		User:              resolved.User,
		Password:          resolved.Password,
		Host:              resolved.Host,
		Port:              resolved.Port,
		Charset:           resolved.Charset,
		MaxOpenConns:      resolved.MaxOpenConns,
		MaxIdleConns:      resolved.MaxIdleConns,
		InterpolateParams: resolved.InterpolateParams,
		UnitName:          resolved.UnitName,
	}
	durations := []struct {
		field  string
		value  string
		target *time.Duration
	}{
		{field: "connect_timeout", value: resolved.ConnectTimeout, target: &config.ConnectTimeout},
		{field: "read_timeout", value: resolved.ReadTimeout, target: &config.ReadTimeout},
		{field: "write_timeout", value: resolved.WriteTimeout, target: &config.WriteTimeout},
		{field: "conn_max_lifetime", value: resolved.ConnMaxLifetime, target: &config.ConnMaxLifetime},
		{field: "conn_max_idle_time", value: resolved.ConnMaxIdleTime, target: &config.ConnMaxIdleTime},
	}
	for _, duration := range durations {
		if duration.value == "" {
			continue
		}
		parsed, err := time.ParseDuration(duration.value)
		if err != nil {
			return frameworkmysql.Config{}, fmt.Errorf("%s: %w", duration.field, err)
		}
		*duration.target = parsed
	}
	return config, nil
}

func validateMySQLBackendConfig(backend string, config MySQLConfig, nonLocal bool) error {
	if strings.TrimSpace(config.Name) == "" {
		return fmt.Errorf("storage.config.name is required for %s backend", backend)
	}
	if strings.TrimSpace(config.DBName) == "" && strings.TrimSpace(config.DBNameEnv) == "" {
		return fmt.Errorf("storage.config.dbname or storage.config.dbname_env is required for %s backend", backend)
	}
	switch backend {
	case "mysql":
		if strings.TrimSpace(config.Host) == "" {
			return fmt.Errorf("storage.config.host is required for mysql backend")
		}
		if strings.TrimSpace(config.User) == "" {
			return fmt.Errorf("storage.config.user is required for mysql backend")
		}
		if config.Port < 1 || config.Port > 65535 {
			return fmt.Errorf("storage.config.port must be between 1 and 65535 for mysql backend")
		}
		if nonLocal && strings.TrimSpace(config.PasswordEnv) == "" {
			if config.Password != "" {
				return fmt.Errorf("literal password in storage.config.password is not allowed for nonlocal mysql backend; use storage.config.password_env")
			}
			return fmt.Errorf("storage.config.password_env is required for nonlocal mysql backend")
		}
	default:
		return fmt.Errorf("unsupported MySQL storage backend %q", backend)
	}
	return nil
}

// --- Redis 配置 (可选的分布式协调层) ---

// RedisConfig 开启分布式的 cache/quota/broker/hotbuffer(替换默认的进程内实现)。
// Enabled=false 或配置文件缺失时,保持进程内实现不变(适合本地/CI/单实例)。
// 多实例部署时置 enabled=true 并指向共享 Redis,配额/实时事件即可跨进程一致。
type RedisConfig struct {
	Enabled         bool     `json:"enabled"`
	Addrs           []string `json:"addrs"`              // 单地址=standalone;多地址=cluster
	Username        string   `json:"username,omitempty"` // Redis ACL 用户名(默认空)
	Password        string   `json:"password"`
	PasswordEnv     string   `json:"password_env,omitempty"`
	DB              int      `json:"db"`
	KeyPrefix       string   `json:"key_prefix"`               // 命名空间,默认 "harness:"
	CacheTTLs       int      `json:"cache_ttl_seconds"`        // 模型响应缓存 TTL(秒),默认 600
	HotTTLs         int      `json:"hot_ttl_seconds"`          // HotStreamBuffer TTL(秒),默认 60
	SnapshotTTLs    int      `json:"snapshot_ttl_seconds"`     // Redis MCP/HotContext/可选快照 TTL(秒);共享环境必须>0
	QuotaLeaseTTLs  int      `json:"quota_lease_ttl_seconds"`  // 并发配额租约 TTL(秒),默认 120
	QuotaCommitTTLs int      `json:"quota_commit_ttl_seconds"` // 配额提交去重窗口(秒),默认 300
	QuotaFailOpen   bool     `json:"quota_fail_open"`          // Redis 不可用时配额行为:false=fail closed(默认),true=fail open
}

func resolveComponentSecrets(cfg *ComponentConfigs) error {
	for tenantID, tenant := range cfg.Models.Tenants {
		for i := range tenant.Providers {
			if err := resolveProviderSecret(&tenant.Providers[i]); err != nil {
				return fmt.Errorf("models tenant %s: %w", tenantID, err)
			}
		}
		cfg.Models.Tenants[tenantID] = tenant
	}
	for i := range cfg.Models.Default.Providers {
		if err := resolveProviderSecret(&cfg.Models.Default.Providers[i]); err != nil {
			return fmt.Errorf("models default: %w", err)
		}
	}
	if cfg.Redis.Enabled && cfg.Redis.PasswordEnv != "" {
		value, ok := os.LookupEnv(cfg.Redis.PasswordEnv)
		if !ok {
			return fmt.Errorf("redis secret environment variable %s is not set", cfg.Redis.PasswordEnv)
		}
		cfg.Redis.Password = value
	}
	if cfg.Auth.JWTSecretEnv != "" {
		value, ok := os.LookupEnv(cfg.Auth.JWTSecretEnv)
		if !ok {
			return fmt.Errorf("auth secret environment variable %s is not set", cfg.Auth.JWTSecretEnv)
		}
		cfg.Auth.JWTSecret = value
	}
	if cfg.Storage.Backend == "mysql" {
		var value MySQLConfig
		if err := decodeStrictRawConfig(cfg.Storage.Config, &value); err != nil {
			return err
		}
		if err := validateMySQLBackendConfig(cfg.Storage.Backend, value, false); err != nil {
			return err
		}
		resolved, err := value.resolveEnvironment()
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(resolved)
		if err != nil {
			return fmt.Errorf("encode resolved storage config: %w", err)
		}
		cfg.Storage.Config = encoded
	}
	if cfg.Artifact.DownloadTokenKeyEnv != "" {
		secret, ok := os.LookupEnv(cfg.Artifact.DownloadTokenKeyEnv)
		if !ok {
			return fmt.Errorf("artifact download token key environment variable %s is not set", cfg.Artifact.DownloadTokenKeyEnv)
		}
		cfg.Artifact.DownloadTokenKey = secret
	}
	return nil
}

func resolveProviderSecret(provider *ProviderConfig) error {
	if provider.APIKeyEnv == "" {
		return nil
	}
	value, ok := os.LookupEnv(provider.APIKeyEnv)
	if !ok {
		return fmt.Errorf("provider %s secret environment variable %s is not set", provider.Name, provider.APIKeyEnv)
	}
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("provider %s secret environment variable %s is empty", provider.Name, provider.APIKeyEnv)
	}
	provider.APIKey = value
	return nil
}
