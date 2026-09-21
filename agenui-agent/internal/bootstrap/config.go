package bootstrap

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	defaultListenAddress       = ":18081"
	defaultReadHeaderTimeout   = 5 * time.Second
	defaultShutdownTimeout     = 10 * time.Second
	defaultHeartbeatInterval   = 15 * time.Second
	defaultMaximumRequestBytes = int64(1 << 20)
)

type ProcessConfig struct {
	ListenAddress                string
	ReadHeaderTimeout            time.Duration
	ShutdownTimeout              time.Duration
	OperatorDetailEndpoint       string
	ExportDataSourceBaseURL      string
	DatabaseDriver               string
	DatabaseDSNEnv               string
	DesignKnowledgeRoot          string
	DesignKnowledgeRevision      string
	DesignKnowledgeReceiptKeyEnv string
	DesignKnowledgeWatch         DesignKnowledgeWatchConfig
	RendererCatalogRoot          string
	RuleWorker                   RuleWorkerConfig
	Bootstrap                    Config
	AllowedOrigins               []string
}

// RuleWorkerConfig controls the in-service layout-rule parse worker. An
// omitted section stays disabled: enabling a model-backed worker must be an
// explicit deployment decision with an explicit model boundary.
type RuleWorkerConfig struct {
	Enabled      bool
	PollInterval time.Duration
	OutputRoot   string
	OutputPrefix string
}

// DesignKnowledgeWatchConfig controls local revision hot reload. Revisions are
// immutable directories and current.json is atomically replaced by the rule
// worker, so no external configuration or object-storage service is required.
type DesignKnowledgeWatchConfig struct {
	Enabled         bool
	PollInterval    time.Duration
	RetainRevisions int
}

type fileConfig struct {
	Server          serverFileConfig          `toml:"server"`
	Harness         harnessFileConfig         `toml:"harness"`
	Agent           agentFileConfig           `toml:"agent"`
	HTTP            httpFileConfig            `toml:"http"`
	Database        databaseFileConfig        `toml:"database"`
	DesignKnowledge designKnowledgeFileConfig `toml:"design_knowledge"`
	Operator        operatorFileConfig        `toml:"operator"`
	Export          exportFileConfig          `toml:"export"`
	RuleWorker      ruleWorkerFileConfig      `toml:"rule_worker"`
	RendererCatalog rendererCatalogFileConfig `toml:"renderer_catalog"`
}

type rendererCatalogFileConfig struct {
	Root string `toml:"root"`
}

type exportFileConfig struct {
	DataSourceBaseURL string `toml:"data_source_base_url"`
}

type ruleWorkerFileConfig struct {
	Enabled      *bool  `toml:"enabled"` // pointer so an absent section defaults to on
	PollMS       int64  `toml:"poll_interval_ms"`
	OutputRoot   string `toml:"output_root"`
	OutputPrefix string `toml:"output_prefix"`
}

type serverFileConfig struct {
	ListenAddress       string   `toml:"listen_address"`
	ReadHeaderTimeoutMS int64    `toml:"read_header_timeout_ms"`
	ShutdownTimeoutMS   int64    `toml:"shutdown_timeout_ms"`
	AllowedOrigins      []string `toml:"allowed_origins"`
}

type harnessFileConfig struct {
	ConfigPath    string        `toml:"config_path"`
	Environment   string        `toml:"environment"`
	MigrationMode MigrationMode `toml:"migration_mode"`
}

type agentFileConfig struct {
	ID      string `toml:"id"`
	Version string `toml:"version"`
}

type httpFileConfig struct {
	MaxRequestBytes     int64 `toml:"max_request_bytes"`
	HeartbeatIntervalMS int64 `toml:"heartbeat_interval_ms"`
}

type databaseFileConfig struct {
	Driver string `toml:"driver"`
	DSNEnv string `toml:"dsn_env"`
}

type operatorFileConfig struct {
	Endpoint string `toml:"endpoint"`
}

type designKnowledgeFileConfig struct {
	Root            string `toml:"root"`
	Revision        string `toml:"revision"`
	ReceiptKeyEnv   string `toml:"receipt_key_env"`
	MaxDocs         int    `toml:"max_docs"`
	MaxBytes        int    `toml:"max_bytes"`
	WatchEnabled    bool   `toml:"watch_enabled"`
	WatchIntervalMS int64  `toml:"watch_interval_ms"`
	RetainRevisions int    `toml:"retain_revisions"`
}

// buildRuleWorkerConfig applies safe defaults. The model-backed worker is
// disabled unless local configuration explicitly opts in.
func buildRuleWorkerConfig(source ruleWorkerFileConfig) RuleWorkerConfig {
	enabled := false
	if source.Enabled != nil {
		enabled = *source.Enabled
	}
	cfg := RuleWorkerConfig{
		Enabled:      enabled,
		PollInterval: durationOrDefault(source.PollMS, 5*time.Second),
		OutputRoot:   strings.TrimSpace(source.OutputRoot),
		OutputPrefix: strings.TrimSpace(source.OutputPrefix),
	}
	if cfg.OutputRoot == "" {
		cfg.OutputRoot = "var/ruleworker/revisions"
	}
	if cfg.OutputPrefix == "" {
		cfg.OutputPrefix = "agenui/design-knowledge"
	}
	return cfg
}

