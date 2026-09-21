package observability

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

type StructuredLogger interface {
	Debug(ctx context.Context, msg string, fields ...Field)
	Info(ctx context.Context, msg string, fields ...Field)
	Warn(ctx context.Context, msg string, fields ...Field)
	Error(ctx context.Context, msg string, err error, fields ...Field)
	With(fields ...Field) StructuredLogger
	Sync()
}

type LoggerConfig struct {
	ServiceName string
	Environment string
	Development bool
	Level       string
	Output      string
	Stdout      io.Writer
	File        *FileLoggerConfig
}

type FileLoggerConfig struct {
	Filename   string
	MaxSizeMB  int
	MaxBackups int
	MaxAgeDays int
	Compress   bool
	LocalTime  bool
}

type ZapLogger struct {
	logger    *zap.Logger
	closeOnce sync.Once
	closeFn   func() error
	closeErr  error
}

func NewZapLogger(cfg LoggerConfig) (*ZapLogger, error) {
	if cfg.Level == "" {
		cfg.Level = "info"
	}
	level := zap.NewAtomicLevel()
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		return nil, fmt.Errorf("parse log level %q: %w", cfg.Level, err)
	}
	if cfg.Output == "" {
		cfg.Output = "stdout"
	}
	if cfg.Output != "stdout" && cfg.Output != "file" && cfg.Output != "both" {
		return nil, fmt.Errorf("unsupported log output %q", cfg.Output)
	}
	if cfg.Output == "stdout" && cfg.File != nil {
		return nil, fmt.Errorf("file log config must not be set for stdout output")
	}
	if (cfg.Output == "file" || cfg.Output == "both") && cfg.File == nil {
		return nil, fmt.Errorf("file log config is required for output %q", cfg.Output)
	}

	cores := make([]zapcore.Core, 0, 2)
	if cfg.Output == "stdout" || cfg.Output == "both" {
		stdout := cfg.Stdout
		if stdout == nil {
			stdout = os.Stdout
		}
		var encoder zapcore.Encoder
		if cfg.Development {
			encoder = zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
		} else {
			encoder = zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
		}
		cores = append(cores, zapcore.NewCore(encoder, zapcore.Lock(zapcore.AddSync(stdout)), level))
	}

	var fileSink *lumberjack.Logger
	if cfg.Output == "file" || cfg.Output == "both" {
		if err := validateFileLoggerConfig(*cfg.File); err != nil {
			return nil, err
		}
		probe, err := os.OpenFile(cfg.File.Filename, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
		if err != nil {
			return nil, fmt.Errorf("open log file %s: %w", cfg.File.Filename, err)
		}
		if err := probe.Close(); err != nil {
			return nil, fmt.Errorf("close log file probe %s: %w", cfg.File.Filename, err)
		}
		fileSink = newRotatingFileSink(*cfg.File)
		cores = append(cores, zapcore.NewCore(
			zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
			zapcore.AddSync(fileSink),
			level,
		))
	}

	core := zapcore.NewTee(cores...)
	if !cfg.Development {
		core = zapcore.NewSamplerWithOptions(core, time.Second, 100, 100)
	}
	stacktraceLevel := zapcore.ErrorLevel
	if cfg.Development {
		stacktraceLevel = zapcore.WarnLevel
	}
	options := []zap.Option{zap.AddCaller(), zap.AddStacktrace(stacktraceLevel)}
	if cfg.Development {
		options = append(options, zap.Development())
	}
	zl := zap.New(core, options...)
	fields := make([]zap.Field, 0, 2)
	if cfg.ServiceName != "" {
		fields = append(fields, zap.String("service", cfg.ServiceName))
	}
	if cfg.Environment != "" {
		fields = append(fields, zap.String("env", cfg.Environment))
	}
	result := &ZapLogger{logger: zl.With(fields...)}
	if fileSink != nil {
		result.closeFn = fileSink.Close
	}
	return result, nil
}

func validateFileLoggerConfig(cfg FileLoggerConfig) error {
	if cfg.Filename == "" {
		return fmt.Errorf("file log filename is required")
	}
	if cfg.MaxSizeMB <= 0 || cfg.MaxBackups <= 0 || cfg.MaxAgeDays <= 0 {
		return fmt.Errorf("file log max size, backups, and age must be greater than zero")
	}
	return nil
}

func newRotatingFileSink(cfg FileLoggerConfig) *lumberjack.Logger {
	return &lumberjack.Logger{
		Filename:   cfg.Filename,
		MaxSize:    cfg.MaxSizeMB,
		MaxBackups: cfg.MaxBackups,
		MaxAge:     cfg.MaxAgeDays,
		Compress:   cfg.Compress,
		LocalTime:  cfg.LocalTime,
	}
}

// WrapZapLogger adapts a caller-owned Zap logger to StructuredLogger. The
// returned adapter never synchronizes or closes the caller's logger.
func WrapZapLogger(logger *zap.Logger) *ZapLogger {
	return &ZapLogger{logger: logger}
}

func (l *ZapLogger) Debug(ctx context.Context, msg string, fields ...Field) {
	l.logger.Debug(msg, appendTraceFields(ctx, fields)...)
}

func (l *ZapLogger) Info(ctx context.Context, msg string, fields ...Field) {
	l.logger.Info(msg, appendTraceFields(ctx, fields)...)
}

func (l *ZapLogger) Warn(ctx context.Context, msg string, fields ...Field) {
	l.logger.Warn(msg, appendTraceFields(ctx, fields)...)
}

func (l *ZapLogger) Error(ctx context.Context, msg string, err error, fields ...Field) {
	l.logger.Error(msg, append(appendTraceFields(ctx, fields), zap.Error(err))...)
}

func (l *ZapLogger) With(fields ...Field) StructuredLogger {
	return &ZapLogger{logger: l.logger.With(fields...)}
}

func (l *ZapLogger) Sync() {
	_ = l.logger.Sync()
}

// Close synchronizes the logger and closes only outputs created by
// NewZapLogger. It is safe to call concurrently and repeatedly.
func (l *ZapLogger) Close() error {
	l.closeOnce.Do(func() {
		_ = l.logger.Sync()
		if l.closeFn != nil {
			l.closeErr = l.closeFn()
		}
	})
	return l.closeErr
}

type loggerContextKey struct{}

func WithLogger(ctx context.Context, logger StructuredLogger) context.Context {
	return context.WithValue(ctx, loggerContextKey{}, logger)
}

func LoggerFrom(ctx context.Context, fallback StructuredLogger) StructuredLogger {
	if logger, ok := ctx.Value(loggerContextKey{}).(StructuredLogger); ok && logger != nil {
		return logger
	}
	return fallback
}

func appendTraceFields(ctx context.Context, fields []Field) []Field {
	tc, ok := TraceContextFrom(ctx)
	if !ok {
		return fields
	}
	traceFields := TraceFields(tc)
	if len(traceFields) == 0 {
		return fields
	}
	out := make([]Field, 0, len(traceFields)+len(fields))
	out = append(out, traceFields...)
	out = append(out, fields...)
	return out
}
