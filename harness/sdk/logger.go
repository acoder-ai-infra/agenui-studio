package harness

import (
	"context"
	"fmt"
	"reflect"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// LogLevel is the severity of a LogRecord delivered to a caller-owned Logger.
type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

// LogRecord is the logger-neutral structured event emitted by Harness.
// Fields include Harness standard fields such as service, env, trace_id,
// run_id and session_id when they are available. Error is populated only for
// error-level events.
type LogRecord struct {
	Level   LogLevel
	Message string
	Error   error
	Fields  map[string]any
}

// Logger is the public, logger-neutral injection interface used by WithLogger.
// Implementations may be called concurrently and must therefore be safe for
// concurrent use. Each call receives its own Fields map. The caller owns the
// implementation and its outputs for the entire Engine lifetime.
type Logger interface {
	Log(ctx context.Context, record LogRecord)
}

func adaptLogger(logger any) (observability.StructuredLogger, error) {
	if logger == nil {
		return nil, fmt.Errorf("requires a non-nil logger")
	}
	switch typed := logger.(type) {
	case *zap.Logger:
		if typed == nil {
			return nil, fmt.Errorf("requires a non-nil *zap.Logger")
		}
		return observability.WrapZapLogger(typed), nil
	case Logger:
		if isNilLogger(typed) {
			return nil, fmt.Errorf("requires a non-nil Logger")
		}
		return &loggerAdapter{logger: typed}, nil
	default:
		return nil, fmt.Errorf("unsupported type %T; want *zap.Logger or harness.Logger", logger)
	}
}

func isNilLogger(logger Logger) bool {
	value := reflect.ValueOf(logger)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// loggerAdapter bridges the public logger-neutral contract to Harness's
// internal structured logger without exposing zap.Field to SDK callers.
// It is immutable: With returns a new adapter, so concurrent logging is safe
// as long as the caller's Logger satisfies its documented concurrency contract.
type loggerAdapter struct {
	logger Logger
	fields []observability.Field
}

func (l *loggerAdapter) Debug(ctx context.Context, msg string, fields ...observability.Field) {
	l.log(ctx, LogLevelDebug, msg, nil, fields)
}

func (l *loggerAdapter) Info(ctx context.Context, msg string, fields ...observability.Field) {
	l.log(ctx, LogLevelInfo, msg, nil, fields)
}

func (l *loggerAdapter) Warn(ctx context.Context, msg string, fields ...observability.Field) {
	l.log(ctx, LogLevelWarn, msg, nil, fields)
}

func (l *loggerAdapter) Error(ctx context.Context, msg string, err error, fields ...observability.Field) {
	l.log(ctx, LogLevelError, msg, err, fields)
}

func (l *loggerAdapter) With(fields ...observability.Field) observability.StructuredLogger {
	bound := make([]observability.Field, 0, len(l.fields)+len(fields))
	bound = append(bound, l.fields...)
	bound = append(bound, fields...)
	return &loggerAdapter{logger: l.logger, fields: bound}
}

// Sync is intentionally a no-op: injected Logger lifecycle belongs to the
// caller, just like a directly injected *zap.Logger.
func (l *loggerAdapter) Sync() {}

func (l *loggerAdapter) log(ctx context.Context, level LogLevel, msg string, err error, fields []observability.Field) {
	allFields := make([]observability.Field, 0, len(l.fields)+len(fields)+16)
	allFields = append(allFields, l.fields...)
	if traceContext, ok := observability.TraceContextFrom(ctx); ok {
		allFields = append(allFields, observability.TraceFields(traceContext)...)
	}
	allFields = append(allFields, fields...)
	l.logger.Log(ctx, LogRecord{
		Level:   level,
		Message: msg,
		Error:   err,
		Fields:  encodeLogFields(allFields),
	})
}

func encodeLogFields(fields []observability.Field) map[string]any {
	encoder := zapcore.NewMapObjectEncoder()
	for _, field := range fields {
		field.AddTo(encoder)
	}
	return encoder.Fields
}