// LoadProcessConfig reads one immutable process configuration snapshot.
// Environment variables remain the responsibility of secret providers and the
// Harness config; request handling never reparses this file.
func LoadProcessConfig(path string) (ProcessConfig, error) {
	if strings.TrimSpace(path) == "" {
		return ProcessConfig{}, errors.New("bootstrap config: path is required")
	}
	var source fileConfig
	metadata, err := toml.DecodeFile(path, &source)
	if err != nil {
		return ProcessConfig{}, fmt.Errorf("bootstrap config: decode %s: %w", path, err)
	}
	if undecoded := metadata.Undecoded(); len(undecoded) > 0 {
		return ProcessConfig{}, fmt.Errorf(
			"bootstrap config: unknown key %s",
			undecoded[0].String(),
		)
	}

	config := ProcessConfig{
		ListenAddress:           strings.TrimSpace(source.Server.ListenAddress),
		ReadHeaderTimeout:       durationOrDefault(source.Server.ReadHeaderTimeoutMS, defaultReadHeaderTimeout),
		ShutdownTimeout:         durationOrDefault(source.Server.ShutdownTimeoutMS, defaultShutdownTimeout),
		OperatorDetailEndpoint:  strings.TrimSpace(source.Operator.Endpoint),
		ExportDataSourceBaseURL: strings.TrimRight(strings.TrimSpace(source.Export.DataSourceBaseURL), "/"),
		DatabaseDriver:          strings.TrimSpace(source.Database.Driver),
		DatabaseDSNEnv:          strings.TrimSpace(source.Database.DSNEnv),
		DesignKnowledgeRoot:     strings.TrimSpace(source.DesignKnowledge.Root),
		DesignKnowledgeRevision: strings.TrimSpace(source.DesignKnowledge.Revision),
		RendererCatalogRoot:     strings.TrimSpace(source.RendererCatalog.Root),
		DesignKnowledgeReceiptKeyEnv: strings.TrimSpace(
			source.DesignKnowledge.ReceiptKeyEnv,
		),
		DesignKnowledgeWatch: DesignKnowledgeWatchConfig{
			Enabled:         source.DesignKnowledge.WatchEnabled,
			PollInterval:    durationOrDefault(source.DesignKnowledge.WatchIntervalMS, time.Second),
			RetainRevisions: source.DesignKnowledge.RetainRevisions,
		},
		RuleWorker:     buildRuleWorkerConfig(source.RuleWorker),
		AllowedOrigins: trimmedNonEmpty(source.Server.AllowedOrigins),
		Bootstrap: Config{
			HarnessConfigPath: strings.TrimSpace(source.Harness.ConfigPath),
			Environment:       strings.TrimSpace(source.Harness.Environment),
			MigrationMode:     source.Harness.MigrationMode,
			AgentID:           strings.TrimSpace(source.Agent.ID),
			AgentVersion:      strings.TrimSpace(source.Agent.Version),
			MaxRequestBytes:   source.HTTP.MaxRequestBytes,
			HeartbeatInterval: durationOrDefault(
				source.HTTP.HeartbeatIntervalMS,
				defaultHeartbeatInterval,
			),
			CloseTimeout:            durationOrDefault(source.Server.ShutdownTimeoutMS, defaultShutdownTimeout),
			DesignKnowledgeMaxDocs:  source.DesignKnowledge.MaxDocs,
			DesignKnowledgeMaxBytes: source.DesignKnowledge.MaxBytes,
			RendererCatalogRoot:     strings.TrimSpace(source.RendererCatalog.Root),
			ExportDataSourceBaseURL: strings.TrimRight(strings.TrimSpace(source.Export.DataSourceBaseURL), "/"),
		},
	}
	if config.ListenAddress == "" {
		config.ListenAddress = defaultListenAddress
	}
	if config.Bootstrap.MaxRequestBytes == 0 {
		config.Bootstrap.MaxRequestBytes = defaultMaximumRequestBytes
	}
	if source.Server.ReadHeaderTimeoutMS < 0 || source.Server.ShutdownTimeoutMS < 0 ||
		source.HTTP.HeartbeatIntervalMS < 0 || source.HTTP.MaxRequestBytes < 0 {
		return ProcessConfig{}, errors.New("bootstrap config: timeouts and size limits must not be negative")
	}
	if source.DesignKnowledge.MaxDocs < 0 ||
		source.DesignKnowledge.MaxBytes < 0 ||
		source.DesignKnowledge.WatchIntervalMS < 0 ||
		source.DesignKnowledge.RetainRevisions < 0 {
		return ProcessConfig{}, errors.New(
			"bootstrap config: image and design knowledge limits must not be negative",
		)
	}
	if (config.DesignKnowledgeRoot == "") != (config.DesignKnowledgeRevision == "") {
		return ProcessConfig{}, errors.New(
			"bootstrap config: design knowledge root and revision must be configured together",
		)
	}
	if (config.DatabaseDriver == "") != (config.DatabaseDSNEnv == "") {
		return ProcessConfig{}, errors.New(
			"bootstrap config: state driver and dsn_env must be configured together",
		)
	}
	if err := validateConfig(config.Bootstrap); err != nil {
		return ProcessConfig{}, err
	}
	return config, nil
}

func (c ProcessConfig) UsesOperatorRuntime() bool {
	return strings.TrimSpace(c.OperatorDetailEndpoint) != ""
}

func trimmedNonEmpty(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func durationOrDefault(milliseconds int64, fallback time.Duration) time.Duration {
	if milliseconds == 0 {
		return fallback
	}
	return time.Duration(milliseconds) * time.Millisecond
}
