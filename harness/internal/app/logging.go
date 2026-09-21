package app

import (
	"fmt"
	"path/filepath"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// NewConfiguredLogger builds the managed logger declared by harness.yaml.
// EnsureRuntimeDirectories must run first so paths.log_dir exists. The caller
// owns the returned logger and must close it.
func NewConfiguredLogger(cfg HarnessConfig) (*observability.ZapLogger, error) {
	loggerConfig := observability.LoggerConfig{
		ServiceName: cfg.Service.Name,
		Environment: string(cfg.Environment),
		Development: cfg.Logging.Development,
		Level:       cfg.Logging.Level,
		Output:      cfg.Logging.Output,
	}
	if cfg.Logging.File != nil {
		loggerConfig.File = &observability.FileLoggerConfig{
			Filename:   filepath.Join(cfg.Paths.LogDir, cfg.Logging.File.Name),
			MaxSizeMB:  cfg.Logging.File.MaxSizeMB,
			MaxBackups: cfg.Logging.File.MaxBackups,
			MaxAgeDays: cfg.Logging.File.MaxAgeDays,
			Compress:   cfg.Logging.File.Compress,
			LocalTime:  cfg.Logging.File.LocalTime,
		}
	}
	logger, err := observability.NewZapLogger(loggerConfig)
	if err != nil {
		return nil, fmt.Errorf("build configured logger: %w", err)
	}
	return logger, nil
}
